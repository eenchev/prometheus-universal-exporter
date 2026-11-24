// Command prometheus-universal-exporter turns sources that were never meant
// for Prometheus, such as HTTP endpoints and files, into Prometheus targets,
// as configured by collectors. See README.md and docs/.
//
// The command line and the process lifecycle are here, the --dry-run report
// is in check.go and --healthcheck in healthcheck.go; everything else is
// under internal/.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
	// The IANA time zones, for a rule's time_zone: carried in the binary, so
	// a zone is known on a host or in an image that has no zone files. The
	// host's files are read first where there are any.
	_ "time/tzdata"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocclient"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/kubeapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// version is the release, set at build time with
//
//	go build -ldflags "-X main.version=1.4.0"
//
// Left empty, the module version that Go stamps into the binary is used: the
// tag for a build from a tagged checkout, or "(devel)". The revision is
// always the commit that Go stamps, when it built from a git checkout.
var version string

// revision is the commit, set at build time with -X main.revision where Go
// cannot stamp it: the image's build stage, which has the sources but not
// git. The commit Go stamps wins when there is one.
var revision string

func init() { applyVersion() }

// applyVersion hands the version set at build time to the build information.
func applyVersion() { exporter.Version, exporter.Revision = version, revision }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run owns the exporter lifecycle and returns the process exit status.
// Keeping it separate from main means that every deferred cleanup still runs
// on the paths that end early, and taking the arguments and output streams as
// parameters lets the command line be tested end to end.
//
// Exit statuses:
//   - 0 on a clean shutdown (including one whose --web.shutdown-timeout ran
//     out), a passing --dry-run, or a passing --healthcheck;
//   - 1 when the configuration is invalid, a --dry-run or a --healthcheck
//     fails, or the server stops with an error;
//   - 2 when the command line itself cannot be parsed.
//
// A second SIGTERM or SIGINT during the shutdown ends the process by that
// signal instead (exitOnSecondSignal).
func run(args []string, stdout, stderr io.Writer) int {
	// SIGHUP is caught before anything else, and until run returns. Its
	// default action ends the process, and a start can take seconds (the
	// configuration is read and its Python scripts are checked), during
	// which a reload sidecar or an operator's kill -HUP may already send
	// one. It waits in the channel, which holds one, and is the first thing
	// that the reload loop finds: one reload once the exporter has started.
	hup := exporter.ReloadSignals()
	defer signal.Stop(hup)
	o, status, ok := parseCommandLine(args, stdout, stderr)
	if !ok {
		return status
	}
	if o.showVersion {
		_, _ = io.WriteString(stdout, exporter.VersionString()+"\n")
		return 0
	}
	// Kubernetes requests name the exporter and its version (kubeapi).
	kubeapi.UserAgent = exporter.UserAgent("prometheus-universal-exporter")

	if o.healthcheck.endpoint != "" {
		return runHealthcheck(o.listenAddress, o.healthcheck.endpoint, stderr)
	}
	if o.printSchema || o.printCollectorFileSchema || o.printStaticTargetsSchema {
		return printSchema(o, stdout, stderr)
	}

	// Each document is read as its own flag says, and the manager is told so
	// its reloads keep expanding.
	var loadOptions, staticTargetsLoadOptions []config.LoadOption
	if o.expandEnv {
		loadOptions = append(loadOptions, config.WithEnvExpansion())
	}
	if o.expandStaticTargetsEnv {
		staticTargetsLoadOptions = append(staticTargetsLoadOptions, config.WithStaticTargetsEnvExpansion())
	}
	// In allocator mode a replica is known by its member name, which
	// otlp.exporter_instance names it by too; otherwise by its host name.
	var instanceName string
	if o.alloc != nil {
		instanceName = o.alloc.member
		loadOptions = append(loadOptions, config.WithExporterInstanceName(instanceName))
	}

	logger, otlpLogs := newExporterLogger(o.logLevel, stderr)
	if o.check {
		return runDryRun(o, instanceName, stdout, logger)
	}
	var manager *config.Manager
	var generation string
	if o.alloc != nil {
		o.configFile = o.alloc.configFile()
		manager, generation = o.alloc.startManager(logger, o.pythonPath, loadOptions, staticTargetsLoadOptions, o.watchConfig)
	} else if manager = loadStartupFiles(o, logger, loadOptions, staticTargetsLoadOptions); manager == nil {
		return 1
	}
	manager.SetPythonPath(o.pythonPath)
	manager.SetEnvExpansion(o.expandEnv)
	manager.SetStaticTargetsEnvExpansion(o.expandStaticTargetsEnv)
	manager.SetExporterInstanceName(instanceName)
	if o.watchConfig {
		manager.SetWatchInterval(o.watchInterval)
	}
	server := newServer(o, manager, logger, otlpLogs)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go manager.ReloadLoop(ctx)
	exporter.ApplyMemoryLimitRatio(o.memoryLimitRatio, logger)
	transform.PythonWorkers().SetMaxWorkers(o.pythonMaxWorkers)
	transform.PythonWorkers().SetMaxIdleWorkers(o.pythonMaxIdleWorkers)
	transform.PythonWorkers().SetIdleTimeout(o.pythonIdleTimeout)
	go transform.PythonWorkers().ReapLoop(ctx, transform.PythonWorkers().ReapInterval())
	// SIGHUP is caught until the process exits, not only until the first
	// SIGTERM. A reload sidecar sending one during the shutdown would
	// otherwise meet Go's default action and end the process at once,
	// cutting off the probes in flight and the last OTLP export. It has been
	// caught since run began; one sent during the start reloads now.
	reloadCtx, stopReloads := context.WithCancel(context.Background())
	defer stopReloads()
	go exporter.ReloadOn(reloadCtx, hup, manager, logger)
	// The static targets keep being scraped, and what they and the probes
	// queue keeps being exported, through --web.shutdown-delay, while the
	// endpoints are still served. The loops therefore have a context of
	// their own instead of the signal's.
	loopsCtx, stopLoops := context.WithCancel(context.Background())
	defer stopLoops()
	loopsDone := startServerLoops(loopsCtx, server)
	// The allocator client heartbeats and takes new bundles until run
	// returns: through --web.shutdown-delay too, its heartbeats saying the
	// exporter is shutting down.
	var allocClient *allocclient.Client
	stopAllocClient := func() {}
	if o.alloc != nil {
		o.alloc.staticTargetsPath = o.staticTargetsEndpoint
		var err error
		allocClient, stopAllocClient, err = o.alloc.startClient(manager, generation, logger)
		if err != nil {
			logger.Error("invalid command line; exiting", "error", err.Error())
			return 2
		}
		defer stopAllocClient()
		server.SetAllocatorStatus(allocatorStatus(allocClient, o.alloc.url, o.alloc.member))
	}

	logger.Info("starting exporter", startupFields(o, manager)...)
	httpServer := exporter.NewHTTPServer(o.listenAddress, server.Handler())
	serverErr := make(chan error, 1)
	go func() { serverErr <- httpServer.ListenAndServe() }()
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			return 1
		}
	case <-ctx.Done():
		// From here, a second SIGTERM or Ctrl-C ends the process at once,
		// saying so, instead of being swallowed while the probes finish and
		// the last OTLP export is sent. It is caught before the first
		// signal's handler is released, so no signal falls between the two.
		exitOnSecondSignal(server, logger)
		stop()
		shutDown(shutdown{
			options:         o,
			server:          server,
			httpServer:      httpServer,
			manager:         manager,
			allocClient:     allocClient,
			stopLoops:       stopLoops,
			loopsDone:       loopsDone,
			stopAllocClient: stopAllocClient,
			logger:          logger,
		})
	}
	return 0
}

