package repository

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The chart's target allocator: allocator.enabled deploys pue-allocator beside
// the exporter and switches the exporter pods to allocator mode. The tests
// render the chart with helm and read the manifests as Kubernetes and the
// Prometheus Operator would, parsed rather than searched for text.

// allocatorProbeJobs is a probe job of the default configuration's collector.
const allocatorProbeJobs = `allocator.probeJobs=[{"name":"websites","collector":"example","params":{"timeout":"10s"},"targets":["https://shop.example"]}]`

// allocatorStaticTargets is a static target document of the default
// configuration's collector, in a values file.
func allocatorStaticTargets(t *testing.T) string {
	t.Helper()
	return valuesFile(t, `allocator:
  staticTargets:
    data: |
      interval: 1m
      targets:
        - name: legacy
          collector: example
          target: http://legacy.example:8080
`)
}

// text is a scalar node's value, or "" for a node that is not there, so a
// missing field fails its check rather than the test.
func text(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	return node.Value
}

// manifests renders the chart and returns its manifests by "Kind/name".
func manifests(t *testing.T, helm string, args ...string) map[string]*yaml.Node {
	t.Helper()
	byName := map[string]*yaml.Node{}
	for _, doc := range renderedDocuments(t, helm, args...) {
		byName[text(path(doc, "kind"))+"/"+text(path(doc, "metadata", "name"))] = doc
	}
	return byName
}

// manifest returns the manifest named key, failing the test without it.
func manifest(t *testing.T, docs map[string]*yaml.Node, key string) *yaml.Node {
	t.Helper()
	doc, found := docs[key]
	if !found {
		names := make([]string, 0, len(docs))
		for name := range docs {
			names = append(names, name)
		}
		slices.Sort(names)
		t.Fatalf("no %s among the rendered manifests %v", key, names)
	}
	return doc
}

// scalars lists the values of a sequence of scalars, such as a container's
// args.
func scalars(node *yaml.Node) []string {
	var values []string
	if node != nil {
		for _, item := range node.Content {
			values = append(values, item.Value)
		}
	}
	return values
}

// named returns the entry of a sequence whose name is name: a volume, a
// mount, an environment variable.
func named(node *yaml.Node, name string) *yaml.Node {
	if node != nil {
		for _, item := range node.Content {
			if path(item, "name") != nil && text(path(item, "name")) == name {
				return item
			}
		}
	}
	return nil
}

// entries returns the entries of a sequence, none for a node that is
// not there.
func entries(node *yaml.Node) []*yaml.Node {
	if node == nil {
		return nil
	}
	return node.Content
}

// matchLabels returns a selector's labels as a map.
func matchLabels(node *yaml.Node) map[string]string {
	labels := map[string]string{}
	if node != nil {
		for i := 0; i+1 < len(node.Content); i += 2 {
			labels[node.Content[i].Value] = node.Content[i+1].Value
		}
	}
	return labels
}

// selects reports whether a selector's labels are all among labels.
func selects(selector, labels map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return len(selector) > 0
}

