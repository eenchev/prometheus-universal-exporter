package repository

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The chart's options, rendered with helm: what each renders, and each value
// that must fail rendering does, with its message. CI installs helm for any
// change to the chart; without helm on the PATH, as on a developer machine
// without it, these tests are skipped and the text tests in charts_test.go
// still run.

func requireHelm(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on the PATH")
	}
	return helm
}

// helmTemplate renders the chart at dir with args, and returns what helm
// printed and whether it succeeded.
func helmTemplate(t *testing.T, helm, dir string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(helm, append([]string{"template", "test", dir}, args...)...) // #nosec G204 -- the test's own arguments
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err == nil
}

// staticTargetsValues writes a values file with static targets, one of them
// exported over OTLP, merged from an x- anchor.
func staticTargetsValues(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	values := `staticTargets:
  enabled: true
  monitor:
    enabled: false
  data: |
    x-shared: &shared
      collector: example
      target: http://x
    interval: 1m
    targets:
      - <<: *shared
        name: exported
        export_via_otlp: true
      - <<: *shared
        name: kept
config:
  data:
    config.yaml: |
      otlp:
        enabled: true
        endpoint: http://otel:4318/v1/metrics
      collectors:
        - name: example
          request: {type: http}
          transform: {type: regex}
          metrics:
            - name: v
              expression: 'v=(\d+)'
` + extra
	if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChartRendersItsOptions(t *testing.T) {
	helm := requireHelm(t)
	for name, tc := range map[string]struct {
		args        []string
		want, avoid []string
	}{
		"defaults": {
			want:  []string{"replicas: 1\n", "runAsUser: 65532", `"--runtime.memory-limit-ratio=0.8"`, "timeoutSeconds: 3", "failureThreshold: 5", "failureThreshold: 3"},
			avoid: []string{"kind: HorizontalPodAutoscaler", "kind: PodDisruptionBudget", "--probe.max-concurrent", "--python.max-workers", "GOMEMLIMIT", "terminationGracePeriodSeconds", "sessionAffinity", "--web.enable-probe-debug", "kind: ServiceMonitor", "kind: NetworkPolicy"},
		},
		"limits": {
			args: []string{"--set", "server.probeMaxConcurrent=64", "--set", "server.pythonMaxWorkers=8", "--set", "goMemLimit.ratio=0.6"},
			want: []string{`"--probe.max-concurrent=64"`, `"--python.max-workers=8"`, `"--runtime.memory-limit-ratio=0.6"`},
		},
		"no memory limit ratio": {
			args:  []string{"--set", "goMemLimit.enabled=false"},
			avoid: []string{"--runtime.memory-limit-ratio"},
		},
		"probe timings": {
			args: []string{"--set", "livenessProbe.timeoutSeconds=7", "--set", "readinessProbe.periodSeconds=4"},
			want: []string{"timeoutSeconds: 7", "periodSeconds: 4", "path: /health", "path: /ready"},
		},
		"disruption budget": {
			args: []string{"--set", "podDisruptionBudget.enabled=true"},
			want: []string{"kind: PodDisruptionBudget", "maxUnavailable: 1\n", "app.kubernetes.io/instance: test"},
		},
		"disruption budget by availability": {
			args:  []string{"--set", "podDisruptionBudget.enabled=true", "--set", "podDisruptionBudget.minAvailable=50%"},
			want:  []string{"minAvailable: 50%"},
			avoid: []string{"maxUnavailable: 1\n"},
		},
		"autoscaling": {
			args:  []string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.maxReplicas=6", "--set", "autoscaling.targetMemoryUtilizationPercentage=70"},
			want:  []string{"kind: HorizontalPodAutoscaler", "maxReplicas: 6", "averageUtilization: 80", "averageUtilization: 70", "kind: Deployment\n    name: test-prometheus-universal-exporter\n"},
			avoid: []string{"replicas: 1\n"},
		},
		"network policy alone": {
			args:  []string{"--set", "networkPolicy.enabled=true"},
			want:  []string{"kind: NetworkPolicy", "policyTypes:\n    - Ingress\n  ingress:\n    - ports:\n        - port: http\n          protocol: TCP"},
			avoid: []string{"- Egress", "port: 53"},
		},
		"network policy with egress rules": {
			args: []string{"--set", "networkPolicy.enabled=true", "--set-json", `networkPolicy.egress=[{"to":[{"ipBlock":{"cidr":"10.0.0.0/8"}}]}]`},
			want: []string{"- Egress", "cidr: 10.0.0.0/8", "- port: 53\n          protocol: UDP", "- port: 53\n          protocol: TCP"},
		},
		"network policy with egress rules and no DNS": {
			args:  []string{"--set", "networkPolicy.enabled=true", "--set", "networkPolicy.allowDNS=false", "--set-json", `networkPolicy.egress=[{"to":[{"ipBlock":{"cidr":"10.0.0.0/8"}}]}]`},
			want:  []string{"- Egress"},
			avoid: []string{"port: 53"},
		},
		"a probe monitor, and the self monitor with it": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"}]`},
			want: []string{"replacement: test-prometheus-universal-exporter.default.svc:8080", "kind: ServiceMonitor\nmetadata:\n  name: test-prometheus-universal-exporter-self"},
		},
		"the self monitor as a pod monitor": {
			args:  []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"}]`, "--set", "selfMetrics.type=pod"},
			want:  []string{"kind: PodMonitor\nmetadata:\n  name: test-prometheus-universal-exporter-self"},
			avoid: []string{"kind: ServiceMonitor\nmetadata:\n  name: test-prometheus-universal-exporter-self"},
		},
		"the self monitor on a cluster that serves it": {
			args: []string{"--api-versions", "monitoring.coreos.com/v1/ServiceMonitor"},
			want: []string{"name: test-prometheus-universal-exporter-self"},
		},
		"a namespace override in the monitors' address": {
			args: []string{"--set", "namespaceOverride=monitoring", "--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example"}]`},
			want: []string{"replacement: test-prometheus-universal-exporter.monitoring.svc:8080"},
		},
		"a compound watch interval": {
			args: []string{"--set", "server.watchConfig=true", "--set", "server.watchConfigInterval=1m30s"},
			want: []string{`"--config.watch-interval=1m30s"`},
		},
		"a disruption budget of no eviction": {
			args: []string{"--set", "podDisruptionBudget.enabled=true", "--set", "podDisruptionBudget.maxUnavailable=0"},
			want: []string{"maxUnavailable: 0\n"},
		},
		"no token mounted on the default account": {
			args: []string{"--set", "serviceAccount.create=false"},
			want: []string{"serviceAccountName: default\n      automountServiceAccountToken: false"},
		},
		"probe debug": {
			args: []string{"--set", "server.probeDebug=true"},
			want: []string{"- --web.enable-probe-debug\n"},
		},
		"session affinity": {
			args: []string{"--set", "service.sessionAffinity=ClientIP"},
			want: []string{"sessionAffinity: ClientIP"},
		},
		"pod scheduling and metadata": {
			args: []string{"--set", "priorityClassName=monitoring",
				"--set", "topologySpreadConstraints[0].maxSkew=1", "--set", "topologySpreadConstraints[0].topologyKey=topology.kubernetes.io/zone", "--set", "topologySpreadConstraints[0].whenUnsatisfiable=ScheduleAnyway",
				"--set", "topologySpreadConstraints[1].maxSkew=2", "--set", "topologySpreadConstraints[1].topologyKey=kubernetes.io/hostname", "--set", "topologySpreadConstraints[1].whenUnsatisfiable=DoNotSchedule", "--set", "topologySpreadConstraints[1].labelSelector.matchLabels.own=yes",
				"--set", "podLabels.team=obs", "--set", "podAnnotations.note=pod", "--set", "podAnnotations.checksum/config=mine"},
			want: []string{`priorityClassName: "monitoring"`, "topologyKey: topology.kubernetes.io/zone", "- labelSelector:\n            matchLabels:\n              app.kubernetes.io/instance: test\n              app.kubernetes.io/name: prometheus-universal-exporter\n          maxSkew: 1",
				"matchLabels:\n              own: \"yes\"\n          maxSkew: 2", "team: obs", "note: pod"},
			avoid: []string{"checksum/config: mine"},
		},
		"a longer shutdown raises the grace period": {
			args: []string{"--set", "server.shutdownTimeout=1m"},
			want: []string{"terminationGracePeriodSeconds: 75"},
		},
		"an IPv6 wildcard listen address": {
			args: []string{"--set", "server.listenAddress=[::]:9115"},
			want: []string{`"--web.listen-address=[::]:9115"`, "containerPort: 9115"},
		},
		// Only the readiness probe refuses a grace period of its own.
		"a liveness probe's grace period": {
			args: []string{"--set", "livenessProbe.terminationGracePeriodSeconds=10"},
			want: []string{"terminationGracePeriodSeconds: 10"},
		},
		"Prometheus durations on the monitors": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1m30s","scrapeTimeout":"1500ms"}]`, "--set", "selfMetrics.interval=1d"},
			want: []string{"interval: 1m30s", "scrapeTimeout: 1500ms", "interval: 1d"},
		},
		// The delay counts too: 20s, the default 15s timeout and 10s more.
		"a longer shutdown delay raises the grace period": {
			args: []string{"--set", "server.shutdownDelay=20s"},
			want: []string{`"--web.shutdown-delay=20s"`, "terminationGracePeriodSeconds: 45"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, tc.args...)
			if !ok {
				t.Fatalf("rendering failed:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in the rendered chart", want)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(out, avoid) {
					t.Errorf("%q is rendered", avoid)
				}
			}
		})
	}
}

