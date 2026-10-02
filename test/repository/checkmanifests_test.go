package repository

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// checkManifests runs tools/check-manifests.py on rendered, as the helm
// checks do.
func checkManifests(t *testing.T, rendered string) (string, error) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	if err := exec.Command(python, "-c", "import yaml").Run(); err != nil {
		t.Skip("python3 has no PyYAML, which tools/check-manifests.py reads with")
	}
	cmd := exec.Command(python, "tools/check-manifests.py")
	cmd.Stdin = strings.NewReader(rendered)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The manifest check reads what helm rendered from a pipe, and a pipeline
// succeeds when its last command does. So a render that failed, and printed
// nothing, must not pass as a chart without mistakes: no manifests is a
// failure, as two manifests in one document are.
func TestTheManifestCheckRefusesARenderOfNothing(t *testing.T) {
	const one = "---\nkind: ConfigMap\nmetadata:\n  name: a\n"
	for _, test := range []struct{ name, rendered, want string }{
		{"nothing", "", "no manifests were rendered"},
		{"comments only", "---\n# Source: chart/templates/x.yaml\n", "no manifests were rendered"},
		{"two manifests in one document", "---\nkind: ConfigMap\nkind: Secret\n", "a `---` separator is missing"},
	} {
		out, err := checkManifests(t, test.rendered)
		if err == nil || !strings.Contains(out, test.want) {
			t.Errorf("%s: err=%v, output %q, want a failure saying %q", test.name, err, out, test.want)
		}
	}
	if out, err := checkManifests(t, one+one); err != nil || !strings.Contains(out, "2 manifests, each its own YAML document") {
		t.Errorf("two manifests, each its own document: err=%v, output %q", err, out)
	}
}

// A check that some text is absent from a render keeps the render first:
// `if helm template ... | grep -q text` passes when helm failed, since grep
// then finds nothing. Neither list of helm checks pipes a render into a
// condition.
func TestNoHelmCheckReadsAFailedRenderAsAnAbsence(t *testing.T) {
	piped := regexp.MustCompile(`if\s+helm\s[^\n]*\|\s*grep`)
	for _, name := range []string{"Makefile", ".github/workflows/ci.yml"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range piped.FindAllString(string(raw), -1) {
			t.Errorf("%s pipes a render into a condition, which a failed render passes: %s", name, line)
		}
	}
}