// With the allocator enabled the chart renders the allocator's Deployment,
// running /bin/pue-allocator from the exporter's image with its files, a
// headless Service on port 9116 named http, its ConfigMaps, a disruption
// budget and a NetworkPolicy; and the exporter pods run in allocator mode:
// the --allocator.* flags pointing at that Service, with the pod's name and
// address from the downward API, a cache emptyDir, and no configuration or
// static target file.
func TestChartRendersTheAllocatorAndTheExporterInAllocatorMode(t *testing.T) {
	helm := requireHelm(t)
	docs := manifests(t, helm, "--set", "allocator.enabled=true", "-f", allocatorStaticTargets(t), "--set-json", allocatorProbeJobs)

	allocator := manifest(t, docs, "Deployment/test-prometheus-universal-exporter-allocator")
	if got := text(path(allocator, "spec", "replicas")); got != "2" {
		t.Errorf("the allocator runs %s replicas, want 2", got)
	}
	container := path(allocator, "spec", "template", "spec", "containers", "0")
	if got := scalars(path(container, "command")); !slices.Equal(got, []string{"/bin/pue-allocator"}) {
		t.Errorf("the allocator's command is %v, want the binary beside the exporter's in the image", got)
	}
	if got := text(path(container, "image")); got != "ghcr.io/eenchev/prometheus-universal-exporter:"+chartAppVersion(t) {
		t.Errorf("the allocator runs image %s, want the exporter's", got)
	}
	wantArgs := []string{
		"--web.listen-address=:9116",
		"--config.file=/etc/prometheus-universal-exporter/config.yaml",
		"--static-targets-file=/etc/prometheus-universal-exporter-static-targets/static-targets.yaml",
		"--probe-targets-file=/etc/prometheus-universal-exporter-probe-targets/probe-targets.yaml",
		"--log.level=info",
		"--settle=15s",
	}
	if got := scalars(path(container, "args")); !slices.Equal(got, wantArgs) {
		t.Errorf("the allocator's args are %v, want %v", got, wantArgs)
	}
	volumes := path(allocator, "spec", "template", "spec", "volumes")
	for volume, configMap := range map[string]string{
		"config":         "test-prometheus-universal-exporter",
		"static-targets": "test-prometheus-universal-exporter-allocator-static-targets",
		"probe-targets":  "test-prometheus-universal-exporter-allocator-probe-targets",
	} {
		if got := path(named(volumes, volume), "configMap", "name"); got == nil || got.Value != configMap {
			t.Errorf("the allocator's volume %s is not the ConfigMap %s: %v", volume, configMap, got)
		}
	}
	probeTargets := text(path(manifest(t, docs, "ConfigMap/test-prometheus-universal-exporter-allocator-probe-targets"), "data", "probe-targets.yaml"))
	var file struct {
		ProbeJobs []struct {
			Name, Collector string
			Params          map[string]string
			Targets         []string
		} `yaml:"probe_jobs"`
	}
	if err := yaml.Unmarshal([]byte(probeTargets), &file); err != nil || len(file.ProbeJobs) != 1 || file.ProbeJobs[0].Name != "websites" || file.ProbeJobs[0].Collector != "example" || file.ProbeJobs[0].Params["timeout"] != "10s" || !slices.Equal(file.ProbeJobs[0].Targets, []string{"https://shop.example"}) {
		t.Errorf("the probe targets file does not hold the probe job as given (%v):\n%s", err, probeTargets)
	}
	if got := text(path(manifest(t, docs, "ConfigMap/test-prometheus-universal-exporter-allocator-static-targets"), "data", "static-targets.yaml")); !strings.Contains(got, "name: legacy") {
		t.Errorf("the static targets file is not the document given:\n%s", got)
	}

	service := manifest(t, docs, "Service/test-prometheus-universal-exporter-allocator")
	if text(path(service, "spec", "clusterIP")) != "None" || text(path(service, "spec", "ports", "0", "name")) != "http" || text(path(service, "spec", "ports", "0", "port")) != "9116" {
		t.Error("the allocator's Service is not headless with port http, 9116")
	}
	allocatorPods := matchLabels(path(allocator, "spec", "template", "metadata", "labels"))
	if !selects(matchLabels(path(service, "spec", "selector")), allocatorPods) {
		t.Error("the allocator's Service does not select the allocator's pods")
	}
	if got := text(path(manifest(t, docs, "PodDisruptionBudget/test-prometheus-universal-exporter-allocator"), "spec", "maxUnavailable")); got != "1" {
		t.Errorf("the allocator's disruption budget allows %s unavailable, want 1", got)
	}

	exporter := manifest(t, docs, "Deployment/test-prometheus-universal-exporter")
	exporterContainer := path(exporter, "spec", "template", "spec", "containers", "0")
	args := scalars(path(exporterContainer, "args"))
	for _, want := range []string{
		"--allocator.url=http://test-prometheus-universal-exporter-allocator.default.svc:9116",
		"--allocator.cache-dir=/var/cache/prometheus-universal-exporter",
		"--allocator.member=$(POD_NAME)",
		"--allocator.advertise=$(POD_IP):8080",
		"--web.static-targets-path=/static-targets",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("the exporter's args %v lack %s", args, want)
		}
	}
	for _, arg := range args {
		for _, refused := range []string{"--config.file", "--static-targets-file", "--allocator.token-file"} {
			if strings.HasPrefix(arg, refused) {
				t.Errorf("the exporter in allocator mode is given %s", arg)
			}
		}
	}
	env := path(exporterContainer, "env")
	for name, field := range map[string]string{"POD_NAME": "metadata.name", "POD_IP": "status.podIP"} {
		if got := path(named(env, name), "valueFrom", "fieldRef", "fieldPath"); got == nil || got.Value != field {
			t.Errorf("the exporter's %s does not come from %s", name, field)
		}
	}
	exporterVolumes := path(exporter, "spec", "template", "spec", "volumes")
	if named(exporterVolumes, "config") != nil || named(path(exporterContainer, "volumeMounts"), "config") != nil {
		t.Error("the exporter in allocator mode mounts the configuration ConfigMap")
	}
	if cache := named(exporterVolumes, "allocator-cache"); path(cache, "emptyDir") == nil {
		t.Error("the exporter's cache is not an emptyDir")
	}
	if mount := named(path(exporterContainer, "volumeMounts"), "allocator-cache"); path(mount, "mountPath") == nil || text(path(mount, "mountPath")) != "/var/cache/prometheus-universal-exporter" {
		t.Error("the exporter's cache is not mounted at its cache directory")
	}

	// Every selector of the exporter's leaves the allocator's pods out, and
	// the allocator's NetworkPolicy lets the exporter's pods in.
	exporterPods := matchLabels(path(exporter, "spec", "template", "metadata", "labels"))
	for _, key := range []string{"Service/test-prometheus-universal-exporter", "ServiceMonitor/test-prometheus-universal-exporter-self", "ServiceMonitor/test-prometheus-universal-exporter-static-targets"} {
		selector := path(manifest(t, docs, key), "spec", "selector")
		if labels := child(selector, "matchLabels"); labels != nil {
			selector = labels
		}
		if selects(matchLabels(selector), allocatorPods) {
			t.Errorf("%s selects the allocator's pods", key)
		}
		if strings.HasPrefix(key, "Service/") && !selects(matchLabels(selector), exporterPods) {
			t.Errorf("%s no longer selects the exporter's pods", key)
		}
	}
	if selects(matchLabels(path(manifest(t, docs, "ServiceMonitor/test-prometheus-universal-exporter-self"), "spec", "selector", "matchLabels")), matchLabels(path(service, "metadata", "labels"))) {
		t.Error("the self-metrics monitor selects the allocator's Service")
	}
	policy := manifest(t, docs, "NetworkPolicy/test-prometheus-universal-exporter-allocator")
	if !selects(matchLabels(path(policy, "spec", "podSelector", "matchLabels")), allocatorPods) {
		t.Error("the allocator's NetworkPolicy does not apply to the allocator's pods")
	}
	if !selects(matchLabels(path(policy, "spec", "ingress", "0", "from", "0", "podSelector", "matchLabels")), exporterPods) {
		t.Error("the allocator's NetworkPolicy does not let the exporter's pods in")
	}
	if got := path(policy, "spec", "ingress", "0", "from", "1", "podSelector", "matchLabels", "app.kubernetes.io/name"); got == nil || got.Value != "prometheus" {
		t.Error("the allocator's NetworkPolicy does not let the Prometheus Operator's Prometheus pods in by default")
	}
}