func TestChartRefusesInvalidOptions(t *testing.T) {
	helm := requireHelm(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"negative probe limit":                 {[]string{"--set", "server.probeMaxConcurrent=-1"}, "probeMaxConcurrent"},
		"fractional worker limit":              {[]string{"--set", "server.pythonMaxWorkers=1.5"}, "pythonMaxWorkers"},
		"memory ratio above 1":                 {[]string{"--set", "goMemLimit.ratio=1.5"}, "ratio"},
		"memory ratio of 0":                    {[]string{"--set", "goMemLimit.ratio=0"}, "ratio"},
		"a probe's own check":                  {[]string{"--set", "livenessProbe.httpGet.path=/x"}, "livenessProbe"},
		"both disruption budgets":              {[]string{"--set", "podDisruptionBudget.enabled=true", "--set", "podDisruptionBudget.minAvailable=1", "--set", "podDisruptionBudget.maxUnavailable=1"}, "sets both minAvailable and maxUnavailable"},
		"fewer maximum than minimum":           {[]string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.minReplicas=5"}, "is below autoscaling.minReplicas"},
		"autoscaling on nothing":               {[]string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.targetCPUUtilizationPercentage=null"}, "needs something to scale on"},
		"a flag the chart manages":             {[]string{"--set", "extraArgs[0]=--probe.max-concurrent=3"}, "server.probeMaxConcurrent"},
		"the memory ratio as an extra":         {[]string{"--set", "extraArgs[0]=--runtime.memory-limit-ratio=0.5"}, "goMemLimit.ratio"},
		"a probe monitor without the Service":  {[]string{"--set", "service.enabled=false", "--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example"}]`}, "service.enabled=false leaves out"},
		"a monitor without a collector":        {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service"}]`}, "names no collector"},
		"a monitor of an unknown collector":    {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"nope"}]`}, `names collector "nope", which config.data does not define`},
		"the self monitor without the Service": {[]string{"--set", "service.enabled=false", "--api-versions", "monitoring.coreos.com/v1/ServiceMonitor"}, "selfMetrics.type service"},
		"a zero watch interval":                {[]string{"--set", "server.watchConfig=true", "--set", "server.watchConfigInterval=0s"}, "watchConfigInterval"},
		"a zero compound watch interval":       {[]string{"--set", "server.watchConfig=true", "--set", "server.watchConfigInterval=0m0s"}, "watchConfigInterval"},
		"probe debug as an extra":              {[]string{"--set", "extraArgs[0]=--web.enable-probe-debug"}, "server.probeDebug"},
		"a pod label the chart sets":           {[]string{"--set", "podLabels.app\\.kubernetes\\.io/name=x"}, "podLabels sets app.kubernetes.io/name"},
		"a spread without a topology key":      {[]string{"--set", "topologySpreadConstraints[0].maxSkew=1"}, "topologyKey"},
		// The chart renders the collector and target parameters itself; a
		// params entry of either would be a second key of the same name.
		"a monitor's params.collector": {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","params":{"collector":["other"]}}]`}, "sets params.collector; the chart renders the collector parameter itself, so set the entry's .collector instead"},
		"a monitor's params.target":    {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example","params":{"target":["http://x"]}}]`}, "sets params.target"},
		// Neither the kubelet's probes nor the Service reach a loopback host.
		"a loopback IPv4 listen address": {[]string{"--set", "server.listenAddress=127.0.0.1:8080"}, "listens on a loopback address"},
		"a loopback name listen address": {[]string{"--set", "server.listenAddress=localhost:8080"}, "listens on a loopback address"},
		"a loopback IPv6 listen address": {[]string{"--set", "server.listenAddress=[::1]:8080"}, "listens on a loopback address"},
		// A monitor is named <fullname>-<name>: a DNS-1123 label, of its own.
		"an upper-case monitor name":                   {[]string{"--set-json", `monitors=[{"name":"Apps","enabled":true,"type":"service","collector":"example"}]`}, "monitors.0.name"},
		"a monitor name with an underscore":            {[]string{"--set-json", `monitors=[{"name":"my_apps","enabled":true,"type":"service","collector":"example"}]`}, "monitors.0.name"},
		"two monitors of one name":                     {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"},{"name":"apps","enabled":true,"type":"pod","collector":"example"}]`}, `both named "apps"`},
		"a monitor named after the self one":           {[]string{"--set-json", `monitors=[{"name":"self","enabled":true,"type":"service","collector":"example"}]`}, "the self-metrics monitor"},
		"a monitor named after the static targets one": {[]string{"--set-json", `monitors=[{"name":"static-targets","enabled":true,"type":"pod","collector":"example"}]`}, "the static targets monitor"},
		// A Service of type ExternalName has no endpoints to probe through.
		"an ExternalName Service": {[]string{"--set", "service.type=ExternalName"}, "service.type"},
		// Kubernetes refuses terminationGracePeriodSeconds on a readiness probe.
		"a readiness probe's grace period": {[]string{"--set", "readinessProbe.terminationGracePeriodSeconds=10"}, "terminationGracePeriodSeconds"},
		// The Prometheus Operator takes Prometheus durations: whole numbers,
		// down to milliseconds.
		"a fractional monitor interval":              {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1.5m"}]`}, "monitors.0.interval"},
		"a monitor scrape timeout in microseconds":   {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","scrapeTimeout":"500us"}]`}, "monitors.0.scrapeTimeout"},
		"a self monitor interval in nanoseconds":     {[]string{"--set", "selfMetrics.interval=30000000000ns"}, "selfMetrics.interval"},
		"a fractional static targets scrape timeout": {[]string{"--set", "staticTargets.monitor.scrapeTimeout=0.5s"}, "staticTargets.monitor.scrapeTimeout"},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, tc.args...)
			if ok || !namesInHelmError(out, tc.want) {
				t.Fatalf("ok=%v, want an error naming %q:\n%s", ok, tc.want, out)
			}
		})
	}
}

