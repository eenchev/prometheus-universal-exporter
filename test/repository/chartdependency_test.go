package repository

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parentChart writes a chart whose one dependency is the exporter's chart, by
// its file:// path, switched by the condition a parent usually gives a
// dependency, and has helm fetch the dependency into it. It returns the
// parent's directory.
func parentChart(t *testing.T, helm, values string) string {
	t.Helper()
	chart, err := filepath.Abs(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	metadata := `apiVersion: v2
name: parent
version: 0.1.0
dependencies:
  - name: prometheus-universal-exporter
    version: ` + readChartMetadata(t).Version + `
    repository: file://` + chart + `
    condition: prometheus-universal-exporter.enabled
`
	for name, content := range map[string]string{"Chart.yaml": metadata, "values.yaml": values} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(helm, "dependency", "build", dir) // #nosec G204 -- the test's own arguments
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm dependency build: %v\n%s", err, out.String())
	}
	return dir
}

// The chart is usable as a dependency of another chart. Helm then passes it
// two keys of its own with the values: global, the parent's global values,
// an empty map when the parent has none, and the enabled the dependency's
// condition reads. A root schema refusing unknown keys refused both, so no
// parent chart could render. The schema still refuses a misspelled value set
// through the parent, and the condition still switches the chart off.
func TestChartRendersAsADependencyOfAParentChart(t *testing.T) {
	helm := requireHelm(t)
	for name, values := range map[string]string{
		"with nothing set":             "",
		"with global values":           "global:\n  imageRegistry: registry.internal\n  team: {name: observability}\n",
		"switched on by its condition": "prometheus-universal-exporter:\n  enabled: true\n  replicaCount: 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, parentChart(t, helm, values))
			if !ok {
				t.Fatalf("the parent chart does not render:\n%s", out)
			}
			for _, want := range []string{"kind: Deployment", "kind: ConfigMap", "name: test-prometheus-universal-exporter\n", "# Source: parent/charts/prometheus-universal-exporter/templates/deployment.yaml"} {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in the rendered parent chart:\n%s", want, out)
				}
			}
			if strings.Contains(values, "replicaCount: 2") && !strings.Contains(out, "replicas: 2\n") {
				t.Errorf("the dependency's values did not reach it:\n%s", out)
			}
		})
	}

	dir := parentChart(t, helm, "global: {team: observability}\nprometheus-universal-exporter:\n  enabled: true\n")
	out, ok := helmTemplate(t, helm, dir, "--set", "prometheus-universal-exporter.enabled=false")
	if !ok || strings.Contains(out, "kind:") {
		t.Errorf("ok=%v; the condition did not switch the dependency off:\n%s", ok, out)
	}
	out, ok = helmTemplate(t, helm, dir, "--set", "prometheus-universal-exporter.replicaCounts=2")
	if ok || !strings.Contains(out, "replicaCounts") {
		t.Errorf("ok=%v, want the schema to refuse a misspelled value of the dependency:\n%s", ok, out)
	}
	out, ok = helmTemplate(t, helm, dir, "--set", "prometheus-universal-exporter.enabled=maybe")
	if ok || !namesInHelmError(out, "enabled") {
		t.Errorf("ok=%v, want the schema to refuse an enabled that is not a boolean:\n%s", ok, out)
	}
}

// helmCheckParent is the parent chart `make helm-test` and CI build and render.
const helmCheckParent = "testdata/chart/parent"

