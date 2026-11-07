//go:build !select_request_types || request_type_kubernetes

package exporter

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/kubetest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// kubernetesCollectors are a collector of the Deployments of the shop
// namespace, exporting each one's desired and available replicas, and one
// of the Pods of the cluster, counting them by phase, both reaching the
// API server SERVER with its CA and token files.
const kubernetesCollectors = `web:
  self_metrics:
    verbose: true
collectors:
  - name: deployments
    request:
      type: kubernetes
      api_server: SERVER
      tls:
        ca_file: DIR/ca.crt
      bearer_token_file: DIR/token
      group: apps
      resource: deployments
      namespaces: [shop]
    transform:
      type: jq
    metrics:
      - name: kube_deployment_spec_replicas
        items: .items[]
        expression: .spec.replicas
        labels:
          - name: deployment
            expression: .metadata.name
      - name: kube_deployment_status_replicas_available
        items: .items[]
        expression: .status.availableReplicas // 0
        labels:
          - name: deployment
            expression: .metadata.name
  - name: pods
    request:
      type: kubernetes
      api_server: SERVER
      tls:
        ca_file: DIR/ca.crt
      bearer_token_file: DIR/token
      resource: pods
      all_namespaces: true
    transform:
      type: jq
    metrics:
      - name: kube_pods
        items: '[.items[].status.phase] | group_by(.) | map({phase: .[0], count: length}) | .[]'
        expression: .count
        labels:
          - name: phase
            expression: .phase
`

// kubernetesExporter is an exporter of kubernetesCollectors and the
// stand-in API server they list, which has two Deployments in shop and
// three Pods, two of them running.
func kubernetesExporter(t *testing.T) (*Server, *kubetest.Server) {
	t.Helper()
	api := kubetest.New(t)
	api.Serve("/apis/apps/v1/namespaces/shop/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "cart", "namespace": "shop"}, "spec": map[string]any{"replicas": 3}, "status": map[string]any{"availableReplicas": 2}},
		{"metadata": map[string]any{"name": "web", "namespace": "shop"}, "spec": map[string]any{"replicas": 2}, "status": map[string]any{}},
	}})
	api.Serve("/api/v1/pods", kubetest.List{Kind: "PodList", APIVersion: "v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "a"}, "status": map[string]any{"phase": "Running"}},
		{"metadata": map[string]any{"name": "b"}, "status": map[string]any{"phase": "Pending"}},
		{"metadata": map[string]any{"name": "c"}, "status": map[string]any{"phase": "Running"}},
	}})
	document := strings.NewReplacer("SERVER", api.URL, "DIR", filepath.ToSlash(api.Dir)).Replace(kubernetesCollectors)
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", document)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t)), api
}

// A kubernetes collector's probe, without a target, gives the series its
// jq rules read from the listing: each Deployment's desired and available
// replicas, a missing count read as 0, and the Pods by phase. The
// collectors page asks for no target, and the verbose self-metrics label
// the listing with the API server's URL and path, and LIST.
func TestAKubernetesCollectorProbe(t *testing.T) {
	server, api := kubernetesExporter(t)
	recorder := probeOnce(t, server, "/probe?collector=deployments", nil)
	want := []string{
		`kube_deployment_spec_replicas{deployment="cart"} 3`,
		`kube_deployment_spec_replicas{deployment="web"} 2`,
		`kube_deployment_status_replicas_available{deployment="cart"} 2`,
		`kube_deployment_status_replicas_available{deployment="web"} 0`,
	}
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v", recorder.Code, got, want)
	}
	recorder = probeOnce(t, server, "/probe?collector=pods", nil)
	want = []string{`kube_pods{phase="Pending"} 1`, `kube_pods{phase="Running"} 2`}
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v", recorder.Code, got, want)
	}
	// The collectors page offers no target to fill in, and says so.
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, `placeholder="none: leave it out" disabled>`) {
		t.Errorf("the collectors page offers a target:\n%s", page)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="deployments",http_method="LIST",url="` + api.URL + `/apis/apps/v1/deployments"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
}

