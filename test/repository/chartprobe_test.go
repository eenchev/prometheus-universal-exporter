package repository

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The chart's Prometheus Operator Probes, one per probes entry: uptime checks
// declared as blackbox_exporter's are, with the exporter's Service as the
// prober. Rendered with helm, and skipped without it, as the other render
// tests are.

// renderedProbes renders the chart with args and returns each Probe by the
// name its entry gives it, and whether the self-metrics monitor rendered.
func renderedProbes(t *testing.T, helm string, args ...string) (probes map[string]*yaml.Node, self bool) {
	t.Helper()
	probes = map[string]*yaml.Node{}
	for _, doc := range renderedDocuments(t, helm, args...) {
		name := path(doc, "metadata", "name").Value
		switch child(doc, "kind").Value {
		case "Probe":
			if keys := colonKeys(doc); len(keys) > 0 {
				t.Errorf("Probe %s has keys run together: %q", name, keys)
			}
			if got := child(doc, "apiVersion").Value; got != "monitoring.coreos.com/v1" {
				t.Errorf("Probe %s has apiVersion %q", name, got)
			}
			probes[strings.TrimPrefix(name, "test-prometheus-universal-exporter-")] = doc
		case "ServiceMonitor", "PodMonitor":
			if strings.HasSuffix(name, "-self") {
				self = true
			}
		}
	}
	return probes, self
}

// values returns the scalar values of the sequence node, or nil.
func values(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	var found []string
	for _, item := range node.Content {
		found = append(found, item.Value)
	}
	return found
}

// probeParams returns a Probe's spec.params as name to values.
func probeParams(t *testing.T, probe *yaml.Node) map[string][]string {
	t.Helper()
	params := path(probe, "spec", "params")
	if params == nil || params.Kind != yaml.SequenceNode {
		t.Fatalf("the Probe has no spec.params list")
	}
	found := map[string][]string{}
	for _, param := range params.Content {
		name := child(param, "name").Value
		if _, twice := found[name]; twice {
			t.Errorf("spec.params names %q twice", name)
		}
		found[name] = values(child(param, "values"))
	}
	return found
}

// wantFields checks each dotted path of probe against its value.
func wantFields(t *testing.T, what string, probe *yaml.Node, want map[string]string) {
	t.Helper()
	for field, value := range want {
		if got := path(probe, strings.Split(field, ".")...); got == nil || got.Value != value {
			t.Errorf("%s: %s is %+v, want %q", what, field, got, value)
		}
	}
}

// Nothing is rendered by default, and probes: [] is a value the chart takes
// and renders nothing for, so the Probe CRD is needed only with an entry.
func TestChartRendersNoProbeByDefault(t *testing.T) {
	helm := requireHelm(t)
	for _, args := range [][]string{nil, {"--set-json", "probes=[]"}} {
		probes, _ := renderedProbes(t, helm, args...)
		if len(probes) != 0 {
			t.Errorf("%v: %d Probes rendered, want none", args, len(probes))
		}
	}
	if !strings.Contains(readChartFile(t, "values.yaml"), "\nprobes: []\n") {
		t.Error("values.yaml does not default probes to an empty list")
	}
}

