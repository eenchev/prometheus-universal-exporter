// Command prometheus-universal-exporter turns HTTP endpoints and files that
// were never meant for Prometheus into Prometheus targets, as configured by
// collectors. See README.md and docs/.
//
// The command line and the process lifecycle are here, and the --dry-run
// report in check.go; everything else is under internal/.
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
	"strings"
	"syscall"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// version is the release, set at build time with
//
//	go build -ldflags "-X main.version=1.4.0"
//
// Left empty, the module version Go stamps into the binary is used: the tag
// of a build from a tagged checkout, or "(devel)". The revision is always
// the commit Go stamps, when it built from a git checkout.
var version string

func init() { applyVersion() }

// applyVersion hands the version set at build time to the build information.
func applyVersion() { exporter.Version = version }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run owns the exporter lifecycle and returns the process exit status. Keeping
// it separate from main means every deferred cleanup still executes on the
// paths that terminate early, and taking the arguments and output streams as
// parameters lets the command line be tested end to end.
//
// Exit statuses: 0 on a clean shutdown or a passing --dry-run, 1 when the
// configuration is invalid, a --dry-run fails or the server stops with an error,
// and 2 when the command line itself cannot be parsed.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("prometheus-universal-exporter", flag.ContinueOnError)
	// The flag package would print its own plain-text complaint to stderr;
	// it is silenced so every line on stderr stays JSON, and the error is
	// logged below instead.
	flags.SetOutput(io.Discard)
	configFile := flags.String("config.file", "/etc/prometheus-universal-exporter/config.yaml", "Path to the exporter configuration")
	listenAddress := flags.String("web.listen-address", ":8080", "Address on which to expose HTTP endpoints")
	selfMetricsPath := flags.String("web.self-metrics-path", exporter.DefaultSelfMetricsPath, "Path of the exporter's own metrics, such as /metrics; it is served there and nowhere else")
	staticTargetsPath := flags.String("web.static-targets-path", exporter.DefaultStaticTargetsPath, "Path the static targets' latest results are served at, for Prometheus to scrape")
	enableLifecycle := flags.Bool("web.enable-lifecycle", false, "Enable POST /-/reload, which reloads the configuration and static target files and reports whether they were accepted. SIGHUP reloads either way")
	shutdownDelay := flags.Duration("web.shutdown-delay", 0, "How long a SIGTERM or SIGINT keeps serving, with /ready answering 503, before the graceful shutdown begins, so a load balancer or Kubernetes stops sending probes first. 0, the default, begins at once")
	shutdownTimeout := flags.Duration("web.shutdown-timeout", exporter.DefaultShutdownTimeout, "How long a SIGTERM or SIGINT waits for the probes in progress to finish before closing their connections. Keep it at least as long as Prometheus's scrape timeout")
	timeoutOffset := flags.Duration("probe.timeout-offset", exporter.DefaultTimeoutOffset, "How much of Prometheus's scrape timeout (X-Prometheus-Scrape-Timeout-Seconds) a probe leaves unused, so it answers with its own error before Prometheus gives up")
	defaultProbeTimeout := flags.Duration("probe.default-timeout", exporter.DefaultProbeTimeout, "How long a probe may take when it names no deadline: no X-Prometheus-Scrape-Timeout-Seconds header and no timeout parameter, as from curl or a script. 0 leaves such a probe unbounded")
	pythonPath := flags.String("python.path", "python3", "Python interpreter used by the python transform")
	targetFile := flags.String("static-targets-file", "", "Optional file of static targets, scraped by the exporter on their intervals and served at --web.static-targets-path; a target with export_via_otlp is also delivered over OTLP")
	watchConfig := flags.Bool("config.watch", false, "Reload the configuration, collector and static target files when they change on disk")
	watchInterval := flags.Duration("config.watch-interval", config.DefaultWatchInterval, "How often to check the configuration files for changes when config.watch is set")
	logLevel := flags.String("log.level", "info", "Log level: debug, info, warn, or error")
	expandEnv := flags.Bool("config.expand-env", false, "Expand ${NAME} environment variable references in the configuration and collector files")
	expandStaticTargetsEnv := flags.Bool("static-targets.expand-env", false, "Expand ${NAME} environment variable references in the static target file")
	printSchema := flags.Bool("config.schema", false, "Print the JSON Schema of the configuration file, for editors, and exit")
	printCollectorFileSchema := flags.Bool("config.collector-file-schema", false, "Print the JSON Schema of a collector file listed under collector_files, for editors, and exit")
	printStaticTargetsSchema := flags.Bool("static-targets-file-schema", false, "Print the JSON Schema of the static target file, for editors, and exit")
	showVersion := flags.Bool("version", false, "Print the version, revision, Go version and request types of this build, and exit")
	check := flags.Bool("dry-run", false, "Validate the configuration and static target files as startup would, print a JSON report to stdout, and exit 0 if they are valid or 1 if not, without starting the exporter")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Asked for, so it goes to stdout, where help text belongs.
			flags.SetOutput(stdout)
			_, _ = io.WriteString(stdout, "Usage of prometheus-universal-exporter:\n")
			flags.PrintDefaults()
			return 0
		}
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}
	// A negative offset is a malformed flag rather than a configuration
	// problem, so it is refused like one, before --dry-run or startup.
	if err := exporter.ValidateShutdownDelay(*shutdownDelay); err != nil {
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}
	if err := exporter.ValidateShutdownTimeout(*shutdownTimeout); err != nil {
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}
	if err := exporter.ValidateTimeoutOffset(*timeoutOffset); err != nil {
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}
	if err := exporter.ValidateDefaultProbeTimeout(*defaultProbeTimeout); err != nil {
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}
	selfMetricsEndpoint, err := exporter.SelfMetricsPath(*selfMetricsPath)
	if err != nil {
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}
	staticTargetsEndpoint, err := exporter.StaticTargetsPath(*staticTargetsPath, selfMetricsEndpoint)
	if err != nil {
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}
	// A misspelt level would otherwise log at info without a word: debug
	// output that never comes, or warnings that were meant to be the least.
	if _, err := parseLogLevel(*logLevel); err != nil {
		newLogger("info", stderr).Error("invalid command line; exiting", "error", err.Error())
		return 2
	}

	if *showVersion {
		_, _ = io.WriteString(stdout, exporter.VersionString()+"\n")
		return 0
	}

	if *printSchema || *printCollectorFileSchema || *printStaticTargetsSchema {
		render := config.SchemaJSON
		switch {
		case *printCollectorFileSchema:
			render = config.CollectorFileSchemaJSON
		case *printStaticTargetsSchema:
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

	// Each document is read as its own flag says, and the manager is told so
	// its reloads keep expanding.
	var loadOptions, staticTargetsLoadOptions []config.LoadOption
	if *expandEnv {
		loadOptions = append(loadOptions, config.WithEnvExpansion())
	}
	if *expandStaticTargetsEnv {
		staticTargetsLoadOptions = append(staticTargetsLoadOptions, config.WithStaticTargetsEnvExpansion())
	}

	logger := newLogger(*logLevel, stderr)
	if *check {
		return runCheck(checkInputs{
			ConfigFile:       *configFile,
			StaticTargetFile: *targetFile,
			PythonPath:       *pythonPath,
			ExpandEnv:        *expandEnv,
			ExpandTargetsEnv: *expandStaticTargetsEnv,
			Watch:            *watchConfig,
			WatchInterval:    *watchInterval,
		}, stdout, logger)
	}
	conf, err := config.Load(*configFile, loadOptions...)
	if err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}
	config.LogNotices(logger, *configFile, conf)

	if err := transform.ValidatePythonScripts(*pythonPath, conf); err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	if err := validateWatchInterval(*watchConfig, *watchInterval); err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	manager := config.NewManager(conf, *configFile, logger)
	manager.SetPythonPath(*pythonPath)
	manager.SetEnvExpansion(*expandEnv)
	manager.SetStaticTargetsEnvExpansion(*expandStaticTargetsEnv)
	if *watchConfig {
		manager.SetWatchInterval(*watchInterval)
	}
	if *targetFile != "" {
		targets, err := config.LoadStaticTargets(*targetFile, staticTargetsLoadOptions...)
		if err == nil {
			err = config.ValidateStaticTargets(targets)
		}
		if err == nil {
			err = config.ValidateStaticTargetsAgainst(targets, conf)
		}
		if err != nil {
			logger.Error("invalid static target configuration; exiting", "file", *targetFile, "error", err)
			return 1
		}
		manager.SetTargets(*targetFile, targets)
		logger.Info("static targets loaded", "file", *targetFile, "targets", len(targets.Targets))
	}
	server := exporter.NewServer(manager, *pythonPath, logger)
	server.SetSelfMetricsPath(selfMetricsEndpoint)
	server.SetStaticTargetsPath(staticTargetsEndpoint)
	server.SetTimeoutOffset(*timeoutOffset)
	server.SetDefaultProbeTimeout(*defaultProbeTimeout)
	server.SetLifecycle(*enableLifecycle)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go manager.ReloadLoop(ctx)
	go transform.PythonWorkers().ReapLoop(ctx, transform.PythonWorkerReapInterval)
	// SIGHUP is caught until the process exits, not only until the first
	// SIGTERM: a reload sidecar sending one during the shutdown would
	// otherwise meet Go's default action and end the process at once,
	// cutting off the probes in flight and the last OTLP export.
	reloadCtx, stopReloads := context.WithCancel(context.Background())
	defer stopReloads()
	go exporter.ReloadOn(reloadCtx, exporter.ReloadSignals(), manager, logger)
	// The static targets keep being scraped, and what they and the probes
	// queue keeps being exported, through --web.shutdown-delay, while the
	// endpoints are still served, so the two loops have a context of their
	// own rather than the signal's.
	loopsCtx, stopLoops := context.WithCancel(context.Background())
	defer stopLoops()
	exportLoopDone := make(chan struct{})
	go func() {
		defer close(exportLoopDone)
		server.OTLPExportLoop(loopsCtx)
	}()
	scrapeLoopDone := make(chan struct{})
	go func() {
		defer close(scrapeLoopDone)
		server.StaticScrapeLoop(loopsCtx)
	}()

	startup := []any{"version", exporter.BuildVersion().Version, "revision", exporter.BuildVersion().Revision, "address", *listenAddress, "collectors", len(conf.Collectors), "collector_files", len(conf.LoadedCollectorFiles),
		"static_targets", len(manager.StaticTargets()), "config_watch", manager.WatchEnabled(),
		"config_expand_env", *expandEnv, "static_targets_expand_env", *expandStaticTargetsEnv, "request_types", fetch.BuiltRequestTypes()}
	// The interval is only meaningful when the watch is on, and its absence
	// would otherwise leave the operator guessing how stale a running
	// configuration can be.
	if manager.WatchEnabled() {
		startup = append(startup, "config_watch_interval", manager.WatchInterval().String())
	}
	logger.Info("starting exporter", startup...)
	httpServer := exporter.NewHTTPServer(*listenAddress, server.Handler())
	serverErr := make(chan error, 1)
	go func() { serverErr <- httpServer.ListenAndServe() }()
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			return 1
		}
	case <-ctx.Done():
		// From here a second SIGTERM or Ctrl-C takes the default action and
		// ends the process at once, rather than being swallowed while the
		// probes finish and the last OTLP export is sent.
		stop()
		// /ready answers 503 from now, and connections are no longer kept
		// alive, so whatever still routes probes here moves away while this
		// exporter keeps answering them for --web.shutdown-delay.
		server.BeginShutdown()
		httpServer.SetKeepAlivesEnabled(false)
		if *shutdownDelay > 0 {
			logger.Info("shutting down: /ready answers 503 while probes are still served for --web.shutdown-delay; a second signal exits at once", "shutdown_delay", shutdownDelay.String())
			time.Sleep(*shutdownDelay)
		}
		logger.Info("shutting down: finishing the probes in progress and sending the last OTLP export; a second signal exits at once", "shutdown_timeout", shutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer cancel()
		// No static target scrape and no periodic export starts from here;
		// the scrapes in flight finish within --web.shutdown-timeout, or are
		// cut short without publishing anything, so a target is never
		// reported down only because the exporter stopped.
		stopLoops()
		go func() {
			<-shutdownCtx.Done()
			server.AbortStaticScrapes()
		}()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			// The wait ran out: the probes still in progress are cut off, and
			// Prometheus records them as failed scrapes.
			logger.Error("probes were still in progress when --web.shutdown-timeout ran out; their connections are closed", "shutdown_timeout", shutdownTimeout.String(), "error", err)
			_ = httpServer.Close()
		}
		// The probes have finished, and the export and static target scrape loops
		// have stopped, so what they queued goes out in one last export,
		// bounded by otlp.timeout.
		<-exportLoopDone
		<-scrapeLoopDone
		server.FlushOTLP()
	}
	return 0
}

// parseLogLevel reads --log.level: debug, info, warn or error, in any case.
func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("--log.level %q is not a level; use debug, info, warn or error", level)
}

// newLogger builds the exporter's logger, JSON lines to out at level, which
// parseLogLevel has checked, and installs it as the default. Every line the
// exporter writes has to be JSON, and not all of them come from a logger
// passed down through the call chain: metric extraction reports a failed rule
// from deep inside a transform, where threading a logger through seven
// signatures would buy nothing. Those lines go through slog's default logger,
// which without this would be the text handler and would emit a differently
// shaped line into the middle of an otherwise machine-readable stream.
func newLogger(level string, out io.Writer) *slog.Logger {
	l, _ := parseLogLevel(level)
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: l}))
	slog.SetDefault(logger)
	return logger
}
