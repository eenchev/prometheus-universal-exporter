//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// otlp.exporter_instance: its defaults, the attribute pue.exporter.instance
// and the host name or the --allocator.member name, filled in at each load,
// and what it is refused beside.

// withHostname makes the host name, as the check reads it, host, or the
// error err.
func withHostname(t *testing.T, host string, err error) {
	t.Helper()
	previous := hostname
	hostname = func() (string, error) { return host, err }
	t.Cleanup(func() { hostname = previous })
}

// exporterInstanceDocument is a configuration whose otlp block is switched
// on, with the exporter_instance block written as block.
func exporterInstanceDocument(block string) string {
	return "otlp:\n  enabled: true\n  endpoint: http://collector.invalid:4318/v1/metrics\n  resource_attributes: {deployment.environment: prod}\n  exporter_instance: " + block + "\n" + testutil.MinimalConfig
}

// Switched on with nothing else, the attribute is pue.exporter.instance and
// its value the host name; the name of WithExporterInstanceName, which an
// exporter in allocator mode gives with its --allocator.member name, takes
// the host name's place; a value written, expanded with --config.expand-env,
// takes the place of both, and an attribute written is kept. A Manager that
// was given the name reads every reload with it.
func TestTheExporterInstanceIsTheHostNameOrTheAllocatorMember(t *testing.T) {
	withHostname(t, "host-a", nil)
	for name, test := range map[string]struct {
		block     string
		options   []LoadOption
		attribute string
		value     string
	}{
		"the defaults":                   {"{enabled: true}", nil, "pue.exporter.instance", "host-a"},
		"the allocator member":           {"{enabled: true}", []LoadOption{WithExporterInstanceName("pod-3")}, "pue.exporter.instance", "pod-3"},
		"a value written as \"\"":        {`{enabled: true, value: ""}`, []LoadOption{WithExporterInstanceName("pod-3")}, "pue.exporter.instance", "pod-3"},
		"a value written":                {"{enabled: true, attribute: k8s.pod.name, value: replica-x}", []LoadOption{WithExporterInstanceName("pod-3")}, "k8s.pod.name", "replica-x"},
		"a value from the environment":   {"{enabled: true, value: \"${PUE_TEST_POD}\"}", []LoadOption{WithEnvExpansion()}, "pue.exporter.instance", "pod-from-env"},
		"service.instance.id, if wanted": {"{enabled: true, attribute: service.instance.id}", nil, "service.instance.id", "host-a"},
	} {
		t.Setenv("PUE_TEST_POD", "pod-from-env")
		cfg, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", exporterInstanceDocument(test.block)), test.options...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, want := cfg.OTLP.ExporterInstance, (model.OTLPExporterInstanceConfig{Enabled: true, Attribute: test.attribute, Value: test.value}); got != want {
			t.Errorf("%s: otlp.exporter_instance is %+v, want %+v", name, got, want)
		}
	}

	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", exporterInstanceDocument("{enabled: true}"))
	cfg, err := Load(path, WithExporterInstanceName("pod-3"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, path, slog.New(slog.DiscardHandler))
	manager.SetExporterInstanceName("pod-3")
	if err := manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	if got := manager.Get().OTLP.ExporterInstance.Value; got != "pod-3" {
		t.Errorf("after a reload the value is %q, want the member name pod-3", got)
	}
}

// Without a value, a member name or a host name there is nothing to name
// the replica with, and the configuration is refused, saying how to give
// one. Switched off, the block, or the otlp block, reads no host name and
// fills nothing in; the target allocator, which reads the configuration it
// serves without the named files and is no replica, leaves the value as
// written.
func TestAnExporterInstanceWithoutAHostNameIsRefused(t *testing.T) {
	for name, fails := range map[string]error{"an error": errors.New("no uts namespace"), "an empty name": nil} {
		withHostname(t, "", fails)
		_, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", exporterInstanceDocument("{enabled: true}")))
		if err == nil || !strings.Contains(err.Error(), "otlp.exporter_instance.value is left out and the host name, its default, cannot be read") || !strings.Contains(err.Error(), "${POD_NAME} with --config.expand-env") {
			t.Errorf("%s: error %v", name, err)
		}
		cfg, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", exporterInstanceDocument("{enabled: true, value: pod-0}")))
		if err != nil || cfg.OTLP.ExporterInstance.Value != "pod-0" {
			t.Errorf("%s, with a value: %v", name, err)
		}
		cfg, err = Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", exporterInstanceDocument("{enabled: true}")), WithoutNamedFiles())
		if err != nil || cfg.OTLP.ExporterInstance != (model.OTLPExporterInstanceConfig{Enabled: true, Attribute: model.DefaultOTLPExporterInstanceAttribute}) {
			t.Errorf("%s, read as the allocator reads it: %+v, %v", name, cfg.OTLP.ExporterInstance, err)
		}
	}
	withHostname(t, "", errors.New("unread"))
	cfg, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", exporterInstanceDocument("{enabled: false, attribute: service.name}")))
	if err != nil || cfg.OTLP.ExporterInstance != (model.OTLPExporterInstanceConfig{Attribute: "service.name"}) {
		t.Errorf("switched off: %+v, %v", cfg.OTLP.ExporterInstance, err)
	}
	cfg = otlpSettings(func(o *model.OTLPConfig) { o.Enabled, o.ExporterInstance.Enabled = false, true })
	if err := Validate(cfg); err != nil || cfg.OTLP.ExporterInstance.Value != "" {
		t.Errorf("in an otlp block switched off: %+v, %v", cfg.OTLP.ExporterInstance, err)
	}
}