// options are the command line's flags, read and checked.
type options struct {
	configFile               string
	listenAddress            string
	selfMetricsPath          string
	staticTargetsPath        string
	enableLifecycle          bool
	debugLevel               string
	debugDangerous           bool
	statusRefresh            string
	gzipLevel                int
	shutdownDelay            time.Duration
	shutdownTimeout          time.Duration
	timeoutOffset            time.Duration
	defaultProbeTimeout      time.Duration
	maxConcurrent            int
	memoryLimitRatio         float64
	pythonPath               string
	pythonMaxWorkers         int
	pythonMaxIdleWorkers     int
	pythonIdleTimeout        time.Duration
	targetFile               string
	watchConfig              bool
	watchInterval            time.Duration
	logLevel                 string
	expandEnv                bool
	expandStaticTargetsEnv   bool
	printSchema              bool
	printCollectorFileSchema bool
	printStaticTargetsSchema bool
	showVersion              bool
	allocator                allocatorFlags
	healthcheck              healthcheckFlag
	check                    bool

	// What the flags above are read into.
	selfMetricsEndpoint   string
	staticTargetsEndpoint string
	debug                 exporter.DebugLevel
	refresh               time.Duration
	alloc                 *allocatorMode
}

// commandLine is the exporter's flag set, with every flag read into o.
func commandLine(o *options) *flag.FlagSet {
	flags := flag.NewFlagSet("prometheus-universal-exporter", flag.ContinueOnError)
	// The flag package would print its own plain-text complaint to stderr;
	// it is silenced so every line on stderr stays JSON, and the error is
	// logged instead.
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.configFile, "config.file", "/etc/prometheus-universal-exporter/config.yaml", "Path to the exporter configuration")
	flags.StringVar(&o.listenAddress, "web.listen-address", ":8080", "Address on which to expose HTTP endpoints")
	flags.StringVar(&o.selfMetricsPath, "web.self-metrics-path", exporter.DefaultSelfMetricsPath, "Path of the exporter's own metrics, such as /metrics; it is served there and nowhere else")
	flags.StringVar(&o.staticTargetsPath, "web.static-targets-path", exporter.DefaultStaticTargetsPath, "Path the static targets' latest results are served at, for Prometheus to scrape")
	flags.BoolVar(&o.enableLifecycle, "web.enable-lifecycle", false, "Enable /-/reload, which on POST or PUT reloads the configuration and static target files and reports whether they were accepted. SIGHUP reloads either way")
	flags.StringVar(&o.debugLevel, "web.debug", "off", "Debugging collectors: off; edit, the /debug page, where a collector written there or a configured one is run on a pasted response and each stage of its trip shown, nothing fetched; or full, which adds fetching a target or a static target's scrape on that page, /probe?...&debug=true and /static-targets?debug=<name>, all of which show what the target answered")
	flags.BoolVar(&o.debugDangerous, "web.debug.dangerous", false, "With --web.debug=full only: let the debug page's playground, a collector written on the page, run Python, name the server's files (any *_file or *_files key, and login) and probe a target with its own request. Whoever can open the page can then run code and read files as the exporter's user; for a debugging session on one's own machine, never a shared deployment")
	flags.StringVar(&o.statusRefresh, "web.status-refresh", "off", "The status page's auto-refresh when its URL gives none with ?refresh=: off, or a duration in whole seconds from 5s to 24h, such as 30s or 1m. The page offers off and 5s to 1h, and keeps the interval chosen in its URL")
	flags.IntVar(&o.gzipLevel, "web.gzip-level", exporter.DefaultGzipLevel, "The gzip level of the answers of /probe, the self-metrics and the static targets to a client that accepts gzip, as Prometheus does: 1, the fastest, to 9, the smallest; or 0 to answer uncompressed whatever the client accepts")
	flags.DurationVar(&o.shutdownDelay, "web.shutdown-delay", 0, "How long a SIGTERM or SIGINT keeps serving, with /ready answering 503, before the graceful shutdown begins, so a load balancer or Kubernetes stops sending probes first. 0, the default, begins at once")
	flags.DurationVar(&o.shutdownTimeout, "web.shutdown-timeout", exporter.DefaultShutdownTimeout, "How long a SIGTERM or SIGINT waits for the probes in progress to finish before closing their connections. Keep it at least as long as Prometheus's scrape timeout")
	flags.DurationVar(&o.timeoutOffset, "probe.timeout-offset", exporter.DefaultTimeoutOffset, "How much of Prometheus's scrape timeout (X-Prometheus-Scrape-Timeout-Seconds) a probe leaves unused, so it answers with its own error before Prometheus gives up")
	flags.DurationVar(&o.defaultProbeTimeout, "probe.default-timeout", exporter.DefaultProbeTimeout, "How long a probe may take without an X-Prometheus-Scrape-Timeout-Seconds header, as from curl or a script. A timeout parameter bounds the request within it and cannot lift it. 0 leaves such a probe unbounded")
	flags.IntVar(&o.maxConcurrent, "probe.max-concurrent", 0, "How many trips to targets, probes and static target scrapes of every collector together, may be in progress at once, each on top of its collector's max_concurrent_probes. A probe over it is answered 503; a static target scrape waits. 0, the default, leaves them bounded only per collector")
	flags.Float64Var(&o.memoryLimitRatio, "runtime.memory-limit-ratio", 0, "Set the Go memory limit to this share of the container's memory limit, read from its cgroup at startup, so the Go runtime collects harder as its heap nears it and leaves the rest to the Python workers. 0, the default, leaves the Go runtime's own default; otherwise from 0.1 to 1, since below 0.1 the runtime would spend its time collecting garbage. GOMEMLIMIT in the environment wins")
	flags.StringVar(&o.pythonPath, "python.path", "python3", "Python interpreter used by the python transform")
	flags.IntVar(&o.pythonMaxWorkers, "python.max-workers", 0, "How many Python workers, of every collector together, may be alive at once, starting, running a script or idle. A run that finds none free takes the place of the idle worker unused for longest, or waits within its probe's deadline. 0, the default, leaves them bounded only per script")
	flags.IntVar(&o.pythonMaxIdleWorkers, "python.max-idle-workers", transform.PythonDefaultMaxIdleWorkers, "How many Python workers of each collector's script stay idle, ready for its next run, after a burst of concurrent scrapes. Each is a Python process of 11 MB or more. 0 keeps none: a worker stops when its run ends, and every run starts an interpreter")
	flags.DurationVar(&o.pythonIdleTimeout, "python.idle-timeout", transform.PythonDefaultIdleTimeout, "How long an idle Python worker is kept unused before it is stopped, at least 1s. Keep it longer than the scrape interval of the Python collectors, or each scrape starts an interpreter")
	flags.StringVar(&o.targetFile, "static-targets-file", "", "Optional file of static targets, scraped by the exporter on their intervals and served at --web.static-targets-path; a target with export_via_otlp is also delivered over OTLP")
	flags.BoolVar(&o.watchConfig, "config.watch", false, "Reload the configuration, collector and static target files when they change on disk, or a grpc collector's descriptor files do")
	flags.DurationVar(&o.watchInterval, "config.watch-interval", config.DefaultWatchInterval, "How often to check the configuration files for changes when config.watch is set")
	flags.StringVar(&o.logLevel, "log.level", "info", "Log level: debug, info, warn, or error")
	flags.BoolVar(&o.expandEnv, "config.expand-env", false, "Expand ${NAME} environment variable references in the configuration and collector files")
	flags.BoolVar(&o.expandStaticTargetsEnv, "static-targets.expand-env", false, "Expand ${NAME} environment variable references in the static target file")
	flags.BoolVar(&o.printSchema, "config.schema", false, "Print the JSON Schema of the configuration file, for editors, and exit")
	flags.BoolVar(&o.printCollectorFileSchema, "config.collector-file-schema", false, "Print the JSON Schema of a collector file listed under collector_files, for editors, and exit")
	flags.BoolVar(&o.printStaticTargetsSchema, "static-targets-file-schema", false, "Print the JSON Schema of the static target file, for editors, and exit")
	flags.BoolVar(&o.showVersion, "version", false, "Print the version, revision, Go version, request types and sql drivers of this build, and exit")
	flags.StringVar(&o.allocator.url, "allocator.url", "", "Take the configuration and this replica's static targets from the target allocator at this http:// URL, kept in --allocator.cache-dir, rather than from --config.file and --static-targets-file, which cannot be given with it")
	flags.StringVar(&o.allocator.cacheDir, "allocator.cache-dir", "", "Directory the bundle from the allocator is written into and loaded from, and a restart resumes from; required with --allocator.url")
	flags.StringVar(&o.allocator.member, "allocator.member", "", "This replica's name in the allocator's member list; the host name, such as the pod's name, when left out")
	flags.StringVar(&o.allocator.advertise, "allocator.advertise", "", "host:port the allocator tells Prometheus to send this replica's probes to; the listen address's host, or else the host's first address that is not a loopback one, with the listen port, when left out")
	flags.StringVar(&o.allocator.tokenFile, "allocator.token-file", "", "File holding the bearer token sent to the allocator, read again for every request")
	flags.Float64Var(&o.allocator.capacity, "allocator.capacity", 1, "This replica's share of the targets' weight against the other members', from 0.1 to 100 with at most three decimals: a replica of capacity 2 is given about twice the weight of one of capacity 1")
	flags.Var(&o.healthcheck, "healthcheck", "Ask the exporter at --web.listen-address whether it is ready, GET /ready, or with --healthcheck=health whether it is alive, GET /health, and exit 0 on 200 or 1 otherwise, within 5s, without starting the exporter or reading its configuration; for a container health check")
	flags.BoolVar(&o.check, "dry-run", false, "Validate the configuration and static target files as startup would, print a JSON report to stdout, and exit 0 if they are valid or 1 if not, without starting the exporter")
	return flags
}

