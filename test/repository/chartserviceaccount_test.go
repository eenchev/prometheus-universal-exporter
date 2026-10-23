package repository

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// serviceAccounts are the names of the ServiceAccounts a render holds.
func serviceAccounts(t *testing.T, helm string, args ...string) []string {
	t.Helper()
	var out []string
	for _, doc := range renderedDocuments(t, helm, args...) {
		if text(path(doc, "kind")) == "ServiceAccount" {
			out = append(out, text(path(doc, "metadata", "name")))
		}
	}
	slices.Sort(out)
	return out
}

// podAccount is the account a Deployment's pods run as and whether they
// mount its token.
func podAccount(t *testing.T, docs map[string]*yaml.Node, deployment string) (account, automount string) {
	t.Helper()
	spec := path(manifest(t, docs, "Deployment/"+deployment), "spec", "template", "spec")
	return text(path(spec, "serviceAccountName")), text(path(spec, "automountServiceAccountToken"))
}

// An existing ServiceAccount is named, never assumed: with
// serviceAccount.create false an empty name fails rendering, rather than
// running the pods as the namespace's default account, which other
// workloads share and kubernetesAccess would grant to, and annotations fail
// too, since the chart never changes an account it did not create. Both are
// refused by the schema and, with the schema skipped, by the templates.
// "default" written out is the operator's choice and renders.
func TestChartRefusesAnExistingServiceAccountItCannotName(t *testing.T) {
	helm := requireHelm(t)
	for what, c := range map[string]struct {
		args   []string
		schema string
		want   string
	}{
		"no name": {
			[]string{"--set", "serviceAccount.create=false"},
			"serviceAccount.name",
			"serviceAccount.create is false but serviceAccount.name is empty; name the existing ServiceAccount",
		},
		"no name with kubernetesAccess": {
			[]string{"--set", "serviceAccount.create=false", "--set", "kubernetesAccess.enabled=true", "--set-json", kubernetesAccessRules},
			"serviceAccount.name",
			"serviceAccount.name is empty",
		},
		"annotations": {
			[]string{"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=metrics", "--set", "serviceAccount.annotations.eks\\.amazonaws\\.com/role-arn=arn:aws:iam::123456789012:role/r"},
			"serviceAccount.annotations",
			"annotations apply only to a ServiceAccount the chart creates, so put them, an IRSA role or a Workload Identity binding among them, on the existing ServiceAccount metrics itself",
		},
	} {
		if out, ok := helmTemplate(t, helm, chartDir, c.args...); ok || !namesInHelmError(out, c.schema) {
			t.Errorf("%s: rendered %v, want the schema to name %s:\n%s", what, ok, c.schema, out)
		}
		if out, ok := helmTemplate(t, helm, chartDir, append([]string{"--skip-schema-validation"}, c.args...)...); ok || !strings.Contains(out, c.want) {
			t.Errorf("%s, the schema skipped: rendered %v, want %q:\n%s", what, ok, c.want, out)
		}
	}

	docs := manifests(t, helm, "--set", "serviceAccount.create=false", "--set", "serviceAccount.name=default")
	if account, _ := podAccount(t, docs, "test-prometheus-universal-exporter"); account != "default" {
		t.Errorf("name: default written out runs the pods as %s", account)
	}
	if _, ok := helmTemplate(t, helm, chartDir, "--set", "serviceAccount.annotations.a=b"); !ok {
		t.Error("annotations on the account the chart creates fail")
	}
}

