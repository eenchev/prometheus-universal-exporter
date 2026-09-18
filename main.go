package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configFile := flag.String("config.file", "/etc/prometheus-universal-exporter/config.yaml", "Path to the exporter configuration")
	listenAddress := flag.String("web.listen-address", ":8080", "Address on which to expose HTTP endpoints")
	selfMetricsPath := flag.String("web.self-metrics-path", "/self-metrics", "Dedicated endpoint for exporter self-health metrics")
	pythonPath := flag.String("python.path", "python3", "Python interpreter used by the python decoder")
	logLevel := flag.String("log.level", "info", "Log level: debug, info, warn, or error")
	flag.Parse()

	logger := newLogger(*logLevel)
	config, err := LoadConfig(*configFile)
	if err != nil {
		logger.Error("invalid startup configuration; exiting", "error", err)
		os.Exit(1)
	}

	manager := NewConfigManager(config, *configFile, logger)
	server := NewServer(manager, *pythonPath, logger)
	server.SetSelfMetricsPath(*selfMetricsPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go manager.ReloadLoop(ctx)
	go server.OTLPExportLoop(ctx)

	logger.Info("starting exporter", "address", *listenAddress, "collectors", len(config.Collectors))
	httpServer := &http.Server{Addr: *listenAddress, Handler: server.Handler()}
	serverErr := make(chan error, 1)
	go func(){serverErr <- httpServer.ListenAndServe()}()
	select {case err:=<-serverErr: if err != nil && err != http.ErrServerClosed {logger.Error("HTTP server stopped", "error", err); os.Exit(1)};case <-ctx.Done():shutdownCtx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();if err:=httpServer.Shutdown(shutdownCtx);err!=nil{logger.Error("HTTP server shutdown failed","error",err)}}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug": l = slog.LevelDebug
	case "warn": l = slog.LevelWarn
	case "error": l = slog.LevelError
	default: l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