// parseCommandLine reads and checks the command line. When ok is false, run
// returns status: 0 after --help, 2 for a command line that is wrong, which
// is logged to stderr.
func parseCommandLine(args []string, stdout, stderr io.Writer) (o *options, status int, ok bool) {
	o = &options{}
	flags := commandLine(o)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Asked for, so it goes to stdout, where help text belongs.
			flags.SetOutput(stdout)
			_, _ = io.WriteString(stdout, "Usage of prometheus-universal-exporter:\n")
			flags.PrintDefaults()
			return nil, 0, false
		}
		return nil, invalidCommandLine(stderr, err), false
	}
	// The flag package stops reading at the first argument that is not a
	// flag, and silently leaves every flag after it unread:
	// "--config.watch true --static-targets-file=targets.yaml" would start an
	// exporter with no static targets. No argument is taken that way, so one
	// is a mistake, most often a value written after its flag instead of
	// with it.
	if flags.NArg() > 0 {
		return nil, invalidCommandLine(stderr, fmt.Errorf("unexpected argument %q; flags take --name=value", flags.Arg(0))), false
	}
	if err := o.checkFlags(flags); err != nil {
		return nil, invalidCommandLine(stderr, err), false
	}
	return o, 0, true
}

// invalidCommandLine logs err as what is wrong with the command line and
// returns the exit status of that, 2.
func invalidCommandLine(stderr io.Writer, err error) int {
	newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
	return 2
}