// An entry with static targets renders a Probe whose prober is the
// exporter's Service at /probe, which sends the collector as a parameter of
// spec.params, not as spec.module, and the targets as its static config, with
// the entry's timing, job name, labels and relabelings; and its presence says
// the Prometheus Operator's kinds exist, so the self-metrics monitor renders.
func TestChartProbesProbeStaticTargetsThroughTheService(t *testing.T) {
	helm := requireHelm(t)
	probes, self := renderedProbes(t, helm, "--set", "namespaceOverride=monitoring", "--set-json", `defaultLabels={"team":"web"}`, "--set-json", `probes=[{
		"name":"sites","collector":"example","interval":"1m","scrapeTimeout":"30s","jobName":"site",
		"labels":{"release":"prom"},"annotations":{"note":"uptime"},
		"targets":["https://www.example.com","http://intranet.example.internal"],"targetLabels":{"tier":"public"},
		"relabelings":[{"sourceLabels":["__param_target"],"targetLabel":"site"}],
		"metricRelabelings":[{"action":"drop","sourceLabels":["__name__"],"regex":"site_http_redirects"}],
		"params":{"timeout":["10s"],"on":["true"]}}]`)
	probe := probes["sites"]
	if len(probes) != 1 || probe == nil {
		t.Fatalf("want the one Probe sites, got %d", len(probes))
	}
	wantFields(t, "sites", probe, map[string]string{
		"metadata.namespace":                    "monitoring",
		"metadata.labels.release":               "prom",
		"metadata.labels.team":                  "web",
		"metadata.annotations.note":             "uptime",
		"spec.prober.url":                       "test-prometheus-universal-exporter.monitoring.svc:8080",
		"spec.prober.scheme":                    "http",
		"spec.prober.path":                      "/probe",
		"spec.interval":                         "1m",
		"spec.scrapeTimeout":                    "30s",
		"spec.jobName":                          "site",
		"spec.targets.staticConfig.labels.tier": "public",
		"spec.targets.staticConfig.relabelingConfigs.0.targetLabel": "site",
		"spec.metricRelabelings.0.regex":                            "site_http_redirects",
	})
	if got := path(probe, "metadata", "labels"); child(got, "app.kubernetes.io/name").Value != "prometheus-universal-exporter" {
		t.Errorf("sites does not carry the chart's labels")
	}
	if got := values(path(probe, "spec", "targets", "staticConfig", "static")); len(got) != 2 || got[0] != "https://www.example.com" || got[1] != "http://intranet.example.internal" {
		t.Errorf("sites: the static targets are %q", got)
	}
	params := probeParams(t, probe)
	for name, want := range map[string]string{"collector": "example", "timeout": "10s", "on": "true"} {
		if got := params[name]; len(got) != 1 || got[0] != want {
			t.Errorf("sites: the %s parameter is %q, want [%s]", name, got, want)
		}
	}
	// The value true is a string the CRD takes, not a YAML boolean.
	for _, param := range path(probe, "spec", "params").Content {
		if child(param, "name").Value == "on" && path(param, "values", "0").Tag != "!!str" {
			t.Error("sites: the parameter value true is not rendered as a string")
		}
	}
	for _, absent := range []string{"module", "basicAuth", "authorization"} {
		if child(child(probe, "spec"), absent) != nil {
			t.Errorf("sites renders spec.%s", absent)
		}
	}
	if path(probe, "spec", "targets", "ingress") != nil {
		t.Error("sites renders an ingress beside its static targets")
	}
	if !self {
		t.Error("the self-metrics monitor is not rendered beside a Probe")
	}

	// Without the timings, the job name and the extras, none is rendered,
	// so the Prometheus Operator's defaults apply.
	probes, _ = renderedProbes(t, helm, "--set-json", `probes=[{"name":"bare","collector":"example","targets":["https://www.example.com"]}]`)
	spec := child(probes["bare"], "spec")
	for _, absent := range []string{"interval", "scrapeTimeout", "jobName", "metricRelabelings"} {
		if child(spec, absent) != nil {
			t.Errorf("bare renders spec.%s without a value for it", absent)
		}
	}
	for _, absent := range []string{"labels", "relabelingConfigs"} {
		if path(spec, "targets", "staticConfig", absent) != nil {
			t.Errorf("bare renders spec.targets.staticConfig.%s without a value for it", absent)
		}
	}
	if path(probes["bare"], "metadata", "namespace") != nil {
		t.Error("bare renders a namespace without namespaceOverride")
	}
}

