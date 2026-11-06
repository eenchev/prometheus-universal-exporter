// Command pue-allocator is the target allocator of
// prometheus-universal-exporter: it serves every exporter replica run with
// --allocator.url its configuration and its share of the static targets, and
// Prometheus the probe targets, each already pointed at a replica, through
// HTTP service discovery. See docs/ALLOCATOR.md.
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
	flags := flag.NewFlagSet("pue-allocator", flag.ContinueOnError)
	// Every line on stderr stays JSON, as the exporter's do.
	flags.SetOutput(io.Discard)
	listenAddress := flags.String("web.listen-address", ":9116", "Address on which to expose the allocator's API, status page, metrics and health endpoints")
	configFile := flags.String("config.file", "/etc/prometheus-universal-exporter/config.yaml", "Path to the configuration the replicas are served, with the collector files it lists, which must be relative paths inside its directory")
	staticTargetsFile := flags.String("static-targets-file", "", "Optional static target file, split among the replicas; a target's shard_key, when set, is hashed in place of its name")
	probeTargetsFile := flags.String("probe-targets-file", "", "Optional probe targets file of probe_jobs, served to Prometheus at /v1/sd/<job>")
	tokenFile := flags.String("web.token-file", "", "Optional file holding a bearer token every request but /healthz and /ready must carry; the status page, /v1/status and /metrics also take it as Basic auth's password, for a browser")
	logLevel := flags.String("log.level", "info", "Log level: debug, info, warn, or error")
	watchInterval := flags.Duration("config.watch-interval", config.DefaultWatchInterval, "How often to read the input files again for changes, at most 24h; 0 reads them once")
	settle := flags.Duration("settle", allocator.DefaultSettle, "How long a changed member list must hold before targets move; a replica back within it keeps its share")
	heartbeatInterval := flags.Duration("heartbeat.interval", allocapi.DefaultHeartbeatInterval, "How often the replicas heartbeat, which every heartbeat reply tells them, from 1s to 2m; a replica not heard from for three intervals stops being a member")
	maxMembers := flags.Int("members.max", allocator.DefaultMaxMembers, "How many live members to take; a heartbeat of a new member beyond them is refused with 429 until one stops heartbeating")
	discoveryRefresh := flags.Duration("discovery.refresh-interval", 0, "How often to ask a discovery source that sets no refresh_interval, from 1s to 24h; 0 leaves each kind its own default: 1m for file_sd and http_sd, 30s for kubernetes and dns_sd")
	maxDiscovered := flags.Int("discovery.max-targets", allocator.DefaultMaxDiscoveredTargets, "How many targets one answer of a discovery source may hold, for a source that sets no max_targets; an answer with more is refused and the source keeps its last good targets")
	peerList := flags.String("peers", "", "Optional allocator instances to run beside, as host:port entries, comma-separated: a name resolved to every address it has, as a headless Service's, or an IP address. List every instance, this one included, so each finds its own address among them. The live instance of the lowest address leads and alone asks the discovery sources; the others serve what it found")
	peerSelf := flags.String("peers.self", "", "This instance's address as its peers reach it, an IP address (the port of --web.listen-address) or ip:port; left out, the instance finds itself among the peers' addresses by asking them")
	peerLeaderOnly := flags.Bool("peers.leader-only", false, "With --peers, only the leader serves the whole of /metrics and of the status page; a follower serves the peers' metrics and its own peer state alone, so a Prometheus scraping every instance has one instance's series of the allocation")
	peerCheckInterval := flags.Duration("peers.check-interval", allocator.DefaultPeerCheckInterval, "With --peers, how often each instance resolves --peers, checks every other instance and, as a follower, fetches the leader's snapshot, from 500ms to 1m")
	peerCheckTimeout := flags.Duration("peers.check-timeout", allocator.DefaultPeerCheckTimeout, "With --peers, how long a check of another instance, and a lookup of --peers, may take, from 100ms to less than --peers.check-interval")
	peerFailureThreshold := flags.Int("peers.failure-threshold", allocator.DefaultPeerFailureThreshold, "With --peers, how many checks in a row an instance must fail before it stops being live, the leader then being replaced, from 1 to 20; a follower whose snapshot fetches fail as often logs so")
	peerSnapshotTimeout := flags.Duration("peers.snapshot-timeout", allocator.DefaultPeerSnapshotTimeout, "With --peers, how long a follower's fetch of the leader's snapshot may take, from --peers.check-timeout to 5m")
	showVersion := flags.Bool("version", false, "Print the version, revision, Go version and request types of this build, and exit")
	printStaticTargetsSchema := flags.Bool("static-targets-file-schema", false, "Print the JSON Schema of the static target file, which may have discovered_targets, for editors, and exit")
	printProbeTargetsSchema := flags.Bool("probe-targets-file-schema", false, "Print the JSON Schema of the probe targets file, for editors, and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(stdout)
			_, _ = io.WriteString(stdout, "Usage of pue-allocator:\n")
			flags.PrintDefaults()
			return 0
		}
		return commandLineError(stderr, err)
	}
	if flags.NArg() > 0 {
		return commandLineError(stderr, fmt.Errorf("unexpected argument %q; flags take --name=value", flags.Arg(0)))
	}
	if err := exporter.ValidateListenAddress(*listenAddress); err != nil {
		return commandLineError(stderr, err)
	}
	level, err := parseLogLevel(*logLevel)
	if err != nil {
		return commandLineError(stderr, err)
	}
	if *watchInterval < 0 || *watchInterval > allocator.MaxWatchInterval {
		return commandLineError(stderr, fmt.Errorf("--config.watch-interval must be from 0 to 24h, got %s; 0 reads the files once", *watchInterval))
	}
	if *settle < 0 {
		return commandLineError(stderr, fmt.Errorf("--settle must not be negative, got %s", *settle))
	}
	if *heartbeatInterval < allocapi.MinHeartbeatInterval || *heartbeatInterval > allocapi.MaxHeartbeatInterval {
		return commandLineError(stderr, fmt.Errorf("--heartbeat.interval must be from 1s to 2m, got %s", *heartbeatInterval))
	}
	if *maxMembers < 1 {
		return commandLineError(stderr, fmt.Errorf("--members.max must be 1 or more, got %d", *maxMembers))
	}
	if *discoveryRefresh != 0 && (*discoveryRefresh < allocator.MinDiscoveryRefresh || *discoveryRefresh > allocator.MaxDiscoveryRefresh) {
		return commandLineError(stderr, fmt.Errorf("--discovery.refresh-interval must be from 1s to 24h, or 0 for each kind's own default, got %s", *discoveryRefresh))
	}
	if *maxDiscovered < 1 {
		return commandLineError(stderr, fmt.Errorf("--discovery.max-targets must be 1 or more, got %d", *maxDiscovered))
	}
	// The peers' timings given, by name, which only --peers takes.
	timingsGiven := []string{}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "peers.check-interval", "peers.check-timeout", "peers.failure-threshold", "peers.snapshot-timeout":
			timingsGiven = append(timingsGiven, f.Name)
		}
	})
	var peerOptions *allocator.PeerOptions
	switch {
	case *peerList != "":
		entries, err := allocator.ParsePeers(*peerList)
		if err != nil {
			return commandLineError(stderr, err)
		}
		if err := checkPeerTimings(*peerCheckInterval, *peerCheckTimeout, *peerFailureThreshold, *peerSnapshotTimeout); err != nil {
			return commandLineError(stderr, err)
		}
		peerOptions = &allocator.PeerOptions{Peers: entries, LeaderOnly: *peerLeaderOnly,
			CheckInterval: *peerCheckInterval, CheckTimeout: *peerCheckTimeout, FailureThreshold: *peerFailureThreshold, SnapshotTimeout: *peerSnapshotTimeout}
		if *peerSelf != "" {
			if peerOptions.Self, err = allocator.ParseSelf(*peerSelf, *listenAddress); err != nil {
				return commandLineError(stderr, err)
			}
		}
	case *peerSelf != "":
		return commandLineError(stderr, errors.New("--peers.self is given without --peers"))
	case *peerLeaderOnly:
		return commandLineError(stderr, errors.New("--peers.leader-only is given without --peers: an instance without peers is its own leader and serves everything"))
	case len(timingsGiven) > 0:
		return commandLineError(stderr, fmt.Errorf("--%s is given without --peers: an instance without peers checks none", timingsGiven[0]))
	}
	if *showVersion {
		b := exporter.BuildVersion()
		_, _ = fmt.Fprintf(stdout, "pue-allocator version %s (revision %s, %s, request types %s)\n", b.Version, b.Revision, b.GoVersion, strings.Join(b.RequestTypes, ","))
		return 0
	}
	if *printStaticTargetsSchema || *printProbeTargetsSchema {
		render := allocator.StaticTargetsSchemaJSON
		if *printProbeTargetsSchema {
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

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	var token *allocator.TokenFile
	if *tokenFile != "" {
		if token, err = allocator.NewTokenFile(*tokenFile, logger); err != nil {
			logger.Error(err.Error()+"; exiting", "file", *tokenFile)
			return 1
		}
	}
	paths := allocator.Paths{Config: *configFile, StaticTargets: *staticTargetsFile, ProbeTargets: *probeTargetsFile, DiscoveryRefresh: *discoveryRefresh}
	inputs, err := loadStartInputs(allocator.LoadInputs, paths, time.Sleep)
	if err != nil {
		logger.Error("invalid input files; exiting", "error", err)
		return 1
	}
	build := exporter.BuildVersion()
	alloc := allocator.New(inputs, allocator.Options{Settle: *settle, HeartbeatInterval: *heartbeatInterval, MaxMembers: *maxMembers, MaxDiscoveredTargets: *maxDiscovered, Logger: logger,
		Address: *listenAddress, Version: build.Version, Revision: build.Revision, WatchInterval: *watchInterval})
	if peerOptions != nil {
		if token != nil {
			peerOptions.Token = token.Token
		}
		// The peers decide whether this instance asks the sources; RunPeers
		// starts once the API listens, so an instance asking its own address
		// finds itself.
		alloc.UsePeers(*peerOptions)
	} else {
		// The sources are asked at once, well before the first member list
		// forms, a settle time after the start.
		alloc.StartDiscovery()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	watchDone := make(chan struct{})
	defer close(watchDone)
	if *watchInterval > 0 {
		ticker := time.NewTicker(*watchInterval)
		defer ticker.Stop()
		go alloc.WatchLoop(watchDone, ticker.C, paths)
	}
	logger.Info("starting allocator", append([]any{"version", build.Version, "address", *listenAddress,
		"config_file", *configFile, "static_targets_file", *staticTargetsFile, "probe_targets_file", *probeTargetsFile,
		"watch_interval", watchInterval.String(), "settle", settle.String(), "heartbeat_interval", heartbeatInterval.String(), "max_members", *maxMembers, "max_discovered_targets", *maxDiscovered, "discovery_refresh_interval", discoveryRefresh.String(), "token", token != nil,
		"peers", *peerList, "peers_self", *peerSelf, "peers_leader_only", *peerLeaderOnly, "peers_check_interval", peerCheckInterval.String(), "peers_check_timeout", peerCheckTimeout.String(),
		"peers_failure_threshold", *peerFailureThreshold, "peers_snapshot_timeout", peerSnapshotTimeout.String()}, inputs.LogAttrs()...)...)
	handler := alloc.Handler("")
	if token != nil {
		handler = alloc.HandlerReading(token)
	}
	server := newServer(*listenAddress, handler)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listenAddress)
	if err != nil {
		logger.Error("HTTP server stopped", "error", err)
		return 1
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	if peerOptions != nil {
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
		logger.Info("shutting down; a second signal exits at once")
		alloc.BeginShutdown()
		if peerOptions != nil {
			// The peers check /v1/peer every --peers.check-interval: serving
			// on, with 503 there, until each has checked lets the next
			// instance lead at once, where a closed listener would only be
			// counted out after --peers.failure-threshold refused checks.
			delay := peerOptions.ShutdownDelay()
			logger.Info("answering the peers that this instance shuts down before stopping the server", "delay", delay.String())
			time.Sleep(delay)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}
	return 0
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

// loadStartInputs reads the input files at the start with load, again after
// sleep(startLoadPause) while they change under the read
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
// shorter than a bundle long poll may wait (allocator.MaxWait), and does not
// cut one: net/http lifts the read deadline once a request's body has been
// read, before the handler's background read of the connection begins, so
// only reading the request is bounded by it. A test holds this to the server
// built here.
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