// checkFlags checks the flags' values, in the order a command line with
// several wrong ones reports the first of, and reads the ones that are read
// into something else. A wrong value is a malformed flag rather than a
// configuration problem, so it is refused like one, before --dry-run or
// startup.
func (o *options) checkFlags(flags *flag.FlagSet) error {
	for _, err := range []error{
		exporter.ValidateListenAddress(o.listenAddress),
		exporter.ValidateShutdownDelay(o.shutdownDelay),
		exporter.ValidateShutdownTimeout(o.shutdownTimeout),
		exporter.ValidateTimeoutOffset(o.timeoutOffset),
		exporter.ValidateDefaultProbeTimeout(o.defaultProbeTimeout),
		exporter.ValidateGzipLevel(o.gzipLevel),
		exporter.ValidateMaxConcurrent(o.maxConcurrent),
		exporter.ValidateMemoryLimitRatio(o.memoryLimitRatio),
		transform.ValidateMaxWorkers(o.pythonMaxWorkers),
		transform.ValidateMaxIdleWorkers(o.pythonMaxIdleWorkers),
		transform.ValidateIdleTimeout(o.pythonIdleTimeout),
	} {
		if err != nil {
			return err
		}
	}
	var err error
	if o.selfMetricsEndpoint, err = exporter.SelfMetricsPath(o.selfMetricsPath); err != nil {
		return err
	}
	if o.staticTargetsEndpoint, err = exporter.StaticTargetsPath(o.staticTargetsPath, o.selfMetricsEndpoint); err != nil {
		return err
	}
	if o.debug, err = exporter.ParseDebugLevel(o.debugLevel); err != nil {
		return err
	}
	if err := exporter.ValidateDebugDangerous(o.debug, o.debugDangerous); err != nil {
		return err
	}
	if o.refresh, err = exporter.ParseStatusRefresh(o.statusRefresh); err != nil {
		return err
	}
	// A misspelt level would otherwise log at info without a word: debug
	// output that never comes, or warnings that were meant to be the least.
	if _, err := parseLogLevel(o.logLevel); err != nil {
		return err
	}
	// A non-positive watch interval is a flag's value, not the
	// configuration's, so it is a command-line error, like a negative
	// duration. Without --config.watch the interval is never read, and is
	// not checked.
	if err := validateWatchInterval(o.watchConfig, o.watchInterval); err != nil {
		return err
	}
	o.alloc, err = o.allocator.setUp(flags, o.listenAddress)
	return err
}