// The attribute is not service.name, which otlp.service_name sets, nor a key
// of otlp.resource_attributes, written "" or not, nor blanks alone, and
// neither it nor the value holds a control character; the exporter_instance
// block is held to enabled as every switched block of otlp is.
func TestAnExporterInstanceAttributeOfTheResourceIsRefused(t *testing.T) {
	withHostname(t, "host-a", nil)
	for block, want := range map[string]string{
		"{enabled: true, attribute: service.name}":           "otlp.exporter_instance.attribute is service.name, which otlp.service_name sets",
		"{enabled: true, attribute: deployment.environment}": "otlp.exporter_instance.attribute is deployment.environment, which otlp.resource_attributes sets too",
		`{enabled: true, attribute: "  "}`:                   "otlp.exporter_instance.attribute is blanks alone",
		`{enabled: true, attribute: "a\tb"}`:                 `otlp.exporter_instance.attribute has the control character '\t' in it`,
		`{enabled: true, value: "pod\n0"}`:                   `otlp.exporter_instance.value has the control character '\n' in it`,
		"{attribute: replica}":                               "otlp.exporter_instance sets attribute but not enabled",
		"{enabled: true, attributes: replica}":               "unknown key \"attributes\" in otlp.exporter_instance",
	} {
		if _, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", exporterInstanceDocument(block))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", block, err, want)
		}
	}
	cfg := otlpSettings(func(o *model.OTLPConfig) {
		o.ResourceAttributes = map[string]string{"pue.exporter.instance": ""}
		o.ExporterInstance.Enabled = true
	})
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "which otlp.resource_attributes sets too") {
		t.Errorf("an attribute of resource_attributes written \"\": %v", err)
	}
}

// A static target exported over OTLP whose resource would have the
// attribute already — from its otlp.resource_attributes, written "" or not,
// its otlp.labels_as_resource, or as service.instance.id with
// otlp.instance_id — is refused, naming the target. The default attribute
// beside otlp.instance_id, and service.instance.id beside a target without
// it, are taken; switched off, the block refuses nothing.
func TestAStaticTargetWithTheExporterInstanceAttributeIsRefused(t *testing.T) {
	config := func(attribute string, enabled bool) *model.Config {
		cfg := otlpSettings(func(o *model.OTLPConfig) {
			o.ExporterInstance = model.OTLPExporterInstanceConfig{Enabled: enabled, Attribute: attribute, Value: "pod-0"}
		})
		if err := Validate(cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	target := func(change func(*model.StaticTarget)) *model.StaticTargetFile {
		st := model.StaticTarget{Name: "eu", Collector: "text", Target: "http://eu.invalid", ExportViaOTLP: true, Labels: map[string]string{"replica": "a"}}
		change(&st)
		return &model.StaticTargetFile{Targets: []model.StaticTarget{st}}
	}
	for name, test := range map[string]struct {
		attribute string
		change    func(*model.StaticTarget)
		want      string
	}{
		"its resource_attributes": {"", func(st *model.StaticTarget) {
			st.OTLP.ResourceAttributes = map[string]string{"pue.exporter.instance": "x"}
		},
			`target "eu" otlp.resource_attributes sets pue.exporter.instance, which otlp.exporter_instance gives every resource the exporter sends`},
		"its resource_attributes written \"\"": {"", func(st *model.StaticTarget) {
			st.OTLP.ResourceAttributes = map[string]string{"pue.exporter.instance": ""}
		},
			`target "eu" otlp.resource_attributes sets pue.exporter.instance`},
		"its labels_as_resource": {"replica", func(st *model.StaticTarget) { st.OTLP.LabelsAsResource = []string{"replica"} },
			`target "eu" otlp.labels_as_resource names "replica", which otlp.exporter_instance gives every resource the exporter sends`},
		"its instance_id": {"service.instance.id", func(st *model.StaticTarget) { st.OTLP.InstanceID = true },
			`target "eu" sets otlp.instance_id, which makes the target's name its resource's service.instance.id, and otlp.exporter_instance.attribute is service.instance.id too`},
	} {
		file := target(test.change)
		if err := ValidateStaticTargetsAgainst(file, config(test.attribute, true)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
		if err := ValidateStaticTargetsAgainst(file, config(test.attribute, false)); err != nil {
			t.Errorf("%s, switched off: %v", name, err)
		}
	}
	for name, test := range map[string]struct {
		attribute string
		change    func(*model.StaticTarget)
	}{
		"the default beside instance_id":          {"", func(st *model.StaticTarget) { st.OTLP.InstanceID = true }},
		"service.instance.id without instance_id": {"service.instance.id", func(st *model.StaticTarget) { st.OTLP.ServiceName = "legacy" }},
		"another label moved to the resource":     {"", func(st *model.StaticTarget) { st.OTLP.LabelsAsResource = []string{"replica"} }},
	} {
		if err := ValidateStaticTargetsAgainst(target(test.change), config(test.attribute, true)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