// An entry with ingress renders the Ingress selector, its namespaces and
// relabelings, and no static config.
func TestChartProbesProbeTheIngressesTheySelect(t *testing.T) {
	helm := requireHelm(t)
	probes, _ := renderedProbes(t, helm, "--set-json", `probes=[
		{"name":"ingresses","collector":"example","ingress":{"selector":{"matchLabels":{"probe":"true"}},"namespaceSelector":{"matchNames":["shop","blog"]}},"relabelings":[{"targetLabel":"kind","replacement":"ingress"}]},
		{"name":"every-ingress","collector":"example","ingress":{"selector":{}}}]`)
	if len(probes) != 2 {
		t.Fatalf("want 2 Probes, got %d", len(probes))
	}
	wantFields(t, "ingresses", probes["ingresses"], map[string]string{
		"spec.prober.path": "/probe",
		"spec.targets.ingress.selector.matchLabels.probe":      "true",
		"spec.targets.ingress.relabelingConfigs.0.replacement": "ingress",
		"spec.targets.ingress.namespaceSelector.matchNames.1":  "blog",
		"spec.targets.ingress.namespaceSelector.matchNames.0":  "shop",
	})
	if got := probeParams(t, probes["ingresses"])["collector"]; len(got) != 1 || got[0] != "example" {
		t.Errorf("ingresses: the collector parameter is %q", got)
	}
	if path(probes["ingresses"], "spec", "targets", "staticConfig") != nil {
		t.Error("ingresses renders a static config")
	}
	selector := path(probes["every-ingress"], "spec", "targets", "ingress", "selector")
	if selector == nil || selector.Kind != yaml.MappingNode || len(selector.Content) != 0 {
		t.Errorf("every-ingress: the selector is %+v, want the empty mapping it was given", selector)
	}
	if path(probes["every-ingress"], "spec", "targets", "ingress", "namespaceSelector") != nil {
		t.Error("every-ingress renders a namespaceSelector without one in the values")
	}
}

// With webAuth.enabled the exporter's Basic Auth protects /probe, so a Probe
// without auth of its own presents the webAuth Secret, as a probe monitor
// does, or every probe would be a 401; one with its own keeps it alone.
func TestChartProbesPresentTheExporterCredential(t *testing.T) {
	helm := requireHelm(t)
	const entries = `probes=[
		{"name":"plain","collector":"example","targets":["https://a.example"]},
		{"name":"bearer","collector":"example","targets":["https://a.example"],"auth":{"enabled":true,"type":"bearer","secretName":"probe-token"}},
		{"name":"basic","collector":"example","ingress":{"selector":{}},"auth":{"enabled":true,"type":"basic","secretName":"probe-user","usernameKey":"user"}}]`
	probes, _ := renderedProbes(t, helm, "--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth", "--set", "webAuth.usernameKey=login", "--set-json", entries)
	for name, want := range map[string]map[string]string{
		"plain":  {"basicAuth.username.name": "exporter-auth", "basicAuth.username.key": "login", "basicAuth.password.name": "exporter-auth", "basicAuth.password.key": "password"},
		"bearer": {"authorization.type": "Bearer", "authorization.credentials.name": "probe-token", "authorization.credentials.key": "token"},
		"basic":  {"basicAuth.username.name": "probe-user", "basicAuth.username.key": "user", "basicAuth.password.name": "probe-user", "basicAuth.password.key": "password"},
	} {
		spec := child(probes[name], "spec")
		if spec == nil {
			t.Errorf("%s is not rendered", name)
			continue
		}
		wantFields(t, name, spec, want)
		if other := map[bool]string{true: "basicAuth", false: "authorization"}[name == "bearer"]; child(spec, other) != nil {
			t.Errorf("%s also renders %s", name, other)
		}
	}
	probes, _ = renderedProbes(t, helm, "--set-json", entries)
	if spec := child(probes["plain"], "spec"); child(spec, "basicAuth") != nil || child(spec, "authorization") != nil {
		t.Error("plain presents a credential without webAuth")
	}
}

