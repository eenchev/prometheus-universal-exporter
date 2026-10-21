package decode

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A TOML body is decoded into the values the json decoder makes: tables as
// mappings, arrays and arrays of tables as lists, integers as ints, floats
// as float64s, nan and inf among them, and each kind of date and time as its
// text, an offset date-time in RFC 3339 with the fraction it was written
// with.
func TestTOMLIsDecodedIntoTheValuesOfJSON(t *testing.T) {
	body := `# status
title = "web"
uptime = 86400
load = 0.25
up = true
errors = nan
limit = inf
ports = [8080, 8081]
started = 1979-05-27T00:32:00.5-07:00
checked = 1979-05-27T07:32:00Z
local = 1979-05-27 07:32:00
day = 1979-05-27
at = 07:32:00

[server.http]
requests = { total = 12, failed = 1 }

[[disks]]
name = "sda"
used = 72

[[disks]]
name = "sdb"
`
	d, err := decodeKind(t, "toml", body)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := d.Data.(map[string]any)
	if d.Kind != "toml" || !ok {
		t.Fatalf("decoded as %s %#v", d.Kind, d.Data)
	}
	if !math.IsNaN(got["errors"].(float64)) || !math.IsInf(got["limit"].(float64), 1) {
		t.Errorf("nan and inf are %#v and %#v", got["errors"], got["limit"])
	}
	delete(got, "errors")
	delete(got, "limit")
	want := map[string]any{
		"title": "web", "uptime": 86400, "load": 0.25, "up": true, "ports": []any{8080, 8081},
		"started": "1979-05-27T00:32:00.5-07:00", "checked": "1979-05-27T07:32:00Z", "local": "1979-05-27T07:32:00", "day": "1979-05-27", "at": "07:32:00",
		"server": map[string]any{"http": map[string]any{"requests": map[string]any{"total": 12, "failed": 1}}},
		"disks":  []any{map[string]any{"name": "sda", "used": 72}, map[string]any{"name": "sdb"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded as %#v, want %#v", got, want)
	}
	if d, err := decodeKind(t, "toml", "# nothing\n"); err != nil || !reflect.DeepEqual(d.Data, map[string]any{}) {
		t.Errorf("a body of comments: %#v, %v", d, err)
	}
}

// What is no TOML fails the decode with the library's message after the
// line it names, without the last key the library read, and a message
// quoting a long token of the body is cut, with its length; the failure is
// the same failure at whichever line it is, and with a token of another
// length.
func TestTOMLRefusesWhatIsNotTOML(t *testing.T) {
	for body, want := range map[string]string{
		"a = 1\na = 2\n":      `TOML decode: line 2: Key 'a' has already been defined.`,
		"a = 1\n\nb = nope\n": `TOML decode: line 3: expected value but found "nope" instead`,
		"[s]\nx = [1, 2\n":    `TOML decode: line 2: expected a comma (',') or array terminator (']'), but got end of file`,
	} {
		_, err := decodeKind(t, "toml", body)
		if err == nil || err.Error() != want {
			t.Errorf("%q: err=%v, want %s", body, err, want)
		}
	}
	long := strings.Repeat("x", 1<<16)
	_, err := decodeKind(t, "toml", "a = "+long+"\n")
	if err == nil || len(err.Error()) > 300 || !strings.HasPrefix(err.Error(), `TOML decode: line 1: expected value but found "xxxx`) || !strings.HasSuffix(err.Error(), "... (65571 bytes)") {
		t.Errorf("a long token: %d bytes, %.300v", len(err.Error()), err)
	}
	_, again := decodeKind(t, "toml", "\n\na = "+long[:1<<15]+"\n")
	if again == nil || model.SameFailureText(again) != model.SameFailureText(err) {
		t.Errorf("the same mistake elsewhere: %q, %q", model.SameFailureText(again), model.SameFailureText(err))
	}
}

// Arrays and inline tables nest MaxDepth deep at most, and are refused past
// it before the library parses them, as is a value named by more than
// maxTOMLTables parts, those of its table header and of its keys:
// the library's cost for a key grows with the square of how deep it is, and
// a single key 10,000 tables deep took it seconds. Within the bounds, and
// with brackets, dots and quotes inside strings and comments, a document
// decodes.
func TestTOMLIsRefusedPastItsNestingBeforeItIsParsed(t *testing.T) {
	dotted := func(n int) string { return strings.TrimSuffix(strings.Repeat("a.", n), ".") }
	inline := func(n int) string { return strings.Repeat("{a=", n) + "1" + strings.Repeat("}", n) }
	for name, body := range map[string]string{
		"16 parts of a key":                         dotted(16) + " = 1\n",
		"8 of a header and 8 of a key":              "[" + dotted(8) + "]\n" + dotted(8) + " = 1\n",
		"15 inline tables":                          "a = " + inline(15) + "\n",
		"arrays nested 9,000 deep in a table":       "[t]\na = " + strings.Repeat("[", 9000) + strings.Repeat("]", 9000) + "\n",
		"brackets and dots in strings and comments": "a = \"[[{{.\" # [[[ {{{\nb = '''\n[[{{\n''' # \"\n[\"x.y\".'z']\nc = \"\"\"\\\"[\"\"\"\"\n",
		"inline tables in arrays":                   "a = [" + strings.Repeat("{b = [", 7) + strings.Repeat("]}", 7) + "]\n",
	} {
		if _, err := decodeKind(t, "toml", body); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct{ body, says string }{
		"17 parts of a key":            {dotted(17) + " = 1\n", "line 1: a value is named by more than 16 parts"},
		"17 parts of a header":         {"\n[" + dotted(17) + "]\n", "line 2: a value is named by more than 16 parts"},
		"9 of a header and 8 of a key": {"[" + dotted(9) + "]\n" + dotted(8) + " = 1\n", "line 2: a value is named by more than 16 parts"},
		"17 inline tables":             {"a = " + inline(17) + "\n", "line 1: a value is named by more than 16 parts"},
		"inline tables in arrays":      {"a = [" + strings.Repeat("{b = [", 16) + strings.Repeat("]}", 16) + "]\n", "line 1: a value is named by more than 16 parts"},
		"a key 10,000 tables deep":     {dotted(10000) + " = 1\n", "line 1: a value is named by more than 16 parts"},
		"arrays nested 10,001 deep":    {"a = " + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + "\n", "line 1: arrays and inline tables nested more than 10000 deep"},
		"a megabyte of [":              {"a = " + strings.Repeat("[\n", 1<<19), "line 10001: arrays and inline tables nested more than 10000 deep"},
		"arrays 9,999 deep in tables":  {"[" + dotted(3) + "]\na = " + strings.Repeat("[", 9999) + strings.Repeat("]", 9999) + "\n", "the document is nested more than 10000 deep"},
	} {
		start := time.Now()
		_, err := decodeKind(t, "toml", tc.body)
		if err == nil || !strings.HasPrefix(err.Error(), "TOML decode: "+tc.says) {
			t.Errorf("%s: err=%.200v, want one that starts %q", name, err, tc.says)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("%s: refused in %v", name, took)
		}
	}
}
