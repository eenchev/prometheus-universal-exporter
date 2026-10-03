package exporter

import (
	"testing"
)

// The static targets endpoint (statictargetsendpoint.go).

func TestStaticTargetsPathIsChecked(t *testing.T) {
	for path, want := range map[string]string{"/static-targets": "/static-targets", "targets": "/targets", "/a/b": "/a/b"} {
		if got, err := StaticTargetsPath(path, "/self-metrics"); err != nil || got != want {
			t.Errorf("StaticTargetsPath(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"", "/", "/probe", "/collectors", "/self-metrics", "/x/", "/{a}", "/x/..", "/./x"} {
		if _, err := StaticTargetsPath(path, "/self-metrics"); err == nil {
			t.Errorf("StaticTargetsPath(%q) was accepted", path)
		}
	}
}
