//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A localfile request.path placeholder whose default no probe could read a
// file with — "..", ".", one with a / or a \ in it — used to load, and then
// failed every probe that left the parameter out, as if the probe had sent
// the value. It stops the load, as the same default of an http path does,
// naming the collector, the placeholder and the default; the rule at load
// is the function a probe's value is held to, so each default refused here
// is refused, in the same words, as a probe's value.
func TestALocalFilePathDefaultNoProbeCouldUseIsRefusedAtLoad(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "eu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "eu", "x.prom"), []byte("up 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, unusable := range map[string]string{
		"a parent directory":  "..",
		"the directory":       ".",
		"two names":           "a/b",
		"a leading slash":     "/etc",
		"a backslash":         `a\b`,
		"a parent, then more": "../x",
		"a NUL byte":          "a\x00b",
	} {
		c := model.Collector{Name: "files", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: "{{param_dir:" + unusable + "}}/x.prom"}}
		err := ValidateRequest(&c)
		if err == nil {
			t.Errorf("%s: the configuration loaded", name)
			continue
		}
		for _, want := range []string{`collector "files" request.path`, "the default of param_dir is " + strconv.Quote(unusable), "every probe that leaves param_dir out would fail", "change the default"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want it to say %s", name, err, want)
			}
		}
		// The same value from a probe is refused for the same reason.
		probed := model.Collector{Name: "files", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: "{{param_dir}}/x.prom"}}
		if err := ValidateRequest(&probed); err != nil {
			t.Fatalf("%s: without the default: %v", name, err)
		}
		_, probeErr := FetchCollector(context.Background(), "", &probed, RequestOverrides{Params: map[string]string{"param_dir": unusable}}, nil)
		if probeErr == nil || !strings.Contains(err.Error(), "("+probeErr.Error()+")") {
			t.Errorf("%s: the load says %v, a probe with the value %v: want the load to give the probe's reason", name, err, probeErr)
		}
	}
	// A default a probe can use loads, an empty one included, and is read
	// with: a placeholder with an empty default adds nothing to the path.
	for path, target := range map[string]string{
		"{{param_dir:eu}}/x.prom":                "",
		"{{param_dir:eu}}/{{param_file:x}}.prom": "",
		"eu/{{param_sub:}}/x.prom":               "",
		"{{param_file:}}":                        "eu/x.prom",
		"{{param_dir:e..u}}/x.prom":              "",
	} {
		c := model.Collector{Name: "files", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: root, Path: path}}
		if err := ValidateRequest(&c); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if strings.Contains(path, "e..u") {
			continue // loads; there is no such directory to read
		}
		resp, err := FetchCollector(context.Background(), target, &c, RequestOverrides{}, nil)
		if err != nil || string(resp.Body) != "up 1\n" {
			t.Errorf("%s: resp=%+v err=%v, want the file read with the default", path, resp, err)
		}
	}
}