// Each probe job renders a ScrapeConfig whose HTTP service discovery reads
// the job's path on the allocator's Service, with no relabeling, since the
// allocator's answer sets the address, the path and the parameters itself.
// The shared token, when set, is presented there, sent by the exporter pods
// and required by the allocator; and the exporter's Basic Auth, when on, is
// presented on the probes.
func TestChartRendersAScrapeConfigPerProbeJob(t *testing.T) {
	helm := requireHelm(t)
	docs := manifests(t, helm, "--set", "allocator.enabled=true", "--set-json", `allocator.probeJobs=[{"name":"websites","collector":"example"},{"name":"apis","collector":"example"}]`,
		"--set", "allocator.scrapeConfig.interval=1m", "--set", "allocator.scrapeConfig.scrapeTimeout=20s", "--set", "allocator.scrapeConfig.labels.release=main")
	for _, job := range []string{"websites", "apis"} {
		scrape := manifest(t, docs, "ScrapeConfig/test-prometheus-universal-exporter-"+job)
		if got := text(path(scrape, "apiVersion")); got != "monitoring.coreos.com/v1alpha1" {
			t.Errorf("the ScrapeConfig of %s is %s", job, got)
		}
		sd := path(scrape, "spec", "httpSDConfigs", "0")
		if got := text(path(sd, "url")); got != "http://test-prometheus-universal-exporter-allocator.default.svc:9116/v1/sd/"+job {
			t.Errorf("the ScrapeConfig of %s reads %s", job, got)
		}
		if got := text(path(sd, "refreshInterval")); got != "30s" {
			t.Errorf("the ScrapeConfig of %s refreshes every %s, want 30s", job, got)
		}
		if path(sd, "authorization") != nil || path(scrape, "spec", "basicAuth") != nil {
			t.Errorf("the ScrapeConfig of %s presents a credential nobody set", job)
		}
		if text(path(scrape, "spec", "scrapeInterval")) != "1m" || text(path(scrape, "spec", "scrapeTimeout")) != "20s" || text(path(scrape, "metadata", "labels", "release")) != "main" {
			t.Errorf("the ScrapeConfig of %s does not carry the interval, timeout and labels given", job)
		}
		for _, key := range []string{"relabelings", "metricsPath", "params"} {
			if path(scrape, "spec", key) != nil {
				t.Errorf("the ScrapeConfig of %s sets %s, which the allocator's answer sets", job, key)
			}
		}
	}

	docs = manifests(t, helm, "--set", "allocator.enabled=true", "--set-json", allocatorProbeJobs, "--set", "allocator.tokenSecret.name=allocator-token", "--set", "allocator.tokenSecret.key=shared",
		"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth")
	scrape := manifest(t, docs, "ScrapeConfig/test-prometheus-universal-exporter-websites")
	auth := path(scrape, "spec", "httpSDConfigs", "0", "authorization")
	if text(path(auth, "type")) != "Bearer" || text(path(auth, "credentials", "name")) != "allocator-token" || text(path(auth, "credentials", "key")) != "shared" {
		t.Error("the ScrapeConfig does not present the allocator's token")
	}
	if text(path(scrape, "spec", "basicAuth", "username", "name")) != "exporter-auth" || text(path(scrape, "spec", "basicAuth", "password", "name")) != "exporter-auth" {
		t.Error("the ScrapeConfig does not present the exporter's Basic Auth on its probes")
	}
	exporter := path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter"), "spec", "template", "spec")
	if !slices.Contains(scalars(path(exporter, "containers", "0", "args")), "--allocator.token-file=/var/run/prometheus-universal-exporter/allocator-token/token") {
		t.Error("the exporter is not given the token file")
	}
	if got := path(named(path(exporter, "volumes"), "allocator-token"), "secret", "items", "0", "key"); got == nil || got.Value != "shared" {
		t.Error("the exporter does not mount the token's key")
	}
	// The credential Secrets stay mounted in allocator mode.
	if named(path(exporter, "volumes"), "web-auth") == nil {
		t.Error("the exporter in allocator mode no longer mounts the webAuth Secret")
	}
	allocator := path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter-allocator"), "spec", "template", "spec")
	if !slices.Contains(scalars(path(allocator, "containers", "0", "args")), "--web.token-file=/var/run/prometheus-universal-exporter/allocator-token/token") {
		t.Error("the allocator is not given the token file")
	}

	// No probe job, or the ScrapeConfigs switched off, renders none.
	for _, args := range [][]string{
		{"--set", "allocator.enabled=true"},
		{"--set", "allocator.enabled=true", "--set-json", allocatorProbeJobs, "--set", "allocator.scrapeConfig.enabled=false"},
	} {
		out, ok := helmTemplate(t, helm, chartDir, args...)
		if !ok || strings.Contains(out, "kind: ScrapeConfig") {
			t.Errorf("%v rendered a ScrapeConfig, or failed (%v):\n%s", args, ok, out)
		}
	}
}

