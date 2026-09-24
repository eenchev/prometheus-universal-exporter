package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Reloads on demand: POST /-/reload with --web.enable-lifecycle, and SIGHUP
// (lifecycle.go).

// reloadable is a server over a configuration file the test can rewrite, and
// optionally a static target file.
type reloadable struct {
	t       *testing.T
	path    string
	targets string
	manager *config.Manager
	server  *Server
}

func newReloadable(t *testing.T, conf string, targets string) *reloadable {
	t.Helper()
	dir := t.TempDir()
	r := &reloadable{t: t, path: filepath.Join(dir, "config.yaml")}
	r.write(r.path, conf)
	cfg, err := config.Load(r.path)
	if err != nil {
		t.Fatal(err)
	}
	r.manager = config.NewManager(cfg, r.path, testutil.QuietLogger(t))
	r.manager.SetPythonPath("python3")
	if targets != "" {
		r.targets = filepath.Join(dir, "targets.yaml")
		r.write(r.targets, targets)
		file, err := config.LoadStaticTargets(r.targets)
		if err != nil {
			t.Fatal(err)
		}
		r.manager.SetTargets(r.targets, file)
	}
	r.server = NewServer(r.manager, "python3", testutil.QuietLogger(t))
	return r
}

func (r *reloadable) write(path, body string) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *reloadable) request(method string, header http.Header) *httptest.ResponseRecorder {
	r.t.Helper()
	request := httptest.NewRequest(method, "/-/reload", nil)
	for name, values := range header {
		request.Header[name] = values
	}
	recorder := httptest.NewRecorder()
	r.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func (r *reloadable) reloads(result string) float64 {
	r.t.Helper()
	return seriesValue(r.t, selfMetrics(r.t, r.server), `http_exporter_config_reloads_total{file="config",result="`+result+`"}`)
}

func TestReloadEndpointNeedsTheLifecycleFlag(t *testing.T) {
	r := newReloadable(t, testutil.CollectorsDocument("first"), "")
	recorder := r.request(http.MethodPost, nil)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "--web.enable-lifecycle") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if r.reloads("success") != 0 || r.reloads("failure") != 0 {
		t.Fatal("a refused request reloaded")
	}
}

func TestReloadEndpoint(t *testing.T) {
	r := newReloadable(t, testutil.CollectorsDocument("first"), "")
	r.server.SetLifecycle(true)

	if recorder := r.request(http.MethodGet, nil); recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "POST, PUT" {
		t.Fatalf("GET: status=%d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}

	// A new configuration is applied at once, with no watch.
	r.write(r.path, testutil.CollectorsDocument("first", "second"))
	recorder := r.request(http.MethodPost, nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "configuration reloaded\n" {
		t.Fatalf("POST: status=%d body=%s", recorder.Code, recorder.Body)
	}
	if got := testutil.CollectorNames(r.manager.Get()); len(got) != 2 {
		t.Fatalf("collectors after reload: %v", got)
	}

	// An unchanged file is reloaded too, and PUT works as POST.
	if recorder := r.request(http.MethodPut, nil); recorder.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", recorder.Code, recorder.Body)
	}

	// A rejected one is answered 500 with the reason, and the previous one
	// stays in force.
	r.write(r.path, testutil.CollectorsDocument("dup", "dup"))
	recorder = r.request(http.MethodPost, nil)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), `duplicate collector "dup"`) || !strings.Contains(recorder.Body.String(), "previous one stays in force") {
		t.Fatalf("rejected POST: status=%d body=%s", recorder.Code, recorder.Body)
	}
	if got := testutil.CollectorNames(r.manager.Get()); len(got) != 2 {
		t.Fatalf("a rejected reload replaced the configuration: %v", got)
	}
	if r.reloads("success") != 2 || r.reloads("failure") != 1 {
		t.Fatalf("reloads success=%v failure=%v", r.reloads("success"), r.reloads("failure"))
	}
}

// The static target file is reloaded with the configuration, and its
// rejection is reported.
func TestReloadEndpointCoversTheTargetFile(t *testing.T) {
	conf := strings.Replace(testutil.CollectorsDocument("text"), "collectors:", "otlp:\n  enabled: true\n  endpoint: http://collector.invalid/v1/metrics\ncollectors:", 1)
	targets := "interval: 1m\ntargets:\n  - name: one\n    collector: text\n    target: http://a.example\n"
	r := newReloadable(t, conf, targets)
	r.server.SetLifecycle(true)
	r.write(r.targets, targets+"  - name: two\n    collector: text\n    target: http://b.example\n")
	if recorder := r.request(http.MethodPost, nil); recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if got := len(r.manager.StaticTargets()); got != 2 {
		t.Fatalf("targets after reload: %d", got)
	}
	r.write(r.targets, "interval: 1m\ntargets:\n  - name: one\n    collector: missing\n    target: http://a.example\n")
	recorder := r.request(http.MethodPost, nil)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "static target file") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if got := len(r.manager.StaticTargets()); got != 2 {
		t.Fatalf("a rejected target file replaced the targets: %d", got)
	}
}

// The endpoint is behind web.basic_auth like the others.
func TestReloadEndpointIsAuthenticated(t *testing.T) {
	conf := "web:\n  basic_auth:\n    enabled: true\n    username: exporter\n    password: secret\n" + testutil.CollectorsDocument("first")
	r := newReloadable(t, conf, "")
	r.server.SetLifecycle(true)
	if recorder := r.request(http.MethodPost, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("without credentials: %d", recorder.Code)
	}
	authorized := httptest.NewRequest(http.MethodPost, "/", nil)
	authorized.SetBasicAuth("exporter", "secret")
	if recorder := r.request(http.MethodPost, authorized.Header); recorder.Code != http.StatusOK {
		t.Fatalf("with credentials: %d %s", recorder.Code, recorder.Body)
	}
}

// SIGHUP reloads.
func TestSIGHUPReloads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGHUP on Windows")
	}
	r := newReloadable(t, testutil.CollectorsDocument("first"), "")
	r.write(r.path, testutil.CollectorsDocument("first", "second"))
	hup := ReloadSignals()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ReloadOn(ctx, hup, r.manager, testutil.QuietLogger(t))
	}()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(r.manager.Get().Collectors) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("SIGHUP did not reload")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// Reloads from every trigger at once do not interleave.
func TestConcurrentReloadsAreSerialized(t *testing.T) {
	r := newReloadable(t, testutil.CollectorsDocument("first"), "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = r.manager.Reload(config.ReloadTriggerHTTP)
		}()
		go func() {
			defer wg.Done()
			_ = r.manager.Reload(config.ReloadTriggerSignal)
		}()
	}
	wg.Wait()
	if got := r.reloads("failure"); got != 0 {
		t.Fatalf("failures=%v", got)
	}
}

// A shutdown turns /ready to 503 at once, and leaves /health answering.
func TestBeginShutdownMakesTheExporterUnready(t *testing.T) {
	server, _ := newCacheTestServer(t, testutil.Collector("app", "text"))
	server.BeginShutdown()
	r := get(server, http.MethodGet, "/ready", "")
	if r.Code != http.StatusServiceUnavailable || !strings.Contains(r.Body.String(), "not ready: the exporter is shutting down") {
		t.Errorf("/ready: %d %q", r.Code, r.Body.String())
	}
	if r := get(server, http.MethodGet, "/health", ""); r.Code != http.StatusOK {
		t.Errorf("/health: %d", r.Code)
	}
}