// A debug probe shows the listing's request, its token masked, and the
// document; a probe with a target is answered 400, and one the service
// account may not list fails saying which permission to grant.
func TestADebugProbeOfAKubernetesCollector(t *testing.T) {
	server, api := kubernetesExporter(t)
	server.SetDebug(DebugFull)
	body := probeOnce(t, server, "/probe?debug=true&collector=deployments", nil).Body.String()
	for _, want := range []string{"1. GET " + api.URL + "/apis/apps/v1/namespaces/shop/deployments?limit=", "-> 200 OK", `"kind":"Deployment"`, `"count":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	if strings.Contains(body, "token-1") {
		t.Errorf("the report shows the token:\n%s", body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=deployments&target=kube-system", nil); recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "leaves target out") {
		t.Errorf("a target: %d %s", recorder.Code, recorder.Body)
	}
	api.Forbid("/api/v1/pods")
	if recorder := probeOnce(t, server, "/probe?collector=pods", nil); recorder.Code == http.StatusOK || !strings.Contains(recorder.Body.String(), "kubernetesAccess.rules") {
		t.Errorf("forbidden: %d %s", recorder.Code, recorder.Body)
	}
}

// A kubernetes collector's label values take the probe's param_<name>
// parameters, as every type's do, and a python transform reads its
// document as data.
func TestAKubernetesCollectorsLabelValuesAndPython(t *testing.T) {
	requirePython(t)
	api := kubetest.New(t)
	api.Serve("/api/v1/namespaces/shop/pods", kubetest.List{Kind: "PodList", APIVersion: "v1", Items: []map[string]any{{"metadata": map[string]any{"name": "a"}}}})
	source := `      api_server: ` + api.URL + `
      tls:
        ca_file: ` + filepath.Join(api.Dir, "ca.crt") + `
      bearer_token_file: ` + filepath.Join(api.Dir, "token") + `
      resource: pods
      namespaces: [shop]
`
	server := labelServer(t, `collectors:
  - name: pods
    request:
      type: kubernetes
`+source+`    transform:
      type: jq
    metrics:
      - name: kube_pods
        expression: .count
        labels:
          - name: env
            value: "{{param_env:dev}}"
  - name: pods_python
    request:
      type: kubernetes
`+source+`    transform:
      type: python
      script: |
        for pod in data["items"]:
            metric(name="kube_pod_info", type="gauge", value=1, labels={"pod": pod["metadata"]["name"], "kind": data["kind"]})
    metrics: []
`)
	for query, want := range map[string]string{
		"/probe?collector=pods&param_env=prod": `kube_pods{env="prod"} 1`,
		"/probe?collector=pods":                `kube_pods{env="dev"} 1`,
		"/probe?collector=pods_python":         `kube_pod_info{kind="Pod",pod="a"} 1`,
	} {
		recorder := probeOnce(t, server, query, nil)
		if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || len(got) != 1 || got[0] != want {
			t.Errorf("%s: %d %v, want %s", query, recorder.Code, got, want)
		}
	}
}

// Ephemeral storage from configuration alone: a collector with kubelet:
// stats/summary exports what each pod uses, from its node's kubelet asked
// with the API server's CA and token, and the nodes whose kubelet
// answered none; one of the Pods exports what each container requests,
// its quantity read by parse_quantity.
func TestEphemeralStorageFromTheKubeletsAndThePods(t *testing.T) {
	api := kubetest.New(t)
	kubelet := kubetest.New(t)
	kubelet.Answer("/stats/summary", `{"node":{"nodeName":"node-0"},"pods":[{"podRef":{"name":"web","namespace":"shop"},"ephemeral-storage":{"usedBytes":1048576}}]}`)
	u, err := url.Parse(kubelet.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	api.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "node-0"}, "status": map[string]any{"addresses": []any{map[string]any{"type": "InternalIP", "address": u.Hostname()}}, "daemonEndpoints": map[string]any{"kubeletEndpoint": map[string]any{"Port": port}}}},
		{"metadata": map[string]any{"name": "node-1"}, "status": map[string]any{}},
	}})
	api.Serve("/api/v1/pods", kubetest.List{Kind: "PodList", APIVersion: "v1", Items: []map[string]any{
		{"metadata": map[string]any{"name": "web"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "resources": map[string]any{"requests": map[string]any{"ephemeral-storage": "2Gi", "cpu": "250m"}}}}}},
	}})
	source := `      api_server: ` + api.URL + `
      tls:
        ca_file: ` + filepath.Join(api.Dir, "ca.crt") + `
      bearer_token_file: ` + filepath.Join(api.Dir, "token") + `
      all_namespaces: true
`
	server := labelServer(t, `collectors:
  - name: ephemeral
    request:
      type: kubernetes
`+source+`      resource: nodes
      kubelet: stats/summary
    transform:
      type: jq
    metrics:
      - name: kube_pod_ephemeral_storage_used_bytes
        items: '.items[].pods[]'
        expression: '."ephemeral-storage".usedBytes'
        labels:
          - name: namespace
            expression: .podRef.namespace
          - name: pod
            expression: .podRef.name
      - name: kube_kubelet_summaries_failed
        expression: .failed | length
  - name: requests
    request:
      type: kubernetes
`+source+`      resource: pods
    transform:
      type: jq
    metrics:
      - name: kube_pod_container_ephemeral_storage_request_bytes
        items: '.items[] | .metadata.name as $pod | .spec.containers[] | {pod: $pod, container: .name, resources}'
        expression: '.resources.requests["ephemeral-storage"] | parse_quantity'
        labels:
          - name: pod
            expression: .pod
      - name: kube_pod_container_cpu_request_cores
        items: '.items[].spec.containers[]'
        expression: '.resources.requests.cpu | parse_quantity'
`)
	for query, want := range map[string][]string{
		"/probe?collector=ephemeral": {`kube_kubelet_summaries_failed 1`, `kube_pod_ephemeral_storage_used_bytes{namespace="shop",pod="web"} 1.048576e+06`},
		"/probe?collector=requests":  {`kube_pod_container_cpu_request_cores 0.25`, `kube_pod_container_ephemeral_storage_request_bytes{pod="web"} 2.147483648e+09`},
	} {
		recorder := probeOnce(t, server, query, nil)
		if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: %d %v, want %v\n%s", query, recorder.Code, got, want, recorder.Body)
		}
	}
}
