package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

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
	selfMetricsPath := flags.String("web.self-metrics-path", "/self-metrics", "Dedicated endpoint for exporter self-health metrics")
	pythonPath := flags.String("python.path", "python3", "Python interpreter used by the python transform")
	targetFile := flags.String("otlp.targets-file", "", "Optional file of scheduled targets scraped by the exporter and delivered over OTLP")
	watchConfig := flags.Bool("config.watch", false, "Reload the configuration and scheduled target files when they change on disk")
	watchInterval := flags.Duration("config.watch-interval", DefaultWatchInterval, "How often to check the configuration files for changes when config.watch is set")
	logLevel := flags.String("log.level", "info", "Log level: debug, info, warn, or error")
	expandEnv := flags.Bool("config.export-env", false, "Expand ${NAME} environment variable references in the configuration and scheduled target files")
	check := flags.Bool("dry-run", false, "Validate the configuration and scheduled target files as startup would, print a JSON report to stdout, and exit 0 if they are valid or 1 if not, without starting the exporter")
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

	// Every document is read the same way, and the manager is told so its
	// reloads keep expanding.
	var loadOptions []LoadOption
	if *expandEnv {
		loadOptions = append(loadOptions, WithEnvExpansion())
	}

	logger := newLogger(*logLevel, stderr)
	if *check {
		return runCheck(checkInputs{
			ConfigFile:    *configFile,
			TargetFile:    *targetFile,
			PythonPath:    *pythonPath,
			ExpandEnv:     *expandEnv,
			Watch:         *watchConfig,
			WatchInterval: *watchInterval,
		}, stdout, logger)
	}
	config, err := LoadConfig(*configFile, loadOptions...)
	if err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	if err := ValidatePythonScripts(*pythonPath, config); err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	if err := validateWatchInterval(*watchConfig, *watchInterval); err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	manager := NewConfigManager(config, *configFile, logger)
	manager.SetPythonPath(*pythonPath)
	manager.SetEnvExpansion(*expandEnv)
	if *watchConfig {
		manager.SetWatchInterval(*watchInterval)
	}
	if *targetFile != "" {
		targets, err := LoadTargetFile(*targetFile, loadOptions...)
		if err == nil {
			err = targets.Validate()
		}
		if err == nil {
			err = targets.ValidateAgainst(config)
		}
		if err != nil {
			logger.Error("invalid scheduled target configuration; exiting", "file", *targetFile, "error", err)
			return 1
		}
		manager.SetTargets(*targetFile, targets)
		logger.Info("scheduled targets loaded", "file", *targetFile, "targets", len(targets.Targets))
	}
	server := NewServer(manager, *pythonPath, logger)
	server.SetSelfMetricsPath(*selfMetricsPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go manager.ReloadLoop(ctx)
	go server.OTLPExportLoop(ctx)

	startup := []any{"address", *listenAddress, "collectors", len(config.Collectors),
		"scheduled_targets", len(manager.Targets()), "config_watch", manager.WatchEnabled(),
		"config_export_env", *expandEnv}
	// The interval is only meaningful when the watch is on, and its absence
	// would otherwise leave the operator guessing how stale a running
	// configuration can be.
	if manager.WatchEnabled() {
		startup = append(startup, "config_watch_interval", manager.WatchInterval().String())
	}
	logger.Info("starting exporter", startup...)
	// ReadHeaderTimeout bounds how long a client may take to send its request
	// headers, so a stalled connection cannot hold a handler open indefinitely.
	httpServer := &http.Server{Addr: *listenAddress, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serverErr := make(chan error, 1)
	go func() { serverErr <- httpServer.ListenAndServe() }()
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			return 1
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown failed", "error", err)
		}
	}
	return 0
}

// newLogger builds the exporter's logger and installs it as the default. Every
// line the exporter writes has to be JSON, and not all of them come from a
// logger passed down through the call chain: metric extraction reports a failed
// rule from deep inside a transform, where threading a logger through seven
// signatures would buy nothing. Those lines go through slog's default logger,
// which without this would be the text handler and would emit a differently
// shaped line into the middle of an otherwise machine-readable stream.
func newLogger(level string, out io.Writer) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: l}))
	slog.SetDefault(logger)
	return logger
}
