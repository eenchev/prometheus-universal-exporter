package main

import (
	"context"
	"errors"
	"flag"
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
	logLevel := flag.String("log.level", "info", "Log level: debug, info, warn, or error")
	flag.Parse()

	logger := newLogger(*logLevel)
	config, err := LoadConfig(*configFile)
	if err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		return 1
	}

	manager := NewConfigManager(config, *configFile, logger)
	if *targetFile != "" {
		targets, err := LoadTargetFile(*targetFile)
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

	logger.Info("starting exporter", "address", *listenAddress, "collectors", len(config.Collectors), "scheduled_targets", len(manager.Targets()))
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

func newLogger(level string) *slog.Logger {
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
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
