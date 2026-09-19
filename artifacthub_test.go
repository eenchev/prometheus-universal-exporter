package main

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Artifact Hub indexes the published OCI repository and reads its metadata from
// Chart.yaml and from .artifacthub-repo.yml. Neither file is exercised by
// rendering the chart, so nothing else in the suite would notice a keyword
// disappearing, a maintainer losing an email address, or the values schema
// drifting away from values.yaml. These tests are that notice.

const chartDir = "charts/prometheus-universal-exporter/"

type chartMetadata struct {
	APIVersion  string   `yaml:"apiVersion"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Type        string   `yaml:"type"`
	Version     string   `yaml:"version"`
	AppVersion  string   `yaml:"appVersion"`
	Home        string   `yaml:"home"`
	Sources     []string `yaml:"sources"`
	Keywords    []string `yaml:"keywords"`
	Maintainers []struct {
		Name  string `yaml:"name"`
		Email string `yaml:"email"`
	} `yaml:"maintainers"`
	Annotations map[string]string `yaml:"annotations"`
}

func readChartMetadata(t *testing.T) chartMetadata {
	t.Helper()
	raw, err := os.ReadFile(chartDir + "Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart chartMetadata
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	return chart
}

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

func TestChartCarriesItsArtifactHubMetadata(t *testing.T) {
	chart := readChartMetadata(t)

	if chart.Name != "prometheus-universal-exporter" {
		t.Errorf("name=%q", chart.Name)
	}
	if strings.TrimSpace(chart.Description) == "" {
		t.Error("description is what Artifact Hub shows on the search card; it cannot be empty")
	}
	// Helm refuses a chart version that is not SemVer, and a release tag is
	// derived from this one.
	if !semver.MatchString(chart.Version) {
		t.Errorf("version=%q is not SemVer", chart.Version)
	}
	if !semver.MatchString(chart.AppVersion) {
		t.Errorf("appVersion=%q is not SemVer", chart.AppVersion)
	}
	if chart.Home == "" || len(chart.Sources) == 0 {
		t.Error("home and sources are what let a reader find the project from the package page")
	}
	if len(chart.Maintainers) == 0 {
		t.Fatal("Artifact Hub shows the maintainers; the chart must name at least one")
	}
	for _, maintainer := range chart.Maintainers {
		if maintainer.Name == "" || !strings.Contains(maintainer.Email, "@") {
			t.Errorf("maintainer %+v needs a name and an email address", maintainer)
		}
	}
}

// The keywords are how somebody searching Artifact Hub for "otlp" or "python"
// arrives here. Each one names something the exporter actually does, so losing
// one costs a discovery path and adding one that is not true costs trust.
func TestChartKeywordsCoverTheAdvertisedCapabilities(t *testing.T) {
	have := map[string]bool{}
	for _, keyword := range readChartMetadata(t).Keywords {
		have[keyword] = true
	}
	for _, want := range []string{
		"prometheus", "exporter", "monitoring", "kubernetes",
		"metrics", "otlp", "python", "json", "yaml",
	} {
		if !have[want] {
			t.Errorf("keyword %q is missing", want)
		}
	}
}

// Two of the annotations carry YAML inside a string. A malformed one is not a
// render error — Helm passes it through untouched and Artifact Hub is left to
// fail on it, out of sight.
func TestArtifactHubAnnotationsAreWellFormed(t *testing.T) {
	annotations := readChartMetadata(t).Annotations
	if annotations["artifacthub.io/category"] == "" {
		t.Error("artifacthub.io/category places the chart in Artifact Hub's navigation")
	}
	for _, key := range []string{"artifacthub.io/images", "artifacthub.io/links"} {
		value, ok := annotations[key]
		if !ok {
			continue
		}
		var entries []map[string]string
		if err := yaml.Unmarshal([]byte(value), &entries); err != nil {
			t.Errorf("%s does not parse as YAML: %v", key, err)
			continue
		}
		for _, entry := range entries {
			if entry["name"] == "" {
				t.Errorf("%s has an entry with no name: %v", key, entry)
			}
		}
	}
}

// Artifact Hub verifies ownership through this file. It is allowed to carry the
// placeholder before registration — a chart has to be published before Artifact
// Hub can be pointed at it — but it must always parse and name its owners.
func TestArtifactHubRepositoryFileIsUsable(t *testing.T) {
	raw, err := os.ReadFile(".artifacthub-repo.yml")
	if err != nil {
		t.Fatal(err)
	}
	var repo struct {
		RepositoryID string `yaml:"repositoryID"`
		Owners       []struct {
			Name  string `yaml:"name"`
			Email string `yaml:"email"`
		} `yaml:"owners"`
	}
	if err := yaml.Unmarshal(raw, &repo); err != nil {
		t.Fatal(err)
	}
	if repo.RepositoryID == "" {
		t.Error("repositoryID must be present, as the placeholder or as the real ID")
	}
	if len(repo.Owners) == 0 {
		t.Fatal("owners is what Artifact Hub checks the claim against")
	}
	for _, owner := range repo.Owners {
		if owner.Name == "" || !strings.Contains(owner.Email, "@") {
			t.Errorf("owner %+v needs a name and an email address", owner)
		}
	}
}

// A packaged chart is build output. Committing one means the repository carries
// a second, stale definition of a released version alongside the source.
func TestPackagedChartsAreNotCommitted(t *testing.T) {
	raw, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n*.tgz\n") {
		t.Error(".gitignore must ignore *.tgz, or a packaged chart lands in a commit")
	}
	packages, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range packages {
		if strings.HasSuffix(entry.Name(), ".tgz") {
			t.Errorf("%s is a packaged chart sitting in the repository root", entry.Name())
		}
	}
}

// Helm validates values against this schema on template, install and upgrade,
// which is what turns a misspelled value into a failed render rather than a pod
// that starts and ignores it. That only holds while the schema and values.yaml
// describe the same thing.
func TestValuesSchemaMatchesValues(t *testing.T) {
	rawSchema, err := os.ReadFile(chartDir + "values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Type                 string                     `json:"type"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatalf("values.schema.json is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("the schema describes %q, not an object", schema.Type)
	}
	// Every value has a default, so requiring one at the top level would break
	// an install that passes no values at all.
	if len(schema.Required) != 0 {
		t.Errorf("the schema requires %v; every value has a default", schema.Required)
	}
	// This is what makes a typo fail instead of being ignored.
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Error("the schema must set additionalProperties: false, or an unknown value renders silently")
	}

	rawValues, err := os.ReadFile(chartDir + "values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(rawValues, &values); err != nil {
		t.Fatal(err)
	}

	var undeclared, unused []string
	for key := range values {
		if _, ok := schema.Properties[key]; !ok {
			undeclared = append(undeclared, key)
		}
	}
	for key := range schema.Properties {
		if _, ok := values[key]; !ok {
			unused = append(unused, key)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(unused)
	for _, key := range undeclared {
		t.Errorf("values.yaml sets %s, which the schema does not declare — setting it would be rejected", key)
	}
	for _, key := range unused {
		t.Errorf("the schema declares %s, which values.yaml does not set", key)
	}
}

// The install commands in the documentation pin a version. A chart bump that
// leaves them behind hands a reader a command that installs something other
// than the chart they are reading about.
func TestDocumentedChartVersionMatchesTheChart(t *testing.T) {
	version := readChartMetadata(t).Version
	pinned := regexp.MustCompile(`--version ([0-9][^\s\\]*)`)
	for _, path := range []string{"README.md", chartDir + "README.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		matches := pinned.FindAllStringSubmatch(string(raw), -1)
		if len(matches) == 0 {
			t.Errorf("%s documents no pinned install; a reader has nothing to copy", path)
		}
		for _, match := range matches {
			if match[1] != version {
				t.Errorf("%s pins --version %s, but Chart.yaml declares %s", path, match[1], version)
			}
		}
	}
}
