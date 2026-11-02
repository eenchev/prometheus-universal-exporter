//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The chart README's example of otlp.exporter_instance renders with the
// allocator, and the configuration it renders loads as both read it: the
// allocator, which serves it, leaves the value as written, and a replica,
// given --allocator.member=$(POD_NAME) with POD_NAME the pod's name, names
// its resources by the pod's name.
func TestChartReadmeExporterInstanceExampleNamesThePod(t *testing.T) {
	helm := requireHelm(t)
	_, section, found := strings.Cut(readChartFile(t, "README.md"), "### OTLP export\n")
	if !found {
		t.Fatal("the chart README has no OTLP export section")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	var values string
	for _, block := range yamlBlocksOf(section) {
		if strings.Contains(block, "exporter_instance:") {
			values = block
		}
	}
	if values == "" {
		t.Fatal("the OTLP export section shows no values with otlp.exporter_instance")
	}
	var configYAML, member, podName string
	for _, doc := range renderedDocuments(t, helm, "-f", valuesFile(t, values)) {
		switch kind, name := child(doc, "kind").Value, path(doc, "metadata", "name").Value; {
		case kind == "ConfigMap" && name == "test-prometheus-universal-exporter":
			configYAML = path(doc, "data", "config.yaml").Value
		case kind == "Deployment" && name == "test-prometheus-universal-exporter":
			container := path(doc, "spec", "template", "spec", "containers", "0")
			for _, arg := range path(container, "args").Content {
				if value, ok := strings.CutPrefix(arg.Value, "--allocator.member="); ok {
					member = value
				}
			}
			if field := path(named(path(container, "env"), "POD_NAME"), "valueFrom", "fieldRef", "fieldPath"); field != nil {
				podName = field.Value
			}
		}
	}
	if member != "$(POD_NAME)" || podName != "metadata.name" {
		t.Fatalf("the replica's --allocator.member is %q, and POD_NAME comes from %q", member, podName)
	}
	file := testutil.WriteIn(t, t.TempDir(), "config.yaml", configYAML)
	served, err := config.Load(file, config.WithoutNamedFiles())
	if err != nil {
		t.Fatalf("the allocator does not load the rendered configuration: %v\n%s", err, configYAML)
	}
	if got := served.OTLP.ExporterInstance; got != (model.OTLPExporterInstanceConfig{Enabled: true, Attribute: model.DefaultOTLPExporterInstanceAttribute}) {
		t.Errorf("the allocator reads otlp.exporter_instance as %+v", got)
	}
	// Kubernetes expands $(POD_NAME) in the argument from the pod's env.
	const pod = "test-prometheus-universal-exporter-7d9c5b6f4-x2kqp"
	replica, err := config.Load(file, config.WithExporterInstanceName(strings.ReplaceAll(member, "$(POD_NAME)", pod)))
	if err != nil {
		t.Fatalf("a replica does not load the rendered configuration: %v", err)
	}
	if got := replica.OTLP.ExporterInstance; got.Attribute != "pue.exporter.instance" || got.Value != pod {
		t.Errorf("a replica names its resources %s=%s, want the pod's name %s", got.Attribute, got.Value, pod)
	}
}
