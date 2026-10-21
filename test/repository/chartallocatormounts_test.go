package repository

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The allocator's extra volumes: the chart mounted nothing into the
// allocator's pod beyond its own files, so a file_sd source, and an http_sd
// source's bearer_token_file and tls files, had nothing to read there.
// allocator.extraVolumes and allocator.extraVolumeMounts mount what the
// values give into the allocator's pod alone, held to what extraVolumes and
// extraVolumeMounts are held to in the exporter's.

// allocatorFileSD is a ConfigMap of file_sd files mounted into the
// allocator's pod, as --set-json arguments.
var allocatorFileSD = []string{
	"--set-json", `allocator.extraVolumes=[{"name":"file-sd","configMap":{"name":"websites-sd"}}]`,
	"--set-json", `allocator.extraVolumeMounts=[{"name":"file-sd","mountPath":"/etc/pue-sd","readOnly":true}]`,
}

// allocator.extraVolumes and extraVolumeMounts are rendered into the
// allocator's pod, after its own volumes and mounts and passed through as
// given, and not into the exporter's; the exporter's extraVolumes stay its
// own. Nothing is rendered by default, and nothing without the allocator.
func TestChartMountsTheAllocatorsExtraVolumes(t *testing.T) {
	helm := requireHelm(t)
	docs := manifests(t, helm, "--set", "allocator.enabled=true")
	pod := path(manifest(t, docs, allocatorDeployment), "spec", "template", "spec")
	if got := volumeNames(path(pod, "volumes")); !reflect.DeepEqual(got, []string{"config"}) {
		t.Errorf("by default the allocator's pod has the volumes %v, want config alone", got)
	}
	if got := volumeNames(path(pod, "containers", "0", "volumeMounts")); !reflect.DeepEqual(got, []string{"config"}) {
		t.Errorf("by default the allocator's container mounts %v, want config alone", got)
	}

	args := append([]string{"--set", "allocator.enabled=true",
		"--set-json", `extraVolumes=[{"name":"exporter-only","emptyDir":{}}]`,
		"--set-json", `extraVolumeMounts=[{"name":"exporter-only","mountPath":"/data"}]`}, allocatorFileSD...)
	docs = manifests(t, helm, args...)
	pod = path(manifest(t, docs, allocatorDeployment), "spec", "template", "spec")
	if got := volumeNames(path(pod, "volumes")); !reflect.DeepEqual(got, []string{"config", "file-sd"}) {
		t.Errorf("the allocator's pod has the volumes %v, want config and then file-sd", got)
	}
	if got := text(path(named(path(pod, "volumes"), "file-sd"), "configMap", "name")); got != "websites-sd" {
		t.Errorf("the allocator's file-sd volume is of the ConfigMap %q, want websites-sd", got)
	}
	mount := named(path(pod, "containers", "0", "volumeMounts"), "file-sd")
	if text(path(mount, "mountPath")) != "/etc/pue-sd" || text(path(mount, "readOnly")) != "true" {
		t.Errorf("the allocator's container does not mount file-sd read-only at /etc/pue-sd: %v", scalars(mount))
	}
	if got := volumeNames(path(pod, "containers", "0", "volumeMounts")); !reflect.DeepEqual(got, []string{"config", "file-sd"}) {
		t.Errorf("the allocator's container mounts %v, want config and then file-sd", got)
	}
	exporter := path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter"), "spec", "template", "spec")
	if named(path(exporter, "volumes"), "file-sd") != nil || named(path(exporter, "containers", "0", "volumeMounts"), "file-sd") != nil {
		t.Error("the exporter's pod mounts allocator.extraVolumes")
	}
	if named(path(exporter, "volumes"), "exporter-only") == nil || named(path(exporter, "containers", "0", "volumeMounts"), "exporter-only") == nil {
		t.Error("the exporter's pod does not mount extraVolumes beside the allocator")
	}

	docs = manifests(t, helm, allocatorFileSD...)
	for name, doc := range docs {
		if strings.Contains(name, "allocator") {
			t.Errorf("without the allocator %s is rendered", name)
		}
		if strings.HasPrefix(name, "Deployment/") && named(path(doc, "spec", "template", "spec", "volumes"), "file-sd") != nil {
			t.Errorf("without the allocator %s mounts allocator.extraVolumes", name)
		}
	}
}

