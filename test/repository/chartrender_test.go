package repository

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
			avoid: []string{"kind: HorizontalPodAutoscaler", "kind: PodDisruptionBudget", "--probe.max-concurrent", "--python.max-workers", "GOMEMLIMIT", "terminationGracePeriodSeconds", "sessionAffinity", "--web.enable-probe-debug"},
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
			want:  []string{"kind: HorizontalPodAutoscaler", "maxReplicas: 6", "averageUtilization: 80", "averageUtilization: 70", "kind: Deployment\n    name: prometheus-universal-exporter\n"},
			avoid: []string{"replicas: 1\n"},
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
		"negative probe limit":            {[]string{"--set", "server.probeMaxConcurrent=-1"}, "probeMaxConcurrent"},
		"fractional worker limit":         {[]string{"--set", "server.pythonMaxWorkers=1.5"}, "pythonMaxWorkers"},
		"memory ratio above 1":            {[]string{"--set", "goMemLimit.ratio=1.5"}, "ratio"},
		"memory ratio of 0":               {[]string{"--set", "goMemLimit.ratio=0"}, "ratio"},
		"a probe's own check":             {[]string{"--set", "livenessProbe.httpGet.path=/x"}, "livenessProbe"},
		"both disruption budgets":         {[]string{"--set", "podDisruptionBudget.enabled=true", "--set", "podDisruptionBudget.minAvailable=1", "--set", "podDisruptionBudget.maxUnavailable=1"}, "sets both minAvailable and maxUnavailable"},
		"fewer maximum than minimum":      {[]string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.minReplicas=5"}, "is below autoscaling.minReplicas"},
		"autoscaling on nothing":          {[]string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.targetCPUUtilizationPercentage=null"}, "needs something to scale on"},
		"a flag the chart manages":        {[]string{"--set", "extraArgs[0]=--probe.max-concurrent=3"}, "server.probeMaxConcurrent"},
		"the memory ratio as an extra":    {[]string{"--set", "extraArgs[0]=--runtime.memory-limit-ratio=0.5"}, "goMemLimit.ratio"},
		"probe debug as an extra":         {[]string{"--set", "extraArgs[0]=--web.enable-probe-debug"}, "server.probeDebug"},
		"a pod label the chart sets":      {[]string{"--set", "podLabels.app\\.kubernetes\\.io/name=x"}, "podLabels sets app.kubernetes.io/name"},
		"a spread without a topology key": {[]string{"--set", "topologySpreadConstraints[0].maxSkew=1"}, "topologyKey"},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, tc.args...)
			if ok || !strings.Contains(out, tc.want) {
				t.Fatalf("ok=%v, want an error naming %q:\n%s", ok, tc.want, out)
			}
		})
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
