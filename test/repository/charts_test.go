package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A Helm template file may render more than one Kubernetes manifest: either
// because it declares several, or because it wraps one in a range over a list
// of values. Every manifest after the first needs its own `---`, and a template
// that omits it emits two manifests concatenated into a single YAML document.
// Nothing in `helm template` or `helm lint` notices — helm prints whatever the
// template produced — so the breakage only appears when someone applies the
// chart and Kubernetes rejects a document with two apiVersion keys. This test
// is the guard, because the chart's own CI steps cannot be.
//
// The rule checked here is deliberately narrow: a template that renders exactly
// one manifest, outside any range, needs no separator of its own, because helm
// already separates the files it renders.

// templateAction matches the block-forming actions of a Go template, in the
// order they appear, so the test can tell whether a line sits inside a range.
var templateAction = regexp.MustCompile(`\{\{-?\s*(if|range|with|define|block|end)\b`)

func chartTemplatePaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("charts/*/templates/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatal("no chart templates found; this test must not silently pass if they move")
	}
	return paths
}

// manifestStarts reports, for each line that begins a manifest, whether it sits
// inside a range and whether a document separator immediately precedes it.
type manifestStart struct {
	line      int
	inRange   bool
	separated bool
}

func manifestStarts(document string) []manifestStart {
	var stack []string
	var previous string
	var starts []manifestStart
	for index, line := range strings.Split(document, "\n") {
		if strings.HasPrefix(line, "apiVersion:") {
			inRange := false
			for _, open := range stack {
				if open == "range" {
					inRange = true
					break
				}
			}
			starts = append(starts, manifestStart{line: index + 1, inRange: inRange, separated: previous == "---"})
		}
		for _, action := range templateAction.FindAllStringSubmatch(line, -1) {
			if action[1] == "end" {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				continue
			}
			stack = append(stack, action[1])
		}
		if strings.TrimSpace(line) != "" {
			previous = strings.TrimSpace(line)
		}
	}
	return starts
}

func TestChartTemplatesSeparateEveryManifest(t *testing.T) {
	for _, path := range chartTemplatePaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			starts := manifestStarts(string(raw))
			if len(starts) == 0 {
				t.Fatalf("%s renders no manifest; it should not be a .yaml template", path)
			}
			for _, start := range starts {
				// One manifest, not repeated, is the whole file: helm puts the
				// separator between files itself.
				if len(starts) == 1 && !start.inRange {
					continue
				}
				if !start.separated {
					t.Errorf("%s:%d: this manifest is not preceded by a `---`, so it merges into the one before it",
						path, start.line)
				}
			}
		})
	}
}

// The self-metrics monitor is appended after the loop over `monitors`, and it
// only renders when a monitor of the same kind was already rendered — so
// without a separator of its own it always collided with one. Pinning it here
// keeps the fix from being undone by an edit to the surrounding block.
func TestSelfMetricsMonitorStartsItsOwnDocument(t *testing.T) {
	for _, name := range []string{"servicemonitor.yaml", "podmonitor.yaml"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("charts/prometheus-universal-exporter/templates/" + name)
			if err != nil {
				t.Fatal(err)
			}
			document := string(raw)
			guard := "{{- if and .Values.selfMetrics.enabled $has"
			index := strings.Index(document, guard)
			if index < 0 {
				t.Fatalf("%s no longer guards the self-metrics monitor as expected", name)
			}
			rest := document[index:]
			end := strings.Index(rest, "apiVersion:")
			if end < 0 {
				t.Fatalf("%s: the self-metrics guard is not followed by a manifest", name)
			}
			if !strings.Contains(rest[:end], "\n---\n") {
				t.Errorf("%s: the self-metrics monitor must open its own document, or it merges into the last monitor", name)
			}
		})
	}
}

