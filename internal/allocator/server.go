package allocator

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// MaxWait is the longest a bundle request's wait parameter holds it: a
// longer one is shortened to it.
const MaxWait = 60 * time.Second

// maxHeartbeatBytes bounds a heartbeat's body.
const maxHeartbeatBytes = 64 << 10

// Handler serves the allocator's API. With a token, every endpoint but
// /healthz and /ready answers 401 to a request whose Authorization header is
// not "Bearer <token>".
func (a *Allocator) Handler(token string) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST "+allocapi.HeartbeatPath, a.serveHeartbeat)
	api.HandleFunc("GET "+allocapi.BundlePath, a.serveBundle)
	api.HandleFunc("GET "+allocapi.SDPathPrefix+"{job}", a.serveSD)
	api.HandleFunc("GET /metrics", a.serveMetrics)
	mux := http.NewServeMux()
	mux.Handle("/", authorized(token, api))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if !a.Ready() {
			http.Error(w, "the allocator is shutting down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("OK\n"))
	})
	return mux
}

// authorized refuses a request without the token, when there is one. The
// token is compared in constant time.
func authorized(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pue-allocator"`)
			http.Error(w, "this endpoint needs the allocator's bearer token in the Authorization header", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Allocator) serveHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb allocapi.Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHeartbeatBytes)).Decode(&hb); err != nil {
		http.Error(w, "the heartbeat is not a JSON object as allocapi.Heartbeat describes: "+err.Error(), http.StatusBadRequest)
		return
	}
	reply, err := a.Heartbeat(hb)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, reply)
}

// checkHeartbeat refuses a heartbeat without a member or an address to
// probe it at.
func checkHeartbeat(hb allocapi.Heartbeat) error {
	if strings.TrimSpace(hb.Member) == "" || len(hb.Member) > 253 || strings.ContainsFunc(hb.Member, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return fmt.Errorf("%w; member %q is not a replica's id: give its pod or host name, at most 253 bytes", ErrBadHeartbeat, hb.Member)
	}
	if !validAddress(hb.Advertise) {
		return fmt.Errorf("%w; advertise %q is not a host:port", ErrBadHeartbeat, hb.Advertise)
	}
	return nil
}

// validAddress reports whether address is a host:port with both.
func validAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	return err == nil && host != "" && port != ""
}

// serveBundle answers GET /v1/bundle?member=<id>[&wait=<duration>]. An
// If-None-Match naming the generation the replica has makes it a long poll:
// the answer waits, for wait at most, until the member's bundle is another,
// and is 304 if it never is. Until the member list has formed the answer is
// 503: the allocator cannot yet tell a replica's share from nothing, and the
// replica keeps what it has.
func (a *Allocator) serveBundle(w http.ResponseWriter, r *http.Request) {
	member := r.URL.Query().Get("member")
	if strings.TrimSpace(member) == "" {
		http.Error(w, "the member parameter is required: the replica's id, as its heartbeats give it", http.StatusBadRequest)
		return
	}
	var wait time.Duration
	if raw := r.URL.Query().Get("wait"); raw != "" {
		var err error
		if wait, err = time.ParseDuration(raw); err != nil || wait < 0 {
			http.Error(w, fmt.Sprintf("the wait parameter %q is not a duration such as 30s", raw), http.StatusBadRequest)
			return
		}
		wait = min(wait, MaxWait)
	}
	have := etagValue(r.Header.Get("If-None-Match"))
	expired := make(chan struct{})
	if wait > 0 {
		timer := a.clock.AfterFunc(wait, func() { close(expired) })
		defer timer.Stop()
	} else {
		close(expired)
	}
	for {
		changes := a.changes()
		b := a.Bundle(member)
		if b != nil && (have == "" || b.Generation != have) {
			w.Header().Set("ETag", `"`+b.Generation+`"`)
			writeJSON(w, b)
			return
		}
		select {
		case <-changes:
			continue
		case <-r.Context().Done():
			return
		case <-a.stopping:
		case <-expired:
		}
		if b = a.Bundle(member); b == nil {
			w.Header().Set("Retry-After", "5")
			http.Error(w, ErrNotSettled.Error(), http.StatusServiceUnavailable)
			return
		}
		if b.Generation != have {
			continue
		}
		w.Header().Set("ETag", `"`+b.Generation+`"`)
		w.WriteHeader(http.StatusNotModified)
		return
	}
}

// etagValue is the generation an If-None-Match names: the first entity tag,
// with its quotes and a weak tag's W/ taken off.
func etagValue(header string) string {
	first, _, _ := strings.Cut(header, ",")
	first = strings.TrimPrefix(strings.TrimSpace(first), "W/")
	return strings.Trim(first, `"`)
}

func (a *Allocator) serveSD(w http.ResponseWriter, r *http.Request) {
	groups, err := a.SD(r.PathValue("job"))
	switch {
	case errors.Is(err, ErrUnknownJob):
		http.Error(w, fmt.Sprintf("there is no probe job %q in the allocator's probe targets file", r.PathValue("job")), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, groups)
}

func (a *Allocator) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = a.WriteMetrics(w)
}

func writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(body, '\n'))
}
