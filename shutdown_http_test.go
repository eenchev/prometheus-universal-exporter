//go:build !select_request_types || request_type_http

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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

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
	return startHeldExporterWith(t, "", args...)
}

// startHeldExporterWith is startHeldExporter with more of the configuration,
// such as an otlp block, after its collectors.
func startHeldExporterWith(t *testing.T, config string, args ...string) *exporterProcess {
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
	conf := testutil.WriteIn(t, t.TempDir(), "config.yaml", "collectors:\n  - name: slow\n    request:\n      type: http\n    transform:\n      type: regex\n    metrics:\n      - name: v\n        type: gauge\n        expression: 'v=(\\d+)'\n"+config)
	args = append([]string{"--config.file=" + conf, "--web.listen-address=" + address}, args...)
	p := &exporterProcess{cmd: exec.Command(os.Args[0], "-test.run=^TestRunHelperProcess$"), exited: make(chan error, 1), logs: &syncBuffer{}, address: address}
	p.cmd.Env = append(os.Environ(), helperArgsEnv+"="+strings.Join(args, "\x1f"))
	p.cmd.Stderr = p.logs
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.exited <- p.cmd.Wait() }()
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	testutil.WaitFor(t, "the exporter to listen", func() bool {
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
	testutil.WaitFor(t, "the probe to start", func() bool {
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
	testutil.WaitFor(t, "/ready to answer 503", func() bool {
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
	testutil.WaitFor(t, "the listener to close", func() bool {
		conn, err := net.DialTimeout("tcp", p.address, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	})
}

// Static targets keep being scraped through --web.shutdown-delay, while their
// endpoint is still served, and the shutdown reports none of them failed.
func TestStaticTargetsAreScrapedThroughTheShutdownDelay(t *testing.T) {
	var scrapes atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		scrapes.Add(1)
		_, _ = w.Write([]byte("v=1\n"))
	}))
	defer target.Close()
	targets := testutil.WriteIn(t, t.TempDir(), "targets.yaml", "interval: 1s\ntargets:\n  - name: fast\n    collector: slow\n    target: "+target.URL+"\n")
	p := startHeldExporter(t, "--static-targets-file="+targets, "--web.shutdown-delay=2500ms", "--web.shutdown-timeout=1s")
	testutil.WaitFor(t, "the static target to be scraped", func() bool { return scrapes.Load() >= 1 })
	p.signal(t, syscall.SIGTERM)
	atSignal := scrapes.Load()
	select {
	case err := <-p.exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, p.logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
	if during := scrapes.Load() - atSignal; during < 1 {
		t.Errorf("the static target was scraped %d times during the 2.5s delay, want at least one", during)
	}
	if strings.Contains(p.logs.String(), "static target scrape failed") || strings.Contains(p.logs.String(), "no scrape slot came free") {
		t.Errorf("the shutdown reported a static target failing:\n%s", p.logs.String())
	}
}

// A SIGHUP during the shutdown reloads, as at any other time, rather than
// ending the process with Go's default action for a signal nobody catches.
func TestASIGHUPDuringTheShutdownDoesNotEndIt(t *testing.T) {
	p := startHeldExporter(t, "--web.shutdown-delay=1500ms", "--web.shutdown-timeout=1s")
	p.signal(t, syscall.SIGTERM)
	testutil.WaitFor(t, "the shutdown to begin", func() bool { return strings.Contains(p.logs.String(), "shutting down") })
	p.signal(t, syscall.SIGHUP)
	select {
	case err := <-p.exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, p.logs.String())
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
	if !strings.Contains(p.logs.String(), "shutting down: finishing the probes in progress") {
		t.Fatalf("the process ended before the graceful shutdown:\n%s", p.logs.String())
	}
}

// The OTLP export keeps running through --web.shutdown-delay, as the static
// targets whose results it delivers keep being scraped: an export arrives
// after the signal and before the delay has ended, which the log tells — the
// last export, sent as the exporter finishes, comes after the line that says
// so. The delay is five intervals long, so the export is one of several the
// delay has room for, whatever the machine is busy with.
func TestOTLPExportsThroughTheShutdownDelay(t *testing.T) {
	var exports atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exports.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	// 1s is the least otlp.interval a configuration may set.
	p := startHeldExporterWith(t, "otlp:\n  enabled: true\n  endpoint: "+collector.URL+"/v1/metrics\n  interval: 1s\n", "--web.shutdown-delay=5s", "--web.shutdown-timeout=1s")
	testutil.WaitFor(t, "an export", func() bool { return exports.Load() >= 1 })
	p.signal(t, syscall.SIGTERM)
	testutil.WaitFor(t, "the delay to begin", func() bool { return strings.Contains(p.logs.String(), "shutting down: /ready answers 503") })
	inDelay := exports.Load()
	testutil.WaitFor(t, "an export during the delay", func() bool { return exports.Load() > inDelay })
	if strings.Contains(p.logs.String(), "shutting down: finishing the probes in progress") {
		t.Errorf("no export arrived in a 5s delay at a 1s interval before the delay ended\n%s", p.logs.String())
	}
	select {
	case err := <-p.exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, p.logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
}

// A SIGHUP sent while the exporter is still starting, its configuration read
// and its Python scripts being checked, does not end the process, as Go's
// default action for a signal nobody catches would: it waits, and the
// exporter reloads once, as soon as it has started.
func TestASIGHUPDuringStartupReloadsOnceStarted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals on Windows")
	}
	dir := t.TempDir()
	reached, release := dir+"/reached", dir+"/release"
	// The interpreter the scripts are checked with says when the check has
	// begun, and holds it until the test has sent its signal.
	python := dir + "/python"
	if err := os.WriteFile(python, []byte("#!/bin/sh\n: > '"+reached+"'\nwhile [ ! -e '"+release+"' ]; do sleep 0.05; done\nexec python3 \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := testutil.WriteIn(t, dir, "config.yaml", "collectors:\n  - name: scripted\n    request:\n      type: http\n    transform:\n      type: python\n      script: |\n        metric(\"x\", 1)\n")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	logs := &syncBuffer{}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunHelperProcess$")
	cmd.Env = append(os.Environ(), helperArgsEnv+"="+strings.Join([]string{"--config.file=" + conf, "--web.listen-address=" + address, "--python.path=" + python}, "\x1f"))
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// waitFor is testutil.WaitFor that also notices the exporter ending.
	waitFor := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !done() {
			select {
			case err := <-exited:
				t.Fatalf("the exporter ended while the test waited for %s: %v\n%s", what, err, logs.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s\n%s", what, logs.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the startup to reach the Python check", func() bool {
		_, err := os.Stat(reached)
		return err == nil
	})
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// The signal is delivered before the startup goes on.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("the exporter to listen", func() bool {
		resp, err := http.Get("http://" + address + "/health")
		if err == nil {
			_ = resp.Body.Close()
		}
		return err == nil
	})
	waitFor("the reload the signal asked for", func() bool {
		return strings.Contains(logs.String(), `"msg":"configuration reloaded","trigger":"sighup"`)
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, logs.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", logs.String())
	}
	if n := strings.Count(logs.String(), `"msg":"configuration reloaded","trigger":"sighup"`); n != 1 {
		t.Fatalf("one SIGHUP reloaded %d times:\n%s", n, logs.String())
	}
}
