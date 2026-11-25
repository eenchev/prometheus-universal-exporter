// Command pue-allocator is the target allocator of
// prometheus-universal-exporter. It serves every exporter replica run with
// --allocator.url its configuration and its share of the static targets, and
// it serves Prometheus the probe targets, each already pointed at a replica,
// through HTTP service discovery. See docs/ALLOCATOR.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/kubeapi"
)

// version and revision are set at build time as the exporter's are
// (-X main.version, -X main.revision), from the same release.
var version, revision string

func init() { exporter.Version, exporter.Revision = version, revision }

// shutdownTimeout bounds the graceful shutdown. Long polls end at its start
// (BeginShutdown), so only a heartbeat or a bundle being written is waited
// for.
const shutdownTimeout = 10 * time.Second

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run owns the allocator's lifecycle and returns the exit status: 0 on a
// clean shutdown, 1 when the input files are refused at the start or the
// server stops with an error, 2 when the command line cannot be parsed.
func run(args []string, stdout, stderr io.Writer) int {
	o, status, ok := parseCommandLine(args, stdout, stderr)
	if !ok {
		return status
	}
	if o.showVersion {
		b := exporter.BuildVersion()
		_, _ = fmt.Fprintf(stdout, "pue-allocator version %s (revision %s, %s, request types %s)\n", b.Version, b.Revision, b.GoVersion, strings.Join(b.RequestTypes, ","))
		return 0
	}
	// Its listings of EndpointSlices name the allocator and its version
	// (kubeapi).
	kubeapi.UserAgent = exporter.UserAgent("pue-allocator")
	if o.printStaticTargetsSchema || o.printProbeTargetsSchema {
		return printSchema(o, stdout, stderr)
	}

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: o.level}))
	slog.SetDefault(logger)
	var token *allocator.TokenFile
	if o.tokenFile != "" {
		var err error
		if token, err = allocator.NewTokenFile(o.tokenFile, logger); err != nil {
			logger.Error(err.Error()+"; exiting", "file", o.tokenFile)
			return 1
		}
	}
	paths := allocator.Paths{Config: o.configFile, StaticTargets: o.staticTargetsFile, ProbeTargets: o.probeTargetsFile, DiscoveryRefresh: o.discoveryRefresh}
	inputs, err := loadStartInputs(allocator.LoadInputs, paths, time.Sleep)
	if err != nil {
		logger.Error("invalid input files; exiting", "error", err)
		return 1
	}
	build := exporter.BuildVersion()
	alloc := allocator.New(inputs, allocator.Options{Settle: o.settle, HeartbeatInterval: o.heartbeatInterval, MaxMembers: o.maxMembers, MaxDiscoveredTargets: o.maxDiscovered, Logger: logger,
		Address: o.listenAddress, Version: build.Version, Revision: build.Revision, WatchInterval: o.watchInterval, BalanceFactor: o.balanceFactor})
	if o.peers != nil {
		if token != nil {
			o.peers.Token = token.Token
		}
		// The peers decide whether this instance asks the sources; RunPeers
		// starts once the API listens, so an instance asking its own address
		// finds itself.
		alloc.UsePeers(*o.peers)
	} else {
		// The sources are asked at once, well before the first member list
		// forms, a settle time after the start.
		alloc.StartDiscovery()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	watchDone := make(chan struct{})
	defer close(watchDone)
	if o.watchInterval > 0 {
		ticker := time.NewTicker(o.watchInterval)
		defer ticker.Stop()
		go alloc.WatchLoop(watchDone, ticker.C, paths)
	}
	logger.Info("starting allocator", append(o.startupFields(build.Version, token != nil), inputs.LogAttrs()...)...)
	handler := alloc.Handler("")
	if token != nil {
		handler = alloc.HandlerReading(token)
	}
	server := newServer(o.listenAddress, handler)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", o.listenAddress)
	if err != nil {
		logger.Error("HTTP server stopped", "error", err)
		return 1
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	if o.peers != nil {
		go alloc.RunPeers(ctx)
	}
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			return 1
		}
	case <-ctx.Done():
		stop()
		shutDown(alloc, server, o.peers, logger)
	}
	return 0
}