// The chart renders some flags itself and rejects an extraArgs entry that
// repeats one of them, because Go's flag package keeps the last occurrence: the
// entry would win silently, and for --web.listen-address the container port and
// the probes would still follow server.listenAddress, leaving a pod that
// listens on one port while Kubernetes checks another. That guard is a list,
// and a list is only as good as the thing keeping it in step with the template
// beside it — so adding a flag to the Deployment without adding it to the guard
// has to fail here rather than in somebody's cluster.
func TestEveryRenderedFlagIsGuardedAgainstExtraArgs(t *testing.T) {
	deployment := readChartFile(t, "templates/deployment.yaml")
	helpers := readChartFile(t, "templates/_helpers.tpl")

	guarded := map[string]bool{}
	define := "{{- define \"prometheus-universal-exporter.extraArgs\" -}}"
	start := strings.Index(helpers, define)
	if start < 0 {
		t.Fatal("the extraArgs guard is gone or renamed; nothing stops a values entry from overriding a chart flag")
	}
	block := helpers[start:]
	if end := strings.Index(block, "{{- end }}\n{{- define"); end > 0 {
		block = block[:end]
	}
	for _, match := range flagReference.FindAllString(block, -1) {
		guarded[match] = true
	}
	if len(guarded) == 0 {
		t.Fatal("the extraArgs guard lists no flags; it has been renamed or emptied")
	}

	rendered := map[string]bool{}
	for _, line := range argLines(deployment) {
		for _, match := range flagReference.FindAllString(line, -1) {
			rendered[match] = true
		}
	}
	if len(rendered) == 0 {
		t.Fatal("the Deployment renders no flags; the args block has moved")
	}
	for flag := range rendered {
		if !guarded[flag] {
			t.Errorf("the Deployment renders %s but extraArgs does not reject it, so an entry repeating it would silently win", flag)
		}
	}
}

var flagReference = regexp.MustCompile(`--[a-z][a-z0-9.-]*`)

// argLines returns the body of the container's args block: the lines indented
// further than the key itself, up to whatever comes next.
func argLines(template string) []string {
	lines := strings.Split(template, "\n")
	start := -1
	indent := 0
	for index, line := range lines {
		if strings.TrimSpace(line) == "args:" {
			start = index + 1
			indent = len(line) - len(strings.TrimLeft(line, " "))
			break
		}
	}
	if start < 0 {
		return nil
	}
	var body []string
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line)-len(strings.TrimLeft(line, " ")) <= indent {
			break
		}
		body = append(body, line)
	}
	return body
}

// The three values have to exist and to default to empty, so a chart nobody
// asked for extras from renders exactly what it rendered before.
func TestExtraValuesDefaultToEmpty(t *testing.T) {
	values := readChartFile(t, "values.yaml")
	for _, key := range []string{"extraArgs", "extraVolumes", "extraVolumeMounts"} {
		if !strings.Contains(values, "\n"+key+": []\n") {
			t.Errorf("values.yaml must declare %s defaulting to []", key)
		}
	}
}

func readChartFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("charts/prometheus-universal-exporter/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Every flag the exporter has is either rendered by the chart from a value, or
// a one-shot flag the chart refuses in extraArgs because it prints something
// and exits. A flag added to the exporter without either fails here, rather
// than leaving chart users to discover it and pass it through extraArgs.
func TestEveryExporterFlagIsHandledByTheChart(t *testing.T) {
	help := helpText(t)
	var flags []string
	for _, match := range regexp.MustCompile(`(?m)^  -([a-z.-]+)`).FindAllStringSubmatch(help, -1) {
		flags = append(flags, "--"+match[1])
	}
	if len(flags) == 0 {
		t.Fatal("the exporter lists no flags")
	}
	rendered := map[string]bool{}
	for _, line := range argLines(readChartFile(t, "templates/deployment.yaml")) {
		for _, match := range flagReference.FindAllString(line, -1) {
			rendered[match] = true
		}
	}
	oneShot := map[string]bool{}
	helpers := readChartFile(t, "templates/_helpers.tpl")
	start := strings.Index(helpers, "{{- $oneShot := dict")
	if start < 0 {
		t.Fatal("the extraArgs guard no longer lists the one-shot flags")
	}
	block, _, _ := strings.Cut(helpers[start:], "-}}")
	for _, match := range regexp.MustCompile(`"(--[a-z][a-z0-9.-]*)"`).FindAllStringSubmatch(block, -1) {
		oneShot[match[1]] = true
	}
	known := map[string]bool{"--help": true, "--h": true}
	for _, flag := range flags {
		known[flag] = true
		switch {
		case rendered[flag] && oneShot[flag]:
			t.Errorf("%s is both rendered by the chart and refused as a one-shot flag", flag)
		case !rendered[flag] && !oneShot[flag]:
			t.Errorf("the exporter has %s, but the chart neither renders it from a value nor refuses it in extraArgs", flag)
		}
	}
	for flag := range oneShot {
		if !known[flag] {
			t.Errorf("the chart refuses %s, which the exporter does not have", flag)
		}
	}
}

// The values behind the flags added for them default to what the exporter
// does without them.
func TestServerFlagValuesDefaults(t *testing.T) {
	values := readChartFile(t, "values.yaml")
	for _, line := range []string{"\n  logLevel: info\n", "\n  probeTimeoutOffset: \"\"\n", "\n  probeDefaultTimeout: \"\"\n", "\n  probeMaxConcurrent: \"\"\n", "\n  pythonMaxWorkers: \"\"\n"} {
		if !strings.Contains(values, line) {
			t.Errorf("values.yaml lacks %q", strings.TrimSpace(line))
		}
	}
	deployment := readChartFile(t, "templates/deployment.yaml")
	// Unset, --probe.timeout-offset is left out, so an image that predates the
	// flag still starts.
	if !strings.Contains(deployment, `{{- with (include "prometheus-universal-exporter.probeTimeoutOffset" .) }}`) {
		t.Error("--probe.timeout-offset must only be rendered when server.probeTimeoutOffset is set")
	}
	if !strings.Contains(deployment, `{{- with (include "prometheus-universal-exporter.probeDefaultTimeout" .) }}`) {
		t.Error("--probe.default-timeout must only be rendered when server.probeDefaultTimeout is set")
	}
}

// GOMEMLIMIT follows the container's memory limit through the downward API,
// and only when there is one and env does not set it.
func TestTheChartSetsGOMEMLIMITFromTheMemoryLimit(t *testing.T) {
	deployment := readChartFile(t, "templates/deployment.yaml")
	for _, want := range []string{
		`.Values.goMemLimit.enabled`,
		`dig "limits" "memory" ""`,
		`"prometheus-universal-exporter.envSets" (list .Values.env "GOMEMLIMIT")`,
		"- name: GOMEMLIMIT\n              valueFrom:\n                resourceFieldRef:\n                  containerName: exporter\n                  resource: limits.memory",
	} {
		if !strings.Contains(deployment, want) {
			t.Errorf("the Deployment lacks %q", want)
		}
	}
	if !strings.Contains(readChartFile(t, "values.yaml"), "\ngoMemLimit:\n  enabled: true\n") {
		t.Error("goMemLimit.enabled must default to true")
	}
}

// The probes' timings come from values, with some slack for liveness, and
// the check itself stays the chart's.
func TestTheProbesTimingsAreValues(t *testing.T) {
	values := readChartFile(t, "values.yaml")
	for _, want := range []string{
		"\nlivenessProbe:\n  periodSeconds: 10\n  timeoutSeconds: 3\n  failureThreshold: 5\n",
		"\nreadinessProbe:\n  periodSeconds: 10\n  timeoutSeconds: 3\n  failureThreshold: 3\n",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml lacks %q", want)
		}
	}
	deployment := readChartFile(t, "templates/deployment.yaml")
	for _, want := range []string{
		"httpGet: {path: /health, port: http}\n            {{- with .Values.livenessProbe }}",
		"httpGet: {path: /ready, port: http}\n            {{- with .Values.readinessProbe }}",
		`"prometheus-universal-exporter.validateProbe" (list "livenessProbe" .Values.livenessProbe)`,
		`"prometheus-universal-exporter.validateProbe" (list "readinessProbe" .Values.readinessProbe)`,
	} {
		if !strings.Contains(deployment, want) {
			t.Errorf("the Deployment lacks %q", want)
		}
	}
}

