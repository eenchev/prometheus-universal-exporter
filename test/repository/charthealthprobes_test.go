package repository

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// healthProbes.mode: the liveness and readiness probes as GETs on the http
// port, or as the exporter's --healthcheck run in the container.

// exporterContainer renders the chart with args and returns its exporter
// container, not the allocator's.
func exporterContainer(t *testing.T, helm string, args ...string) *yaml.Node {
	t.Helper()
	for _, doc := range renderedDocuments(t, helm, args...) {
		if child(doc, "kind").Value != "Deployment" {
			continue
		}
		for _, container := range path(doc, "spec", "template", "spec", "containers").Content {
			if child(container, "name").Value == "exporter" {
				return container
			}
		}
	}
	t.Fatal("no exporter container rendered")
	return nil
}

// By default both probes GET their endpoint on the http port, and run
// nothing in the container.
func TestTheChartsHealthProbesAreGETsByDefault(t *testing.T) {
	helm := requireHelm(t)
	container := exporterContainer(t, helm)
	for probe, endpoint := range map[string]string{"livenessProbe": "/health", "readinessProbe": "/ready"} {
		if got := path(container, probe, "httpGet", "path"); got == nil || got.Value != endpoint || path(container, probe, "httpGet", "port").Value != "http" {
			t.Errorf("%s does not GET %s on the http port", probe, endpoint)
		}
		if path(container, probe, "exec") != nil {
			t.Errorf("%s runs a command", probe)
		}
	}
}

// With healthProbes.mode exec each probe runs the exporter's --healthcheck
// for its endpoint, at the listen address the container's args give, with or
// without the allocator, and its timings still come from the values. The
// command's flags are the exporter's.
func TestTheChartsHealthProbesRunTheHealthcheckInExecMode(t *testing.T) {
	helm := requireHelm(t)
	help := helpText(t)
	if !strings.Contains(help, "\n  -healthcheck\n") {
		t.Fatalf("the exporter has no --healthcheck flag:\n%s", help)
	}
	for name, args := range map[string][]string{
		"default address":    {"--set", "healthProbes.mode=exec"},
		"another address":    {"--set", "healthProbes.mode=exec", "--set", "server.listenAddress=0.0.0.0:9115"},
		"an IPv6 wildcard":   {"--set", "healthProbes.mode=exec", "--set", "server.listenAddress=[::]:9115"},
		"with the allocator": {"--set", "healthProbes.mode=exec", "--set", "allocator.enabled=true", "--set", "server.listenAddress=:9115"},
		"with timings":       {"--set", "healthProbes.mode=exec", "--set", "livenessProbe.timeoutSeconds=7"},
	} {
		t.Run(name, func(t *testing.T) {
			container := exporterContainer(t, helm, args...)
			listen := ""
			for _, arg := range child(container, "args").Content {
				if value, found := strings.CutPrefix(arg.Value, "--web.listen-address="); found {
					listen = value
				}
			}
			if listen == "" {
				t.Fatal("the container has no --web.listen-address")
			}
			for probe, endpoint := range map[string]string{"livenessProbe": "health", "readinessProbe": "ready"} {
				if path(container, probe, "httpGet") != nil {
					t.Errorf("%s still GETs on the http port", probe)
				}
				var command []string
				if node := path(container, probe, "exec", "command"); node != nil {
					for _, part := range node.Content {
						command = append(command, part.Value)
					}
				}
				want := []string{"/bin/prometheus-universal-exporter", "--healthcheck=" + endpoint, "--web.listen-address=" + listen}
				if strings.Join(command, " ") != strings.Join(want, " ") {
					t.Errorf("%s runs %q, want %q", probe, command, want)
				}
			}
			if name == "with timings" {
				if got := path(container, "livenessProbe", "timeoutSeconds"); got == nil || got.Value != "7" {
					t.Error("the liveness probe's timeoutSeconds is not the value's")
				}
			}
		})
	}
}

// A mode the chart does not have fails rendering, by the values schema and,
// with it skipped, by the templates.
func TestTheChartRefusesAHealthProbesModeItHasNot(t *testing.T) {
	helm := requireHelm(t)
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "healthProbes.mode=tcp"); ok || !namesInHelmError(out, "healthProbes.mode") {
		t.Errorf("ok=%v, want the schema to refuse healthProbes.mode:\n%s", ok, out)
	}
	requireSchemaSkipping(t, helm)
	want := `healthProbes.mode "tcp" must be http or exec`
	if out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "--set", "healthProbes.mode=tcp"); ok || !strings.Contains(out, want) {
		t.Errorf("ok=%v, want an error saying %q:\n%s", ok, want, out)
	}
}

// --healthcheck in extraArgs would make the container check and exit rather
// than serve, so it is refused, pointing to the exec probes.
func TestTheChartRefusesTheHealthcheckInExtraArgs(t *testing.T) {
	helm := requireHelm(t)
	want := `extraArgs entry "--healthcheck=ready" would make the exporter run its health check and exit, so the pod would never serve; set healthProbes.mode to exec for probes that run it`
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "extraArgs[0]=--healthcheck=ready"); ok || !strings.Contains(out, want) {
		t.Errorf("ok=%v, want an error saying %q:\n%s", ok, want, out)
	}
}