// options are the command line's flags, read and checked.
type options struct {
	listenAddress            string
	configFile               string
	staticTargetsFile        string
	probeTargetsFile         string
	tokenFile                string
	logLevel                 string
	watchInterval            time.Duration
	settle                   time.Duration
	heartbeatInterval        time.Duration
	balanceFactor            float64
	maxMembers               int
	discoveryRefresh         time.Duration
	maxDiscovered            int
	peerList                 string
	peerSelf                 string
	peerLeaderOnly           bool
	peerCheckInterval        time.Duration
	peerCheckTimeout         time.Duration
	peerFailureThreshold     int
	peerSnapshotTimeout      time.Duration
	showVersion              bool
	printStaticTargetsSchema bool
	printProbeTargetsSchema  bool

	// What the flags above are read into.
	level slog.Level
	// peers is how the instance runs beside its peers, nil without --peers.
	peers *allocator.PeerOptions
}

// commandLine is the allocator's flag set, with every flag read into o.
func commandLine(o *options) *flag.FlagSet {
	flags := flag.NewFlagSet("pue-allocator", flag.ContinueOnError)
	// Every line on stderr stays JSON, as the exporter's do.
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.listenAddress, "web.listen-address", ":9116", "Address on which to expose the allocator's API, status page, metrics and health endpoints")
	flags.StringVar(&o.configFile, "config.file", "/etc/prometheus-universal-exporter/config.yaml", "Path to the configuration the replicas are served, with the collector files it lists, which must be relative paths inside its directory")
	flags.StringVar(&o.staticTargetsFile, "static-targets-file", "", "Optional static target file, split among the replicas; a target's shard_key, when set, is hashed in place of its name")
	flags.StringVar(&o.probeTargetsFile, "probe-targets-file", "", "Optional probe targets file of probe_jobs, served to Prometheus at /v1/sd/<job>")
	flags.StringVar(&o.tokenFile, "web.token-file", "", "Optional file holding a bearer token every request but /healthz and /ready must carry; the status page, /v1/status and /metrics also take it as Basic auth's password, for a browser")
	flags.StringVar(&o.logLevel, "log.level", "info", "Log level: debug, info, warn, or error")
	flags.DurationVar(&o.watchInterval, "config.watch-interval", config.DefaultWatchInterval, "How often to read the input files again for changes, at most 24h; 0 reads them once")
	flags.DurationVar(&o.settle, "settle", allocator.DefaultSettle, "How long a changed member list must hold before targets move, at most 1h; a replica back within it keeps its share")
	flags.DurationVar(&o.heartbeatInterval, "heartbeat.interval", allocapi.DefaultHeartbeatInterval, "How often the replicas heartbeat, which every heartbeat reply tells them, from 1s to 2m; a replica not heard from for three intervals stops being a member")
	flags.Float64Var(&o.balanceFactor, "allocation.balance-factor", allocator.DefaultBalanceFactor, "How far above its fair share of the targets' weight, the total times its part of the members' capacity, a member's load may go before a target that hashes to it goes to the next member, from 1 to 10 with at most three decimals; 1 holds every member to its fair share, 10 lets hashing alone place the targets of up to 10 members")
	flags.IntVar(&o.maxMembers, "members.max", allocator.DefaultMaxMembers, "How many live members to take; a heartbeat of a new member beyond them is refused with 429 until one stops heartbeating")
	flags.DurationVar(&o.discoveryRefresh, "discovery.refresh-interval", 0, "How often to ask a discovery source that sets no refresh_interval, from 1s to 24h; 0 leaves each kind its own default: 1m for file_sd and http_sd, 30s for kubernetes and dns_sd")
	flags.IntVar(&o.maxDiscovered, "discovery.max-targets", allocator.DefaultMaxDiscoveredTargets, "How many targets one answer of a discovery source may hold, for a source that sets no max_targets; an answer with more is refused and the source keeps its last good targets")
	flags.StringVar(&o.peerList, "peers", "", "Optional allocator instances to run beside, as host:port entries, comma-separated: a name resolved to every address it has, as a headless Service's, or an IP address. List every instance, this one included, so each finds its own address among them. The live instance of the lowest address leads and alone asks the discovery sources; the others serve what it found")
	flags.StringVar(&o.peerSelf, "peers.self", "", "This instance's address as its peers reach it, an IP address (the port of --web.listen-address) or ip:port; left out, the instance finds itself among the peers' addresses by asking them")
	flags.BoolVar(&o.peerLeaderOnly, "peers.leader-only", false, "With --peers, only the leader serves the whole of /metrics and of the status page; a follower serves the peers' metrics and its own peer state alone, so a Prometheus scraping every instance has one instance's series of the allocation")
	flags.DurationVar(&o.peerCheckInterval, "peers.check-interval", allocator.DefaultPeerCheckInterval, "With --peers, how often each instance resolves --peers, checks every other instance and, as a follower, fetches the leader's snapshot, from 500ms to 1m")
	flags.DurationVar(&o.peerCheckTimeout, "peers.check-timeout", allocator.DefaultPeerCheckTimeout, "With --peers, how long a check of another instance, and a lookup of --peers, may take, from 100ms to less than --peers.check-interval")
	flags.IntVar(&o.peerFailureThreshold, "peers.failure-threshold", allocator.DefaultPeerFailureThreshold, "With --peers, how many checks in a row an instance must fail before it stops being live, the leader then being replaced, from 1 to 20; a follower whose snapshot fetches fail as often logs so")
	flags.DurationVar(&o.peerSnapshotTimeout, "peers.snapshot-timeout", allocator.DefaultPeerSnapshotTimeout, "With --peers, how long a follower's fetch of the leader's snapshot may take, from --peers.check-timeout to 5m")
	flags.BoolVar(&o.showVersion, "version", false, "Print the version, revision, Go version and request types of this build, and exit")
	flags.BoolVar(&o.printStaticTargetsSchema, "static-targets-file-schema", false, "Print the JSON Schema of the static target file, which may have discovered_targets, for editors, and exit")
	flags.BoolVar(&o.printProbeTargetsSchema, "probe-targets-file-schema", false, "Print the JSON Schema of the probe targets file, for editors, and exit")
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
			flags.SetOutput(stdout)
			_, _ = io.WriteString(stdout, "Usage of pue-allocator:\n")
			flags.PrintDefaults()
			return nil, 0, false
		}
		return nil, commandLineError(stderr, err), false
	}
	if flags.NArg() > 0 {
		return nil, commandLineError(stderr, fmt.Errorf("unexpected argument %q; flags take --name=value", flags.Arg(0))), false
	}
	if err := o.checkFlags(); err != nil {
		return nil, commandLineError(stderr, err), false
	}
	if err := o.readPeers(flags); err != nil {
		return nil, commandLineError(stderr, err), false
	}
	return o, 0, true
}