// printSchema prints the JSON Schema that the command line asks for.
func printSchema(o *options, stdout, stderr io.Writer) int {
	render := config.SchemaJSON
	switch {
	case o.printCollectorFileSchema:
		render = config.CollectorFileSchemaJSON
	case o.printStaticTargetsSchema:
		render = config.StaticTargetsSchemaJSON
	}
	schema, err := render()
	if err != nil {
		newLogger("info", stderr).Error("rendering the configuration schema failed", "error", err)
		return 1
	}
	_, _ = stdout.Write(schema)
	return 0
}

// runDryRun checks the configuration and static target files for
// --dry-run. In allocator mode the files checked are the bundle in the cache
// directory, which a restart would start with.
func runDryRun(o *options, instanceName string, stdout io.Writer, logger *slog.Logger) int {
	var configErr error
	if o.alloc != nil {
		o.configFile, o.targetFile, configErr = o.alloc.checkFiles()
	}
	return runCheck(checkInputs{
		ConfigFile:       o.configFile,
		ConfigError:      configErr,
		StaticTargetFile: o.targetFile,
		PythonPath:       o.pythonPath,
		ExpandEnv:        o.expandEnv,
		ExpandTargetsEnv: o.expandStaticTargetsEnv,
		ExporterInstance: instanceName,
		Watch:            o.watchConfig,
		WatchInterval:    o.watchInterval,
	}, stdout, logger)
}