// In allocator mode the exporter is always a Deployment, scaled by the
// HorizontalPodAutoscaler, with no Service of its own beside its one Service,
// and the values schema refuses an allocator.cache key: the cache is the
// emptyDir alone.
func TestChartRunsTheExporterAsADeploymentInAllocatorMode(t *testing.T) {
	helm := requireHelm(t)
	docs := manifests(t, helm, "--set", "allocator.enabled=true", "--set", "autoscaling.enabled=true")
	manifest(t, docs, "Deployment/test-prometheus-universal-exporter")
	if text(path(manifest(t, docs, "HorizontalPodAutoscaler/test-prometheus-universal-exporter"), "spec", "scaleTargetRef", "kind")) != "Deployment" {
		t.Error("the HorizontalPodAutoscaler does not scale the Deployment")
	}
	for name := range docs {
		if strings.HasPrefix(name, "StatefulSet/") || (strings.HasPrefix(name, "Service/") && name != "Service/test-prometheus-universal-exporter" && name != "Service/test-prometheus-universal-exporter-allocator") {
			t.Errorf("allocator mode renders %s", name)
		}
	}
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true", "--set", "allocator.cache.persistent=true"); ok || !strings.Contains(out, "cache") {
		t.Errorf("the values schema accepted allocator.cache, or refused it without naming it (%v):\n%s", ok, out)
	}
}