// checkFlags checks the flags' values, in the order a command line with
// several wrong ones reports the first of.
func (o *options) checkFlags() error {
	if err := exporter.ValidateListenAddress(o.listenAddress); err != nil {
		return err
	}
	var err error
	if o.level, err = parseLogLevel(o.logLevel); err != nil {
		return err
	}
	switch {
	case o.watchInterval < 0 || o.watchInterval > allocator.MaxWatchInterval:
		return fmt.Errorf("--config.watch-interval must be from 0 to 24h, got %s; 0 reads the files once", o.watchInterval)
	case o.settle < 0 || o.settle > allocator.MaxSettle:
		return fmt.Errorf("--settle must be from 0 to 1h, got %s", o.settle)
	case o.heartbeatInterval < allocapi.MinHeartbeatInterval || o.heartbeatInterval > allocapi.MaxHeartbeatInterval:
		return fmt.Errorf("--heartbeat.interval must be from 1s to 2m, got %s", o.heartbeatInterval)
	case o.maxMembers < 1:
		return fmt.Errorf("--members.max must be 1 or more, got %d", o.maxMembers)
	}
	if err := allocator.CheckBalanceFactor(o.balanceFactor); err != nil {
		return err
	}
	if o.discoveryRefresh != 0 && (o.discoveryRefresh < allocator.MinDiscoveryRefresh || o.discoveryRefresh > allocator.MaxDiscoveryRefresh) {
		return fmt.Errorf("--discovery.refresh-interval must be from 1s to 24h, or 0 for each kind's own default, got %s", o.discoveryRefresh)
	}
	if o.maxDiscovered < 1 {
		return fmt.Errorf("--discovery.max-targets must be 1 or more, got %d", o.maxDiscovered)
	}
	return nil
}