// namesInHelmError reports whether helm's error out names want. Helm writes
// the path of a value the schema refuses as monitors.0.name up to 3.16 and as
// '/monitors/0/name' since, so a dotted path is also looked for in that form.
func namesInHelmError(out, want string) bool {
	if strings.Contains(out, want) {
		return true
	}
	return !strings.ContainsAny(want, " /") && strings.Contains(out, "'/"+strings.ReplaceAll(want, ".", "/")+"'")
}

// Both ways helm has reported a value the schema refuses: up to 3.16, and
// since (these lines are from helm 3.22 in CI).
func TestNamesInHelmError(t *testing.T) {
	for _, tc := range []struct {
		out, want string
		named     bool
	}{
		{"- monitors.0.name: Does not match pattern", "monitors.0.name", true},
		{"- at '/monitors/0/name': 'Apps' does not match pattern", "monitors.0.name", true},
		{"- at '/staticTargets/monitor/scrapeTimeout': '0.5s' does not match pattern", "staticTargets.monitor.scrapeTimeout", true},
		{"- at '/service/type': value must be one of 'ClusterIP', 'NodePort', 'LoadBalancer'", "service.type", true},
		{"- at '/readinessProbe': additional properties 'terminationGracePeriodSeconds' not allowed", "terminationGracePeriodSeconds", true},
		{"- at '/monitors/0/interval': '1.5m' does not match pattern", "monitors.0.name", false},
		{"- at '/monitors/0/nameX': ...", "monitors.0.name", false},
	} {
		if got := namesInHelmError(tc.out, tc.want); got != tc.named {
			t.Errorf("namesInHelmError(%q, %q) = %v, want %v", tc.out, tc.want, got, tc.named)
		}
	}
}

