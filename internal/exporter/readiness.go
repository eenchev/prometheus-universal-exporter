package exporter

import (
	"fmt"
	"net/http"
	"strings"
)

// /health says the process is alive. /ready says whether it should be sent
// probes. A rejected reload, of the configuration or of the static target
// file, leaves it ready: it keeps answering probes with the configuration in
// force. A pod that is not ready is taken out of its Service. Every replica
// rejects the same ConfigMap edit, so making them unready would leave the
// Service without endpoints and fail every probe the exporter could still
// answer. The rejection is reported where it can be seen without that cost:
// http_exporter_config_last_reload_successful is 0, the ERROR log line, and
// /-/reload's 500.
//
// An exporter whose OTLP exports keep failing delivers nothing to its
// backend, but it still answers probes. Making it unready would take it out
// of its Service and stop those too. So failing exports make it unready only
// when otlp.unready_after_failures asks for it. That suits an exporter that
// exists to deliver its targets over OTLP. It becomes ready again at the
// next export that gets through.
//
// The reasons are listed in the body, one per line, for whoever looks. They
// never include an error's text: /ready is not authenticated, and an error
// can quote a path, a URL or a line of the configuration.

// shuttingDownReason is the one reason /ready gives while the exporter stops.
const shuttingDownReason = "the exporter is shutting down"

// notReadyReasons lists why the exporter is not ready, empty when it is.
func (s *Server) notReadyReasons() []string {
	if s.stopping.Load() {
		return []string{shuttingDownReason}
	}
	var reasons []string
	if s.manager.Awaiting() {
		reasons = append(reasons, awaitingReadyReason)
	}
	cfg := s.manager.Get().OTLP
	if failing := s.otlp.failing(cfg.Endpoint); cfg.Enabled && cfg.Endpoint != "" && cfg.UnreadyAfterFailures > 0 && failing >= cfg.UnreadyAfterFailures {
		reasons = append(reasons, fmt.Sprintf("the last %d OTLP exports failed", failing))
	}
	return reasons
}

func (s *Server) readyHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if reasons := s.notReadyReasons(); len(reasons) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: " + strings.Join(reasons, "\nnot ready: ") + "\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
