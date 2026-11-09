package repository

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// exporterRBAC are the RBAC objects rendered for the exporter's own
// ServiceAccount, by "Kind/name" or "Kind/namespace/name" for a Role and a
// RoleBinding, leaving the allocator's out.
func exporterRBAC(t *testing.T, helm string, args ...string) []string {
	t.Helper()
	var out []string
	for _, doc := range renderedDocuments(t, helm, args...) {
		kind, name := text(path(doc, "kind")), text(path(doc, "metadata", "name"))
		if strings.HasSuffix(name, "-allocator") {
			continue
		}
		switch kind {
		case "Role", "RoleBinding":
			out = append(out, kind+"/"+text(path(doc, "metadata", "namespace"))+"/"+name)
		case "ClusterRole", "ClusterRoleBinding":
			out = append(out, kind+"/"+name)
		}
	}
	slices.Sort(out)
	return out
}

// kubernetesAccessRules are rules for a Deployment collector and a Pod one.
const kubernetesAccessRules = `kubernetesAccess.rules=[{"apiGroups":["apps"],"resources":["deployments"]},{"apiGroups":[""],"resources":["pods"]}]`

// kubernetesAccess lets the exporter's own ServiceAccount list, and only
// list, the resources its rules name, with a Role and a RoleBinding in each
// namespace it lists, the release's when it lists none, or with clusterWide
// a ClusterRole and a ClusterRoleBinding; the pods and the ServiceAccount
// then mount the token. Without it nothing is granted and no token is
// mounted, and an extra mount at the token's directory, which would hide
// the token, fails rendering with it.
func TestChartGrantsTheExporterTheKubernetesAccessItsCollectorsNeed(t *testing.T) {
	tier.Slow(t)
	helm := requireHelm(t)
	const name = "test-prometheus-universal-exporter"
	automount := func(docs map[string]*yaml.Node) (pod, account string) {
		return text(path(manifest(t, docs, "Deployment/"+name), "spec", "template", "spec", "automountServiceAccountToken")),
			text(path(manifest(t, docs, "ServiceAccount/"+name), "automountServiceAccountToken"))
	}

	if got := exporterRBAC(t, helm); len(got) != 0 {
		t.Errorf("without kubernetesAccess rendered %v", got)
	}
	if pod, account := automount(manifests(t, helm)); pod != "false" || account != "false" {
		t.Errorf("without kubernetesAccess the pod mounts its token: %s, and the account: %s", pod, account)
	}

	namespaced := []string{"--set", "kubernetesAccess.enabled=true", "--set-json", kubernetesAccessRules, "--set-json", `kubernetesAccess.namespaces=["shop","billing"]`}
	want := []string{"Role/billing/" + name, "Role/shop/" + name, "RoleBinding/billing/" + name, "RoleBinding/shop/" + name}
	if got := exporterRBAC(t, helm, namespaced...); !slices.Equal(got, want) {
		t.Errorf("rendered %v, want %v", got, want)
	}
	if pod, account := automount(manifests(t, helm, namespaced...)); pod != "true" || account != "true" {
		t.Errorf("with kubernetesAccess the pod mounts its token: %s, and the account: %s", pod, account)
	}
	for _, doc := range renderedDocuments(t, helm, namespaced...) {
		switch text(path(doc, "kind")) {
		case "Role":
			rules := entries(path(doc, "rules"))
			if len(rules) != 2 || !slices.Equal(scalars(path(rules[0], "apiGroups")), []string{"apps"}) || !slices.Equal(scalars(path(rules[0], "resources")), []string{"deployments"}) ||
				!slices.Equal(scalars(path(rules[1], "apiGroups")), []string{""}) || !slices.Equal(scalars(path(rules[1], "resources")), []string{"pods"}) {
				t.Errorf("the Role allows %v", rules)
			}
			for _, rule := range rules {
				if !slices.Equal(scalars(path(rule, "verbs")), []string{"list"}) {
					t.Errorf("a rule allows %v, want list alone", scalars(path(rule, "verbs")))
				}
			}
		case "RoleBinding":
			if text(path(doc, "roleRef", "name")) != name || text(path(doc, "subjects", "0", "name")) != name || text(path(doc, "subjects", "0", "namespace")) != "default" {
				t.Error("the RoleBinding does not bind the Role to the exporter's ServiceAccount")
			}
		}
	}

	release := []string{"--set", "kubernetesAccess.enabled=true", "--set-json", kubernetesAccessRules}
	if got, want := exporterRBAC(t, helm, release...), []string{"Role/default/" + name, "RoleBinding/default/" + name}; !slices.Equal(got, want) {
		t.Errorf("without namespaces rendered %v, want %v", got, want)
	}
	account := append(slices.Clone(release), "--set", "serviceAccount.create=false", "--set", "serviceAccount.name=metrics")
	for _, doc := range renderedDocuments(t, helm, account...) {
		if text(path(doc, "kind")) == "RoleBinding" && text(path(doc, "subjects", "0", "name")) != "metrics" {
			t.Errorf("the RoleBinding binds %s, want the account the pods run as, metrics", text(path(doc, "subjects", "0", "name")))
		}
	}

	cluster := append(slices.Clone(release), "--set", "kubernetesAccess.clusterWide=true")
	if got, want := exporterRBAC(t, helm, cluster...), []string{"ClusterRole/default-" + name, "ClusterRoleBinding/default-" + name}; !slices.Equal(got, want) {
		t.Errorf("clusterWide rendered %v, want %v", got, want)
	}
	binding := manifest(t, manifests(t, helm, cluster...), "ClusterRoleBinding/default-"+name)
	if text(path(binding, "roleRef", "kind")) != "ClusterRole" || text(path(binding, "subjects", "0", "name")) != name {
		t.Error("the ClusterRoleBinding does not bind the ClusterRole to the exporter's ServiceAccount")
	}
	// kubelet adds get on the nodes' stats, or their proxy, to the
	// ClusterRole's rules; without it, the rules alone.
	for kubelet, want := range map[string]string{"": "", "stats": "nodes/stats", "proxy": "nodes/proxy"} {
		rules := entries(path(manifest(t, manifests(t, helm, append(slices.Clone(cluster), "--set", "kubernetesAccess.kubelet="+kubelet)...), "ClusterRole/default-"+name), "rules"))
		if want == "" {
			if len(rules) != 2 {
				t.Errorf("without kubelet the ClusterRole allows %v", rules)
			}
			continue
		}
		if len(rules) != 3 || !slices.Equal(scalars(path(rules[2], "apiGroups")), []string{""}) || !slices.Equal(scalars(path(rules[2], "resources")), []string{want}) || !slices.Equal(scalars(path(rules[2], "verbs")), []string{"get"}) {
			t.Errorf("kubelet %s: the ClusterRole allows %v, want get on %s after the rules", kubelet, rules, want)
		}
	}

	mount := append(slices.Clone(release), "--set-json", `extraVolumes=[{"name":"x","emptyDir":{}}]`, "--set-json", `extraVolumeMounts=[{"name":"x","mountPath":"/var/run/secrets/kubernetes.io/serviceaccount"}]`)
	if out, ok := helmTemplate(t, helm, chartDir, mount...); ok || !strings.Contains(out, "the service account token directory") {
		t.Errorf("a mount at the token's directory rendered:\n%s", out)
	}
	if _, ok := helmTemplate(t, helm, chartDir, mount[4:]...); !ok {
		t.Error("without kubernetesAccess a mount at the token's directory fails")
	}
}

