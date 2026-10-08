package transform

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// interpretersOfTheSandbox are the interpreters whose sandbox the tests below
// hold: python3 from PATH, the one every workflow installs, and each release
// the worker is known to run on, by name, so a machine with both runs both.
// The image and CI have 3.12 only; 3.13 imports what a traceback needs at
// other times than 3.12 does, so it is run where it is installed, and a
// subtest says it is skipped where it is not.
var interpretersOfTheSandbox = []string{"python3", "python3.12", "python3.13"}

// eachInterpreterOfTheSandbox runs check once for each of those interpreters
// that is installed, as a subtest named after it, and skips the others saying
// which is missing.
func eachInterpreterOfTheSandbox(t *testing.T, check func(t *testing.T, python string)) {
	t.Helper()
	requirePython(t)
	for _, python := range interpretersOfTheSandbox {
		t.Run(python, func(t *testing.T) {
			if _, err := exec.LookPath(python); err != nil {
				t.Skipf("%s is not installed, so the worker is not tested on it here", python)
			}
			check(t, python)
		})
	}
}

// runScriptWith runs a script in a worker of the given interpreter, on a
// response whose body is not JSON.
func runScriptWith(python, name, script string) error {
	c := workerCollector(name, script)
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte("not json"), Headers: http.Header{}}
	_, err := executePython(context.Background(), python, c.Transform.Script, &decode.Decoded{Kind: "text", Data: "not json", Raw: r.Body}, r, c)
	return err
}

// A script that fails inside a library is shown the whole traceback, the
// library's frames with their source lines among it, on every interpreter the
// worker runs on. The traceback module reads a library's source through
// linecache and tokenize.open, which opens it with what tokenize took for
// open when it was imported: Python 3.13 imports tokenize only when linecache
// first reads a file, after the sandbox is in place, so the read was refused
// and the error was a stand-in naming only its type.
func TestAScriptFailingInALibraryShowsTheLibrarysFramesOnEveryInterpreter(t *testing.T) {
	eachInterpreterOfTheSandbox(t, func(t *testing.T, python string) {
		for _, test := range []struct{ script, line string }{
			{"import json\njson.loads('x')", "json.loads('x')"},
			{"x = 1\nresponse.json()", "response.json()"},
		} {
			err := runScriptWith(python, "library_frame", test.script)
			if err == nil {
				t.Fatalf("%q did not fail", test.script)
			}
			text := err.Error()
			for _, want := range []string{
				"python transform failed: Traceback (most recent call last):\n",
				"File \"<collector-python>\", line 2, in <module>\n    " + test.line + "\n",
				string(filepath.Separator) + filepath.Join("json", "decoder.py") + "\", line ",
				", in raw_decode\n    raise JSONDecodeError(\"Expecting value\", s, err.value) from None\n",
				"\njson.decoder.JSONDecodeError: Expecting value: line 1 column 1 (char 0)",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("%q: the error lacks %q:\n%s", test.script, want, text)
				}
			}
		}
	})
}

// Importing tokenize before the sandbox, as the worker does so that a
// traceback can read a library's source, does not hand scripts a way to read
// files: tokenize.open, and linecache, which reads with it, open a module's
// source and time zone data, as the importer may, and refuse any other file.
// Before, on Python 3.12, where tokenize was imported with linecache before
// the sandbox, both read any file the worker could.
func TestTokenizeAndLinecacheReadAModulesSourceAndNoOtherFile(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("not for scripts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eachInterpreterOfTheSandbox(t, func(t *testing.T, python string) {
		for _, script := range []string{
			"import tokenize\nmetric(name='read', value=len(tokenize.open(%q).read()))",
			"import linecache\nmetric(name='read', value=len(linecache.getline(%q, 1)))",
			"import traceback\nmetric(name='read', value=len(traceback.linecache.getlines(%q)))",
		} {
			script = strings.Replace(script, "%q", "'"+secret+"'", 1)
			err := runScriptWith(python, "tokenize_open", script)
			if err == nil || !strings.Contains(err.Error(), "\nRuntimeError: operation disabled by exporter") {
				t.Errorf("%q read the file, or failed otherwise: %v", script, err)
			}
		}
		// A module's source is still read: the failing line of a library is
		// shown above, and linecache gives a line of one read by name.
		err := runScriptWith(python, "tokenize_source", "import linecache, json\nline = linecache.getline(json.__file__, 1)\nmetric(name='read', value=len(line))\nif not line: fail('no line of json.__file__')")
		if err != nil {
			t.Errorf("linecache did not read a module's source: %v", err)
		}
	})
}