// loadStartupFiles reads the configuration and static target files the
// exporter starts with, and returns the manager that holds them, or nil when
// they are not valid, which it logs.
func loadStartupFiles(o *options, logger *slog.Logger, loadOptions, staticTargetsLoadOptions []config.LoadOption) *config.Manager {
	// The files are stamped before they are read, so an edit made while
	// they are read is seen by the first watch tick. With the watch on, the
	// descriptor files that the configuration names are stamped as it is
	// read, before it is checked against them. Without the watch nothing
	// would compare that stamp, so the configuration is read without it.
	configStamp := config.TakeStamp(o.configFile, true, loadOptions...)
	targetsStamp := config.TakeStamp(o.targetFile, false)
	load := config.Load
	if o.watchConfig {
		load = func(path string, opts ...config.LoadOption) (*model.Config, error) {
			return config.LoadStamped(path, &configStamp, opts...)
		}
	}
	conf, err := load(o.configFile, loadOptions...)
	if err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return nil
	}
	config.LogNotices(logger, o.configFile, conf)

	if err := config.ValidatePythonScripts(o.pythonPath, conf); err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return nil
	}

	manager := config.NewManager(conf, o.configFile, logger)
	manager.UseStamp(configStamp)
	if o.targetFile == "" {
		return manager
	}
	targets, err := config.LoadStaticTargets(o.targetFile, staticTargetsLoadOptions...)
	if err == nil {
		err = config.ValidateStaticTargets(targets)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(targets, conf)
	}
	if err != nil {
		logger.Error("invalid static target configuration; exiting", "file", o.targetFile, "error", err)
		return nil
	}
	manager.SetTargets(o.targetFile, targets)
	manager.UseTargetsStamp(targetsStamp)
	logger.Info("static targets loaded", "file", o.targetFile, "targets", len(targets.Targets))
	return manager
}

// newServer is the exporter's server, set up as the flags say.
func newServer(o *options, manager *config.Manager, logger *slog.Logger, otlpLogs *exporter.OTLPLogs) *exporter.Server {
	server := exporter.NewServer(manager, o.pythonPath, logger)
	server.SetOTLPLogs(otlpLogs)
	server.SetSelfMetricsPath(o.selfMetricsEndpoint)
	server.SetStaticTargetsPath(o.staticTargetsEndpoint)
	server.SetTimeoutOffset(o.timeoutOffset)
	server.SetDefaultProbeTimeout(o.defaultProbeTimeout)
	server.SetMaxConcurrent(o.maxConcurrent)
	server.SetLifecycle(o.enableLifecycle)
	server.SetDebug(o.debug)
	server.SetDebugDangerous(o.debugDangerous)
	if o.debugDangerous {
		logger.Warn(exporter.DebugDangerousWarning)
	}
	server.SetStatusRefresh(o.refresh)
	server.SetGzipLevel(o.gzipLevel)
	if o.alloc != nil {
		server.SetAwaitingReason(o.alloc.awaitingSentence())
	}
	return server
}

// startServerLoops starts the server's loops of ctx: the OTLP exports of
// metrics, logs and traces, and the static target scrapes. The channel it
// returns is closed once all of them have returned.
func startServerLoops(ctx context.Context, server *exporter.Server) <-chan struct{} {
	loops := []func(context.Context){server.OTLPExportLoop, server.StaticScrapeLoop, server.OTLPLogExportLoop, server.OTLPTraceExportLoop}
	var running sync.WaitGroup
	for _, loop := range loops {
		running.Go(func() { loop(ctx) })
	}
	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()
	return done
}

// startupFields are the fields of the log line that says the exporter is
// starting.
func startupFields(o *options, manager *config.Manager) []any {
	conf := manager.Get()
	fields := []any{"version", exporter.BuildVersion().Version, "revision", exporter.BuildVersion().Revision, "address", o.listenAddress, "collectors", len(conf.Collectors), "collector_files", len(conf.LoadedCollectorFiles),
		"static_targets", len(manager.StaticTargets()), "config_watch", manager.WatchEnabled(),
		"config_expand_env", o.expandEnv, "static_targets_expand_env", o.expandStaticTargetsEnv, "request_types", fetch.BuiltRequestTypes(), "sql_drivers", fetch.BuiltSQLDrivers()}
	// The interval is only meaningful when the watch is on, and its absence
	// would otherwise leave the operator guessing how stale a running
	// configuration can be.
	if manager.WatchEnabled() {
		fields = append(fields, "config_watch_interval", manager.WatchInterval().String())
	}
	if a := o.alloc; a != nil {
		fields = append(fields, "allocator", a.url, "allocator_cache_dir", a.cacheDir, "allocator_member", a.member, "allocator_advertise", a.advertise, "allocator_capacity", a.capacity,
			"awaiting_configuration", manager.Awaiting())
	}
	// The name every OTLP resource carries, as the configuration resolved
	// it: a host name read at startup is otherwise nowhere to be seen.
	if e := conf.OTLP.ExporterInstance; conf.OTLP.Enabled && e.Enabled {
		fields = append(fields, "otlp_exporter_instance_attribute", e.Attribute, "otlp_exporter_instance", e.Value)
	}
	return fields
}