// The notes warn when static targets are rendered with more than one replica
// or with autoscaling, naming the targets exported over OTLP. helm template
// does not render NOTES.txt, so a copy of the chart renders it as a manifest.
func TestChartNotesWarnAboutReplicatedStaticTargets(t *testing.T) {
	helm := requireHelm(t)
	dir := filepath.Join(t.TempDir(), "chart")
	if err := os.CopyFS(dir, os.DirFS(chartDir)); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "templates", "NOTES.txt")); err != nil {
		t.Fatal(err)
	}
	wrapped := "{{- define \"test.notes\" -}}\n" + string(notes) + "\n{{- end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "_notes.tpl"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := "kind: Notes\nnotes: {{ include \"test.notes\" . | toJson }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "notes.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	render := func(extra string) string {
		out, ok := helmTemplate(t, helm, dir, "-f", staticTargetsValues(t, extra), "--show-only", "templates/notes.yaml")
		if !ok {
			t.Fatalf("rendering failed:\n%s", out)
		}
		return out
	}
	if out := render(""); strings.Contains(out, "WARNING") {
		t.Fatalf("one replica is warned about:\n%s", out)
	}
	for extra, want := range map[string]string{
		"replicaCount: 2\n": "replicaCount 2",
		"autoscaling:\n  enabled: true\n  maxReplicas: 4\n": "autoscaling up to 4 replicas",
	} {
		out := render(extra)
		for _, fragment := range []string{"WARNING", want, "export_via_otlp (exported)", "duplicate series"} {
			if !strings.Contains(out, fragment) {
				t.Errorf("%q: no %q in:\n%s", extra, fragment, out)
			}
		}
		if strings.Contains(out, "kept") {
			t.Errorf("a target not exported over OTLP is named:\n%s", out)
		}
	}
}

// The release's objects are named after the release, so two releases in one
// namespace do not collide: <release>-<chart>, the release name alone when
// it holds the chart's, and fullnameOverride outright.
func TestChartNamesFollowTheRelease(t *testing.T) {
	helm := requireHelm(t)
	for release, want := range map[string]string{
		"blue":                               "name: blue-prometheus-universal-exporter\n",
		"green":                              "name: green-prometheus-universal-exporter\n",
		"prometheus-universal-exporter":      "name: prometheus-universal-exporter\n",
		"prod-prometheus-universal-exporter": "name: prod-prometheus-universal-exporter\n",
	} {
		cmd := exec.Command(helm, "template", release, chartDir, "--show-only", "templates/deployment.yaml") // #nosec G204 -- the test's own arguments
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("release %s: want %q in\n%s (%v)", release, want, out, err)
		}
	}
	out, ok := helmTemplate(t, helm, chartDir, "--set", "fullnameOverride=exporter", "--show-only", "templates/deployment.yaml")
	if !ok || !strings.Contains(out, "name: exporter\n") {
		t.Errorf("fullnameOverride: %s", out)
	}
	// A static-targets-only release gets its self monitor too.
	out, ok = helmTemplate(t, helm, chartDir, "-f", staticTargetsValues(t, ""), "--set", "staticTargets.monitor.enabled=true")
	if !ok || !strings.Contains(out, "name: test-prometheus-universal-exporter-self") {
		t.Errorf("a static-targets-only release has no self monitor:\n%s", out)
	}
}

