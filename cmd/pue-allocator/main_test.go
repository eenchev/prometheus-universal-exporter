package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A malformed command line exits 2 with one JSON line naming what to change,
// before any file is read.
func TestAMalformedCommandLineExits2(t *testing.T) {
	for args, want := range map[string]string{
		"--nope":                       "flag provided but not defined",
		"stray":                        `unexpected argument "stray"`,
		"--settle=-1s":                 "--settle must not be negative",
		"--members.max=0":              "--members.max must be 1 or more",
		"--config.watch-interval=-1s":  "--config.watch-interval must not be negative",
		"--log.level=verbose":          `--log.level "verbose" is not a level`,
		"--web.listen-address=nowhere": "--web.listen-address",
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
	for _, flag := range []string{"-web.listen-address", "-config.file", "-static-targets-file", "-probe-targets-file", "-web.token-file", "-log.level", "-config.watch-interval", "-settle", "-members.max", "-version"} {
		if !strings.Contains(stdout.String(), "  "+flag+" ") && !strings.Contains(stdout.String(), "  "+flag+"\n") {
			t.Errorf("-h does not list %s:\n%s", flag, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), `(default ":9116")`) || !strings.Contains(stdout.String(), "(default 15s)") || !strings.Contains(stdout.String(), "(default 1000)") {
		t.Errorf("-h does not give the defaults of the listen address, the settle time and the member bound:\n%s", stdout.String())
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
