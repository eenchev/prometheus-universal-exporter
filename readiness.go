package main

import (
	"fmt"
	"net/http"
	"strings"
)

// /health says the process is alive; /ready says whether it is doing what it
// was configured to do. An exporter that keeps answering probes with the last
// valid configuration after a reload was rejected is alive and useful, but it
// is not running the configuration somebody deployed, and an exporter whose
// OTLP exports have failed several times in a row is delivering nothing to its
// backend. Both are reported as not ready until they recover: the next accepted
// reload, the next export that gets through.
//
// The reasons are listed in the body, one per line, for whoever looks. They
// never include an error's text: /ready is not authenticated, and an error can
// quote a path, a URL or a line of the configuration.

// notReadyReasons lists why the exporter is not ready, empty when it is.
func (s *Server) notReadyReasons() []string {
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
	if failing := s.otlp.failing(); cfg.Enabled && cfg.Endpoint != "" && failing >= otlpUnreadyAfter {
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