// The notes warn about a disruption budget that allows no eviction.
// A chart version deploys the exporter release it was validated against:
// image.tag defaults to empty, which renders Chart.yaml's appVersion, never a
// moving tag such as latest. A tag set in the values wins.
func TestChartImageDefaultsToTheAppVersion(t *testing.T) {
	helm := requireHelm(t)
	chart := readChartMetadata(t)
	dir := chartDir
	out, ok := helmTemplate(t, helm, dir)
	if !ok {
		t.Fatalf("render failed:\n%s", out)
	}
	if want := `image: "ghcr.io/eenchev/prometheus-universal-exporter:` + chart.AppVersion + `"`; !strings.Contains(out, want) {
		t.Errorf("the default image is not the appVersion; want %s", want)
	}
	out, ok = helmTemplate(t, helm, dir, "--set", "image.tag=9.9.9")
	if !ok || !strings.Contains(out, `image: "ghcr.io/eenchev/prometheus-universal-exporter:9.9.9"`) {
		t.Errorf("image.tag=9.9.9 was not rendered as given:\n%s", out)
	}
}

func TestChartNotesWarnAboutABudgetOfNoEviction(t *testing.T) {
	helm := requireHelm(t)
	dir := filepath.Join(t.TempDir(), "chart")
	if err := os.CopyFS(dir, os.DirFS(chartDir)); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "templates", "NOTES.txt")); err != nil {
		t.Fatal(err)
	}
	wrapped := "{{- define \"test.notes\" -}}\n" + string(notes) + "\n{{- end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "_notes.tpl"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "notes.yaml"), []byte("kind: Notes\nnotes: {{ include \"test.notes\" . | toJson }}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for value, warned := range map[string]bool{"0": true, "0%": true, "1": false} {
		out, ok := helmTemplate(t, helm, dir, "--set", "podDisruptionBudget.enabled=true", "--set-string", "podDisruptionBudget.maxUnavailable="+value, "--show-only", "templates/notes.yaml")
		if !ok {
			t.Fatalf("rendering failed:\n%s", out)
		}
		if strings.Contains(out, "allows no voluntary eviction") != warned {
			t.Errorf("maxUnavailable %s: warned %v:\n%s", value, !warned, out)
		}
	}
}

