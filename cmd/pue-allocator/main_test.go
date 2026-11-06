package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A malformed command line exits 2 with one JSON line naming what to change,
// before any file is read.
func TestAMalformedCommandLineExits2(t *testing.T) {
	for args, want := range map[string]string{
		"--nope":                                        "flag provided but not defined",
		"stray":                                         `unexpected argument "stray"`,
		"--settle=-1s":                                  "--settle must not be negative",
		"--members.max=0":                               "--members.max must be 1 or more",
		"--heartbeat.interval=999ms":                    "--heartbeat.interval must be from 1s to 2m, got 999ms",
		"--heartbeat.interval=2m1s":                     "--heartbeat.interval must be from 1s to 2m, got 2m1s",
		"--heartbeat.interval=0s":                       "--heartbeat.interval must be from 1s to 2m, got 0s",
		"--discovery.max-targets=0":                     "--discovery.max-targets must be 1 or more",
		"--discovery.refresh-interval=999ms":            "--discovery.refresh-interval must be from 1s to 24h, or 0 for each kind's own default, got 999ms",
		"--discovery.refresh-interval=-1s":              "--discovery.refresh-interval must be from 1s to 24h, or 0 for each kind's own default, got -1s",
		"--discovery.refresh-interval=24h0m0.001s":      "--discovery.refresh-interval must be from 1s to 24h, or 0 for each kind's own default, got 24h0m0.001s",
		"--config.watch-interval=-1s":                   "--config.watch-interval must be from 0 to 24h, got -1s",
		"--config.watch-interval=25h":                   "--config.watch-interval must be from 0 to 24h, got 25h0m0s",
		"--log.level=verbose":                           `--log.level "verbose" is not a level`,
		"--web.listen-address=nowhere":                  "--web.listen-address",
		"--peers=pue-allocator":                         `--peers entry "pue-allocator" is not a host:port`,
		"--peers=a:9116,,b:9116":                        `--peers entry "" is not a host:port`,
		"--peers.self=10.0.3.7":                         "--peers.self is given without --peers",
		"--peers.leader-only":                           "--peers.leader-only is given without --peers",
		"--peers=a:9116 --peers.self=pue-allocator-0":   `--peers.self "pue-allocator-0" is not an IP address`,
		"--peers.check-interval=1s":                     "--peers.check-interval is given without --peers",
		"--peers.check-timeout=1s":                      "--peers.check-timeout is given without --peers",
		"--peers.failure-threshold=3":                   "--peers.failure-threshold is given without --peers",
		"--peers.snapshot-timeout=30s":                  "--peers.snapshot-timeout is given without --peers",
		"--peers=a:9116 --peers.check-interval=499ms":   "--peers.check-interval must be from 500ms to 1m, got 499ms",
		"--peers=a:9116 --peers.check-interval=1m1s":    "--peers.check-interval must be from 500ms to 1m, got 1m1s",
		"--peers=a:9116 --peers.check-interval=0s":      "--peers.check-interval must be from 500ms to 1m, got 0s",
		"--peers=a:9116 --peers.check-timeout=99ms":     "--peers.check-timeout must be from 100ms to less than --peers.check-interval (2s), got 99ms",
		"--peers=a:9116 --peers.check-timeout=2s":       "--peers.check-timeout must be from 100ms to less than --peers.check-interval (2s), got 2s",
		"--peers=a:9116 --peers.check-interval=800ms":   "--peers.check-timeout must be from 100ms to less than --peers.check-interval (800ms), got 1s",
		"--peers=a:9116 --peers.failure-threshold=0":    "--peers.failure-threshold must be from 1 to 20, got 0",
		"--peers=a:9116 --peers.failure-threshold=21":   "--peers.failure-threshold must be from 1 to 20, got 21",
		"--peers=a:9116 --peers.snapshot-timeout=5m1s":  "--peers.snapshot-timeout must be from --peers.check-timeout (1s) to 5m, got 5m1s",
		"--peers=a:9116 --peers.snapshot-timeout=999ms": "--peers.snapshot-timeout must be from --peers.check-timeout (1s) to 5m, got 999ms",
	} {
		var stdout, stderr bytes.Buffer
		if code := run(strings.Fields(args), &stdout, &stderr); code != 2 {
			t.Errorf("%s: exited %d, want 2", args, code)
		}
		var line map[string]any
		if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || !strings.Contains(line["error"].(string), want) {
			t.Errorf("%s: logged %q, want a JSON line with %q", args, stderr.String(), want)
		}
	}
}

