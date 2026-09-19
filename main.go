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
	"syscall"
	"time"
)

func main() { os.Exit(run()) }

// run owns the exporter lifecycle and returns the process exit status. Keeping
// it separate from main means every deferred cleanup still executes on the
// paths that terminate early.
func run() int {
	configFile := flag.String("config.file", "/etc/prometheus-universal-exporter/config.yaml", "Path to the exporter configuration")
	listenAddress := flag.String("web.listen-address", ":8080", "Address on which to expose HTTP endpoints")
	selfMetricsPath := flag.String("web.self-metrics-path", "/self-metrics", "Dedicated endpoint for exporter self-health metrics")
	pythonPath := flag.String("python.path", "python3", "Python interpreter used by the python transform")
	targetFile := flag.String("otlp.targets-file", "", "Optional file of scheduled targets scraped by the exporter and delivered over OTLP")
	watchConfig := flag.Bool("config.watch", false, "Reload the configuration and scheduled target files when they change on disk")
	watchInterval := flag.Duration("config.watch-interval", DefaultWatchInterval, "How often to check the configuration files for changes when config.watch is set")
	logLevel := flag.String("log.level", "info", "Log level: debug, info, warn, or error")
	expandEnv := flag.Bool("config.export-env", false, "Expand ${NAME} environment variable references in the configuration and scheduled target files")
	flag.Parse()

	// Every document is read the same way, and the manager is told so its
	// reloads keep expanding.
	var loadOptions []LoadOption
	if *expandEnv {
		loadOptions = append(loadOptions, WithEnvExpansion())
	}

	logger := newLogger(*logLevel, os.Stderr)
	config, err := LoadConfig(*configFile, loadOptions...)
	if err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	if err := ValidatePythonScripts(*pythonPath, config); err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	if *watchConfig && *watchInterval <= 0 {
		logger.Error("invalid startup configuration; exiting", "error", fmt.Sprintf("config.watch-interval must be positive, got %s", *watchInterval))
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
