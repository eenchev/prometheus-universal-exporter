//go:build !select_request_types || request_type_http

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An exporter in allocator mode, run in a child process against a stand-in
// for the target allocator (allocator.go, internal/allocclient).

// allocatorStandIn answers heartbeats, passing them on to the test, and
// bundle requests with the bundle it holds, holding a request that already
// has it until it holds another.
type allocatorStandIn struct {
	server     *httptest.Server
	heartbeats chan allocapi.Heartbeat
	mu         sync.Mutex
	bundle     allocapi.Bundle
	changed    chan struct{}
}

func newAllocatorStandIn(t *testing.T) *allocatorStandIn {
	t.Helper()
	a := &allocatorStandIn{heartbeats: make(chan allocapi.Heartbeat, 256), changed: make(chan struct{})}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case allocapi.HeartbeatPath:
			var hb allocapi.Heartbeat
			_ = json.NewDecoder(r.Body).Decode(&hb)
			a.heartbeats <- hb
			_ = json.NewEncoder(w).Encode(allocapi.HeartbeatReply{})
		case allocapi.BundlePath:
			for {
				a.mu.Lock()
				b, changed := a.bundle, a.changed
				a.mu.Unlock()
				if b.Generation != "" && r.Header.Get("If-None-Match") != `"`+b.Generation+`"` {
					_ = json.NewEncoder(w).Encode(b)
					return
				}
				select {
				case <-changed:
				case <-r.Context().Done():
					return
				}
			}
		}
	}))
	t.Cleanup(a.server.Close)
	return a
}

func (a *allocatorStandIn) serve(files map[string]string) string {
	b := allocapi.Bundle{Files: files}
	b.Seal()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bundle = b
	close(a.changed)
	a.changed = make(chan struct{})
	return b.Generation
}

// startAllocatorExporter runs the exporter in a child process with args, on
// a free port, its --web.shutdown-delay held until the test ends it, and
// waits for it to listen.
func startAllocatorExporter(t *testing.T, args ...string) *exporterProcess {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no signals on Windows")
	}
	for attempt := 1; ; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		p := &exporterProcess{cmd: exec.Command(os.Args[0], "-test.run=^TestRunHelperProcess$"), exited: make(chan error, 1), ended: make(chan struct{}), logs: &syncBuffer{}, address: address}
		p.cmd.Env = append(os.Environ(), helperArgsEnv+"="+strings.Join(append([]string{"--web.listen-address=" + address}, args...), "\x1f"), helperHoldsDelayEnv+"=1")
		p.cmd.Stderr = p.logs
		held, err := p.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		p.endDelay = sync.OnceFunc(func() { _ = held.Close() })
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			p.exited <- p.cmd.Wait()
			close(p.ended)
		}()
		t.Cleanup(func() { _ = p.cmd.Process.Kill() })
		listening := false
		testutil.WaitFor(t, "the exporter to listen", func() bool {
			select {
			case <-p.ended:
				return true
			default:
			}
			code, _ := p.get(t, "/health")
			listening = code == http.StatusOK
			return listening
		})
		if listening {
			return p
		}
		if attempt == 5 || !strings.Contains(p.logs.String(), "address already in use") {
			t.Fatalf("the exporter ended before it listened:\n%s", p.logs.String())
		}
	}
}

// get asks the exporter for path and returns the status and the body, 0
// when it did not answer.
func (p *exporterProcess) get(t *testing.T, path string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + p.address + path)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// allocatorTestConfig is a bundle's configuration: one regex collector.
const allocatorTestConfig = "collectors:\n  - name: a\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: a_value\n        expression: 'value=(\\d+)'\n"

// valueTarget answers value=7.
func valueTarget(t *testing.T) string {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("value=7\n")) }))
	t.Cleanup(target.Close)
	return "/probe?collector=a&target=" + url.QueryEscape(target.URL)
}

