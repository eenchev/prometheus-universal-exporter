package repository

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// allocatorRBAC are the RBAC objects and ServiceAccounts rendered for the
// allocator, by "Kind/name" or "Kind/namespace/name" for a Role and a
// RoleBinding.
func allocatorRBAC(t *testing.T, helm string, args ...string) []string {
	t.Helper()
	var out []string
	for _, doc := range renderedDocuments(t, helm, args...) {
		kind := text(path(doc, "kind"))
		key := kind + "/" + text(path(doc, "metadata", "name"))
		switch kind {
		case "Role", "RoleBinding":
			out = append(out, kind+"/"+text(path(doc, "metadata", "namespace"))+"/"+text(path(doc, "metadata", "name")))
		case "ClusterRole", "ClusterRoleBinding":
			out = append(out, key)
		case "ServiceAccount":
			if strings.HasSuffix(key, "-allocator") {
				out = append(out, key)
			}
		}
	}
	slices.Sort(out)
	return out
}

// A kubernetes discovery source, in a probe job or in the static target
// document's discovered_targets, runs the allocator as a ServiceAccount of
// its own, with its token mounted, allowed to get, list and watch
// EndpointSlices in each namespace a source lists, the release's for one
// that lists none, and cluster-wide for one with all_namespaces; without
// one, nothing of it is rendered and the allocator keeps the exporter's
// ServiceAccount without a token.
func TestChartGrantsTheAllocatorEndpointSlicesForKubernetesDiscovery(t *testing.T) {
	helm := requireHelm(t)
	const name = "test-prometheus-universal-exporter-allocator"
	podSpec := func(docs map[string]*yaml.Node) (string, string) {
		spec := path(manifest(t, docs, "Deployment/"+name), "spec", "template", "spec")
		return text(path(spec, "serviceAccountName")), text(path(spec, "automountServiceAccountToken"))
	}

	for what, jobs := range map[string]string{
		"no discovery":   allocatorProbeJobs,
		"http_sd alone":  `allocator.probeJobs=[{"name":"websites","collector":"example","discovery":[{"http_sd":{"url":"https://cmdb.example/sd"}}]}]`,
		"file_sd alone ": `allocator.probeJobs=[{"name":"websites","collector":"example","targets":["https://a.example"],"discovery":[{"file_sd":{"files":["sd/*.json"]}}]}]`,
	} {
		docs := manifests(t, helm, "--set", "allocator.enabled=true", "--set-json", jobs)
		if got := allocatorRBAC(t, helm, "--set", "allocator.enabled=true", "--set-json", jobs); len(got) != 0 {
			t.Errorf("%s: rendered %v", what, got)
		}
		if account, automount := podSpec(docs); account != "test-prometheus-universal-exporter" || automount != "false" {
			t.Errorf("%s: the allocator runs as %s with automount %s, want the exporter's account without a token", what, account, automount)
		}
	}

	kubernetesJobs := []string{"--set", "allocator.enabled=true", "--set-json",
		`allocator.probeJobs=[{"name":"databases","collector":"example","target_template":"https://{{address}}/","discovery":[{"kubernetes":{"namespaces":["db","cache"],"port":"metrics"}},{"kubernetes":{"port":9187},"labels":{"namespace":"__meta_kubernetes_namespace"}}]}]`}
	docs := manifests(t, helm, kubernetesJobs...)
	want := []string{
		"Role/cache/" + name, "Role/db/" + name, "Role/default/" + name,
		"RoleBinding/cache/" + name, "RoleBinding/db/" + name, "RoleBinding/default/" + name,
		"ServiceAccount/" + name,
	}
	if got := allocatorRBAC(t, helm, kubernetesJobs...); !slices.Equal(got, want) {
		t.Fatalf("rendered %v, want %v", got, want)
	}
	if account, automount := podSpec(docs); account != name || automount != "true" {
		t.Errorf("the allocator runs as %s with automount %s, want its own account with its token", account, automount)
	}
	var role *yaml.Node
	for _, doc := range renderedDocuments(t, helm, "--set", "allocator.enabled=true", "--set-json",
		`allocator.probeJobs=[{"name":"databases","collector":"example","target_template":"https://{{address}}/","discovery":[{"kubernetes":{"namespaces":["db"],"port":"metrics"}}]}]`) {
		switch text(path(doc, "kind")) {
		case "Role":
			role = doc
		case "RoleBinding":
			if text(path(doc, "roleRef", "name")) != name || text(path(doc, "subjects", "0", "name")) != name || text(path(doc, "subjects", "0", "namespace")) != "default" {
				t.Error("the RoleBinding does not bind the Role to the allocator's ServiceAccount")
			}
		}
	}
	rule := path(role, "rules", "0")
	if !slices.Equal(scalars(path(rule, "apiGroups")), []string{"discovery.k8s.io"}) || !slices.Equal(scalars(path(rule, "resources")), []string{"endpointslices"}) || !slices.Equal(scalars(path(rule, "verbs")), []string{"get", "list", "watch"}) {
		t.Errorf("the Role allows %v", rule)
	}
	probeTargets := text(path(manifest(t, docs, "ConfigMap/test-prometheus-universal-exporter-allocator-probe-targets"), "data", "probe-targets.yaml"))
	if !strings.Contains(probeTargets, "discovery:") || !strings.Contains(probeTargets, "target_template: https://{{address}}/") {
		t.Errorf("the probe targets file does not hold the discovery as given:\n%s", probeTargets)
	}

	static := valuesFile(t, `allocator:
  enabled: true
  staticTargets:
    data: |
      interval: 1m
      discovered_targets:
        - name: node
          collector: example
          target_template: http://{{address}}/
          discovery:
            - kubernetes: {all_namespaces: true, port: metrics}
`)
	docs = manifests(t, helm, "-f", static)
	want = []string{"ClusterRole/default-" + name, "ClusterRoleBinding/default-" + name, "ServiceAccount/" + name}
	if got := allocatorRBAC(t, helm, "-f", static); !slices.Equal(got, want) {
		t.Errorf("a static target document's source of every namespace rendered %v, want %v", got, want)
	}
	binding := manifest(t, docs, "ClusterRoleBinding/default-"+name)
	if text(path(binding, "roleRef", "kind")) != "ClusterRole" || text(path(binding, "subjects", "0", "name")) != name {
		t.Error("the ClusterRoleBinding does not bind the ClusterRole to the allocator's ServiceAccount")
	}

	if got := allocatorRBAC(t, helm, "-f", static, "--set", "allocator.rbac.create=false"); !slices.Equal(got, []string{"ServiceAccount/" + name}) {
		t.Errorf("with allocator.rbac.create false rendered %v, want the ServiceAccount alone", got)
	}
}

