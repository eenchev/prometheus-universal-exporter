package main

import (
	"fmt"
	"net/http"
	"strings"
)

// /health says the process is alive; /ready says whether it is doing what it
// was configured to do. An exporter that keeps answering probes with the last
// valid configuration after a reload was rejected is alive and useful, but it
// is not running the configuration somebody deployed, so it is reported as not
// ready until the next accepted reload.
//
// An exporter whose OTLP exports keep failing is delivering nothing to its
// backend, but it still answers probes, and a pod that is not ready is taken
// out of its Service, which would stop those too. So failing exports make it
// unready only when otlp.unready_after_failures asks for it, as it should for
// an exporter that exists to deliver scheduled targets over OTLP; ready again
// at the next export that gets through.
//
// The reasons are listed in the body, one per line, for whoever looks. They
// never include an error's text: /ready is not authenticated, and an error can
// quote a path, a URL or a line of the configuration.

// notReadyReasons lists why the exporter is not ready, empty when it is.
func (s *Server) notReadyReasons() []string {
	if s.stopping.Load() {
		return []string{"the exporter is shutting down"}
	}
	var reasons []string
	if s.manager.reloads != nil {
		for _, file := range s.manager.reloads.rejected() {
			name := "configuration"
			if file == reloadFileTargets {
				name = "scheduled target file"
			}
			reasons = append(reasons, fmt.Sprintf("the last reload of the %s was rejected; the previous one is still in force", name))
		}
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