// What kubernetesAccess may not grant fails rendering, by the schema and,
// with the schema skipped, by the templates: no rules, a wildcard, the core
// group's secrets, namespaces beside clusterWide, and kubelet without
// clusterWide or other than stats or proxy.
func TestChartRefusesKubernetesAccessThatGrantsTooMuchOrNothing(t *testing.T) {
	tier.Slow(t)
	helm := requireHelm(t)
	for what, c := range map[string]struct {
		args []string
		want string
	}{
		"no rules":              {[]string{"--set", "kubernetesAccess.enabled=true"}, "kubernetesAccess.rules lists nothing"},
		"a group wildcard":      {[]string{"--set", "kubernetesAccess.enabled=true", "--set-json", `kubernetesAccess.rules=[{"apiGroups":["*"],"resources":["pods"]}]`}, `apiGroups has "*"`},
		"a resource wildcard":   {[]string{"--set", "kubernetesAccess.enabled=true", "--set-json", `kubernetesAccess.rules=[{"apiGroups":["apps"],"resources":["*"]}]`}, `resources has "*"`},
		"secrets":               {[]string{"--set", "kubernetesAccess.enabled=true", "--set-json", `kubernetesAccess.rules=[{"apiGroups":["apps",""],"resources":["deployments","secrets"]}]`}, "grants secrets of the core group"},
		"both scopes":           {[]string{"--set", "kubernetesAccess.enabled=true", "--set", "kubernetesAccess.clusterWide=true", "--set-json", `kubernetesAccess.namespaces=["a"]`, "--set-json", kubernetesAccessRules}, "both clusterWide and namespaces"},
		"kubelet in namespaces": {[]string{"--set", "kubernetesAccess.enabled=true", "--set", "kubernetesAccess.kubelet=stats", "--set-json", kubernetesAccessRules}, "kubernetesAccess.kubelet needs kubernetesAccess.clusterWide"},
		"another kubelet":       {[]string{"--set", "kubernetesAccess.enabled=true", "--set", "kubernetesAccess.clusterWide=true", "--set", "kubernetesAccess.kubelet=exec", "--set-json", kubernetesAccessRules}, `kubernetesAccess.kubelet is "exec"`},
	} {
		if out, ok := helmTemplate(t, helm, chartDir, c.args...); ok {
			t.Errorf("%s: rendered", what)
		} else if what != "no rules" && !strings.Contains(out, "kubernetesAccess") {
			t.Errorf("%s: the schema refuses it for something else:\n%s", what, out)
		}
		if out, ok := helmTemplate(t, helm, chartDir, append([]string{"--skip-schema-validation"}, c.args...)...); ok || !strings.Contains(out, c.want) {
			t.Errorf("%s, the schema skipped: rendered %v\n%s", what, ok, out)
		}
	}
	if _, ok := helmTemplate(t, helm, chartDir, "--set", "kubernetesAccess.enabled=true", "--set-json", `kubernetesAccess.rules=[{"apiGroups":["example.com"],"resources":["secrets"]}]`); !ok {
		t.Error("secrets of another group, a custom resource's, are refused")
	}
}

