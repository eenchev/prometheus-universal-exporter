package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// A reload can be asked for, not only waited for: config management that has
// just written the files can reload the exporter at once and learn whether the
// new configuration was accepted, as it can with Prometheus.
//
//   - SIGHUP reloads, always. The result is logged, and the reload self-metrics
//     (reloadstatus.go) record it.
//   - POST or PUT /-/reload reloads when --web.enable-lifecycle is set, and
//     answers 200 when every file was accepted or 500 with the reason when one
//     was rejected, the previous configuration staying in force. Without the
//     flag it answers 403, so the endpoint cannot be used to make the exporter
//     reread its files unless the operator asked for it. It sits behind
//     web.basic_auth like the other endpoints.
//
// Both reload the configuration, with its collector files, and the scheduled
// target file whether or not they changed, through the same path the watch
// uses.

// SetLifecycle enables the /-/reload endpoint.
func (s *Server) SetLifecycle(enabled bool) { s.lifecycle = enabled }

func (s *Server) reloadHandler(w http.ResponseWriter, r *http.Request) {
	if !s.lifecycle {
		http.Error(w, "Lifecycle API is not enabled; start the exporter with --web.enable-lifecycle to reload over HTTP, or send it SIGHUP.", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.Header().Set("Allow", "POST, PUT")
		http.Error(w, "use POST or PUT to reload the configuration", http.StatusMethodNotAllowed)
		return
	}
	if err := s.manager.Reload(reloadTriggerHTTP); err != nil {
		http.Error(w, "failed to reload the configuration; the previous one stays in force: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("configuration reloaded\n"))
}

// reloadSignals starts delivering SIGHUP to the returned channel, before it
// returns, so a signal sent right after is not missed.
func reloadSignals() chan os.Signal {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	return hup
}

// reloadOn reloads on every signal from hup until ctx ends.
func reloadOn(ctx context.Context, hup chan os.Signal, manager *ConfigManager, logger *slog.Logger) {
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			// The outcome is logged by the reload itself.
			if err := manager.Reload(reloadTriggerSignal); err != nil {
				logger.Debug("reload on SIGHUP rejected", "error", err)
			}
		}
	}
}
