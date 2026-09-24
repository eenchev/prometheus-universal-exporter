package fetch

import (
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func pathCollector(path string) model.Collector {
	c := testutil.Collector("tenants", "text")
	c.Request.Path = path
	return c
}

// The token that holds a value's place through path.Join must never reach the
// wire, whatever the value.
func TestNoPlaceholderTokenLeaksIntoTheURL(t *testing.T) {
	c := pathCollector("/{{param_a}}/{{param_b:x}}/{{param_a}}/")
	for _, value := range []string{"v", "a/b", "%00", "0", "1"} {
		u, err := ResolveRequestURL("http://h.example/base", &c, RequestOverrides{Params: map[string]string{"param_a": value}})
		if err != nil {
			t.Fatal(err)
		}
		rendered := u.String()
		if strings.Contains(u.Path, "\x00") || strings.Contains(rendered, "%00") && value != "%00" {
			t.Fatalf("value %q left a token in %q", value, rendered)
		}
		escaped := url.PathEscape(value)
		want := "http://h.example/base/" + escaped + "/x/" + escaped + "/"
		if rendered != want {
			t.Fatalf("value %q rendered %q, want %q", value, rendered, want)
		}
	}
}