// shutdown is what the graceful shutdown stops.
type shutdown struct {
	*options
	server          *exporter.Server
	httpServer      *http.Server
	manager         *config.Manager
	allocClient     *allocclient.Client
	stopLoops       func()
	loopsDone       <-chan struct{}
	stopAllocClient func()
	logger          *slog.Logger
}

// shutDown is the graceful shutdown after a SIGTERM or SIGINT.
func shutDown(s shutdown) {
	logger := s.logger
	// /ready answers 503 from now, and connections are no longer kept
	// alive, so whatever still routes probes here moves away while this
	// exporter keeps answering them for --web.shutdown-delay.
	s.server.BeginShutdown()
	s.httpServer.SetKeepAlivesEnabled(false)
	// The allocator hears of the shutdown at once, and moves this replica's
	// share to the others, rather than when its heartbeats stop.
	if s.allocClient != nil {
		heartbeatCtx, cancel := context.WithTimeout(context.Background(), shutdownHeartbeatTimeout)
		s.allocClient.ShuttingDown(heartbeatCtx)
		cancel()
	}
	if s.shutdownDelay > 0 {
		logger.Info("shutting down: /ready answers 503 while probes are still served for --web.shutdown-delay; a second signal exits at once", "shutdown_delay", s.shutdownDelay.String())
		waitOutShutdownDelay(s.shutdownDelay)
	}
	// No static target scrape and no periodic export starts from here, and
	// the scrapes in flight are cut short at once, without publishing
	// anything, so a target is never reported down only because the
	// exporter stopped. They are not waited for: what they would publish is
	// served by this process alone, which is stopping, and a target that
	// does not answer would hold the shutdown for all of
	// --web.shutdown-timeout. The last OTLP export goes without them, with
	// what the earlier scrapes queued.
	s.stopLoops()
	s.server.AbortStaticScrapes()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	probes, others := s.server.InProgress()
	logger.Info(shutdownMessage(s.manager.Get().OTLP, probes, others, s.shutdownTimeout), "probes", probes, "other_requests", others, "shutdown_timeout", s.shutdownTimeout.String())
	if err := s.awaitProbes(shutdownCtx); err != nil {
		// The wait ran out: the probes still in progress are cut off, and
		// Prometheus records them as failed scrapes.
		probes, others := s.server.InProgress()
		logger.Error("shutting down: gave up waiting for "+inProgressText(probes, others)+"; --web.shutdown-timeout ran out and their connections are closed", "probes", probes, "other_requests", others, "shutdown_timeout", s.shutdownTimeout.String(), "error", err)
		_ = s.httpServer.Close()
	}
	// The probes have finished, and the export and static target scrape
	// loops have stopped, so what they queued goes out in one last export,
	// bounded by otlp.timeout.
	<-s.loopsDone
	// The allocator client stops once nothing is served any more, and is
	// waited for: a bundle it is writing is put in force or refused before
	// the process ends, rather than left half written.
	s.stopAllocClient()
	s.server.FlushOTLP()
	// Nothing reads any more: the connections kept across scrapes, an
	// sshfile collector's to its hosts and a sql collector's to its
	// databases, are closed rather than dropped with the process.
	fetch.CloseConnections()
}

// awaitProbes waits until the probes in progress have been answered, or ctx
// ends, which it returns as an error. Prometheus is waiting for their
// answers. The log says every shutdownProgressInterval what is still
// awaited, so a wait from a terminal does not look hung.
func (s shutdown) awaitProbes(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- s.httpServer.Shutdown(ctx) }()
	progress := time.NewTicker(shutdownProgressInterval)
	defer progress.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-progress.C:
			if probes, others := s.server.InProgress(); probes+others > 0 {
				deadline, _ := ctx.Deadline()
				left := max(time.Until(deadline), 0).Round(time.Second)
				s.logger.Info("shutting down: still waiting for "+inProgressText(probes, others)+", "+left.String()+" left", "probes", probes, "other_requests", others, "time_left", left.String())
			}
		}
	}
}

// waitOutShutdownDelay waits out --web.shutdown-delay. It is a variable only
// for the tests, which run the exporter in a child process and have it wait
// there for as long, and then until the test says that the delay may end
// (TestRunHelperProcess). What a test has the exporter do during its delay
// then has no bound but the test's own, whereas a delay of a few seconds can
// be over before a busy enough machine has done it. Only a test may set it,
// which a repository test checks (TestOnlyTestsHoldTheShutdownDelay).
var waitOutShutdownDelay = time.Sleep

