package repository

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// chartGrace renders the chart with args and returns the exporter's and
// the allocator's pod specs, the allocator's nil when it is not rendered.
func chartGrace(t *testing.T, helm string, args ...string) (exporter, allocator *yaml.Node) {
	t.Helper()
	docs := manifests(t, helm, args...)
	exporter = path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter"), "spec", "template", "spec")
	if doc, ok := docs["Deployment/test-prometheus-universal-exporter-allocator"]; ok {
		allocator = path(doc, "spec", "template", "spec")
	}
	return exporter, allocator
}

// A preStop hook is rendered as the container's lifecycle.preStop, the
// exporter's from preStop and the allocator's from allocator.preStop; a
// sleep's seconds count in the grace period the chart renders, an exec or
// httpGet hook's not, which takes the time it takes.
// allocator.terminationGracePeriodSeconds sets the allocator's grace
// period, with or without discovery.
func TestChartRendersPreStopHooksAndTheAllocatorsGracePeriod(t *testing.T) {
	helm := requireHelm(t)
	discoveryJob := `allocator.probeJobs=[{"name":"websites","collector":"example","discovery":[{"http_sd":{"url":"https://cmdb.example/sd"}}]}]`
	for _, c := range []struct {
		name                      string
		args                      []string
		exporterGrace, allocGrace string
		exporterHook, allocHook   string
	}{
		{"the defaults", []string{"--set", "allocator.enabled=true"}, "", "", "", ""},
		{"an exporter sleep", []string{"--set", "preStop.sleep.seconds=10"}, "40", "", "10", ""},
		{"an exporter sleep beside a set grace period", []string{"--set", "preStop.sleep.seconds=10", "--set", "terminationGracePeriodSeconds=90"}, "90", "", "10", ""},
		{"an exporter exec", []string{"--set-json", `preStop={"exec":{"command":["/bin/drain","--now"]}}`}, "", "", "/bin/drain", ""},
		{"an allocator sleep without discovery", []string{"--set", "allocator.enabled=true", "--set", "allocator.preStop.sleep.seconds=20"}, "", "35", "", "20"},
		{"an allocator sleep with discovery", []string{"--set", "allocator.enabled=true", "--set-json", discoveryJob, "--set", "allocator.preStop.sleep.seconds=20"}, "", "38", "", "20"},
		{"a short allocator sleep with discovery", []string{"--set", "allocator.enabled=true", "--set-json", discoveryJob, "--set", "allocator.preStop.sleep.seconds=5"}, "", "30", "", "5"},
		{"an allocator grace period without discovery", []string{"--set", "allocator.enabled=true", "--set", "allocator.terminationGracePeriodSeconds=60"}, "", "60", "", ""},
		{"an allocator grace period with discovery", []string{"--set", "allocator.enabled=true", "--set-json", discoveryJob, "--set", "allocator.terminationGracePeriodSeconds=120"}, "", "120", "", ""},
		{"an allocator httpGet", []string{"--set", "allocator.enabled=true", "--set-json", `allocator.preStop={"httpGet":{"path":"/ready","port":"http"}}`}, "", "", "", "/ready"},
	} {
		exporter, allocator := chartGrace(t, helm, c.args...)
		if got := text(path(exporter, "terminationGracePeriodSeconds")); got != c.exporterGrace {
			t.Errorf("%s: the exporter's grace period is %q, want %q", c.name, got, c.exporterGrace)
		}
		if got := text(path(allocator, "terminationGracePeriodSeconds")); got != c.allocGrace {
			t.Errorf("%s: the allocator's grace period is %q, want %q", c.name, got, c.allocGrace)
		}
		hook := func(spec *yaml.Node) string {
			preStop := path(spec, "containers", "0", "lifecycle", "preStop")
			switch {
			case preStop == nil:
				return ""
			case path(preStop, "sleep") != nil:
				return text(path(preStop, "sleep", "seconds"))
			case path(preStop, "exec") != nil:
				return text(path(preStop, "exec", "command", "0"))
			}
			return text(path(preStop, "httpGet", "path"))
		}
		if got := hook(exporter); got != c.exporterHook {
			t.Errorf("%s: the exporter's preStop is %q, want %q", c.name, got, c.exporterHook)
		}
		if got := hook(allocator); got != c.allocHook {
			t.Errorf("%s: the allocator's preStop is %q, want %q", c.name, got, c.allocHook)
		}
	}
}

