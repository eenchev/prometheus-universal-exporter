//go:build !select_request_types || request_type_http

package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What the shutdown waits for (main.go): the probes in progress, which it
// names, and not the static target scrapes in flight, which it cuts short.

// silentListener accepts connections and never answers on them, as a
// target that has hung does. accepted counts the connections.
func silentListener(t *testing.T) (address string, accepted *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted = &atomic.Int64{}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
			accepted.Add(1)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return listener.Addr().String(), accepted
}

// soonScrapedName is a target name whose first scrape is due within half a
// second of the start: the schedule spreads first scrapes over ten seconds
// by an FNV-1a hash of the name (exporter.scheduleOffset), and a test need
// not wait out that window.
func soonScrapedName(t *testing.T) string {
	t.Helper()
	for i := range 100000 {
		name := fmt.Sprintf("hung_%d", i)
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(name))
		if time.Duration(hash.Sum64()%uint64(10*time.Second)) < 500*time.Millisecond {
			return name
		}
	}
	t.Fatal("no name is scheduled that soon")
	return ""
}

// A shutdown does not wait for a static target scrape in flight: it cuts it
// short at once, and logs no failure for it, as the target did not fail.
// With nothing else in progress the exporter exits 0 at once.
//
// The target accepts the connection and never answers, and the interval, the
// scrape's own bound, is a minute, so only the shutdown ends the scrape. A
// shutdown that waited for it took all of --web.shutdown-timeout, 15
// seconds; five is the bound of a busy machine.
func TestAShutdownDoesNotWaitForStaticTargetScrapes(t *testing.T) {
	address, accepted := silentListener(t)
	targets := testutil.WriteIn(t, t.TempDir(), "targets.yaml", "interval: 1m\ntargets:\n  - name: "+soonScrapedName(t)+"\n    collector: slow\n    target: http://"+address+"/\n")
	p := startExporterProcess(t, "", childOptions{}, "--static-targets-file="+targets, "--web.shutdown-timeout=15s")
	testutil.WaitFor(t, "the static target scrape to reach the target", func() bool { return accepted.Load() >= 1 })
	start := time.Now()
	p.signal(t, syscall.SIGINT)
	select {
	case err := <-p.exited:
		if err != nil {
			t.Fatalf("the exporter exited with %v\n%s", err, p.logs.String())
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("the shutdown took %s, waiting for the static target scrape", took)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
	if !strings.Contains(p.logs.String(), `"msg":"shutting down: no probes in progress; exiting","probes":0`) {
		t.Errorf("the shutdown did not say it had nothing to wait for:\n%s", p.logs.String())
	}
	for _, line := range strings.Split(p.logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) || strings.Contains(line, `"level":"WARN"`) {
			t.Errorf("the shutdown logged: %s", line)
		}
	}
}

// A shutdown with a probe in progress waits for it, within
// --web.shutdown-timeout, and says so: what it waits for and for how long as
// it begins, what it still waits for every five seconds, and, when the
// timeout runs out, that it gave up on it. A static target scrape in flight
// is cut short meanwhile, and is not among what it waits for.
//
// The probe's target never answers, so the shutdown waits all of its seven
// seconds, which holds one progress line, at five.
func TestAShutdownSaysWhatItWaitsFor(t *testing.T) {
	address, accepted := silentListener(t)
	targets := testutil.WriteIn(t, t.TempDir(), "targets.yaml", "interval: 1m\ntargets:\n  - name: "+soonScrapedName(t)+"\n    collector: slow\n    target: http://"+address+"/\n")
	p := startHeldExporter(t, "--static-targets-file="+targets, "--web.shutdown-timeout=7s")
	testutil.WaitFor(t, "the static target scrape to reach the target", func() bool { return accepted.Load() >= 1 })
	start := time.Now()
	p.signal(t, syscall.SIGINT)
	select {
	case err := <-p.exited:
		if err != nil {
			t.Fatalf("the exporter exited with %v\n%s", err, p.logs.String())
		}
		if took := time.Since(start); took < 6900*time.Millisecond {
			t.Errorf("the shutdown took %s, less than its timeout, with the probe in progress", took)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", p.logs.String())
	}
	for _, want := range []string{
		`"msg":"shutting down: waiting for 1 probe in progress, at most 7s; a second signal exits at once","probes":1,"other_requests":0,"shutdown_timeout":"7s"`,
		// Two seconds are left at five, or less on a machine late with the
		// line.
		`"msg":"shutting down: still waiting for 1 probe in progress, `,
		`s left","probes":1,"other_requests":0,"time_left":"`,
		`"level":"ERROR","msg":"shutting down: gave up waiting for 1 probe in progress; --web.shutdown-timeout ran out and their connections are closed","probes":1`,
	} {
		if !strings.Contains(p.logs.String(), want) {
			t.Errorf("the log is missing %s:\n%s", want, p.logs.String())
		}
	}
	if n := strings.Count(p.logs.String(), "still waiting for"); n != 1 {
		t.Errorf("%d progress lines in a wait of 7s, want 1:\n%s", n, p.logs.String())
	}
	if strings.Contains(p.logs.String(), "static target scrape failed") {
		t.Errorf("the static target scrape the shutdown cut short was logged as failed:\n%s", p.logs.String())
	}
}

// A second signal in --web.shutdown-delay ends the process at once too,
// killed by the signal, and says so.
//
// The exporter stays in its delay until the test lets it go (endDelay),
// which it never does: only the second signal ends it.
func TestASecondSignalInTheShutdownDelayExitsAtOnce(t *testing.T) {
	p := startHeldExporter(t, "--web.shutdown-delay=1s", "--web.shutdown-timeout=10m")
	p.signal(t, syscall.SIGTERM)
	testutil.WaitFor(t, "the delay to begin", func() bool { return strings.Contains(p.logs.String(), "shutting down: /ready answers 503") })
	p.signal(t, syscall.SIGTERM)
	select {
	case err := <-p.exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the process ended with %v, want it killed by the signal", err)
		}
		if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Errorf("the process ended with %v, want it killed by SIGTERM", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the second signal did not end the process\n%s", p.logs.String())
	}
	if !strings.Contains(p.logs.String(), `"msg":"second signal: exiting now, 1 probe in progress left unfinished","signal":"terminated"`) {
		t.Errorf("the second signal was not logged:\n%s", p.logs.String())
	}
}

// An exporter started with SIGINT ignored, as a shell starts a background
// job, is not ended by the signal's default action, which is to ignore it:
// a second SIGINT has it exit with the status a shell reports for a process
// the signal killed, 130, rather than go on with the shutdown.
//
// The shutdown would wait ten minutes for the probe, so a process that has
// exited 130 half a minute after the second signal was ended by it.
func TestASecondSignalEndsAnExporterStartedWithItIgnored(t *testing.T) {
	p := startExporterProcess(t, "", childOptions{holdProbe: true, ignoreSIGINT: true}, "--web.shutdown-timeout=10m")
	p.signal(t, syscall.SIGINT)
	testutil.WaitFor(t, "the shutdown to begin", func() bool { return strings.Contains(p.logs.String(), "shutting down: waiting for 1 probe") })
	p.signal(t, syscall.SIGINT)
	select {
	case err := <-p.exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 130 {
			t.Fatalf("the process ended with %v, want exit status 130\n%s", err, p.logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the second signal did not end the process\n%s", p.logs.String())
	}
	if !strings.Contains(p.logs.String(), "second signal: exiting now, 1 probe in progress left unfinished") {
		t.Errorf("the second signal was not logged:\n%s", p.logs.String())
	}
}