// The PodDisruptionBudget is optional, off by default, selects the
// Deployment's pods and takes one of minAvailable and maxUnavailable.
func TestThePodDisruptionBudget(t *testing.T) {
	pdb := readChartFile(t, "templates/poddisruptionbudget.yaml")
	deployment := readChartFile(t, "templates/deployment.yaml")
	for _, want := range []string{
		"{{- if .Values.podDisruptionBudget.enabled }}",
		"apiVersion: policy/v1\nkind: PodDisruptionBudget",
		"sets both minAvailable and maxUnavailable",
		"maxUnavailable: {{ $pdb.maxUnavailable | default 1 }}",
		"app.kubernetes.io/name: {{ include \"prometheus-universal-exporter.name\" . }}\n      app.kubernetes.io/instance: {{ .Release.Name }}",
	} {
		if !strings.Contains(pdb, want) {
			t.Errorf("the PodDisruptionBudget lacks %q", want)
		}
	}
	if !strings.Contains(deployment, "app.kubernetes.io/name: {{ include \"prometheus-universal-exporter.name\" . }}\n      app.kubernetes.io/instance: {{ .Release.Name }}") {
		t.Error("the Deployment's selector changed; the PodDisruptionBudget must select the same pods")
	}
	if !strings.Contains(readChartFile(t, "values.yaml"), "\npodDisruptionBudget:\n  enabled: false\n") {
		t.Error("podDisruptionBudget must be disabled by default")
	}
}

// Every replica scrapes every static target, which the chart says in its
// notes when it renders more than one or autoscales, naming the targets
// exported over OTLP.
func TestTheChartWarnsAboutReplicatedStaticTargets(t *testing.T) {
	notes := readChartFile(t, "templates/NOTES.txt")
	for _, want := range []string{
		"{{- if or (gt $replicas 1) .Values.autoscaling.enabled }}",
		"{{- $replicas = int .Values.autoscaling.maxReplicas }}",
		`(get . "export_via_otlp")`,
		"OTLP once per replica, as duplicate series",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("NOTES.txt lacks %q", want)
		}
	}
}

// The HorizontalPodAutoscaler is optional, scales the Deployment, and then
// owns its replica count.
func TestTheHorizontalPodAutoscaler(t *testing.T) {
	hpa := readChartFile(t, "templates/horizontalpodautoscaler.yaml")
	for _, want := range []string{
		"{{- if .Values.autoscaling.enabled }}",
		"apiVersion: autoscaling/v2\nkind: HorizontalPodAutoscaler",
		"kind: Deployment\n    name: {{ include \"prometheus-universal-exporter.fullname\" . }}",
		"is below autoscaling.minReplicas",
		"needs something to scale on",
	} {
		if !strings.Contains(hpa, want) {
			t.Errorf("the HorizontalPodAutoscaler lacks %q", want)
		}
	}
	if !strings.Contains(readChartFile(t, "templates/deployment.yaml"), "{{- if not .Values.autoscaling.enabled }}") {
		t.Error("the Deployment renders replicas under autoscaling")
	}
	if !strings.Contains(readChartFile(t, "values.yaml"), "\nautoscaling:\n  enabled: false\n") {
		t.Error("autoscaling must be disabled by default")
	}
}