// With serviceAccount.create false and a name, the chart renders no
// ServiceAccount, the exporter's pods run as the named account, the
// kubernetesAccess bindings, namespaced and cluster-wide, name it, and the
// allocator without a kubernetes source runs as it too.
func TestChartRunsTheExporterAsAnExistingServiceAccount(t *testing.T) {
	helm := requireHelm(t)
	const name = "test-prometheus-universal-exporter"
	existing := []string{"--set", "serviceAccount.create=false", "--set", "serviceAccount.name=metrics"}

	if got := serviceAccounts(t, helm, existing...); len(got) != 0 {
		t.Errorf("rendered ServiceAccounts %v, want none", got)
	}
	if account, automount := podAccount(t, manifests(t, helm, existing...), name); account != "metrics" || automount != "false" {
		t.Errorf("the pods run as %s with automount %s, want metrics without a token", account, automount)
	}

	access := append(slices.Clone(existing), "--set", "kubernetesAccess.enabled=true", "--set-json", kubernetesAccessRules)
	docs := manifests(t, helm, access...)
	if account, automount := podAccount(t, docs, name); account != "metrics" || automount != "true" {
		t.Errorf("with kubernetesAccess the pods run as %s with automount %s, want metrics with its token", account, automount)
	}
	if got := text(path(manifest(t, docs, "RoleBinding/"+name), "subjects", "0", "name")); got != "metrics" {
		t.Errorf("the RoleBinding binds %s, want metrics", got)
	}
	cluster := manifest(t, manifests(t, helm, append(slices.Clone(access), "--set", "kubernetesAccess.clusterWide=true")...), "ClusterRoleBinding/default-"+name)
	if got := text(path(cluster, "subjects", "0", "name")); got != "metrics" {
		t.Errorf("the ClusterRoleBinding binds %s, want metrics", got)
	}

	allocator := append(slices.Clone(existing), "--set", "allocator.enabled=true", "--set-json", allocatorProbeJobs)
	if account, _ := podAccount(t, manifests(t, helm, allocator...), name+"-allocator"); account != "metrics" {
		t.Errorf("the allocator without a kubernetes source runs as %s, want the exporter's metrics", account)
	}
}

// allocator.serviceAccount.name runs the allocator as an existing account,
// with or without a kubernetes source, and the chart creates no
// <fullname>-allocator. With a kubernetes source the pods mount its token
// and allocator.rbac.create binds the Roles and ClusterRole to it; with
// rbac.create false nothing is rendered. Without a kubernetes source the
// allocator still runs as it, with serviceAccount.automount's token. A key
// other than name, such as annotations, is refused.
func TestChartRunsTheAllocatorAsANamedServiceAccount(t *testing.T) {
	helm := requireHelm(t)
	const name = "test-prometheus-universal-exporter-allocator"
	sources := []string{"--set", "allocator.enabled=true", "--set", "allocator.serviceAccount.name=pue-discovery", "--set-json",
		`allocator.probeJobs=[{"name":"databases","collector":"example","target_template":"https://{{address}}/","discovery":[{"kubernetes":{"namespaces":["db"],"port":"metrics"}},{"kubernetes":{"all_namespaces":true,"port":"metrics"}}]}]`}

	docs := manifests(t, helm, sources...)
	want := []string{"ClusterRole/default-" + name, "ClusterRoleBinding/default-" + name, "Role/db/" + name, "RoleBinding/db/" + name}
	if got := allocatorRBAC(t, helm, sources...); !slices.Equal(got, want) {
		t.Errorf("rendered %v, want %v and no allocator ServiceAccount", got, want)
	}
	if account, automount := podAccount(t, docs, name); account != "pue-discovery" || automount != "true" {
		t.Errorf("the allocator runs as %s with automount %s, want pue-discovery with its token", account, automount)
	}
	for _, key := range []string{"RoleBinding/" + name, "ClusterRoleBinding/default-" + name} {
		binding := manifest(t, docs, key)
		if got := text(path(binding, "subjects", "0", "name")); got != "pue-discovery" || text(path(binding, "subjects", "0", "namespace")) != "default" {
			t.Errorf("%s binds %s, want pue-discovery", key, got)
		}
	}

	if got := allocatorRBAC(t, helm, append(slices.Clone(sources), "--set", "allocator.rbac.create=false")...); len(got) != 0 {
		t.Errorf("with allocator.rbac.create false rendered %v, want nothing", got)
	}

	plain := []string{"--set", "allocator.enabled=true", "--set", "allocator.serviceAccount.name=pue-discovery", "--set-json", allocatorProbeJobs}
	if got := allocatorRBAC(t, helm, plain...); len(got) != 0 {
		t.Errorf("without a kubernetes source rendered %v", got)
	}
	if account, automount := podAccount(t, manifests(t, helm, plain...), name); account != "pue-discovery" || automount != "false" {
		t.Errorf("without a kubernetes source the allocator runs as %s with automount %s, want pue-discovery without a token", account, automount)
	}

	if out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.serviceAccount.annotations.a=b"); ok || !strings.Contains(out, "annotations") {
		t.Errorf("a key allocator.serviceAccount does not take rendered %v:\n%s", ok, out)
	}
}