// In allocator mode the exporter pods' checksum leaves the configuration
// out, since it reaches them from the allocator without a restart, while the
// allocator's pods roll on it; without the allocator the exporter's pods roll
// on it as before.
func TestChartRollsTheAllocatorAndNotTheExporterOnAConfigurationChange(t *testing.T) {
	helm := requireHelm(t)
	checksums := func(args ...string) (exporter, allocator string) {
		docs := manifests(t, helm, args...)
		exporter = text(path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter"), "spec", "template", "metadata", "annotations", "checksum/config"))
		if doc, found := docs["Deployment/test-prometheus-universal-exporter-allocator"]; found {
			allocator = text(path(doc, "spec", "template", "metadata", "annotations", "checksum/config"))
		}
		return exporter, allocator
	}
	changed := valuesFile(t, "config:\n  data:\n    config.yaml: |\n      collectors: []\n")
	exporterBefore, allocatorBefore := checksums("--set", "allocator.enabled=true")
	exporterAfter, allocatorAfter := checksums("--set", "allocator.enabled=true", "-f", changed)
	if exporterBefore != exporterAfter {
		t.Error("a configuration change rolls the exporter's pods in allocator mode")
	}
	if allocatorBefore == allocatorAfter || allocatorBefore == "" {
		t.Error("a configuration change does not roll the allocator's pods")
	}
	_, staticAfter := checksums("--set", "allocator.enabled=true", "-f", allocatorStaticTargets(t))
	if staticAfter == allocatorBefore {
		t.Error("a static target change does not roll the allocator's pods")
	}
	plainBefore, _ := checksums()
	plainAfter, _ := checksums("-f", changed)
	if plainBefore == plainAfter {
		t.Error("without the allocator a configuration change no longer rolls the exporter's pods")
	}
}

// With the allocator serving static targets the static targets monitor reads
// every exporter pod's share; with egress rules the exporter's NetworkPolicy
// lets its pods reach the allocator.
func TestChartScrapesTheAllocatorsStaticTargetsAndLetsThePodsReachIt(t *testing.T) {
	helm := requireHelm(t)
	docs := manifests(t, helm, "--set", "allocator.enabled=true", "-f", allocatorStaticTargets(t), "--set", "networkPolicy.enabled=true", "--set-json", `networkPolicy.egress=[{"to":[{"ipBlock":{"cidr":"10.0.0.0/8"}}]}]`)
	monitor := manifest(t, docs, "ServiceMonitor/test-prometheus-universal-exporter-static-targets")
	if text(path(monitor, "spec", "endpoints", "0", "path")) != "/static-targets" {
		t.Error("the static targets monitor does not read the static targets path")
	}
	egress := path(manifest(t, docs, "NetworkPolicy/test-prometheus-universal-exporter"), "spec", "egress")
	allocatorPods := matchLabels(path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter-allocator"), "spec", "template", "metadata", "labels"))
	reached := false
	for _, rule := range entries(egress) {
		if selects(matchLabels(path(rule, "to", "0", "podSelector", "matchLabels")), allocatorPods) && text(path(rule, "ports", "0", "port")) == "9116" {
			reached = true
		}
	}
	if !reached {
		t.Error("the exporter's egress rules do not let its pods reach the allocator")
	}
	// Without static targets at the allocator there is nothing to scrape.
	out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true")
	if !ok || strings.Contains(out, "-static-targets\n") {
		t.Errorf("the static targets monitor or ConfigMap rendered with no static targets, or rendering failed (%v)", ok)
	}
}

// The combinations the allocator cannot run with fail rendering with a
// sentence saying what to change, refused by the values schema where it can
// say so, and by the templates with the schema skipped.
func TestChartRefusesWhatTheAllocatorCannotRunWith(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	for name, tc := range map[string]struct {
		args             []string
		schema, template string
	}{
		"the allocator beside static targets": {[]string{"--set", "allocator.enabled=true", "--set", "staticTargets.enabled=true", "--set", "staticTargets.data=targets: []"},
			"staticTargets.enabled", "allocator.enabled and staticTargets.enabled are both true"},
		"a probe monitor beside the allocator": {[]string{"--set", "allocator.enabled=true", "--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"}]`},
			"monitors.0.enabled", "monitors entry apps is enabled beside allocator.enabled"},
		"a probe job of an unknown collector": {[]string{"--set", "allocator.enabled=true", "--set-json", `allocator.probeJobs=[{"name":"websites","collector":"nope"}]`},
			"", `allocator.probeJobs entry websites names collector "nope", which config.data does not define`},
		"a probe job without a collector": {[]string{"--set", "allocator.enabled=true", "--set-json", `allocator.probeJobs=[{"name":"websites"}]`},
			"allocator.probeJobs.0", "allocator.probeJobs entry websites names no collector"},
		"a probe job named in upper case": {[]string{"--set", "allocator.enabled=true", "--set-json", `allocator.probeJobs=[{"name":"Websites","collector":"example"}]`},
			"allocator.probeJobs.0.name", `allocator.probeJobs entry #0 has name "Websites"`},
		"two probe jobs of one name": {[]string{"--set", "allocator.enabled=true", "--set-json", `allocator.probeJobs=[{"name":"w","collector":"example"},{"name":"w","collector":"example"}]`},
			"", `allocator.probeJobs entries #0 and #1 are both named "w"`},
		"no allocator replica": {[]string{"--set", "allocator.enabled=true", "--set", "allocator.replicas=0"},
			"allocator.replicas", "allocator.replicas 0 must be a whole number from 1 to 2147483647"},
		"a settle that is no duration": {[]string{"--set", "allocator.enabled=true", "--set", "allocator.settle=soon"},
			"allocator.settle", `allocator.settle "soon" must be a Go duration`},
		"an unknown log level": {[]string{"--set", "allocator.enabled=true", "--set", "allocator.logLevel=loud"},
			"allocator.logLevel", `allocator.logLevel "loud" must be debug, info, warn or error`},
		"the pod's address in env": {[]string{"--set", "allocator.enabled=true", "--set-json", `env=[{"name":"POD_IP","value":"10.0.0.1"}]`},
			"", "env has a POD_IP entry"},
		"an allocator flag as an extra": {[]string{"--set", "allocator.enabled=true", "--set-json", `extraArgs=["--allocator.member=x"]`},
			"", "sets --allocator.member, which the chart already manages"},
		"static target names beside the allocator": {[]string{"--set", "allocator.enabled=true", "--set", "allocator.staticTargets.data=targets: []", "--set-json", `staticTargets.monitor.targets=["legacy"]`},
			"", "staticTargets.monitor.targets is set beside allocator.enabled"},
		"a mount over the cache": {[]string{"--set", "allocator.enabled=true", "--set-json", `extraVolumeMounts=[{"name":"x","mountPath":"/var/cache/prometheus-universal-exporter/"}]`},
			"", "which the chart already mounts as the allocator cache directory"},
		"a credential over the token": {[]string{"--set", "allocator.enabled=true", "--set", "allocator.tokenSecret.name=t", "--set", "webAuth.enabled=true", "--set", "webAuth.secretName=s", "--set", "webAuth.mountPath=/var/run/prometheus-universal-exporter/allocator-token"},
			"", "is also the allocator token directory"},
		"a scrape timeout over its interval": {[]string{"--set", "allocator.enabled=true", "--set", "allocator.scrapeConfig.interval=10s", "--set", "allocator.scrapeConfig.scrapeTimeout=1m"},
			"", "allocator.scrapeConfig has scrapeTimeout 1m, longer than its interval 10s"},
	} {
		t.Run(name, func(t *testing.T) {
			want := tc.schema
			if want == "" {
				want = tc.template
			}
			if out, ok := helmTemplate(t, helm, chartDir, tc.args...); ok || !namesInHelmError(out, want) {
				t.Errorf("with the schema: ok=%v, want an error naming %q:\n%s", ok, want, out)
			}
			if out, ok := helmTemplate(t, helm, chartDir, append([]string{"--skip-schema-validation"}, tc.args...)...); ok || !strings.Contains(out, tc.template) {
				t.Errorf("without the schema: ok=%v, want an error naming %q:\n%s", ok, tc.template, out)
			}
		})
	}
}

// chartAppVersion returns the appVersion of Chart.yaml, the exporter release
// an empty image tag deploys.
func chartAppVersion(t *testing.T) string {
	t.Helper()
	var chart struct {
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal([]byte(readChartFile(t, "Chart.yaml")), &chart); err != nil || chart.AppVersion == "" {
		t.Fatalf("Chart.yaml has no appVersion: %v", err)
	}
	return chart.AppVersion
}