// volumeNames lists the names of a pod's volumes or a container's mounts.
func volumeNames(node *yaml.Node) []string {
	var out []string
	for _, entry := range entries(node) {
		out = append(out, text(path(entry, "name")))
	}
	return out
}

// An allocator.extraVolumeMounts entry at a path the allocator's pod mounts
// already, inside one, or at a path another entry has, fails rendering, by
// the templates' own check with the schema skipped and with it; the values
// schema refuses one inside the configuration directory by itself, by the
// exporter's pattern. A directory is taken only while the pod mounts it: the
// static target and probe targets directories while the allocator reads
// those files, the token's with a token, the service account token's while
// that token is mounted.
func TestChartRefusesAnAllocatorMountWhereTheAllocatorsPodMountsAlready(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	const static = "allocator:\n  staticTargets:\n    data: |\n      interval: 1m\n      targets:\n        - name: legacy\n          collector: example\n          target: http://legacy.example:8080\n"
	const probes = "allocator:\n  probeJobs: [{name: websites, collector: example, targets: ['https://shop.example']}]\n"
	const kubernetes = "allocator:\n  probeJobs: [{name: pods, collector: example, discovery: [{kubernetes: {port: metrics}}]}]\n"
	const token = "allocator:\n  tokenSecret: {name: allocator-token}\n"
	const automount = "serviceAccount: {automount: true}\n"
	for name, tc := range map[string]struct {
		values, mount string
		// schema is set where the values schema refuses the mount itself.
		schema  bool
		message string
	}{
		"the configuration directory": {"", "/etc/prometheus-universal-exporter/", false,
			`allocator.extraVolumeMounts uses mountPath "/etc/prometheus-universal-exporter/", which the chart already mounts as the configuration directory /etc/prometheus-universal-exporter; choose another path`},
		"inside the configuration directory": {"", "/etc/prometheus-universal-exporter/sd", true,
			`allocator.extraVolumeMounts uses mountPath "/etc/prometheus-universal-exporter/sd", which is inside the configuration directory /etc/prometheus-universal-exporter, a read-only ConfigMap volume in which the mount point cannot be created, so the container would never start; mount it outside /etc/prometheus-universal-exporter; to add a file to it, add the file to config.data instead`},
		"the static target directory": {static, "/etc/prometheus-universal-exporter-static-targets", false,
			`which the chart already mounts as the static target directory /etc/prometheus-universal-exporter-static-targets`},
		"inside the static target directory": {static, "/etc/prometheus-universal-exporter-static-targets/sd/x.json", false,
			`which is inside the static target directory /etc/prometheus-universal-exporter-static-targets, a read-only ConfigMap volume`},
		"the probe targets directory": {probes, "//etc/prometheus-universal-exporter-probe-targets/.", false,
			`which the chart already mounts as the probe targets directory /etc/prometheus-universal-exporter-probe-targets`},
		"inside the probe targets directory": {probes, "/etc/prometheus-universal-exporter-probe-targets/sd", false,
			`which is inside the probe targets directory /etc/prometheus-universal-exporter-probe-targets, a read-only ConfigMap volume`},
		"the token directory": {token, "/var/run/prometheus-universal-exporter/allocator-token", false,
			`which the chart already mounts as the allocator token directory /var/run/prometheus-universal-exporter/allocator-token`},
		"inside the token directory": {token, "/var/run/prometheus-universal-exporter/allocator-token/ca.crt", false,
			`which is inside the allocator token directory /var/run/prometheus-universal-exporter/allocator-token, a read-only Secret volume`},
		"the service account token of a kubernetes source": {kubernetes, "/var/run/secrets/kubernetes.io/serviceaccount", false,
			`which the chart already mounts as the service account token directory /var/run/secrets/kubernetes.io/serviceaccount`},
		"inside the automounted service account token": {automount, "/var/run/secrets/kubernetes.io/serviceaccount/token", false,
			`which is inside the service account token directory /var/run/secrets/kubernetes.io/serviceaccount, a read-only projected volume`},
		"one path twice": {"", "/etc/pue-sd", false,
			`allocator.extraVolumeMounts uses mountPath "/etc/pue-sd/" twice, and a pod cannot mount two volumes at one path; give each entry a path of its own`},
	} {
		t.Run(name, func(t *testing.T) {
			list := "[{name: x, mountPath: '" + tc.mount + "'}]"
			if name == "one path twice" {
				list = "[{name: x, mountPath: '" + tc.mount + "'}, {name: z, mountPath: '" + tc.mount + "/'}]"
			}
			values := valuesFile(t, mergeAllocatorValues(tc.values, "  enabled: true\n  extraVolumes: [{name: x, emptyDir: {}}, {name: z, emptyDir: {}}]\n  extraVolumeMounts: "+list+"\n"))
			out, ok := helmTemplate(t, helm, chartDir, "-f", values)
			if ok {
				t.Fatalf("rendered:\n%.400s", out)
			}
			if tc.schema && !schemaRefusal(out, "allocator.extraVolumeMounts.0.mountPath", "not") {
				t.Errorf("the values schema does not refuse the mount:\n%s", out)
			}
			if !tc.schema && !strings.Contains(out, tc.message) {
				t.Errorf("no %q in helm's error:\n%s", tc.message, out)
			}
			skipped, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", values)
			if ok || !strings.Contains(skipped, tc.message) {
				t.Errorf("with the schema skipped: ok=%v, want an error with %q:\n%s", ok, tc.message, skipped)
			}
		})
	}

	// A directory the pod does not mount is free, and so is a path beside
	// one, or that climbs out of one again.
	for name, mount := range map[string]string{
		"the static target directory without static targets": "/etc/prometheus-universal-exporter-static-targets",
		"the probe targets directory without probe jobs":     "/etc/prometheus-universal-exporter-probe-targets/sd",
		"the token directory without a token":                "/var/run/prometheus-universal-exporter/allocator-token",
		"the service account token without one mounted":      "/var/run/secrets/kubernetes.io/serviceaccount",
		"beside the configuration directory":                 "/etc/prometheus-universal-exporter-sd",
		"out of the configuration directory again":           "/etc/prometheus-universal-exporter/a/../../pue-sd",
	} {
		file := valuesFile(t, mergeAllocatorValues("", "  enabled: true\n  extraVolumes: [{name: x, emptyDir: {}}]\n  extraVolumeMounts: [{name: x, mountPath: '"+mount+"'}]\n"))
		for _, skip := range []bool{false, true} {
			args := []string{"-f", file}
			if skip {
				args = append(args, "--skip-schema-validation")
			}
			if out, ok := helmTemplate(t, helm, chartDir, args...); !ok {
				t.Errorf("%s (schema skipped: %v): rendering failed:\n%s", name, skip, out)
			}
		}
	}
}

// mergeAllocatorValues is a values document of values, with lines added
// under its allocator key, which it is given one if it has none.
func mergeAllocatorValues(values, allocator string) string {
	if strings.Contains(values, "allocator:\n") {
		return strings.Replace(values, "allocator:\n", "allocator:\n"+allocator, 1)
	}
	return values + "allocator:\n" + allocator
}

// allocator.extraVolumes and extraVolumeMounts take what extraVolumes and
// extraVolumeMounts take, the mount path refused by the same pattern inside
// the configuration directory, which the allocator's pod mounts too.
func TestChartSchemaTakesTheAllocatorsExtraVolumesAsTheExportersOwn(t *testing.T) {
	for _, name := range []string{"extraVolumes", "extraVolumeMounts"} {
		own, exporters := schemaAt(t, "allocator."+name), schemaAt(t, name)
		if own["type"] != exporters["type"] || !reflect.DeepEqual(own["items"], exporters["items"]) {
			t.Errorf("values.schema.json takes allocator.%s as %v, and %s as %v; they must be the same", name, own["items"], name, exporters["items"])
		}
	}
}
