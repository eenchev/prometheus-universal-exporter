package decode

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A logfmt body is the list of its records, one for each line that holds a
// pair, each a mapping of the keys to their values as text: a quoted value
// read with its escapes, a key written alone null, a key with = and nothing
// after it the empty string, and of a key written twice the later value.
func TestLogfmtIsTheListOfTheRecordsOfItsLines(t *testing.T) {
	body := "level=info msg=\"GET /api \\\"ok\\\"\" duration_ms=12.5 cached\n" +
		"\n   \r\n" +
		"level=warn  path=/a=b\tempty= retry=1 retry=2 note=\"caf\\u00e9 \\ud83d\\ude00 \\n\"\r\n" +
		"x=1"
	want := []any{
		map[string]any{"level": "info", "msg": `GET /api "ok"`, "duration_ms": "12.5", "cached": nil},
		map[string]any{"level": "warn", "path": "/a", "empty": "", "retry": "2", "note": "café 😀 \n"},
		map[string]any{"x": "1"},
	}
	// A bare value may hold no =, which separates a key from its value.
	if _, err := decodeKind(t, "logfmt", body); err == nil || err.Error() != `logfmt decode: line 4, column 20: unexpected '=' in a value; quote a value that holds it` {
		t.Errorf("err=%v", err)
	}
	body = strings.Replace(body, "path=/a=b", "path=/a", 1)
	d, err := decodeKind(t, "logfmt", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "logfmt" || !reflect.DeepEqual(d.Data, want) {
		t.Errorf("decoded as %s %#v, want %#v", d.Kind, d.Data, want)
	}
	for _, empty := range []string{"", "\n\n", " \t\r\n"} {
		if d, err := decodeKind(t, "logfmt", empty); err != nil || !reflect.DeepEqual(d.Data, []any{}) {
			t.Errorf("%q: %#v, %v", empty, d, err)
		}
	}
}

// What is no logfmt fails the decode naming the line, the column and the
// byte that cannot be there, and never more of the body; the failure is the
// same failure at whichever line it is.
func TestLogfmtRefusesWhatIsNotLogfmt(t *testing.T) {
	long := strings.Repeat("v", 1<<16)
	for body, want := range map[string]string{
		"a=1\n=1\n":                 `logfmt decode: line 2, column 1: unexpected '=' where a key`,
		"a\"b=1":                    `logfmt decode: line 1, column 2: unexpected '"' where a key`,
		"a=b\"c":                    `logfmt decode: line 1, column 4: unexpected '"' in a value; quote a value that holds it`,
		"a=\"x\"y":                  `logfmt decode: line 1, column 6: unexpected 'y' after a quoted value`,
		"a=1 b=\"" + long + "\nc=2": `logfmt decode: line 1, column 7: the quoted value has no closing quote on its line`,
		"a=\"x\\qy\"":               `logfmt decode: line 1, column 5: invalid escape \q in a quoted value`,
		"a=\"\\u12\"":               `logfmt decode: line 1, column 4: \u in a quoted value is not followed by four hexadecimal digits`,
		"a=\"x\ty\"":                `logfmt decode: line 1, column 5: control character '\t' in a quoted value; escape it`,
		"a=\"x\\":                   `logfmt decode: line 1, column 3: the quoted value has no closing quote on its line`,
		"k=" + long + "=":           `logfmt decode: line 1, column 65539: unexpected '=' in a value; quote a value that holds it`,
	} {
		_, err := decodeKind(t, "logfmt", body)
		if err == nil || err.Error() != want {
			t.Errorf("%.40q: err=%v, want %s", body, err, want)
		}
	}
	_, first := decodeKind(t, "logfmt", "a=\"x\n")
	_, later := decodeKind(t, "logfmt", "a=1\nb=2\n  a=\"x\n")
	if first == nil || later == nil || model.SameFailureText(first) != model.SameFailureText(later) {
		t.Errorf("the same mistake on another line is another failure: %q, %q", model.SameFailureText(first), model.SameFailureText(later))
	}
}
