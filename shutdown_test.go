package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
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
