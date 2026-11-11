package decode

import (
	"reflect"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The unquoted scalars the configuration guide says YAML reads as numbers
// or booleans are read so, and the quoted ones as text: a label made of an
// unquoted 017 is 15, and the guide says what to do about it.
func TestUnquotedYAMLScalarsAreReadAsTheGuideSays(t *testing.T) {
	v, err := decodeYAML([]byte("a: 017\nb: 0x1F\nc: 007\nd: 08\ne: 1.10\nf: 1e3\ng: yes\nh: on\ni: No\nj: true\nk: False\nl: \"017\"\nm: '1.10'\n"))
	want := map[string]any{"a": 15, "b": 31, "c": 7, "d": 8, "e": 1.1, "f": 1000.0, "g": "yes", "h": "on", "i": "No", "j": true, "k": false, "l": "017", "m": "1.10"}
	if got := model.Normalize(v); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("%#v, %v; want %#v", got, err, want)
	}
}