// A grace period shorter than what a stopping pod needs, its preStop
// sleep counted, fails rendering naming it; and so does a hook that is no
// lifecycle handler Kubernetes runs, by the schema and, with it skipped, by
// the templates.
func TestChartRefusesAGracePeriodShorterThanItsPreStop(t *testing.T) {
	helm := requireHelm(t)
	discoveryJob := `allocator.probeJobs=[{"name":"websites","collector":"example","discovery":[{"http_sd":{"url":"https://cmdb.example/sd"}}]}]`
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"an exporter sleep past its grace period": {[]string{"--set", "preStop.sleep.seconds=30", "--set", "terminationGracePeriodSeconds=45"}, "terminationGracePeriodSeconds 45 is shorter than the 60 seconds a stopping pod needs: preStop's sleep (30s)"},
		"an allocator grace period too short":     {[]string{"--set", "allocator.enabled=true", "--set", "allocator.terminationGracePeriodSeconds=10"}, "allocator.terminationGracePeriodSeconds 10 is shorter than the 15 seconds a stopping instance needs"},
		"an allocator sleep past its grace period": {[]string{"--set", "allocator.enabled=true", "--set-json", discoveryJob, "--set", "allocator.preStop.sleep.seconds=20", "--set", "allocator.terminationGracePeriodSeconds=30"},
			"allocator.terminationGracePeriodSeconds 30 is shorter than the 38 seconds a stopping instance needs: allocator.preStop's sleep (20s), a peer check interval and timeout (3s)"},
	} {
		if out, ok := helmTemplate(t, helm, chartDir, tc.args...); ok || !strings.Contains(out, tc.want) {
			t.Errorf("%s: ok=%v, want an error saying %q:\n%s", name, ok, tc.want, out)
		}
	}
	requireSchemaSkipping(t, helm)
	for name, tc := range map[string]struct {
		hook, schema, template string
	}{
		"a tcpSocket":           {`{"tcpSocket":{"port":8080}}`, "additional properties 'tcpSocket' not allowed", "preStop has tcpSocket; it is one of exec, httpGet and sleep"},
		"two handlers":          {`{"sleep":{"seconds":1},"httpGet":{"port":"http"}}`, "maxProperties: got 2, want 1", "preStop must set exactly one of exec, httpGet and sleep, not 2"},
		"no handler":            {`{}`, "minProperties: got 0, want 1", "preStop must set exactly one of exec, httpGet and sleep, not 0"},
		"an empty command":      {`{"exec":{"command":[]}}`, "minItems", "preStop.exec.command must be the command and its arguments"},
		"an httpGet of no port": {`{"httpGet":{"path":"/x"}}`, "missing property 'port'", "preStop.httpGet needs its port"},
		"a negative sleep":      {`{"sleep":{"seconds":-1}}`, "minimum", "preStop.sleep.seconds -1 must be a whole number from 0 to 2147483647"},
		"a fractional sleep":    {`{"sleep":{"seconds":1.5}}`, "integer", "preStop.sleep.seconds 1.5 must be a whole number"},
		"a sleep of no seconds": {`{"sleep":{}}`, "missing property 'seconds'", "preStop.sleep needs its seconds"},
	} {
		for _, at := range []string{"preStop", "allocator.preStop"} {
			args := []string{"--set", "allocator.enabled=true", "--set-json", at + "=" + tc.hook}
			if out, ok := helmTemplate(t, helm, chartDir, args...); ok || !strings.Contains(out, tc.schema) {
				t.Errorf("%s at %s: ok=%v, want the schema to say %q:\n%s", name, at, ok, tc.schema, out)
			}
			want := strings.Replace(tc.template, "preStop", at, 1)
			if out, ok := helmTemplate(t, helm, chartDir, append([]string{"--skip-schema-validation"}, args...)...); ok || !strings.Contains(out, want) {
				t.Errorf("%s at %s, the schema skipped: ok=%v, want the templates to say %q:\n%s", name, at, ok, want, out)
			}
		}
	}
}