// The parent chart that `make helm-test` and CI render is the parent the
// test above writes for itself, kept as files: one dependency, the chart, by
// a file:// path that reaches it from where the parent lies, at the chart's
// version, which a chart bump would otherwise leave behind with helm unable
// to find the dependency, and switched by the same condition. Both build it
// in a copy, so the tree holds neither the charts directory nor the
// Chart.lock that `helm dependency build` writes; one built in place would
// render the dependency it was built with, whatever the chart has become.
func TestTheParentChartOfTheHelmChecksDependsOnTheChartAsItIs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(helmCheckParent, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var parent struct {
		Name         string `yaml:"name"`
		Dependencies []struct {
			Name, Version, Repository, Condition string
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(raw, &parent); err != nil {
		t.Fatal(err)
	}
	if parent.Name != "parent" || len(parent.Dependencies) != 1 {
		t.Fatalf("%s/Chart.yaml is the chart %q with %d dependencies, want parent with the exporter's chart alone", helmCheckParent, parent.Name, len(parent.Dependencies))
	}
	chart := readChartMetadata(t)
	dependency := parent.Dependencies[0]
	if dependency.Name != chart.Name || dependency.Version != chart.Version {
		t.Errorf("the parent depends on %s %s, but the chart is %s %s; set the version in %s/Chart.yaml", dependency.Name, dependency.Version, chart.Name, chart.Version, helmCheckParent)
	}
	if dependency.Condition != chart.Name+".enabled" {
		t.Errorf("the parent's condition is %q, want %s.enabled", dependency.Condition, chart.Name)
	}
	path, isFile := strings.CutPrefix(dependency.Repository, "file://")
	if !isFile || filepath.IsAbs(path) || filepath.Join(helmCheckParent, path) != filepath.Clean(chartDir) {
		t.Errorf("the parent's repository %q does not lead from %s to %s by a relative file:// path", dependency.Repository, helmCheckParent, chartDir)
	}

	entries, err := os.ReadDir(helmCheckParent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != "Chart.yaml" && name != "values.yaml" {
			t.Errorf("%s holds %s; `helm dependency build` writes charts and Chart.lock, so build the parent in a copy, as `make helm-test` does", helmCheckParent, name)
		}
	}

	// What the checks look for in the render comes from the parent's values:
	// global values, which helm hands every dependency, and a value of the
	// chart's under its name. The condition's own value is left to the
	// checks, which set it.
	rawValues, err := os.ReadFile(filepath.Join(helmCheckParent, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(rawValues, &values); err != nil {
		t.Fatal(err)
	}
	if global, _ := values["global"].(map[string]any); len(global) == 0 {
		t.Errorf("the parent's values hold no global values: %v", values)
	}
	own, _ := values[chart.Name].(map[string]any)
	if own["replicaCount"] != 2 {
		t.Errorf("the parent's values do not set the chart's replicaCount to the 2 the checks look for: %v", values)
	}
	if _, set := own["enabled"]; set {
		t.Errorf("the parent's values set the condition, which the checks set themselves: %v", values)
	}

	helm := requireHelm(t)
	tree := t.TempDir()
	for from, to := range map[string]string{chartDir: filepath.Join(tree, chartDir), helmCheckParent: filepath.Join(tree, helmCheckParent)} {
		if err := os.CopyFS(to, os.DirFS(from)); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(tree, helmCheckParent)
	cmd := exec.Command(helm, "dependency", "build", dir) // #nosec G204 -- the test's own arguments
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm dependency build: %v\n%s", err, out.String())
	}
	rendered, ok := helmTemplate(t, helm, dir, "--set", chart.Name+".enabled=true")
	if !ok {
		t.Fatalf("the parent chart does not render:\n%s", rendered)
	}
	for _, want := range []string{"# Source: parent/charts/prometheus-universal-exporter/templates/deployment.yaml\n", "replicas: 2\n"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("no %q in the rendered parent chart:\n%s", want, rendered)
		}
	}
	if off, ok := helmTemplate(t, helm, dir, "--set", chart.Name+".enabled=false"); !ok || strings.Contains(off, "kind:") {
		t.Errorf("ok=%v; the condition did not switch the dependency off:\n%s", ok, off)
	}
}

// The README's dependency example pins the chart's version, as its install
// commands do: a chart bump that leaves it behind hands a reader a parent
// chart that fetches another release.
func TestChartReadmeDependencyExamplePinsTheChartVersion(t *testing.T) {
	readme := readChartFile(t, "README.md")
	_, section, found := strings.Cut(readme, "\n## Using the chart as a dependency\n")
	if !found {
		t.Fatal("the chart README does not document using the chart as a dependency")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	var parent struct {
		Dependencies []struct {
			Name, Version, Repository, Condition string
		} `yaml:"dependencies"`
	}
	blocks := yamlBlocksOf(section)
	if len(blocks) != 2 {
		t.Fatalf("%d examples in the section, want the parent's Chart.yaml and values.yaml", len(blocks))
	}
	if err := yaml.Unmarshal([]byte(blocks[0]), &parent); err != nil || len(parent.Dependencies) != 1 {
		t.Fatalf("the example Chart.yaml does not hold one dependency: %v\n%s", err, blocks[0])
	}
	chart := readChartMetadata(t)
	dependency := parent.Dependencies[0]
	if dependency.Name != chart.Name || dependency.Version != chart.Version {
		t.Errorf("the example depends on %s %s, but the chart is %s %s", dependency.Name, dependency.Version, chart.Name, chart.Version)
	}
	if dependency.Condition != chart.Name+".enabled" || dependency.Repository != "oci://ghcr.io/eenchev/charts" {
		t.Errorf("the example's condition or repository is not the chart's: %+v", dependency)
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(blocks[1]), &values); err != nil {
		t.Fatal(err)
	}
	if _, ok := values["global"]; !ok {
		t.Error("the example values show no global values")
	}
	if own, _ := values[chart.Name].(map[string]any); own["enabled"] != true {
		t.Errorf("the example values do not switch the chart on under its name: %v", values)
	}
}

// yamlBlocksOf returns the fenced YAML blocks of text.
func yamlBlocksOf(text string) []string {
	var blocks []string
	for _, part := range strings.Split(text, "```yaml\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	return blocks
}