// The values schema takes a probe job's discovery as the allocator does:
// a job with discovery needs no targets, and a source with two kinds or a
// key the allocator refuses is refused.
func TestChartSchemaChecksProbeJobDiscovery(t *testing.T) {
	helm := requireHelm(t)
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true", "--set-json",
		`allocator.probeJobs=[{"name":"w","collector":"example","target_template":"https://{{address}}/","discovery":[{"name":"cmdb","http_sd":{"url":"https://cmdb.example/sd","refresh_interval":"30s","tls":{"ca_file":"/ca.crt"}},"max_targets":100}]}]`); !ok {
		t.Fatalf("a probe job of discovery alone was refused:\n%s", out)
	}
	for what, c := range map[string]struct{ jobs, want string }{
		"two kinds":           {`[{"name":"w","collector":"example","discovery":[{"http_sd":{"url":"https://a/"},"file_sd":{"files":["a.json"]}}]}]`, "allocator.probeJobs.0.discovery.0"},
		"an unknown key":      {`[{"name":"w","collector":"example","discovery":[{"http_sd":{"url":"https://a/","bogus":1}}]}]`, "bogus"},
		"a file of no format": {`[{"name":"w","collector":"example","discovery":[{"file_sd":{"files":["a.txt"]}}]}]`, "allocator.probeJobs.0.discovery.0.file_sd.files.0"},
		"no port":             {`[{"name":"w","collector":"example","discovery":[{"kubernetes":{}}]}]`, "port"},
		"a bad template":      {`[{"name":"w","collector":"example","target_template":"https://x/","discovery":[{"kubernetes":{"port":"m"}}]}]`, "allocator.probeJobs.0.target_template"},
		"empty discovery":     {`[{"name":"w","collector":"example","discovery":[]}]`, "allocator.probeJobs.0"},
	} {
		if out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true", "--set-json", "allocator.probeJobs="+c.jobs); ok || !namesInHelmError(out, c.want) {
			t.Errorf("%s: ok=%v, want an error naming %q:\n%s", what, ok, c.want, out)
		}
	}
}