// kubernetesAccess.extraRules are added as written, with their verbs and
// resourceNames, to the Role or ClusterRole after the chart's own; they
// alone are enough to enable the access. "*" and the core group's secrets
// are refused, by the schema and by the templates alike, unless
// allowBroadExtraRules grants them as written.
func TestChartAddsTheOperatorsExtraKubernetesRules(t *testing.T) {
	tier.Slow(t)
	helm := requireHelm(t)
	extra := `kubernetesAccess.extraRules=[{"apiGroups":[""],"resources":["configmaps"],"verbs":["get","watch"],"resourceNames":["cluster-info"]}]`
	for _, scope := range [][]string{{"--set", "kubernetesAccess.clusterWide=true"}, {"--set-json", `kubernetesAccess.namespaces=["shop"]`}} {
		args := append([]string{"--set", "kubernetesAccess.enabled=true", "--set-json", kubernetesAccessRules, "--set-json", extra}, scope...)
		found := false
		for _, doc := range renderedDocuments(t, helm, args...) {
			kind := text(path(doc, "kind"))
			if (kind != "Role" && kind != "ClusterRole") || strings.HasSuffix(text(path(doc, "metadata", "name")), "-allocator") {
				continue
			}
			rules := entries(path(doc, "rules"))
			last := rules[len(rules)-1]
			if len(rules) != 3 || !slices.Equal(scalars(path(last, "verbs")), []string{"get", "watch"}) || !slices.Equal(scalars(path(last, "resourceNames")), []string{"cluster-info"}) ||
				!slices.Equal(scalars(path(last, "resources")), []string{"configmaps"}) {
				t.Errorf("%v: the %s allows %v", scope, kind, rules)
			}
			found = true
		}
		if !found {
			t.Errorf("%v: no Role or ClusterRole rendered", scope)
		}
	}
	if got := exporterRBAC(t, helm, "--set", "kubernetesAccess.enabled=true", "--set-json", extra); len(got) != 2 {
		t.Errorf("extraRules alone rendered %v, want a Role and its RoleBinding", got)
	}
	for what, c := range map[string]struct{ rule, want string }{
		"a verb wildcard":     {`{"apiGroups":["apps"],"resources":["deployments"],"verbs":["*"]}`, `extraRules[0].verbs has "*"`},
		"a resource wildcard": {`{"apiGroups":["apps"],"resources":["*"],"verbs":["get"]}`, `extraRules[0].resources has "*"`},
		"secrets":             {`{"apiGroups":[""],"resources":["secrets"],"verbs":["get"]}`, "extraRules[0] grants secrets of the core group"},
		"no verbs":            {`{"apiGroups":["apps"],"resources":["deployments"]}`, "extraRules[0] needs apiGroups"},
	} {
		args := []string{"--set", "kubernetesAccess.enabled=true", "--set-json", "kubernetesAccess.extraRules=[" + c.rule + "]"}
		if _, ok := helmTemplate(t, helm, chartDir, args...); ok {
			t.Errorf("%s: rendered", what)
		}
		if out, ok := helmTemplate(t, helm, chartDir, append([]string{"--skip-schema-validation"}, args...)...); ok || !strings.Contains(out, c.want) {
			t.Errorf("%s, the schema skipped: rendered %v\n%s", what, ok, out)
		}
		if what == "no verbs" {
			continue
		}
		if _, ok := helmTemplate(t, helm, chartDir, append(args, "--set", "kubernetesAccess.allowBroadExtraRules=true")...); !ok {
			t.Errorf("%s with allowBroadExtraRules: refused", what)
		}
	}
}
