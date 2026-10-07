//go:build !select_request_types || request_type_grpc

package main

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// watchedExporter is the exporter running in a child process with the watch
// on, saying when each tick of its watch is over (helperSaysTicksEnv), and
// the lines of its standard error as they come.
type watchedExporter struct {
	t *testing.T
	// exited is sent the process's end, once all it wrote has been read.
	exited chan error
	// more is sent to when a line has come.
	more chan struct{}
	mu   sync.Mutex
	// lines are all the lines so far, and read how many of them next has
	// given out.
	lines []string
	read  int
}

// logs is everything the exporter wrote so far.
func (e *watchedExporter) logs() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.lines, "\n")
}

// next is the next line the exporter wrote, waited for; it fails the test
// when the exporter ends first, or writes nothing for half a minute, the
// bound of a hang.
func (e *watchedExporter) next(what string) string {
	e.t.Helper()
	for {
		e.mu.Lock()
		if e.read < len(e.lines) {
			line := e.lines[e.read]
			e.read++
			e.mu.Unlock()
			return line
		}
		e.mu.Unlock()
		select {
		case <-e.more:
		case err := <-e.exited:
			e.t.Fatalf("the exporter ended while the test waited for %s: %v\n%s", what, err, e.logs())
		case <-time.After(30 * time.Second):
			e.t.Fatalf("timed out waiting for %s\n%s", what, e.logs())
		}
	}
}

// reloadedByTheWatch is what the line of a reload the watch made holds,
// after its time and level.
const reloadedByTheWatch = `"msg":"configuration reloaded","trigger":"watch"`

// throughTicks reads on until the exporter has said ticks more ticks of its
// watch are over, and returns how many reloads the watch logged meanwhile.
// A tick's reload is logged before the tick is said to be over, so the
// reloads counted are those of exactly these ticks, and of no other.
func (e *watchedExporter) throughTicks(ticks int, what string) (reloads int) {
	e.t.Helper()
	for ticks > 0 {
		switch line := e.next(what); {
		case line == watchTickedLine:
			ticks--
		case strings.Contains(line, reloadedByTheWatch):
			reloads++
		}
	}
	return reloads
}

// The exporter stamps the descriptor files of the configuration it starts
// with before it checks the configuration against them, as it stamps the
// configuration file before it reads it: a .proto file that a collector's
// file imports, edited while the exporter is still starting — its
// configuration read and its Python scripts being checked — is not taken
// as read, and the first tick of the watch reloads for it, once: the two
// ticks after it reload nothing. A second edit, of the file the collector
// names, is then one reload more, at one tick, and the two ticks after that
// one reload nothing either, so the exporter has reloaded twice when it
// ends.
//
// That a tick reloaded nothing is waited for, not timed: the exporter in
// the child says when each tick is over, behind whatever the tick logged
// (TestRunHelperProcess), and the test reads its lines in their order. An
// exporter that reloaded at every tick would log a reload before each of
// those lines.
func TestADescriptorFileEditedDuringStartupIsReloadedByTheFirstTickOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals and no named pipes on Windows")
	}
	dir := t.TempDir()
	// The interpreter the scripts are checked with says when the first
	// check, the startup's, has begun, and holds it until the test has
	// edited the file; the checks of the reloads go straight on. It says
	// so, and waits, on two named pipes of which the test holds both ends:
	// neither side waits for the other to open one, and an interpreter that
	// still waits is let go when the test closes its ends.
	reached, release, released := dir+"/reached", dir+"/release", dir+"/released"
	pipes := map[string]*os.File{}
	for _, path := range []string{reached, release} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		pipe, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		pipes[path] = pipe
		t.Cleanup(func() { _ = pipe.Close() })
	}
	python := dir + "/python"
	if err := os.WriteFile(python, []byte("#!/bin/sh\nif [ ! -e '"+released+"' ]; then\n  echo reached > '"+reached+"'\n  read -r line < '"+release+"'\nfi\nexec python3 \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	const serviceSource = "syntax = \"proto3\";\npackage w;\nimport \"types.proto\";\nservice S { rpc Get(Request) returns (Request); }\n"
	service := testutil.WriteIn(t, dir, "protos/svc.proto", serviceSource)
	const types = "syntax = \"proto3\";\npackage w;\nmessage Request { string queue = 1; }\n"
	imported := testutil.WriteIn(t, dir, "protos/types.proto", types)
	conf := testutil.WriteIn(t, dir, "config.yaml", "collectors:\n  - name: scripted\n    request:\n      type: grpc\n      rpc: w.S/Get\n      message: '{\"queue\": \"orders\"}'\n"+
		"      descriptors: proto\n      proto_files: ["+service+"]\n    transform:\n      type: python\n      script: |\n        metric(\"x\", 1)\n")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	stderr, written, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunHelperProcess$")
	cmd.Env = append(os.Environ(), helperSaysTicksEnv+"=1",
		helperArgsEnv+"="+strings.Join([]string{"--config.file=" + conf, "--web.listen-address=" + address, "--python.path=" + python, "--config.watch", "--config.watch-interval=20ms"}, "\x1f"))
	cmd.Stderr = written
	// The exporter leads a process group of its own, which the interpreter
	// it starts is in: a test that fails with the startup still held ends
	// the interpreter with the exporter.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = written.Close()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	e := &watchedExporter{t: t, exited: make(chan error, 1), more: make(chan struct{}, 1)}
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			e.mu.Lock()
			e.lines = append(e.lines, scanner.Text())
			e.mu.Unlock()
			select {
			case e.more <- struct{}{}:
			default:
			}
		}
		// A line that could not be read ends the reading, and is the test's
		// to know of as the process's end is.
		err := scanner.Err()
		_ = stderr.Close()
		if waitErr := cmd.Wait(); err == nil {
			err = waitErr
		}
		e.exited <- err
	}()

	startupReached := make(chan error, 1)
	go func() {
		_, err := pipes[reached].Read(make([]byte, 1))
		startupReached <- err
	}()
	select {
	case err := <-startupReached:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-e.exited:
		t.Fatalf("the exporter ended before its startup reached the Python check: %v\n%s", err, e.logs())
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for the startup to reach the Python check\n%s", e.logs())
	}
	if err := os.WriteFile(imported, []byte(types+"message Other { string name = 1; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(released, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pipes[release].WriteString("go on\n"); err != nil {
		t.Fatal(err)
	}

	if reloads := e.throughTicks(1, "the first tick of the watch"); reloads != 1 {
		t.Fatalf("the first tick reloaded %d times for the file edited during the startup, want once:\n%s", reloads, e.logs())
	}
	if reloads := e.throughTicks(2, "the two ticks after the first"); reloads != 0 {
		t.Fatalf("the two ticks after the first reloaded %d times with nothing changed:\n%s", reloads, e.logs())
	}

	if err := os.WriteFile(service, []byte(serviceSource+"// a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for reloaded := false; !reloaded; {
		reloaded = strings.Contains(e.next("the reload for the file the collector names"), reloadedByTheWatch)
	}
	if reloads := e.throughTicks(3, "the tick that reloaded for the named file and the two after it"); reloads != 0 {
		t.Fatalf("the tick that reloaded for the second edit and the two after it reloaded %d times more:\n%s", reloads, e.logs())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-e.exited:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, e.logs())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the exporter did not exit\n%s", e.logs())
	}
	if n := strings.Count(e.logs(), reloadedByTheWatch); n != 2 || strings.Contains(e.logs(), "reload rejected") {
		t.Fatalf("two edits reloaded %d times, or a reload was rejected:\n%s", n, e.logs())
	}
}
