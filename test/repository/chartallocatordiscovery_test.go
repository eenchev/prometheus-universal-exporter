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
// its own, with its token mounted, allowed only to list EndpointSlices,
// all the allocator does, in each namespace a source lists, the release's
// for one that lists none, and cluster-wide for one with all_namespaces;
// without one, nothing of it is rendered and the allocator keeps the exporter's
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
	if !slices.Equal(scalars(path(rule, "apiGroups")), []string{"discovery.k8s.io"}) || !slices.Equal(scalars(path(rule, "resources")), []string{"endpointslices"}) || !slices.Equal(scalars(path(rule, "verbs")), []string{"list"}) {
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
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true", "--set-json",
		`allocator.probeJobs=[{"name":"w","collector":"example","discovery":[{"dns_sd":{"names":["_metrics._tcp.example.com"],"server":"10.0.0.53"}},{"dns_sd":{"names":["a.example"],"type":"A","port":9100}}]}]`); !ok {
		t.Fatalf("a probe job of dns_sd sources was refused:\n%s", out)
	}
	for what, c := range map[string]struct{ jobs, want string }{
		"two kinds":           {`[{"name":"w","collector":"example","discovery":[{"http_sd":{"url":"https://a/"},"file_sd":{"files":["a.json"]}}]}]`, "allocator.probeJobs.0.discovery.0"},
		"an unknown key":      {`[{"name":"w","collector":"example","discovery":[{"http_sd":{"url":"https://a/","bogus":1}}]}]`, "bogus"},
		"a file of no format": {`[{"name":"w","collector":"example","discovery":[{"file_sd":{"files":["a.txt"]}}]}]`, "allocator.probeJobs.0.discovery.0.file_sd.files.0"},
		"no port":             {`[{"name":"w","collector":"example","discovery":[{"kubernetes":{}}]}]`, "port"},
		"a bad template":      {`[{"name":"w","collector":"example","target_template":"https://x/","discovery":[{"kubernetes":{"port":"m"}}]}]`, "allocator.probeJobs.0.target_template"},
		"empty discovery":     {`[{"name":"w","collector":"example","discovery":[]}]`, "allocator.probeJobs.0"},
		"a port for SRV":      {`[{"name":"w","collector":"example","discovery":[{"dns_sd":{"names":["a.example"],"port":9100}}]}]`, "allocator.probeJobs.0.discovery.0.dns_sd"},
		"an A without a port": {`[{"name":"w","collector":"example","discovery":[{"dns_sd":{"names":["a.example"],"type":"A"}}]}]`, "allocator.probeJobs.0.discovery.0.dns_sd"},
	} {
		if out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true", "--set-json", "allocator.probeJobs="+c.jobs); ok || !namesInHelmError(out, c.want) {
			t.Errorf("%s: ok=%v, want an error naming %q:\n%s", what, ok, c.want, out)
		}
	}
}