// renderedDocuments renders the chart with args and parses every YAML
// document it prints, failing the test when rendering or parsing fails.
func renderedDocuments(t *testing.T, helm string, args ...string) []*yaml.Node {
	t.Helper()
	out, ok := helmTemplate(t, helm, chartDir, args...)
	if !ok {
		t.Fatalf("rendering failed:\n%s", out)
	}
	var docs []*yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(out))
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("the rendered chart does not parse: %v\n%s", err, out)
		}
		if len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
			docs = append(docs, doc.Content[0])
		}
	}
}

// child returns the value of key in the mapping node, or nil.
func child(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// path follows keys, and list indexes written as numbers, from node.
func path(node *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		if node != nil && node.Kind == yaml.SequenceNode {
			index, err := strconv.Atoi(key)
			if err != nil || index >= len(node.Content) {
				return nil
			}
			node = node.Content[index]
			continue
		}
		node = child(node, key)
	}
	return node
}

// colonKeys lists the mapping keys under node holding a colon: what a
// template renders when a trimmed newline runs two keys together, as
// "selector:matchLabels:" parses as one key of that name.
func colonKeys(node *yaml.Node) []string {
	var found []string
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if strings.Contains(node.Content[i].Value, ":") {
				found = append(found, node.Content[i].Value)
			}
		}
	}
	for _, c := range node.Content {
		found = append(found, colonKeys(c)...)
	}
	return found
}

// Every monitor the chart renders is checked as the Prometheus Operator would
// read it, parsed rather than searched for text: a spec.selector that is a
// non-empty mapping, and no key that is two keys run together. A text search
// for "selector:" passes on "selector:matchLabels:", which parses as neither.
func TestChartMonitorsParseWithASelector(t *testing.T) {
	helm := requireHelm(t)
	targetsFile := staticTargetsValues(t, "")
	for name, tc := range map[string]struct {
		args []string
		// The monitors that must be rendered, by name, with their kind.
		want map[string]string
		// The labels a probe monitor's selector must match, by monitor name.
		selects map[string]map[string]string
	}{
		"service and pod monitors without a target selector": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"},{"name":"pods","enabled":true,"type":"pod","collector":"example"}]`},
			want: map[string]string{"test-prometheus-universal-exporter-apps": "ServiceMonitor", "test-prometheus-universal-exporter-pods": "PodMonitor", "test-prometheus-universal-exporter-self": "ServiceMonitor"},
			selects: map[string]map[string]string{
				"test-prometheus-universal-exporter-apps": {"app.kubernetes.io/name": "target"},
				"test-prometheus-universal-exporter-pods": {"app.kubernetes.io/name": "target"},
			},
		},
		"service and pod monitors with a target selector": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","targetSelector":{"matchLabels":{"team":"a"}}},{"name":"pods","enabled":true,"type":"pod","collector":"example","targetSelector":{"matchLabels":{"team":"b"}}}]`, "--set", "selfMetrics.type=pod"},
			want: map[string]string{"test-prometheus-universal-exporter-apps": "ServiceMonitor", "test-prometheus-universal-exporter-pods": "PodMonitor", "test-prometheus-universal-exporter-self": "PodMonitor"},
			selects: map[string]map[string]string{
				"test-prometheus-universal-exporter-apps": {"team": "a"},
				"test-prometheus-universal-exporter-pods": {"team": "b"},
			},
		},
		"the static targets and self monitors as services": {
			args: []string{"-f", targetsFile, "--set", "staticTargets.monitor.enabled=true"},
			want: map[string]string{"test-prometheus-universal-exporter-static-targets": "ServiceMonitor", "test-prometheus-universal-exporter-self": "ServiceMonitor"},
		},
		"the static targets and self monitors as pods": {
			args: []string{"-f", targetsFile, "--set", "staticTargets.monitor.enabled=true", "--set", "staticTargets.monitor.type=pod", "--set", "selfMetrics.type=pod"},
			want: map[string]string{"test-prometheus-universal-exporter-static-targets": "PodMonitor", "test-prometheus-universal-exporter-self": "PodMonitor"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := map[string]string{}
			for _, doc := range renderedDocuments(t, helm, tc.args...) {
				kind := child(doc, "kind").Value
				if keys := colonKeys(doc); len(keys) > 0 {
					t.Errorf("%s has keys run together: %q", kind, keys)
				}
				if kind != "ServiceMonitor" && kind != "PodMonitor" {
					continue
				}
				name := path(doc, "metadata", "name").Value
				got[name] = kind
				selector := path(doc, "spec", "selector")
				if selector == nil || selector.Kind != yaml.MappingNode || len(selector.Content) == 0 {
					t.Errorf("%s %s has no spec.selector", kind, name)
					continue
				}
				labels := child(selector, "matchLabels")
				if labels == nil || labels.Kind != yaml.MappingNode || len(labels.Content) == 0 {
					t.Errorf("%s %s has no spec.selector.matchLabels", kind, name)
				}
				for key, value := range tc.selects[name] {
					if v := child(labels, key); v == nil || v.Value != value {
						t.Errorf("%s %s does not select %s=%s", kind, name, key, value)
					}
				}
			}
			for name, kind := range tc.want {
				if got[name] != kind {
					t.Errorf("want %s %s, got %q", kind, name, got[name])
				}
			}
		})
	}
}