// What the chart refuses of a probes entry, by the values schema and, with
// the schema skipped, by the templates, each saying which entry and why.
func TestChartRefusesInvalidProbes(t *testing.T) {
	helm := requireHelm(t)
	// The refusals are of these entries and not of the key: an entry the
	// chart takes renders, so a schema without probes, which would refuse
	// every case below, fails here.
	if out, ok := helmTemplate(t, helm, chartDir, "--set-json", `probes=[{"name":"s","collector":"example","targets":["a"]}]`); !ok {
		t.Fatalf("a valid entry does not render:\n%s", out)
	}
	skips := true
	if out, _ := helmTemplate(t, helm, chartDir, "--skip-schema-validation"); strings.Contains(out, "unknown flag") {
		skips = false
	}
	for name, tc := range map[string]struct {
		args []string
		// What the templates say with the schema skipped; empty when only
		// the schema refuses it.
		want string
		// Whether the values schema refuses it too; when not, the
		// templates' message is checked with the schema.
		schema bool
	}{
		"neither targets nor ingress": {[]string{`probes=[{"name":"s","collector":"example"}]`}, `probes entry "s" has neither targets nor ingress`, true},
		"an empty ingress":            {[]string{`probes=[{"name":"s","collector":"example","ingress":{}}]`}, `probes entry "s" has neither targets nor ingress`, true},
		"both targets and ingress":    {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"ingress":{"selector":{}}}]`}, `probes entry "s" has both targets and ingress`, true},
		"an ingress without selector": {[]string{`probes=[{"name":"s","collector":"example","ingress":{"namespaceSelector":{"any":true}}}]`}, `probes entry "s" has ingress without a selector`, true},
		"no collector":                {[]string{`probes=[{"name":"s","targets":["a"]}]`}, `probes entry "s" names no collector`, true},
		"an undefined collector":      {[]string{`probes=[{"name":"s","collector":"site","targets":["a"]}]`}, `probes entry "s" names collector "site", which config.data does not define`, false},
		"no name":                     {[]string{`probes=[{"collector":"example","targets":["a"]}]`}, `probes entry #0 has the name ""`, true},
		"a name in upper case":        {[]string{`probes=[{"name":"Sites","collector":"example","targets":["a"]}]`}, `probes entry #0 has the name "Sites"`, true},
		"a name with an underscore":   {[]string{`probes=[{"name":"my_sites","collector":"example","targets":["a"]}]`}, `must be a DNS-1123 label`, true},
		"two entries of one name":     {[]string{`probes=[{"name":"s","collector":"example","targets":["a"]},{"name":"s","collector":"example","targets":["b"]}]`}, `probes entries #0 and #1 are both named "s"`, false},
		"params.collector":            {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"params":{"collector":["x"]}}]`}, `probes entry "s" sets params.collector; the chart renders the collector parameter itself`, true},
		"params.target":               {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"params":{"target":["x"]}}]`}, `probes entry "s" sets params.target`, true},
		"a timeout over the interval": {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"interval":"10s","scrapeTimeout":"1m"}]`}, `probes entry "s" has scrapeTimeout 1m, longer than its interval 10s`, false},
		"auth without a Secret":       {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"auth":{"enabled":true,"type":"basic"}}]`}, `probes entry "s" enables auth without a Secret`, true},
		"auth without a type":         {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"auth":{"enabled":true,"secretName":"x"}}]`}, `probes entry "s" enables auth without a type`, true},
		"no Service":                  {[]string{`probes=[{"name":"s","collector":"example","targets":["a"]}]`, "--set", "service.enabled=false"}, `probes entry "s" probes through the exporter's Service, its prober, which service.enabled=false leaves out`, false},
		"beside the allocator": {[]string{`probes=[{"name":"s","collector":"example","targets":["a"]}]`, "--set", "allocator.enabled=true", "--set-json", `allocator.probeJobs=[{"name":"websites","collector":"example","targets":["https://shop.example"]}]`},
			`probes entry "s" is set beside allocator.enabled`, true},
		"an unknown key":           {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"module":"http_2xx"}]`}, "", true},
		"an empty target":          {[]string{`probes=[{"name":"s","collector":"example","targets":[""]}]`}, "", true},
		"an empty param":           {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"params":{"timeout":[]}}]`}, "", true},
		"a fraction":               {[]string{`probes=[{"name":"s","collector":"example","targets":["a"],"interval":"1.5m"}]`}, "", true},
		"an unknown namespace key": {[]string{`probes=[{"name":"s","collector":"example","ingress":{"selector":{},"namespaceSelector":{"all":true}}}]`}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--set-json"}, tc.args...)
			out, ok := helmTemplate(t, helm, chartDir, args...)
			if ok {
				t.Fatalf("rendered:\n%s", out)
			}
			if tc.schema && !strings.Contains(out, "don't meet the specifications of the schema") {
				t.Errorf("the values schema does not refuse it:\n%s", out)
			}
			if !tc.schema && !strings.Contains(out, tc.want) {
				t.Errorf("want an error saying %q:\n%s", tc.want, out)
			}
			if tc.want == "" || !skips {
				return
			}
			out, ok = helmTemplate(t, helm, chartDir, append([]string{"--skip-schema-validation"}, args...)...)
			if ok || !strings.Contains(out, tc.want) {
				t.Errorf("with the schema skipped, ok=%v, want an error saying %q:\n%s", ok, tc.want, out)
			}
		})
	}
}
