// Package kubetest is a stand-in Kubernetes API server for the tests of
// internal/kubeapi and of the kubernetes request type: an HTTPS server that
// answers a listing of the objects a test gave it at a path, a page at a
// time as the limit asks with a continue token for the rest, refuses a
// request without the token in its service account directory, or the one
// it was told to require, with 401,
// answers 403 and 404 as the API server does, 429 as its priority and
// fairness does, 410 Expired for a continue token it was told to have
// lost, a listing of the objects' metadata alone when it is asked for
// that, can be slow to answer, and records every request it was sent. It writes the service account directory a pod has, the
// token, the server's CA and the namespace, and can set the environment
// that points a pod at its API server. It imports nothing but the standard
// library.
package kubetest

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Server is a stand-in API server.
type Server struct {
	*httptest.Server
	// Dir is the service account directory: token, ca.crt and namespace.
	Dir string

	mu       sync.Mutex
	token    string
	lists    map[string]List
	forbid   map[string]bool
	raw      map[string]string
	requests []Request
	holdFrom int
	// throttle is how many more requests of a path are answered 429, and
	// retryAfter their Retry-After; expire whether the next request of a
	// path with a continue token is answered 410 Expired, and with what
	// continue token of its own ("" for none).
	throttle   map[string]int
	retryAfter map[string]string
	expire     map[string]*string
}

// List is what a listing of a path answers: the list's kind and
// apiVersion, and its objects.
type List struct {
	Kind, APIVersion string
	Items            []map[string]any
}

// Request is a request as the server was sent it.
type Request struct {
	Path          string
	Query         url.Values
	Authorization string
	// UserAgent and Accept are the headers of the same names.
	UserAgent, Accept string
	// Traceparent and Tracestate are the W3C trace context it carried.
	Traceparent, Tracestate string
}

// New starts a server, with a service account directory whose token is
// token-1 and whose namespace is monitoring.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Dir: t.TempDir(), lists: map[string]List{}, forbid: map[string]bool{}, raw: map[string]string{}, throttle: map[string]int{}, retryAfter: map[string]string{}, expire: map[string]*string{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	s.Write(t, "ca.crt", string(ca))
	s.Write(t, "token", "token-1\n")
	s.Write(t, "namespace", "monitoring\n")
	return s
}

// Write writes a file of the service account directory.
func (s *Server) Write(t testing.TB, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// InCluster sets the environment Kubernetes gives a pod to reach the
// server, for the rest of the test.
func (s *Server) InCluster(t *testing.T) {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
}

// Serve has the server answer a listing of path with list.
func (s *Server) Serve(path string, list List) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists[path] = list
}

// Answer has the server answer a listing of path 200 with body as it is,
// as an API server, or what stands before it, answers what is no list.
func (s *Server) Answer(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw[path] = body
}

// Forbid has the server refuse a listing of path with 403, as it refuses a
// service account without the permission.
func (s *Server) Forbid(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forbid[path] = true
}

// RequireToken has the server take token alone from now on, in place of
// the one in its service account directory, as an API server takes no
// token it did not issue.
func (s *Server) RequireToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

// Throttle has the server answer the next n requests of path 429 Too Many
// Requests, with retryAfter as their Retry-After ("" for none), as the
// API server's priority and fairness refuses a request it has no seat for.
func (s *Server) Throttle(path string, n int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle[path], s.retryAfter[path] = n, retryAfter
}

// Expire has the server answer the next request of path that carries a
// continue token 410 Expired, as the API server answers a token whose
// version etcd compacted away: inconsistent, with the token the Status
// carries to go on with the listing as it is now, the token asked with;
// otherwise with none.
func (s *Server) Expire(path string, inconsistent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := ""
	if inconsistent {
		token = "same"
	}
	s.expire[path] = &token
}

// HoldFrom has the server hold the answer to its n-th request, counted from
// 1 since it started, and to every one after it, until the client gives up,
// so a test of a deadline does not hang on how fast the machine is.
func (s *Server) HoldFrom(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdFrom = n
}

// Requests are the requests the server was sent, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Path: r.URL.Path, Query: r.URL.Query(), Authorization: r.Header.Get("Authorization"), Traceparent: r.Header.Get("Traceparent"), Tracestate: r.Header.Get("Tracestate"), UserAgent: r.Header.Get("User-Agent"), Accept: r.Header.Get("Accept")})
	list, listed := s.lists[r.URL.Path]
	forbidden := s.forbid[r.URL.Path]
	raw, answered := s.raw[r.URL.Path]
	token := s.token
	hold := s.holdFrom > 0 && len(s.requests) >= s.holdFrom
	throttled, retryAfter := s.throttle[r.URL.Path] > 0, s.retryAfter[r.URL.Path]
	if throttled {
		s.throttle[r.URL.Path]--
	}
	expired := s.expire[r.URL.Path]
	if expired != nil && r.URL.Query().Get("continue") != "" {
		delete(s.expire, r.URL.Path)
	} else {
		expired = nil
	}
	s.mu.Unlock()
	if hold {
		<-r.Context().Done()
		return
	}
	if token == "" {
		raw, _ := os.ReadFile(filepath.Join(s.Dir, "token"))
		token = strings.TrimSpace(string(raw))
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		status(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	if throttled {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		status(w, http.StatusTooManyRequests, "TooManyRequests", "the server has received too many requests and has asked us to try again later")
		return
	}
	resource := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if forbidden {
		status(w, http.StatusForbidden, "Forbidden", resource+` is forbidden: User "system:serviceaccount:monitoring:exporter" cannot list resource "`+resource+`"`)
		return
	}
	if answered {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, raw)
		return
	}
	if !listed {
		status(w, http.StatusNotFound, "NotFound", "the server could not find the requested resource")
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = len(list.Items)
	}
	if expired != nil {
		body := map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Expired", "code": http.StatusGone,
			"message": "The provided continue parameter is too old to display a consistent list result."}
		if *expired != "" {
			body["metadata"] = map[string]any{"continue": r.URL.Query().Get("continue")}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	accept := r.Header.Get("Accept")
	metadataOnly := accept == "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
	if accept != "application/json" && !metadataOnly {
		status(w, http.StatusNotAcceptable, "NotAcceptable", "only the following media types are accepted: application/json")
		return
	}
	from := 0
	if next := r.URL.Query().Get("continue"); next != "" {
		if from, err = strconv.Atoi(next); err != nil || from > len(list.Items) {
			status(w, http.StatusGone, "Expired", "the continue token is not one this server gave")
			return
		}
	}
	to := min(from+limit, len(list.Items))
	metadata := map[string]any{"resourceVersion": "1"}
	if to < len(list.Items) {
		metadata["continue"] = strconv.Itoa(to)
	}
	items := list.Items[from:to]
	if items == nil {
		items = []map[string]any{}
	}
	kind, apiVersion := list.Kind, list.APIVersion
	if metadataOnly {
		// The objects' metadata alone, each a PartialObjectMetadata.
		partial := make([]map[string]any, len(items))
		for i, item := range items {
			partial[i] = map[string]any{"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1", "metadata": item["metadata"]}
		}
		items, kind, apiVersion = partial, "PartialObjectMetadataList", "meta.k8s.io/v1"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": kind, "apiVersion": apiVersion, "metadata": metadata, "items": items})
}

// status answers a Status object, as the API server answers a failure.
func status(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "message": message, "code": code})
}
