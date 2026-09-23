package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Shutting down: the wait for the probes in progress, and the second signal
// (main.go).

// helperArgsEnv carries the arguments of a child process running run.
const helperArgsEnv = "PUE_TEST_RUN_ARGS"

// TestRunHelperProcess is not a test: it is the exporter, run in a child
// process by tests that need to signal it.
func TestRunHelperProcess(_ *testing.T) {
	args := os.Getenv(helperArgsEnv)
	if args == "" {
		return
	}
	os.Exit(run(strings.Split(args, "\x1f"), os.Stdout, os.Stderr))
}

// syncBuffer is a bytes.Buffer safe to read while a child process writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// exporterProcess is the exporter running in a child process with a probe to
// a target that never answers in progress, so its shutdown has to wait.
type exporterProcess struct {
	cmd    *exec.Cmd
	exited chan error
	logs   *syncBuffer
	// address is where the exporter listens.
	address string
}

func startHeldExporter(t *testing.T, args ...string) *exporterProcess {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no signals on Windows")
	}
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); target.Close() })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	config := writeIn(t, t.TempDir(), "config.yaml", "collectors:\n  - name: slow\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: v\n        type: gauge\n        expression: 'v=(\\d+)'\n")
	args = append([]string{"--config.file=" + config, "--web.listen-address=" + address}, args...)
	p := &exporterProcess{cmd: exec.Command(os.Args[0], "-test.run=^TestRunHelperProcess$"), exited: make(chan error, 1), logs: &syncBuffer{}, address: address}
	p.cmd.Env = append(os.Environ(), helperArgsEnv+"="+strings.Join(args, "\x1f"))
	p.cmd.Stderr = p.logs
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.exited <- p.cmd.Wait() }()
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	waitFor(t, "the exporter to listen", func() bool {
		resp, err := http.Get("http://" + address + "/health")
		if err == nil {
			_ = resp.Body.Close()
		}
		return err == nil
	})
	// A probe that will not finish keeps the graceful shutdown waiting.
	go func() {
		resp, err := http.Get("http://" + address + "/probe?collector=slow&target=" + target.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	waitFor(t, "the probe to start", func() bool {
		resp, err := http.Get("http://" + address + "/self-metrics")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return strings.Contains(b.String(), `http_exporter_probes_in_flight{collector="slow"} 1`)
	})
	return p
}

func (p *exporterProcess) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// A second SIGINT during shutdown ends the process at once, instead of it
// waiting for the probes in progress.
func TestSecondSignalExitsAtOnce(t *testing.T) {
	p := startHeldExporter(t, "--web.shutdown-timeout=30s")
	p.signal(t, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-p.exited:
		t.Fatalf("the exporter exited before the second signal, with the probe in progress: %v\n%s", err, p.logs.String())
	default:
	}
	start := time.Now()
	p.signal(t, syscall.SIGINT)
	select {
	case err := <-p.exited:
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("the second signal took %s to end the process", took)
		}
		var exit *exec.ExitError
		if err == nil || !errors.As(err, &exit) {
			t.Fatalf("the process ended with %v, want it killed by the signal", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("the second signal did not end the process\n%s", p.logs.String())
	}
}

// --web.shutdown-timeout bounds the wait for the probes in progress; the
// exporter then closes them, says so, and exits 0.
func TestShutdownTimeoutBoundsTheWait(t *testing.T) {
	p := startHeldExporter(t, "--web.shutdown-timeout=1s")
	start := time.Now()
	p.signal(t, syscall.SIGTERM)
	select {
	case err := <-p.exited:
		took := time.Since(start)
		if err != nil {
			t.Fatalf("the exporter exited with %v\n%s", err, p.logs.String())
		}
		if took < 900*time.Millisecond || took > 4*time.Second {
			t.Fatalf("the shutdown took %s with a 1s timeout", took)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
	for _, want := range []string{`"shutdown_timeout":"1s"`, "probes were still in progress when --web.shutdown-timeout ran out"} {
		if !strings.Contains(p.logs.String(), want) {
			t.Errorf("the log is missing %s:\n%s", want, p.logs.String())
		}
	}
}

func TestShutdownTimeoutMustBePositive(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--web.shutdown-timeout=" + value}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--web.shutdown-timeout must be positive") {
			t.Errorf("--web.shutdown-timeout=%s: exit %d, %s", value, code, stderr.String())
		}
	}
}

// With --web.shutdown-delay, a SIGTERM first makes /ready answer 503 while
// probes are still served, and only then begins the graceful shutdown.
func TestShutdownDelayKeepsServingWhileUnready(t *testing.T) {
	p := startHeldExporter(t, "--web.shutdown-delay=1500ms", "--web.shutdown-timeout=1s")
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v=7\n"))
	}))
	defer fast.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	read := func(path string) (int, string, error) {
		resp, err := client.Get("http://" + p.address + path)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), nil
	}
	if code, _, err := read("/ready"); err != nil || code != http.StatusOK {
		t.Fatalf("before the signal /ready is %d, %v", code, err)
	}
	start := time.Now()
	p.signal(t, syscall.SIGTERM)
	waitFor(t, "/ready to answer 503", func() bool {
		code, body, err := read("/ready")
		return err == nil && code == http.StatusServiceUnavailable && strings.Contains(body, "not ready: the exporter is shutting down")
	})
	code, body, err := read("/probe?collector=slow&target=" + url.QueryEscape(fast.URL))
	if err != nil || code != http.StatusOK || !strings.Contains(body, "v 7") {
		t.Fatalf("a probe during the delay: %d %v\n%s", code, err, body)
	}
	if code, _, err := read("/health"); err != nil || code != http.StatusOK {
		t.Errorf("/health during the delay: %d %v", code, err)
	}
	select {
	case err := <-p.exited:
		took := time.Since(start)
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, p.logs.String())
		}
		if took < 1500*time.Millisecond {
			t.Errorf("exited after %s, before the delay ended", took)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
	if !strings.Contains(p.logs.String(), `"shutdown_delay":"1.5s"`) {
		t.Errorf("the log does not name the delay:\n%s", p.logs.String())
	}
}

// Without a delay the listener closes at once, as before.
func TestNoShutdownDelayStopsListeningAtOnce(t *testing.T) {
	p := startHeldExporter(t, "--web.shutdown-timeout=3s")
	p.signal(t, syscall.SIGTERM)
	waitFor(t, "the listener to close", func() bool {
		conn, err := net.DialTimeout("tcp", p.address, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	})
}

func TestShutdownDelayMustNotBeNegative(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--web.shutdown-delay=-1s"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--web.shutdown-delay must not be negative") {
		t.Errorf("exit %d, %s", code, stderr.String())
	}
}

func TestBeginShutdownMakesTheExporterUnready(t *testing.T) {
	server, _ := newCacheTestServer(t, testCollector("app", "text"))
	server.BeginShutdown()
	r := get(server, http.MethodGet, "/ready", "")
	if r.Code != http.StatusServiceUnavailable || !strings.Contains(r.Body.String(), "not ready: the exporter is shutting down") {
		t.Errorf("/ready: %d %q", r.Code, r.Body.String())
	}
	if r := get(server, http.MethodGet, "/health", ""); r.Code != http.StatusOK {
		t.Errorf("/health: %d", r.Code)
	}
}
