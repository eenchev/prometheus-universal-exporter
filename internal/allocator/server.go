package allocator

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
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
// not "Bearer <token>"; what a person reads in a browser, which cannot send
// a bearer token, takes the token as Basic auth's password too (browsable).
func (a *Allocator) Handler(token string) http.Handler {
	if token == "" {
		return a.handler(nil)
	}
	return a.handler(func() (string, error) { return token, nil })
}

// HandlerReading is Handler with the token of f, read again when the file
// changes (TokenFile).
func (a *Allocator) HandlerReading(f *TokenFile) http.Handler {
	return a.handler(f.Token)
}

// handler is Handler with token giving the token for each request, nil for
// none.
func (a *Allocator) handler(token func() (string, error)) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST "+allocapi.HeartbeatPath, a.serveHeartbeat)
	api.HandleFunc("GET "+allocapi.BundlePath, a.serveBundle)
	api.HandleFunc("GET "+allocapi.SDPathPrefix+"{job}", a.serveSD)
	api.HandleFunc("GET /metrics", a.serveMetrics)
	api.HandleFunc("GET "+PeerPath, a.servePeer)
	api.HandleFunc("GET "+SnapshotPath, a.serveSnapshot)
	api.HandleFunc("GET /{$}", a.serveStatusPage)
	api.HandleFunc("GET "+StatusPath, a.serveStatus)
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

// authorized refuses a request without the token, when there is one, and
// every request while the token cannot be had. The token is compared in
// constant time.
//
// A browser cannot send a bearer token, so the requests a person makes to
// read the allocator, GET or HEAD of the status page, the status and
// /metrics (browsable), also take the token as the password of Basic auth,
// of any user name, and their 401 asks for it, so the browser prompts. Only
// those: a browser sends Basic credentials it was given with any request to
// the allocator, a page of another site's included, and none of them
// changes anything, nor can another site read their answers. The token is
// never taken from the URL, which logs and browser history keep.
func authorized(token func() (string, error), next http.Handler) http.Handler {
	if token == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := token()
		if err == nil && (subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+current)) == 1 ||
			browsable(r) && basicToken(r, current)) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="pue-allocator"`)
		if browsable(r) {
			w.Header().Add("WWW-Authenticate", `Basic realm="pue-allocator", charset="UTF-8"`)
			http.Error(w, "this page needs the allocator's token: as the password of Basic auth, of any user name, or as a bearer token in the Authorization header", http.StatusUnauthorized)
			return
		}
		http.Error(w, "this endpoint needs the allocator's bearer token in the Authorization header", http.StatusUnauthorized)
	})
}

// browsable says r reads what a person reads in a browser: the status page,
// the status or /metrics.
func browsable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/", StatusPath, "/metrics":
		return true
	}
	return false
}

// basicToken says r carries Basic auth whose password is token, compared in
// constant time; the user name is not looked at.
func basicToken(r *http.Request, token string) bool {
	_, password, ok := r.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(password), []byte(token)) == 1
}

func (a *Allocator) serveHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb allocapi.Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHeartbeatBytes)).Decode(&hb); err != nil {
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("the heartbeat is larger than %d bytes, more than any heartbeat needs", tooLarge.Limit), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "the heartbeat is not a JSON object as allocapi.Heartbeat describes: "+err.Error(), http.StatusBadRequest)
		return
	}
	reply, err := a.Heartbeat(hb)
	switch {
	case errors.Is(err, ErrTooManyMembers):
		// Retry-After is the member TTL: the soonest a silent member can
		// have freed its place.
		w.Header().Set("Retry-After", strconv.Itoa(int(allocapi.MemberTTL/time.Second)))
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, reply)
}

// maxInstanceBytes bounds a heartbeat's instance, which the allocator keeps
// for each member.
const maxInstanceBytes = 64

// checkHeartbeat refuses a heartbeat without a member, an address to probe
// it at, an instance or a sequence.
func checkHeartbeat(hb allocapi.Heartbeat) error {
	if strings.TrimSpace(hb.Member) == "" || len(hb.Member) > 253 || strings.ContainsFunc(hb.Member, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return fmt.Errorf("%w; member %q is not a replica's id: give its pod or host name, at most 253 bytes", ErrBadHeartbeat, hb.Member)
	}
	if !validAddress(hb.Advertise) {
		return fmt.Errorf("%w; advertise %q is not a host:port with a port from 1 to 65535", ErrBadHeartbeat, hb.Advertise)
	}
	if hb.Instance == "" || len(hb.Instance) > maxInstanceBytes {
		return fmt.Errorf("%w; instance %q is not a replica process's name of 1 to %d bytes", ErrBadHeartbeat, hb.Instance, maxInstanceBytes)
	}
	if hb.Sequence == 0 {
		return fmt.Errorf("%w; the sequence counts a replica's heartbeats from 1", ErrBadHeartbeat)
	}
	return nil
}

// validAddress reports whether address is a host:port with a host and a
// decimal port from 1 to 65535: the address service discovery hands
// Prometheus, where a named port, port 0 or one out of range would only fail
// later, at every probe.
func validAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" || strings.TrimLeft(port, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
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
