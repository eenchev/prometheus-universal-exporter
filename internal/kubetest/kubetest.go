// Package kubetest is a stand-in Kubernetes API server for the tests of
// internal/kubeapi and of the kubernetes request type: an HTTPS server that
// answers a listing of the objects a test gave it at a path, a page at a
// time as the limit asks with a continue token for the rest, refuses a
// request without the token in its service account directory, or the one
// it was told to require, with 401,
// answers 403 and 404 as the API server does, can be slow to answer, and
// records every request it was sent. It writes the service account directory a pod has, the
// token, the server's CA and the namespace, and can set the environment
// that points a pod at its API server. It imports nothing but the standard
// library.
package kubetest

import (
	"encoding/json"
	"encoding/pem"
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
	"time"
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
	requests []Request
	delay    time.Duration
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
}

// New starts a server, with a service account directory whose token is
// token-1 and whose namespace is monitoring.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Dir: t.TempDir(), lists: map[string]List{}, forbid: map[string]bool{}}
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

// Slow has the server take d to answer each request from now on, or its
// client to give up first.
func (s *Server) Slow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// Requests are the requests the server was sent, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Path: r.URL.Path, Query: r.URL.Query(), Authorization: r.Header.Get("Authorization")})
	list, listed := s.lists[r.URL.Path]
	forbidden := s.forbid[r.URL.Path]
	token := s.token
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if token == "" {
		raw, _ := os.ReadFile(filepath.Join(s.Dir, "token"))
		token = strings.TrimSpace(string(raw))
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		status(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	resource := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if forbidden {
		status(w, http.StatusForbidden, "Forbidden", resource+` is forbidden: User "system:serviceaccount:monitoring:exporter" cannot list resource "`+resource+`"`)
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": list.Kind, "apiVersion": list.APIVersion, "metadata": metadata, "items": items})
}

// status answers a Status object, as the API server answers a failure.
func status(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "message": message, "code": code})
}
