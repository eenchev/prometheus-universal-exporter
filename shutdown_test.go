package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
)

// Shutting down: the wait for the probes in progress, and the second signal
// (main.go).

// helperArgsEnv carries the arguments of a child process running run.
const helperArgsEnv = "PUE_TEST_RUN_ARGS"

// helperHoldsDelayEnv, when set, has the child process stay in its
// --web.shutdown-delay, once that is over, until its standard input is
// closed.
const helperHoldsDelayEnv = "PUE_TEST_HOLD_SHUTDOWN_DELAY"

// TestRunHelperProcess is not a test: it is the exporter, run in a child
// process by tests that need to signal it.
//
// A test that has the exporter do something in its --web.shutdown-delay
// cannot have that done in time by a delay however long: the delay is the
// exporter's own clock, and a machine busy enough is later. So the delay of
// such a child is waited out and then goes on until the test, which holds
// the other end of the child's standard input, closes it: the test waits for
// what it is about, and then lets the shutdown begin.
//
// The child leaves a request's headers half a minute, the bound of a hang,
// where the exporter leaves them ten seconds: every test that runs a child
// asks it over a real connection, and none is about that limit
// (exporter.SetReadHeaderTimeout).
func TestRunHelperProcess(_ *testing.T) {
	args := os.Getenv(helperArgsEnv)
	if args == "" {
		return
	}
	if os.Getenv(helperHoldsDelayEnv) != "" {
		waitOutShutdownDelay = func(delay time.Duration) {
			time.Sleep(delay)
			_, _ = io.Copy(io.Discard, os.Stdin)
		}
	}
	exporter.SetReadHeaderTimeout(30 * time.Second)
	os.Exit(run(strings.Split(args, "\x1f"), os.Stdout, os.Stderr))
}

func TestShutdownTimeoutMustBePositive(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--web.shutdown-timeout=" + value}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--web.shutdown-timeout must be positive") {
			t.Errorf("--web.shutdown-timeout=%s: exit %d, %s", value, code, stderr.String())
		}
	}
}

func TestShutdownDelayMustNotBeNegative(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--web.shutdown-delay=-1s"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--web.shutdown-delay must not be negative") {
		t.Errorf("exit %d, %s", code, stderr.String())
	}
}