// An allocator with a discovery source, in a probe job or in the static
// target document's discovered_targets, runs its pods as peers: two by
// default, or allocator.replicas, rolled out with the default strategy,
// each given --peers with the headless Service's name, the one the exporter
// pods heartbeat to, and --peers.self with its pod IP from the downward API;
// its NetworkPolicy admits the allocator's own pods, which check each other
// and fetch the leader's discovered targets, and its disruption budget stays
// maxUnavailable 1. An allocator of files alone gets neither the flags nor
// the rule.
func TestChartRunsAllocatorPeersWithDiscovery(t *testing.T) {
	helm := requireHelm(t)
	const name = "test-prometheus-universal-exporter-allocator"
	discoveryJob := `allocator.probeJobs=[{"name":"websites","collector":"example","discovery":[{"http_sd":{"url":"https://cmdb.example/sd"}}]}]`
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
            - file_sd: {files: [/etc/pue-sd/nodes.json]}
`)
	peerArgs := []string{"--peers=" + name + ".monitoring.svc:9116", "--peers.self=$(POD_IP)"}
	allocatorSelector := []string{"app.kubernetes.io/instance=test", "app.kubernetes.io/name=prometheus-universal-exporter-allocator"}
	// fromSelectors are the label selectors the allocator's NetworkPolicy
	// admits, each as its sorted key=value pairs.
	fromSelectors := func(docs map[string]*yaml.Node) [][]string {
		var out [][]string
		from := path(manifest(t, docs, "NetworkPolicy/"+name), "spec", "ingress", "0", "from")
		for _, peer := range from.Content {
			labels := path(peer, "podSelector", "matchLabels")
			if labels == nil {
				continue
			}
			var pairs []string
			for i := 0; i+1 < len(labels.Content); i += 2 {
				pairs = append(pairs, labels.Content[i].Value+"="+labels.Content[i+1].Value)
			}
			slices.Sort(pairs)
			out = append(out, pairs)
		}
		return out
	}
	for what, c := range map[string]struct {
		args     []string
		replicas string
	}{
		"a probe job's source":                {[]string{"--set", "allocator.enabled=true", "--set-json", discoveryJob}, "2"},
		"a static target entry's":             {[]string{"-f", static}, "2"},
		"a probe job's source, replicas 3":    {[]string{"--set", "allocator.enabled=true", "--set", "allocator.replicas=3", "--set-json", discoveryJob}, "3"},
		"a static target entry's, replicas 1": {[]string{"-f", static, "--set", "allocator.replicas=1"}, "1"},
	} {
		docs := manifests(t, helm, append([]string{"--namespace", "monitoring"}, c.args...)...)
		spec := path(manifest(t, docs, "Deployment/"+name), "spec")
		if replicas, strategy := text(path(spec, "replicas")), text(path(spec, "strategy", "type")); replicas != c.replicas || strategy != "" {
			t.Errorf("with %s the allocator has %s replicas and the strategy %q, want %s and the default, a RollingUpdate", what, replicas, strategy, c.replicas)
		}
		container := path(spec, "template", "spec", "containers", "0")
		if args := scalars(path(container, "args")); !slices.Equal(args[len(args)-2:], peerArgs) {
			t.Errorf("with %s the allocator's arguments end %v, want %v", what, args[len(args)-2:], peerArgs)
		}
		env := path(container, "env", "0")
		if text(path(env, "name")) != "POD_IP" || text(path(env, "valueFrom", "fieldRef", "fieldPath")) != "status.podIP" {
			t.Errorf("with %s the allocator's POD_IP is not its pod's IP: %v", what, path(container, "env"))
		}
		if !slices.ContainsFunc(fromSelectors(docs), func(s []string) bool { return slices.Equal(s, allocatorSelector) }) {
			t.Errorf("with %s the allocator's NetworkPolicy admits %v, not the allocator's own pods", what, fromSelectors(docs))
		}
		pdb := path(manifest(t, docs, "PodDisruptionBudget/"+name), "spec")
		if text(path(pdb, "maxUnavailable")) != "1" || path(pdb, "minAvailable") != nil {
			t.Errorf("with %s the allocator's disruption budget is %v, want maxUnavailable 1 alone", what, pdb)
		}
	}

	docs := manifests(t, helm, "--set", "allocator.enabled=true", "--set-json", allocatorProbeJobs)
	spec := path(manifest(t, docs, "Deployment/"+name), "spec")
	if replicas := text(path(spec, "replicas")); replicas != "2" {
		t.Errorf("without discovery the allocator has %s replicas, want 2", replicas)
	}
	container := path(spec, "template", "spec", "containers", "0")
	if args := strings.Join(scalars(path(container, "args")), " "); strings.Contains(args, "--peers") || path(container, "env") != nil {
		t.Errorf("without discovery the allocator is run as a peer: %s", args)
	}
	if got := fromSelectors(docs); slices.ContainsFunc(got, func(s []string) bool { return slices.Equal(s, allocatorSelector) }) {
		t.Errorf("without discovery the allocator's NetworkPolicy admits its own pods: %v", got)
	}
}

// allocator.leaderOnly runs the peers with --peers.leader-only, after their
// other flags, so only the leader's /metrics, which the ServiceMonitor
// scrapes on every pod, has the allocation's series; left out, the flag is
// not given.
func TestChartRunsTheAllocatorLeaderOnly(t *testing.T) {
	helm := requireHelm(t)
	const name = "test-prometheus-universal-exporter-allocator"
	discoveryJob := `allocator.probeJobs=[{"name":"websites","collector":"example","discovery":[{"http_sd":{"url":"https://cmdb.example/sd"}}]}]`
	for leaderOnly, want := range map[string][]string{
		"true":  {"--peers.self=$(POD_IP)", "--peers.leader-only"},
		"false": {"--peers=" + name + ".monitoring.svc:9116", "--peers.self=$(POD_IP)"},
	} {
		docs := manifests(t, helm, "--namespace", "monitoring", "--set", "allocator.enabled=true", "--set", "allocator.leaderOnly="+leaderOnly, "--set-json", discoveryJob)
		args := scalars(path(manifest(t, docs, "Deployment/"+name), "spec", "template", "spec", "containers", "0", "args"))
		if !slices.Equal(args[len(args)-2:], want) {
			t.Errorf("with allocator.leaderOnly %s the allocator's arguments end %v, want %v", leaderOnly, args[len(args)-2:], want)
		}
	}
}

// With a discovery source the peers' timings render --peers.check-interval,
// --peers.check-timeout, --peers.snapshot-timeout and
// --peers.failure-threshold when set, after --peers.self, and nothing when
// empty or null; the pods' grace period covers a stopping pod's shutdown
// delay, a check interval and a check timeout, its 10s server shutdown
// and 5s for its exit, Kubernetes' 30 while that is less, as with the
// defaults. Without a discovery source no grace period is rendered.
func TestChartSetsThePeersTimings(t *testing.T) {
	helm := requireHelm(t)
	const name = "test-prometheus-universal-exporter-allocator"
	discoveryJob := `allocator.probeJobs=[{"name":"websites","collector":"example","discovery":[{"http_sd":{"url":"https://cmdb.example/sd"}}]}]`
	for _, c := range []struct {
		args  []string
		flags []string
		grace string
	}{
		{nil, nil, "30"},
		{[]string{"--set", "allocator.peerCheckInterval=5s", "--set", "allocator.peerFailureThreshold=5"}, []string{"--peers.check-interval=5s", "--peers.failure-threshold=5"}, "30"},
		{[]string{"--set", "allocator.peerCheckInterval=20s", "--set", "allocator.peerCheckTimeout=500ms", "--set", "allocator.peerSnapshotTimeout=2m"},
			[]string{"--peers.check-interval=20s", "--peers.check-timeout=500ms", "--peers.snapshot-timeout=2m"}, "36"},
		{[]string{"--set", "allocator.peerCheckInterval=1m", "--set", "allocator.peerCheckTimeout=59.5s", "--set", "allocator.peerSnapshotTimeout=5m", "--set", "allocator.peerFailureThreshold=20"},
			[]string{"--peers.check-interval=1m", "--peers.check-timeout=59.5s", "--peers.snapshot-timeout=5m", "--peers.failure-threshold=20"}, "135"},
		{[]string{"--set", "allocator.peerCheckInterval=", "--set", "allocator.peerFailureThreshold=null"}, nil, "30"},
	} {
		docs := manifests(t, helm, append([]string{"--namespace", "monitoring", "--set", "allocator.enabled=true", "--set-json", discoveryJob}, c.args...)...)
		spec := path(manifest(t, docs, "Deployment/"+name), "spec", "template", "spec")
		args := scalars(path(spec, "containers", "0", "args"))
		at := slices.Index(args, "--peers.self=$(POD_IP)")
		if at < 0 || !slices.Equal(args[at+1:], c.flags) {
			t.Errorf("%v: the allocator's arguments are %v, want %v after --peers.self", c.args, args, c.flags)
		}
		if got := text(path(spec, "terminationGracePeriodSeconds")); got != c.grace {
			t.Errorf("%v: the allocator's grace period is %q, want %s", c.args, got, c.grace)
		}
	}
	docs := manifests(t, helm, "--set", "allocator.enabled=true")
	if grace := path(manifest(t, docs, "Deployment/"+name), "spec", "template", "spec", "terminationGracePeriodSeconds"); grace != nil {
		t.Errorf("without discovery the allocator's grace period is %s, want Kubernetes' default", text(grace))
	}
}