// The chart README's Service account section shows running as an existing
// account by name, which renders as it says, and the TCP, Redis and LDAP
// sections point to one TLS files subsection whose example mounts a Secret's
// ca.crt, tls.crt and tls.key and names them in the collector's tls block;
// the example renders and its configuration names the mounted files.
func TestChartReadmeShowsTheServiceAccountAndTheTLSFiles(t *testing.T) {
	helm := requireHelm(t)
	readme := readChartFile(t, "README.md")
	section := func(heading string) string {
		t.Helper()
		_, body, found := strings.Cut(readme, "\n"+heading+"\n")
		if !found {
			t.Fatalf("the chart README has no %q", heading)
		}
		level := heading[:strings.Index(heading, " ")+1]
		end := len(body)
		for _, stop := range []string{"\n## ", "\n" + level} {
			if i := strings.Index(body, stop); i >= 0 && i < end {
				end = i
			}
		}
		return body[:end]
	}

	account := section("## Service account")
	blocks := yamlBlocksOf(account)
	if len(blocks) == 0 {
		t.Fatal("the Service account section shows no values")
	}
	if got := serviceAccounts(t, helm, "-f", valuesFile(t, blocks[0])); len(got) != 0 {
		t.Errorf("the Service account example renders ServiceAccounts %v, want none", got)
	}
	for _, want := range []string{"automount", "kubernetesAccess", "annotations", "#s3-object-collectors", "allocator.serviceAccount.name"} {
		if !strings.Contains(account, want) {
			t.Errorf("the Service account section does not mention %s", want)
		}
	}

	for _, heading := range []string{"## Redis collectors", "## TCP collectors", "## LDAP collectors"} {
		if !strings.Contains(section(heading), "(#tls-files)") {
			t.Errorf("%s does not point to TLS files", heading)
		}
	}
	tls := yamlBlocksOf(section("### TLS files"))
	if len(tls) != 1 {
		t.Fatalf("the TLS files subsection shows %d examples, want 1", len(tls))
	}
	docs := manifests(t, helm, "-f", valuesFile(t, tls[0]))
	spec := path(manifest(t, docs, "Deployment/test-prometheus-universal-exporter"), "spec", "template", "spec")
	mounted := ""
	for _, mount := range entries(path(spec, "containers", "0", "volumeMounts")) {
		if text(path(mount, "name")) == "haproxy-tls" {
			mounted = text(path(mount, "mountPath"))
		}
	}
	if mounted == "" {
		t.Fatal("the TLS files example mounts no haproxy-tls volume")
	}
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal([]byte(tls[0]), &values); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Collectors []struct {
			Request struct {
				Type string            `yaml:"type"`
				TLS  map[string]string `yaml:"tls"`
			} `yaml:"request"`
		} `yaml:"collectors"`
	}
	if err := yaml.Unmarshal([]byte(values.Config.Data["config.yaml"]), &cfg); err != nil || len(cfg.Collectors) != 1 {
		t.Fatalf("the example's configuration does not parse to one collector: %v", err)
	}
	request := cfg.Collectors[0].Request
	if request.Type != "tcp" {
		t.Errorf("the example's collector has request type %q, want tcp", request.Type)
	}
	for key, file := range map[string]string{"ca_file": "ca.crt", "cert_file": "tls.crt", "key_file": "tls.key"} {
		if got := request.TLS[key]; got != mounted+"/"+file {
			t.Errorf("tls.%s is %q, want %s/%s", key, got, mounted, file)
		}
	}
}
