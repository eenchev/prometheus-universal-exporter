package repository

import (
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
)

// The allocator's pods as the chart renders them beyond what
// chartallocator_test.go covers: their scheduling and metadata, how long a
// new one waits before it counts as available, the ServiceMonitor of its own
// metrics, the expansion of its static targets on the exporter pods, and the
// probe job keys, held to the allocator's own decoder.

const allocatorDeployment = "Deployment/test-prometheus-universal-exporter-allocator"

// The allocator's pods are scheduled by allocator's own values, never the
// exporter's, and by default spread softly over nodes, so one node going away
// does not take every allocator with it; a constraint without a
// labelSelector spreads the allocator's pods, and [] renders none.
// allocator.podLabels and podAnnotations reach the pods, and checksum/config
// stays the chart's.
func TestChartSchedulesTheAllocatorByItsOwnValues(t *testing.T) {
	helm := requireHelm(t)
	docs := manifests(t, helm, "--set", "allocator.enabled=true",
		"--set", "priorityClassName=exporter", "--set-json", `nodeSelector={"pool":"exporter"}`,
		"--set-json", `tolerations=[{"key":"exporter","operator":"Exists"}]`, "--set-json", `affinity={"podAntiAffinity":{}}`,
		"--set-json", `podLabels={"team":"exporter"}`, "--set-json", `podAnnotations={"note":"exporter"}`)
	pod := path(manifest(t, docs, allocatorDeployment), "spec", "template")
	for _, key := range []string{"priorityClassName", "nodeSelector", "tolerations", "affinity"} {
		if path(pod, "spec", key) != nil {
			t.Errorf("the allocator's pods take the exporter's %s", key)
		}
	}
	if path(pod, "metadata", "labels", "team") != nil || path(pod, "metadata", "annotations", "note") != nil {
		t.Error("the allocator's pods take the exporter's podLabels or podAnnotations")
	}
	allocatorPods := matchLabels(path(pod, "metadata", "labels"))
	spread := entries(path(pod, "spec", "topologySpreadConstraints"))
	if len(spread) != 1 || text(path(spread[0], "topologyKey")) != "kubernetes.io/hostname" || text(path(spread[0], "whenUnsatisfiable")) != "ScheduleAnyway" ||
		text(path(spread[0], "maxSkew")) != "1" || !selects(matchLabels(path(spread[0], "labelSelector", "matchLabels")), allocatorPods) {
		t.Errorf("the allocator's pods are not spread softly over nodes by default: %v", spread)
	}
	exporterPods := matchLabels(path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter"), "spec", "template", "metadata", "labels"))
	if len(spread) == 1 && selects(matchLabels(path(spread[0], "labelSelector", "matchLabels")), exporterPods) {
		t.Error("the allocator's default spread counts the exporter's pods")
	}

	docs = manifests(t, helm, "--set", "allocator.enabled=true",
		"--set", "allocator.priorityClassName=allocator", "--set-json", `allocator.nodeSelector={"pool":"allocator"}`,
		"--set-json", `allocator.tolerations=[{"key":"allocator","operator":"Exists"}]`,
		"--set-json", `allocator.affinity={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"pool","operator":"In","values":["allocator"]}]}]}}}`,
		"--set-json", `allocator.topologySpreadConstraints=[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"DoNotSchedule"},{"maxSkew":2,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"ScheduleAnyway","labelSelector":{"matchLabels":{"tier":"control"}}}]`,
		"--set-json", `allocator.podLabels={"team":"allocator"}`, "--set-json", `allocator.podAnnotations={"note":"allocator","checksum/config":"mine"}`)
	pod = path(manifest(t, docs, allocatorDeployment), "spec", "template")
	if text(path(pod, "spec", "priorityClassName")) != "allocator" || text(path(pod, "spec", "nodeSelector", "pool")) != "allocator" ||
		text(path(pod, "spec", "tolerations", "0", "key")) != "allocator" ||
		text(path(pod, "spec", "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms", "0", "matchExpressions", "0", "key")) != "pool" {
		t.Error("the allocator's pods do not carry allocator.priorityClassName, nodeSelector, tolerations and affinity as given")
	}
	spread = entries(path(pod, "spec", "topologySpreadConstraints"))
	if len(spread) != 2 || text(path(spread[0], "topologyKey")) != "topology.kubernetes.io/zone" || !selects(matchLabels(path(spread[0], "labelSelector", "matchLabels")), matchLabels(path(pod, "metadata", "labels"))) ||
		!maps.Equal(matchLabels(path(spread[1], "labelSelector", "matchLabels")), map[string]string{"tier": "control"}) {
		t.Errorf("the allocator's topologySpreadConstraints are not the ones given, the first spreading the allocator's pods: %v", spread)
	}
	if text(path(pod, "metadata", "labels", "team")) != "allocator" || text(path(pod, "metadata", "annotations", "note")) != "allocator" {
		t.Error("the allocator's pods do not carry allocator.podLabels and podAnnotations")
	}
	if got := text(path(pod, "metadata", "annotations", "checksum/config")); len(got) != 64 {
		t.Errorf("allocator.podAnnotations replaced checksum/config with %q", got)
	}

	docs = manifests(t, helm, "--set", "allocator.enabled=true", "--set-json", "allocator.topologySpreadConstraints=[]")
	if got := path(manifest(t, docs, allocatorDeployment), "spec", "template", "spec", "topologySpreadConstraints"); got != nil {
		t.Errorf("allocator.topologySpreadConstraints: [] renders %v, want none", scalars(got))
	}
}

// /ready answers as soon as an allocator has read its inputs, but its first
// member list takes effect max(settle, 10s) after it starts, and the exporter
// pods hear of it at their next heartbeat, 10s on. The Deployment's
// minReadySeconds covers both, so a rollout does not count a new allocator
// available while it hands out nothing; an empty settle is the allocator's
// default, 15s.
func TestChartHoldsANewAllocatorUnavailableUntilItAllocates(t *testing.T) {
	helm := requireHelm(t)
	for settle, want := range map[string]string{"": "25", "15s": "25", "0s": "20", "1.5s": "20", "500ms": "20", "1m30.5s": "101", "2m": "130", "10s1ms": "21"} {
		docs := manifests(t, helm, "--set", "allocator.enabled=true", "--set-string", "allocator.settle="+settle)
		if got := text(path(manifest(t, docs, allocatorDeployment), "spec", "minReadySeconds")); got != want {
			t.Errorf("with settle %q the allocator's minReadySeconds is %q, want %s", settle, got, want)
		}
	}
}

// allocator.serviceMonitor renders a ServiceMonitor of the allocator's
// /metrics on its http port, selecting the allocator's Service and nothing of
// the exporter's, with the interval, timeout and labels given, and the token
// as Bearer credentials when one is set, since the allocator wants it on
// /metrics too. It is off by default, and needs no CRD check of its own, being
// asked for; with it the self-metrics monitor renders too, as beside any other
// monitor the chart renders.
func TestChartMonitorsTheAllocatorsOwnMetrics(t *testing.T) {
	helm := requireHelm(t)
	const name = "ServiceMonitor/test-prometheus-universal-exporter-allocator"
	if _, found := manifests(t, helm, "--set", "allocator.enabled=true")[name]; found {
		t.Error("the allocator's ServiceMonitor renders by default")
	}
	if _, found := manifests(t, helm, "--set", "allocator.serviceMonitor.enabled=true")[name]; found {
		t.Error("the allocator's ServiceMonitor renders without the allocator")
	}
	docs := manifests(t, helm, "--set", "allocator.enabled=true", "--set", "allocator.serviceMonitor.enabled=true",
		"--set", "allocator.serviceMonitor.interval=30s", "--set", "allocator.serviceMonitor.scrapeTimeout=10s", "--set", "allocator.serviceMonitor.labels.release=main")
	monitor := manifest(t, docs, name)
	selector := matchLabels(path(monitor, "spec", "selector", "matchLabels"))
	if !selects(selector, matchLabels(path(manifest(t, docs, "Service/test-prometheus-universal-exporter-allocator"), "metadata", "labels"))) {
		t.Error("the allocator's ServiceMonitor does not select the allocator's Service")
	}
	if selects(selector, matchLabels(path(manifest(t, docs, "Service/test-prometheus-universal-exporter"), "metadata", "labels"))) {
		t.Error("the allocator's ServiceMonitor selects the exporter's Service")
	}
	endpoints := entries(path(monitor, "spec", "endpoints"))
	if len(endpoints) != 1 || text(path(endpoints[0], "port")) != "http" || text(path(endpoints[0], "path")) != "/metrics" ||
		text(path(endpoints[0], "interval")) != "30s" || text(path(endpoints[0], "scrapeTimeout")) != "10s" || text(path(monitor, "metadata", "labels", "release")) != "main" {
		t.Errorf("the allocator's ServiceMonitor does not scrape /metrics on port http with the interval, timeout and labels given")
	}
	if len(endpoints) == 1 && text(path(endpoints[0], "authorization", "credentials", "name")) != "test-prometheus-universal-exporter-allocator-token" {
		t.Error("the allocator's ServiceMonitor does not present the token the chart generated")
	}
	if _, found := docs["ServiceMonitor/test-prometheus-universal-exporter-self"]; !found {
		t.Error("beside the allocator's ServiceMonitor the self-metrics monitor is left out")
	}

	docs = manifests(t, helm, "--set", "allocator.enabled=true", "--set", "allocator.serviceMonitor.enabled=true", "--set", "allocator.tokenSecret.name=allocator-token", "--set", "allocator.tokenSecret.key=shared")
	auth := path(manifest(t, docs, name), "spec", "endpoints", "0", "authorization")
	if text(path(auth, "type")) != "Bearer" || text(path(auth, "credentials", "name")) != "allocator-token" || text(path(auth, "credentials", "key")) != "shared" {
		t.Error("the allocator's ServiceMonitor does not present allocator.tokenSecret's token as Bearer credentials")
	}
}

// allocator.staticTargets.expandEnv renders --static-targets.expand-env on the
// exporter pods, which read the static target file of the allocator's bundle
// and expand it as their own; the allocator, which serves the document as
// written, never gets it. Without the value neither does.
func TestChartExpandsTheAllocatorsStaticTargetsOnTheExporterPods(t *testing.T) {
	helm := requireHelm(t)
	const flag = "--static-targets.expand-env"
	for _, expand := range []bool{false, true} {
		args := []string{"--set", "allocator.enabled=true", "-f", allocatorStaticTargets(t)}
		if expand {
			args = append(args, "--set", "allocator.staticTargets.expandEnv=true")
		}
		docs := manifests(t, helm, args...)
		exporter := scalars(path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter"), "spec", "template", "spec", "containers", "0", "args"))
		if slices.Contains(exporter, flag) != expand {
			t.Errorf("with allocator.staticTargets.expandEnv %v the exporter's args are %v", expand, exporter)
		}
		if got := scalars(path(manifest(t, docs, allocatorDeployment), "spec", "template", "spec", "containers", "0", "args")); slices.Contains(got, flag) {
			t.Errorf("the allocator is given %s: %v", flag, got)
		}
	}
}

// A probe job's keys are the allocator's: it decodes the probe targets file
// strictly, so the values schema and the templates take exactly the yaml keys
// of allocator.ProbeJob, and the schema requires what the allocator refuses
// to start without, the name, the collector and at least one target or one
// discovery source.
func TestChartProbeJobKeysAreTheAllocatorsKeys(t *testing.T) {
	var want []string
	job := reflect.TypeFor[allocator.ProbeJob]()
	for i := range job.NumField() {
		key, _, _ := strings.Cut(job.Field(i).Tag.Get("yaml"), ",")
		if key != "" && key != "-" {
			want = append(want, key)
		}
	}
	slices.Sort(want)
	if len(want) < 6 {
		t.Fatalf("read only %v from allocator.ProbeJob; the reader has stopped finding its keys", want)
	}

	items, _ := schemaAt(t, "allocator.probeJobs")["items"].(map[string]any)
	properties, _ := items["properties"].(map[string]any)
	if got := slices.Sorted(maps.Keys(properties)); !slices.Equal(got, want) {
		t.Errorf("values.schema.json's probe job takes the keys %v, but the allocator's ProbeJob decodes %v", got, want)
	}
	if items["additionalProperties"] != false {
		t.Error("values.schema.json's probe job takes keys of any name, which the allocator refuses")
	}
	var required []string
	if list, ok := items["required"].([]any); ok {
		for _, key := range list {
			required = append(required, key.(string))
		}
	}
	slices.Sort(required)
	if !slices.Equal(required, []string{"collector", "name"}) {
		t.Errorf("values.schema.json's probe job requires %v, want collector and name", required)
	}
	// And at least one target or one discovery source, either of which the
	// allocator takes.
	var either []string
	alternatives, _ := items["anyOf"].([]any)
	for _, alternative := range alternatives {
		a, _ := alternative.(map[string]any)
		list, _ := a["required"].([]any)
		props, _ := a["properties"].(map[string]any)
		if len(list) == 1 {
			key, _ := list[0].(string)
			if bound, _ := props[key].(map[string]any); bound["minItems"] == float64(1) {
				either = append(either, key)
			}
		}
	}
	slices.Sort(either)
	if !slices.Equal(either, []string{"discovery", "targets"}) {
		t.Errorf("values.schema.json's probe job needs one of %v not empty, want targets or discovery, without which the allocator refuses it", either)
	}

	match := regexp.MustCompile(`has \$key \(list((?: "[a-z_]+")+)\)`).FindStringSubmatch(readChartFile(t, "templates/_helpers.tpl"))
	if match == nil {
		t.Fatal("templates/_helpers.tpl no longer lists the keys a probe job takes")
	}
	got := strings.Fields(strings.ReplaceAll(match[1], `"`, ""))
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the templates take the probe job keys %v, but the allocator's ProbeJob decodes %v", got, want)
	}
}