// An exporter in allocator mode with an empty cache directory starts at
// once and serves its endpoints, unready, with /probe answering 503 with a
// sentence that names the allocator; the first bundle makes it ready and
// probes are answered from it, and the bundle is in the cache directory. Its
// first signal sends a heartbeat saying it is shutting down at once, before
// --web.shutdown-delay is over.
func TestAllocatorModeIsUnreadyUntilTheFirstBundle(t *testing.T) {
	a := newAllocatorStandIn(t)
	dir := t.TempDir()
	p := startAllocatorExporter(t, "--allocator.url="+a.server.URL, "--allocator.cache-dir="+dir, "--allocator.member=pod-0", "--allocator.advertise=10.0.0.7:8080", "--web.shutdown-delay=1s")
	probe := valueTarget(t)
	if code, body := p.get(t, "/ready"); code != http.StatusServiceUnavailable || !strings.Contains(body, "no configuration yet") {
		t.Fatalf("/ready before the bundle: %d %q", code, body)
	}
	if code, body := p.get(t, probe); code != http.StatusServiceUnavailable || body != "no configuration yet: waiting for the allocator at "+a.server.URL+"\n" {
		t.Fatalf("/probe before the bundle: %d %q", code, body)
	}
	if code, _ := p.get(t, "/self-metrics"); code != http.StatusOK {
		t.Fatalf("/self-metrics before the bundle: %d", code)
	}
	if hb := <-a.heartbeats; hb.Member != "pod-0" || hb.Advertise != "10.0.0.7:8080" || hb.Generation != "" {
		t.Fatalf("the first heartbeat: %+v", hb)
	}
	generation := a.serve(map[string]string{"config.yaml": allocatorTestConfig})
	testutil.WaitFor(t, "the exporter to be ready", func() bool {
		code, _ := p.get(t, "/ready")
		return code == http.StatusOK
	})
	if code, body := p.get(t, probe); code != http.StatusOK || !strings.Contains(body, "a_value 7") {
		t.Fatalf("/probe after the bundle: %d %q", code, body)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.yaml")); err != nil || string(b) != allocatorTestConfig { // #nosec G304 -- a test's file
		t.Fatalf("the cache directory's config.yaml: %q %v", b, err)
	}
	if _, body := p.get(t, "/self-metrics"); !strings.Contains(body, `http_exporter_allocator_generation_info{generation="`+generation+`"} 1`) {
		t.Errorf("the self-metrics do not name the generation:\n%s", body)
	}
	p.signal(t, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		select {
		case hb := <-a.heartbeats:
			if !hb.ShuttingDown {
				continue
			}
			if hb.Generation != generation {
				t.Errorf("the shutting-down heartbeat: %+v", hb)
			}
		case <-ctx.Done():
			t.Fatalf("no heartbeat said the exporter is shutting down\n%s", p.logs.String())
		}
		break
	}
	p.endDelay()
	select {
	case err := <-p.exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, p.logs.String())
		}
	case <-ctx.Done():
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
}