// --version and -h answer on stdout and exit 0.
func TestVersionAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "pue-allocator version ") {
		t.Errorf("--version: exited %d with %q", code, stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("-h exited %d", code)
	}
	for _, flag := range []string{"-web.listen-address", "-config.file", "-static-targets-file", "-probe-targets-file", "-web.token-file", "-log.level", "-config.watch-interval", "-settle", "-heartbeat.interval", "-members.max", "-discovery.max-targets", "-discovery.refresh-interval", "-peers", "-peers.self", "-peers.leader-only", "-peers.check-interval", "-peers.check-timeout", "-peers.failure-threshold", "-peers.snapshot-timeout", "-version", "-static-targets-file-schema", "-probe-targets-file-schema"} {
		if !strings.Contains(stdout.String(), "  "+flag+" ") && !strings.Contains(stdout.String(), "  "+flag+"\n") {
			t.Errorf("-h does not list %s:\n%s", flag, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), `(default ":9116")`) || !strings.Contains(stdout.String(), "(default 15s)") || !strings.Contains(stdout.String(), "(default 10s)") || !strings.Contains(stdout.String(), "(default 1000)") || !strings.Contains(stdout.String(), "(default 10000)") {
		t.Errorf("-h does not give the defaults of the listen address, the settle time, the heartbeat interval and the member bound:\n%s", stdout.String())
	}
}

// The help of the allocator's flags calls an allocator process an instance,
// as its pages, its answers and its docs do, never a copy.
func TestTheFlagHelpSaysInstance(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("-h exited %d", code)
	}
	if m := regexp.MustCompile(`(?i).*\bcop(y|ies)\b.*`).FindAllString(stdout.String(), -1); len(m) > 0 {
		t.Errorf("-h says copy:\n%s", strings.Join(m, "\n"))
	}
	if !strings.Contains(stdout.String(), "List every instance, this one included") {
		t.Errorf("-h does not tell to list every instance in --peers:\n%s", stdout.String())
	}
}

// --static-targets-file-schema and --probe-targets-file-schema print the
// schemas of the allocator's own files, for editors, and exit 0 without
// reading a file.
func TestSchemaFlagsPrintTheSchemas(t *testing.T) {
	for flag, render := range map[string]func() ([]byte, error){
		"--static-targets-file-schema": allocator.StaticTargetsSchemaJSON,
		"--probe-targets-file-schema":  allocator.ProbeTargetsSchemaJSON,
	} {
		want, err := render()
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{flag, "--config.file=/nonexistent/config.yaml"}, &stdout, &stderr); code != 0 || stdout.String() != string(want) || stderr.Len() != 0 {
			t.Errorf("%s: exited %d, stderr %q, stdout starts %.80q", flag, code, stderr.String(), stdout.String())
		}
	}
}

// Input files that are refused, and a token file that is empty, stop the
// start with exit 1 and a JSON line saying why.
func TestRefusedInputsExit1(t *testing.T) {
	dir := t.TempDir()
	empty := testutil.WriteIn(t, dir, "token", " \n")
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"a missing configuration": {[]string{"--config.file=" + filepath.Join(dir, "missing.yaml")}, "invalid input files"},
		"an empty token file":     {[]string{"--config.file=" + filepath.Join(dir, "missing.yaml"), "--web.token-file=" + empty}, "the token file is empty"},
		"a missing token file":    {[]string{"--web.token-file=" + filepath.Join(dir, "missing")}, "the token file cannot be read"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(c.args, &stdout, &stderr); code != 1 {
			t.Errorf("%s: exited %d, want 1", name, code)
		}
		if lines := testutil.AssertJSONLines(t, &stderr, 1); !strings.Contains(lines[0]["msg"].(string), c.want) {
			t.Errorf("%s: logged %q, want %q", name, lines[0]["msg"], c.want)
		}
	}
}

// Input files that change while the start reads them, as a ConfigMap being
// swapped does, are read again after a pause, up to five times, and only
// then stop the start; any other error stops it at once.
func TestTheStartReadsInputsAgainThatChangeWhileRead(t *testing.T) {
	refused := errors.New("refused")
	in := &allocator.Inputs{}
	for name, c := range map[string]struct {
		answers       []error
		reads, pauses int
		want          error
	}{
		"changing twice":         {[]error{allocator.ErrChanged, allocator.ErrChanged, nil}, 3, 2, nil},
		"changing always":        {[]error{allocator.ErrChanged, allocator.ErrChanged, allocator.ErrChanged, allocator.ErrChanged, allocator.ErrChanged, nil}, 5, 4, allocator.ErrChanged},
		"refused":                {[]error{refused, nil}, 1, 0, refused},
		"changing, then refused": {[]error{allocator.ErrChanged, refused, nil}, 2, 1, refused},
	} {
		reads, pauses := 0, []time.Duration{}
		load := func(allocator.Paths) (*allocator.Inputs, error) {
			err := c.answers[reads]
			reads++
			if err != nil {
				return nil, err
			}
			return in, nil
		}
		got, err := loadStartInputs(load, allocator.Paths{}, func(d time.Duration) { pauses = append(pauses, d) })
		if reads != c.reads || len(pauses) != c.pauses || !errors.Is(err, c.want) || (err == nil) != (got == in) {
			t.Errorf("%s: read %d times with pauses %v, got %v, %v; want %d reads, %d pauses and %v", name, reads, pauses, got, err, c.reads, c.pauses, c.want)
		}
		for _, d := range pauses {
			if d != startLoadPause {
				t.Errorf("%s: paused %s, want %s", name, d, startLoadPause)
			}
		}
		if errors.Is(c.want, allocator.ErrChanged) && !strings.Contains(fmt.Sprint(err), "5 times") {
			t.Errorf("%s: the error %q does not say how often the files were read", name, err)
		}
	}
}