// readPeers reads --peers and the flags only it takes into o.peers.
func (o *options) readPeers(flags *flag.FlagSet) error {
	// The peers' timings given, by name, which only --peers takes.
	timingsGiven := []string{}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "peers.check-interval", "peers.check-timeout", "peers.failure-threshold", "peers.snapshot-timeout":
			timingsGiven = append(timingsGiven, f.Name)
		}
	})
	switch {
	case o.peerList != "":
		entries, err := allocator.ParsePeers(o.peerList)
		if err != nil {
			return err
		}
		if err := checkPeerTimings(o.peerCheckInterval, o.peerCheckTimeout, o.peerFailureThreshold, o.peerSnapshotTimeout); err != nil {
			return err
		}
		o.peers = &allocator.PeerOptions{Peers: entries, LeaderOnly: o.peerLeaderOnly,
			CheckInterval: o.peerCheckInterval, CheckTimeout: o.peerCheckTimeout, FailureThreshold: o.peerFailureThreshold, SnapshotTimeout: o.peerSnapshotTimeout}
		if o.peerSelf != "" {
			if o.peers.Self, err = allocator.ParseSelf(o.peerSelf, o.listenAddress); err != nil {
				return err
			}
		}
	case o.peerSelf != "":
		return errors.New("--peers.self is given without --peers")
	case o.peerLeaderOnly:
		return errors.New("--peers.leader-only is given without --peers: an instance without peers is its own leader and serves everything")
	case len(timingsGiven) > 0:
		return fmt.Errorf("--%s is given without --peers: an instance without peers checks none", timingsGiven[0])
	}
	return nil
}

// printSchema prints the JSON Schema that the command line asks for.
func printSchema(o *options, stdout, stderr io.Writer) int {
	render := allocator.StaticTargetsSchemaJSON
	if o.printProbeTargetsSchema {
		render = allocator.ProbeTargetsSchemaJSON
	}
	schema, err := render()
	if err != nil {
		slog.New(slog.NewJSONHandler(stderr, nil)).Error("rendering the schema failed", "error", err.Error())
		return 1
	}
	_, _ = stdout.Write(schema)
	return 0
}

// startupFields are the fields of the log line that says the allocator is
// starting, but for its inputs'.
func (o *options) startupFields(version string, token bool) []any {
	return []any{"version", version, "address", o.listenAddress,
		"config_file", o.configFile, "static_targets_file", o.staticTargetsFile, "probe_targets_file", o.probeTargetsFile,
		"watch_interval", o.watchInterval.String(), "settle", o.settle.String(), "heartbeat_interval", o.heartbeatInterval.String(), "max_members", o.maxMembers, "max_discovered_targets", o.maxDiscovered, "discovery_refresh_interval", o.discoveryRefresh.String(), "token", token,
		"peers", o.peerList, "peers_self", o.peerSelf, "peers_leader_only", o.peerLeaderOnly, "peers_check_interval", o.peerCheckInterval.String(), "peers_check_timeout", o.peerCheckTimeout.String(),
		"peers_failure_threshold", o.peerFailureThreshold, "peers_snapshot_timeout", o.peerSnapshotTimeout.String()}
}

