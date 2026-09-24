package transform

import (
	"strings"
	"testing"
)

// A Python script's label values are written as a jq label's are
// (labeltext.go): a number or a boolean is its text, None leaves the label
// off, and a list or a dict is refused, where a number used to fail the whole
// transform on decoding.
func TestPythonLabelValuesAreWrittenAsText(t *testing.T) {
	requirePython(t)
	c := workerCollector("labels", `metric(name="up", value=1, labels={
    "text": "web-01", "id": 1234567, "big": 1e6, "ratio": 0.5, "tiny": 1e-7,
    "huge": 1e21, "on": True, "off": False, "missing": None, 7: "key"})`)
	set, err := runWorkerScript(t, c)
	if err != nil {
		t.Fatal(err)
	}
	got := set.Metrics[0].Labels
	for name, want := range map[string]string{
		"text": "web-01", "id": "1234567", "big": "1000000", "ratio": "0.5", "tiny": "1e-07",
		"huge": "1e+21", "on": "true", "off": "false", "7": "key",
	} {
		if got[name] != want {
			t.Errorf("label %s = %q, want %q", name, got[name], want)
		}
	}
	if _, set := got["missing"]; set {
		t.Errorf("a None label was set: %q", got["missing"])
	}
	// The same numbers through a jq label are written the same way.
	for _, value := range []any{1234567.0, 1e6, 0.5, 1e-7, 1e21} {
		text, _ := labelText(value)
		found := false
		for _, v := range got {
			if v == text {
				found = true
			}
		}
		if !found {
			t.Errorf("jq writes %v as %q, which no Python label matched: %v", value, text, got)
		}
	}
}

func TestPythonLabelValuesRefuseListsAndNonMappings(t *testing.T) {
	requirePython(t)
	for script, want := range map[string]string{
		`metric(name="up", value=1, labels={"tags": ["a", "b"]})`: `label 'tags' is a list, not a single value`,
		`metric(name="up", value=1, labels=[("a", "b")])`:         "metric labels must be a mapping",
	} {
		_, err := runWorkerScript(t, workerCollector("bad_labels", script))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", script, err, want)
		}
	}
}
