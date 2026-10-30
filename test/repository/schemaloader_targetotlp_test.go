//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A static target's otlp.labels_as_resource and otlp.instance_id go through
// the static target file's committed schema and its loader, with the
// configuration they are checked against: a label named twice, and a value
// that is no list or no boolean, are refused by both; what a schema cannot
// see — a name that is not among the target's labels, a name or
// service.instance.id its resource has already as an attribute, and either
// key on a target not exported over OTLP — by the exporter alone, with the
// message. A target that sets them in order is taken by both.
func TestSchemaAndExporterAgreeOnAStaticTargetsResourceKeys(t *testing.T) {
	schema := loadSchemaFile(t, staticTargetsSchemaFile)
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "otlp:\n  enabled: true\n  endpoint: http://collector.invalid:4318/v1/metrics\n  resource_attributes: {region: eu}\n"+testutil.MinimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	document := func(otlp string) string {
		return "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\n    export_via_otlp: true\n    labels: {city: berlin, region: eu, site: one}\n    otlp: " + otlp + "\n"
	}
	verdicts := func(written string) ([]string, error) {
		file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", written))
		if err == nil {
			err = config.ValidateStaticTargets(file)
		}
		if err == nil {
			err = config.ValidateStaticTargetsAgainst(file, cfg)
		}
		return schemaProblems(t, schema, written), err
	}
	for _, accepted := range []string{
		"{labels_as_resource: [city]}", "{labels_as_resource: [city, site], instance_id: true}", "{labels_as_resource: []}", "{instance_id: false}",
		"{labels_as_resource: [city], resource_attributes: {region: ''}}", "{labels_as_resource: [city], resource_attributes: {service.instance.id: x}}",
	} {
		if problems, err := verdicts(document(accepted)); len(problems) != 0 || err != nil {
			t.Errorf("otlp: %s: want it taken by both; the schema says %v, the exporter %v", accepted, problems, err)
		}
	}
	for written, message := range map[string]string{
		"{labels_as_resource: [city, city]}": `target "a" otlp.labels_as_resource names "city" twice`,
		"{labels_as_resource: city}":         "expected a list of values, not a string",
		"{labels_as_resource: [[city]]}":     "expected a single value, not a list",
		"{instance_id: maybe}":               "expected true or false, not a string",
	} {
		if problems, err := verdicts(document(written)); len(problems) == 0 || err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("otlp: %s: want it refused by the schema and by the exporter with %q; the schema says %v, the exporter %v", written, message, problems, err)
		}
	}
	for written, message := range map[string]string{
		"{labels_as_resource: [town]}":                                       `target "a" otlp.labels_as_resource names "town", which is not one of its labels`,
		"{labels_as_resource: [region]}":                                     `target "a" otlp.labels_as_resource names "region", which its resource has already as an attribute, from the exporter's otlp.resource_attributes`,
		"{labels_as_resource: [site], resource_attributes: {site: two}}":     `target "a" otlp.labels_as_resource names "site", which its resource has already as an attribute, from its otlp.resource_attributes`,
		"{instance_id: true, resource_attributes: {service.instance.id: x}}": `target "a" sets otlp.instance_id, which makes the target's name its resource's service.instance.id, and its resource has that attribute already, from its otlp.resource_attributes`,
	} {
		if problems, err := verdicts(document(written)); len(problems) != 0 || err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("otlp: %s: want it past the schema and refused by the exporter with %q; the schema says %v, the exporter %v", written, message, problems, err)
		}
	}
	// Either key is the target's resource, which only a target exported
	// over OTLP has.
	for _, written := range []string{"{labels_as_resource: [city]}", "{instance_id: true}"} {
		notExported := strings.Replace(document(written), "    export_via_otlp: true\n", "", 1)
		if problems, err := verdicts(notExported); len(problems) != 0 || err == nil || !strings.Contains(err.Error(), `target "a" sets otlp, which only a target with export_via_otlp: true uses`) {
			t.Errorf("otlp: %s without export_via_otlp: want it past the schema and refused by the exporter; the schema says %v, the exporter %v", written, problems, err)
		}
	}
}