// installBundle lays files out in the cache directory dir as the allocator
// client installs a bundle (internal/allocclient/cache.go): in
// ..gen-<generation>, with ..data linking to it and each top-level name
// linking through ..data. It returns the generation.
func installBundle(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	generation := allocapi.Generation(files)
	linked := map[string]bool{}
	for name, content := range files {
		testutil.WriteIn(t, filepath.Join(dir, "..gen-"+generation), name, content)
		top, _, _ := strings.Cut(name, "/")
		if !linked[top] {
			linked[top] = true
			if err := os.Symlink("..data/"+top, filepath.Join(dir, top)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink("..gen-"+generation, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	return generation
}

// An exporter in allocator mode whose cache directory holds a bundle starts
// with it at once, ready, although the allocator cannot be reached.
func TestAllocatorModeResumesFromTheCacheDirectoryWithoutTheAllocator(t *testing.T) {
	dir := t.TempDir()
	installBundle(t, dir, map[string]string{"config.yaml": allocatorTestConfig})
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	p := startAllocatorExporter(t, "--allocator.url="+gone.URL, "--allocator.cache-dir="+dir, "--allocator.member=pod-0", "--allocator.advertise=10.0.0.7:8080")
	if code, body := p.get(t, "/ready"); code != http.StatusOK {
		t.Fatalf("/ready: %d %q\n%s", code, body, p.logs.String())
	}
	if code, body := p.get(t, valueTarget(t)); code != http.StatusOK || !strings.Contains(body, "a_value 7") {
		t.Fatalf("/probe: %d %q", code, body)
	}
}

// A bundle in the cache directory that does not load is not started with:
// the exporter awaits the allocator, and says why.
func TestAllocatorModeAwaitsTheAllocatorWhenTheCachedBundleDoesNotLoad(t *testing.T) {
	dir := t.TempDir()
	installBundle(t, dir, map[string]string{"config.yaml": "collectors: []\n"})
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	p := startAllocatorExporter(t, "--allocator.url="+gone.URL, "--allocator.cache-dir="+dir, "--allocator.member=pod-0", "--allocator.advertise=10.0.0.7:8080")
	if code, _ := p.get(t, "/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("/ready: %d", code)
	}
	if !strings.Contains(p.logs.String(), "the bundle in the allocator's cache directory does not load; waiting for the allocator") {
		t.Errorf("the log does not say why:\n%s", p.logs.String())
	}
}

// --dry-run in allocator mode passes the bundle in the cache directory that
// loads.
func TestDryRunInAllocatorModePassesACachedBundle(t *testing.T) {
	dir := t.TempDir()
	installBundle(t, dir, map[string]string{"config.yaml": allocatorTestConfig})
	out := runCheckCLI(t, "--allocator.url=http://pue-allocator:8080", "--allocator.cache-dir="+dir)
	if out.code != 0 || out.result(t, "config").File != filepath.Join(dir, "config.yaml") {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

// A config.yaml in the cache directory that the allocator client did not
// install, such as one put there by hand, is no bundle: the exporter starts
// without a configuration, unready, and removes the file, so neither a
// reload nor the watch puts it in force while the heartbeats report no
// generation.
func TestAllocatorModeIgnoresAConfigurationItDidNotInstall(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "config.yaml", allocatorTestConfig)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	p := startAllocatorExporter(t, "--allocator.url="+gone.URL, "--allocator.cache-dir="+dir, "--allocator.member=pod-0", "--allocator.advertise=10.0.0.7:8080", "--config.watch")
	if code, body := p.get(t, "/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("/ready: %d %q\n%s", code, body, p.logs.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("the config.yaml put there by hand is still in the cache directory: %v", err)
	}
}

// --dry-run in allocator mode fails a config.yaml in the cache directory that
// the allocator client did not install, which a start ignores, and leaves
// it there.
func TestDryRunInAllocatorModeFailsAConfigurationItDidNotInstall(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "config.yaml", allocatorTestConfig)
	out := runCheckCLI(t, "--allocator.url=http://pue-allocator:8080", "--allocator.cache-dir="+dir)
	if result := out.result(t, "config"); out.code != 1 || !strings.Contains(strings.Join(result.Errors, " "), "is not a bundle the allocator client installed") {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Errorf("--dry-run changed the cache directory: %v", err)
	}
}

// blockingReloader is a configuration manager whose reload of a new bundle
// waits until the test releases it.
type blockingReloader struct {
	entered, release chan struct{}
	once             sync.Once
}

func (r *blockingReloader) CheckReplacement(string, string) error { return nil }

func (r *blockingReloader) ReloadReplacing(_ string, replace func() (func() error, error)) error {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	_, err := replace()
	return err
}

// Stopping the allocator client waits for its Run to return: a bundle being
// put in force when the exporter shuts down is finished, not cut short by
// the process ending.
func TestStoppingTheAllocatorClientWaitsForItsRun(t *testing.T) {
	a := newAllocatorStandIn(t)
	a.serve(map[string]string{"config.yaml": allocatorTestConfig})
	mode := &allocatorMode{url: a.server.URL, cacheDir: t.TempDir(), member: "pod-0", advertise: "10.0.0.7:8080"}
	r := &blockingReloader{entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(r.release) })
	t.Cleanup(release)
	_, stop, err := mode.startClient(r, "", newLogger("error", io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the client did not put the bundle in force")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while the client was putting a bundle in force")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("stop did not return once the bundle was in force")
	}
}