// parseLogLevel reads --log.level: debug, info, warn or error, in any case.
func parseLogLevel(level string) (slog.Level, error) {
	if l, ok := model.LogLevel(level); ok {
		return l, nil
	}
	return slog.LevelInfo, fmt.Errorf("--log.level %q is not a level; use debug, info, warn or error", level)
}

// newLogger builds the exporter's logger, writing JSON lines to out at level
// (which parseLogLevel has checked), and installs it as the default. Every
// line that the exporter writes has to be JSON, and not all of them come from
// a logger passed down through the call chain: metric extraction reports a
// failed rule from deep inside a transform, where threading a logger through
// seven signatures would buy nothing. Those lines go through slog's default
// logger, which without this would be the text handler, and would emit a
// differently shaped line into an otherwise machine-readable stream.
func newLogger(level string, out io.Writer) *slog.Logger {
	l, _ := parseLogLevel(level)
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: l}))
	slog.SetDefault(logger)
	return logger
}

// newExporterLogger is newLogger for the running exporter: its lines go
// through the tee that exports them over OTLP when otlp.logs says so, as
// well as to out, and so do those of slog's default logger.
func newExporterLogger(level string, out io.Writer) (*slog.Logger, *exporter.OTLPLogs) {
	l, _ := parseLogLevel(level)
	logs := exporter.NewOTLPLogs(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: l}))
	logger := slog.New(logs.Handler())
	slog.SetDefault(logger)
	return logger, logs
}

// shutdownMessage is the line that the graceful shutdown begins with: what it
// waits for (the probes and the other requests in progress), for at most
// timeout, and, when OTLP export is on, that the last export is sent. It
// mentions the export only then, since FlushOTLP sends one only then; an
// exporter without OTLP used to say it would send an export that it never
// sent. With nothing to wait for and no export to send, the exporter exits at
// once and says only that.
func shutdownMessage(otlp model.OTLPConfig, probes, others int, timeout time.Duration) string {
	export := otlp.Enabled && otlp.Endpoint != ""
	switch {
	case probes+others > 0 && export:
		return "shutting down: waiting for " + inProgressText(probes, others) + ", at most " + timeout.String() + ", then sending the last OTLP export; a second signal exits at once"
	case probes+others > 0:
		return "shutting down: waiting for " + inProgressText(probes, others) + ", at most " + timeout.String() + "; a second signal exits at once"
	case export:
		return "shutting down: no probes in progress; sending the last OTLP export; a second signal exits at once"
	}
	return "shutting down: no probes in progress; exiting"
}

// inProgressText is what is in progress, in words: "1 probe", "2 probes and
// 1 other request", "1 request".
func inProgressText(probes, others int) string {
	counted := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	}
	switch {
	case probes > 0 && others > 0:
		return counted(probes, "probe", "probes") + " and " + counted(others, "other request", "other requests") + " in progress"
	case others > 0:
		return counted(others, "request", "requests") + " in progress"
	}
	return counted(probes, "probe", "probes") + " in progress"
}

// shutdownProgressInterval is how often a shutdown waiting for requests in
// progress says what it is still waiting for.
const shutdownProgressInterval = 5 * time.Second

// signalExitStatus is the status a shell reports for a process killed by
// sig, 128 and the signal's number, for a process that has to exit on the
// signal rather than be killed by it.
func signalExitStatus(sig os.Signal) int {
	if n, ok := sig.(syscall.Signal); ok {
		return 128 + int(n)
	}
	return 1
}

// exitOnSecondSignal ends the process on the next SIGTERM or SIGINT, after
// logging what that leaves unfinished. The process is ended by that signal,
// with the signal's default action, as it would have been without the
// handler: a shell reports 130 for SIGINT and 143 for SIGTERM, and a
// supervisor sees a process killed by the signal, not one that exited 0. A
// process that started with the signal ignored exits with that same status
// instead.
func exitOnSecondSignal(server *exporter.Server, logger *slog.Logger) {
	second := make(chan os.Signal, 1)
	signal.Notify(second, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-second
		probes, others := server.InProgress()
		line := "second signal: exiting now"
		if probes+others > 0 {
			line += ", " + inProgressText(probes, others) + " left unfinished"
		}
		logger.Warn(line, "signal", sig.String(), "probes", probes, "other_requests", others)
		signal.Reset(sig)
		if self, err := os.FindProcess(os.Getpid()); err == nil && self.Signal(sig) == nil {
			// The signal ends the process as soon as it is delivered; this
			// goroutine only waits for that. A process started with the
			// signal ignored, as a shell starts a background job with SIGINT,
			// goes back to ignoring it, and exits below instead.
			time.Sleep(time.Second)
		}
		os.Exit(signalExitStatus(sig))
	}()
}