// shutDown is the graceful shutdown after a SIGTERM or SIGINT.
func shutDown(alloc *allocator.Allocator, server *http.Server, peers *allocator.PeerOptions, logger *slog.Logger) {
	logger.Info("shutting down; a second signal exits at once")
	alloc.BeginShutdown()
	if peers != nil {
		// The peers check /v1/peer every --peers.check-interval. Serving on,
		// with 503 there, until each has checked lets the next instance lead
		// at once; a closed listener would only be counted out after
		// --peers.failure-threshold refused checks.
		delay := peers.ShutdownDelay()
		logger.Info("answering the peers that this instance shuts down before stopping the server", "delay", delay.String())
		time.Sleep(delay)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
	}
}

// checkPeerTimings holds the peers' timings to their bounds
// (allocator.MinPeerCheckInterval, ...), each given or its default.
func checkPeerTimings(interval, timeout time.Duration, threshold int, snapshot time.Duration) error {
	switch {
	case interval < allocator.MinPeerCheckInterval || interval > allocator.MaxPeerCheckInterval:
		return fmt.Errorf("--peers.check-interval must be from 500ms to 1m, got %s", interval)
	case timeout < allocator.MinPeerCheckTimeout || timeout >= interval:
		return fmt.Errorf("--peers.check-timeout must be from 100ms to less than --peers.check-interval (%s), got %s", interval, timeout)
	case threshold < 1 || threshold > allocator.MaxPeerFailureThreshold:
		return fmt.Errorf("--peers.failure-threshold must be from 1 to 20, got %d", threshold)
	case snapshot < timeout || snapshot > allocator.MaxPeerSnapshotTimeout:
		return fmt.Errorf("--peers.snapshot-timeout must be from --peers.check-timeout (%s) to 5m, got %s", timeout, snapshot)
	}
	return nil
}

// startLoadTries is how often the start reads input files that keep
// changing while they are read, startLoadPause apart: a ConfigMap being
// swapped is a moment's change, not a refusal to exit for.
const (
	startLoadTries = 5
	startLoadPause = time.Second
)

// loadStartInputs reads the input files at startup with load, and again
// after sleep(startLoadPause) while they change during the read
// (allocator.ErrChanged), up to startLoadTries times. Any other error is
// returned at once.
func loadStartInputs(load func(allocator.Paths) (*allocator.Inputs, error), p allocator.Paths, sleep func(time.Duration)) (*allocator.Inputs, error) {
	for try := 1; ; try++ {
		in, err := load(p)
		if !errors.Is(err, allocator.ErrChanged) {
			return in, err
		}
		if try == startLoadTries {
			return nil, fmt.Errorf("%w, %d times, %s apart", err, startLoadTries, startLoadPause)
		}
		sleep(startLoadPause)
	}
}

// newServer is the allocator's HTTP server: the exporter's, with its header
// and read timeouts and its header size bound. The read timeout, 30s, is
// shorter than a bundle long poll may wait (allocator.MaxWait), but does not
// cut one short: net/http lifts the read deadline once a request's body has
// been read, before the handler's background read of the connection begins,
// so only reading the request is bounded by it. A test checks this against
// the server built here.
func newServer(address string, handler http.Handler) *http.Server {
	return exporter.NewHTTPServer(address, handler)
}

func commandLineError(stderr io.Writer, err error) int {
	slog.New(slog.NewJSONHandler(stderr, nil)).Error("invalid command line; exiting", "error", err.Error())
	return 2
}

// parseLogLevel reads --log.level as the exporter does: debug, info, warn or
// error, in any case.
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
