//go:build !select_request_types || request_type_kubernetes

package repository

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/kubetest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/KUBERNETES.md work as written against a stand-in API
// server, each collector pointed at it with api_server and its CA and token
// files, where the page's collectors reach their own cluster's: the main
// collector's and the Deployments' replicas, the Certificates' readiness and
// expiry, the Pods of every phase, the nodes' conditions, allocatable and
// capacity; the page's other collector, outside the
// cluster, loads; and so does the static target against them.
func TestKubernetesDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/KUBERNETES.md", "yaml")
	api := kubetest.New(t)
	deployments := kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "cart", "namespace": "shop"}, "spec": map[string]any{"replicas": 3}, "status": map[string]any{"availableReplicas": 3}},
		{"metadata": map[string]any{"name": "web", "namespace": "shop"}, "spec": map[string]any{"replicas": 2}, "status": map[string]any{}},
	}}
	api.Serve("/apis/apps/v1/namespaces/shop/deployments", deployments)
	api.Serve("/apis/cert-manager.io/v1/certificates", kubetest.List{Kind: "CertificateList", APIVersion: "cert-manager.io/v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "web", "namespace": "shop"}, "status": map[string]any{"notAfter": "2027-01-02T03:04:05Z", "conditions": []map[string]any{{"type": "Ready", "status": "True"}}}},
		{"metadata": map[string]any{"name": "api", "namespace": "shop"}, "status": map[string]any{"conditions": []map[string]any{{"type": "Ready", "status": "False"}}}},
	}})
	api.Serve("/api/v1/pods", kubetest.List{Kind: "PodList", APIVersion: "v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "a"}, "status": map[string]any{"phase": "Running"}},
		{"metadata": map[string]any{"name": "b"}, "status": map[string]any{"phase": "Running"}},
		{"metadata": map[string]any{"name": "c"}, "status": map[string]any{"phase": "Failed"}},
	}})
	api.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "n1"}, "status": map[string]any{
			"conditions":  []map[string]any{{"type": "Ready", "status": "True"}, {"type": "DiskPressure", "status": "False"}},
			"capacity":    map[string]any{"cpu": "4", "memory": "16Gi", "ephemeral-storage": "100Gi", "pods": "110"},
			"allocatable": map[string]any{"cpu": "3800m", "memory": "15Gi", "ephemeral-storage": "90Gi", "pods": "110"},
		}},
		{"metadata": map[string]any{"name": "n2"}, "status": map[string]any{"conditions": []map[string]any{{"type": "DiskPressure", "status": "True"}}}},
	}})
	// Each collector reaches the stand-in as one outside the cluster
	// reaches its API server.
	reach := "      type: kubernetes\n      api_server: " + api.URL + "\n      tls:\n        ca_file: " + filepath.Join(api.Dir, "ca.crt") + "\n      bearer_token_file: " + filepath.Join(api.Dir, "token") + "\n"
	conf := pageBlock(t, blocks, "collectors:\n") + pageBlock(t, blocks, "  - name: deployment_replicas\n") + pageBlock(t, blocks, "  - name: certificates\n") + pageBlock(t, blocks, "  - name: pod_phases\n") + pageBlock(t, blocks, "  - name: node_status\n")
	conf = strings.ReplaceAll(conf, "      type: kubernetes\n", reach)
	dir := t.TempDir()
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for collector, wants := range map[string][]string{
		"deployments":         {`kube_deployment_spec_replicas{deployment="cart",namespace="shop"} 3`, `kube_deployment_status_replicas_available{deployment="web",namespace="shop"} 0`},
		"deployment_replicas": {`kube_deployment_fully_available{deployment="cart"} 1`, `kube_deployment_fully_available{deployment="web"} 0`},
		"certificates": {
			`certmanager_certificate_ready{certificate="web",namespace="shop"} 1`, `certmanager_certificate_ready{certificate="api",namespace="shop"} 0`,
			`certmanager_certificate_expiration_timestamp_seconds{certificate="web",namespace="shop"} 1.798859045e+09`,
		},
		"pod_phases": {`kube_pods{phase="Running"} 2`, `kube_pods{phase="Failed"} 1`, `kube_pods{phase="Pending"} 0`, `kube_pods{phase="Unknown"} 0`},
		"node_status": {
			`kube_node_condition{condition="Ready",node="n1"} 1`, `kube_node_condition{condition="DiskPressure",node="n1"} 0`, `kube_node_condition{condition="DiskPressure",node="n2"} 1`,
			`kube_node_allocatable{node="n1",resource="cpu"} 3.8`, `kube_node_allocatable{node="n1",resource="ephemeral-storage"} 9.663676416e+10`, `kube_node_capacity{node="n1",resource="memory"} 1.7179869184e+10`,
			`kube_node_capacity{node="n1",resource="pods"} 110`,
		},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector="+collector, nil))
		for _, want := range wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s: %d, want %q in\n%s", collector, recorder.Code, want, recorder.Body)
			}
		}
	}
	if strings.Contains(probed(exp, "certificates"), `certificate="api",namespace="shop"} 1.`) {
		t.Error("a Certificate without notAfter has an expiry")
	}

	outside := "collectors:\n  - name: nodes\n    request:\n" + pageBlock(t, blocks, "    request:\n      type: kubernetes\n      api_server: ")[len("    request:\n"):] + "    transform:\n      type: jq\n    metrics:\n      - name: kube_nodes\n        expression: .count\n"
	if _, err := config.Load(testutil.WriteIn(t, dir, "outside.yaml", outside)); err != nil {
		t.Errorf("%v\n%s", err, outside)
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", pageBlock(t, blocks, "interval: 1m\ntargets:\n")))
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// probed is the exposition of a probe of collector.
func probed(exp *exporter.Server, collector string) string {
	r := httptest.NewRecorder()
	exp.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/probe?collector="+collector, nil))
	return r.Body.String()
}

// Every kubernetesAccess snippet of docs/KUBERNETES.md renders with the
// chart as written, and grants something: the grant beside each example
// is one the chart takes.
func TestTheKubernetesPagesGrantsRender(t *testing.T) {
	helm := requireHelm(t)
	found := 0
	for _, block := range pageBlocks(t, "docs/KUBERNETES.md", "yaml") {
		if !strings.HasPrefix(block, "kubernetesAccess:\n") {
			continue
		}
		found++
		values := testutil.WriteIn(t, t.TempDir(), "values.yaml", block)
		out, ok := helmTemplate(t, helm, chartDir, "-f", values)
		if !ok || !strings.Contains(out, "kind: ClusterRole") && !strings.Contains(out, "kind: Role") {
			t.Errorf("the grant\n%s\ndoes not render a Role:\n%s", block, out)
		}
	}
	if found < 6 {
		t.Errorf("found %d grants in docs/KUBERNETES.md, want one beside each example that needs it", found)
	}
}
