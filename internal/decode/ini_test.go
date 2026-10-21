package decode

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An INI body is a mapping of its sections to their keys, and of the keys
// before the first section to their values, every value text as written:
// comments are whole lines, a key is split from its value at the first = or
// :, a section written twice is one, and of a key written twice the later
// value is kept. A dotted section name is one name.
func TestINIIsAMappingOfItsSections(t *testing.T) {
	body := "; generated\r\ntimeout = 30\n\n[server]\nhost=web01 \n  port : 8080\nurl = http://a/b#top;x=1\n" +
		"# comment\n[ db.primary ] ; the main one\nconnections=12\nquoted = \"a b\"\nempty =\n[server]\nport = 9090\n"
	want := map[string]any{
		"timeout":    "30",
		"server":     map[string]any{"host": "web01", "port": "9090", "url": "http://a/b#top;x=1"},
		"db.primary": map[string]any{"connections": "12", "quoted": `"a b"`, "empty": ""},
	}
	d, err := decodeKind(t, "ini", body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "ini" || !reflect.DeepEqual(d.Data, want) {
		t.Errorf("decoded as %s %#v, want %#v", d.Kind, d.Data, want)
	}
	if d, err := decodeKind(t, "ini", "; nothing\n\n"); err != nil || !reflect.DeepEqual(d.Data, map[string]any{}) {
		t.Errorf("a body of comments: %#v, %v", d, err)
	}
}

// What is no INI fails the decode naming the line, quoting no more than the
// start of what it names; the failure is the same failure at whichever line
// it is.
func TestINIRefusesWhatIsNotINI(t *testing.T) {
	long := strings.Repeat("x", 1<<16)
	for body, want := range map[string]string{
		"[server\nport=1\n":            "INI decode: line 1: the section name has no closing ]",
		"a=1\n[s] x\n":                 `INI decode: line 2: "x" follows the section name; only a comment, starting with ; or #, may`,
		"[ ]\n":                        "INI decode: line 1: the section has no name",
		"[s]\njust words\n":            "INI decode: line 2: neither a section, a key = value nor a comment",
		"[s]\n = 1\n":                  "INI decode: line 2: the line has no key before its '='",
		"[s]\n: 1\n":                   "INI decode: line 2: the line has no key before its ':'",
		"server=1\n\n[server]\nport=1": `INI decode: line 3: section "server" has the name of the key at line 1, written before the first section; rename one`,
		"[" + long + "] " + long:       `INI decode: line 1: "` + strings.Repeat("x", 64) + `"... (65536 bytes) follows the section name; only a comment, starting with ; or #, may`,
	} {
		_, err := decodeKind(t, "ini", body)
		if err == nil || err.Error() != want {
			t.Errorf("%.40q: err=%v, want %s", body, err, want)
		}
	}
	_, first := decodeKind(t, "ini", "[s\n")
	_, later := decodeKind(t, "ini", "a=1\n\n[s\n")
	if first == nil || later == nil || model.SameFailureText(first) != model.SameFailureText(later) {
		t.Errorf("the same mistake on another line is another failure: %q, %q", model.SameFailureText(first), model.SameFailureText(later))
	}
}