// With webAuth.enabled, the exporter's Basic Auth protects /probe as well as
// its own metrics, so a probe monitor without auth of its own presents the
// webAuth Secret, as the self-metrics and static targets monitors do;
// without, every scrape it makes is a 401. A monitor with its own auth keeps
// it.
func TestChartProbeMonitorsPresentTheExporterCredential(t *testing.T) {
	helm := requireHelm(t)
	monitors := `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"},{"name":"pods","enabled":true,"type":"pod","collector":"example"},{"name":"own","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"type":"bearer","secretName":"own-token"}}]`
	endpoint := func(doc *yaml.Node) *yaml.Node {
		if child(doc, "kind").Value == "PodMonitor" {
			return path(doc, "spec", "podMetricsEndpoints", "0")
		}
		return path(doc, "spec", "endpoints", "0")
	}
	probes := func(docs []*yaml.Node) map[string]*yaml.Node {
		found := map[string]*yaml.Node{}
		for _, doc := range docs {
			if kind := child(doc, "kind").Value; kind == "ServiceMonitor" || kind == "PodMonitor" {
				if name := path(doc, "metadata", "name").Value; strings.HasSuffix(name, "-apps") || strings.HasSuffix(name, "-pods") || strings.HasSuffix(name, "-own") {
					found[name] = endpoint(doc)
				}
			}
		}
		if len(found) != 3 {
			t.Fatalf("want 3 probe monitors, got %d", len(found))
		}
		return found
	}

	for name, ep := range probes(renderedDocuments(t, helm, "--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth", "--set", "webAuth.usernameKey=user", "--set-json", monitors)) {
		if strings.HasSuffix(name, "-own") {
			if child(ep, "basicAuth") != nil || path(ep, "authorization", "credentials", "name").Value != "own-token" {
				t.Errorf("%s does not keep its own bearer credential", name)
			}
			continue
		}
		for _, want := range [][]string{{"username", "name", "exporter-auth"}, {"username", "key", "user"}, {"password", "name", "exporter-auth"}, {"password", "key", "password"}} {
			if v := path(ep, "basicAuth", want[0], want[1]); v == nil || v.Value != want[2] {
				t.Errorf("%s: basicAuth.%s.%s is not %q", name, want[0], want[1], want[2])
			}
		}
	}
	for name, ep := range probes(renderedDocuments(t, helm, "--set-json", monitors)) {
		if child(ep, "basicAuth") != nil {
			t.Errorf("%s presents a credential without webAuth", name)
		}
	}
}
