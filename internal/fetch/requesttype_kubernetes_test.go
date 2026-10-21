//go:build !select_request_types || request_type_kubernetes

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/kubetest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// kubeAPI is a stand-in API server the exporter's pod reaches as its own
// cluster's, with the service account directory the server wrote.
func kubeAPI(t *testing.T) *kubetest.Server {
	t.Helper()
	s := kubetest.New(t)
	s.InCluster(t)
	old := kubernetesServiceAccountDir
	kubernetesServiceAccountDir = s.Dir
	t.Cleanup(func() { kubernetesServiceAccountDir = old })
	return s
}

// deployments are n Deployments of namespace, each with managed fields.
func deployments(namespace string, n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{
			"metadata": map[string]any{
				"name": fmt.Sprintf("app-%03d", i), "namespace": namespace, "labels": map[string]string{"app": "shop"},
				"managedFields": []map[string]any{{"manager": "kubectl", "operation": "Update", "fieldsV1": map[string]any{"f:spec": map[string]any{}}}},
			},
			"spec":   map[string]any{"replicas": 3},
			"status": map[string]any{"replicas": 3, "availableReplicas": i % 4},
		}
	}
	return out
}

// kubernetesCollector is a validated kubernetes collector of resource in
// group.
func kubernetesCollector(t *testing.T, group, resource string, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "kube", Request: model.RequestConfig{Type: RequestTypeKubernetes, Group: group, Resource: resource}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// listDoc is a scrape of c, and its document.
func listDoc(t *testing.T, c *model.Collector) (kubernetesDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, "", c, RequestOverrides{}, nil)
	if err != nil {
		return kubernetesDocument{}, err
	}
	if resp.Headers.Get("Content-Type") != "application/json" || !resp.NoStatus {
		t.Errorf("response %+v", resp)
	}
	var doc kubernetesDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v\n%s", err, resp.Body)
	}
	return doc, nil
}

// itemNames are the namespace/name of each object of doc.
func itemNames(t *testing.T, doc kubernetesDocument) []string {
	t.Helper()
	var out []string
	for _, raw := range doc.Items {
		var item struct {
			Metadata struct{ Name, Namespace string } `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		out = append(out, item.Metadata.Namespace+"/"+item.Metadata.Name)
	}
	return out
}

// A kubernetes collector lists its resource in the exporter's own
// namespace, from the service account's namespace file, a page of 100 at a
// time with the continue token of each, with the service account's token
// and its selectors, and answers every object as a JSON document: the
// objects' kind and apiVersion, their count and the objects, each without
// its managed fields and otherwise as the API server sent it.
func TestAKubernetesCollectorListsEveryPageAsADocument(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/apis/apps/v1/namespaces/monitoring/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("monitoring", 250)})
	c := kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) {
		c.Request.LabelSelector, c.Request.FieldSelector = "app=shop", "metadata.name!=other"
	})
	doc, err := listDoc(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "Deployment" || doc.APIVersion != "apps/v1" || doc.Count != 250 || len(doc.Items) != 250 {
		t.Fatalf("kind %q, api_version %q, count %d, %d items", doc.Kind, doc.APIVersion, doc.Count, len(doc.Items))
	}
	names := itemNames(t, doc)
	if names[0] != "monitoring/app-000" || names[249] != "monitoring/app-249" || !slices.IsSorted(names) {
		t.Errorf("listed %v ... %v", names[:2], names[248:])
	}
	for _, raw := range doc.Items {
		if strings.Contains(string(raw), "managedFields") || !strings.Contains(string(raw), `"availableReplicas"`) || !strings.Contains(string(raw), `"labels":{"app":"shop"}`) {
			t.Fatalf("an object is %s", raw)
		}
	}
	requests := s.Requests()
	if len(requests) != 3 {
		t.Fatalf("%d requests, want one per page of 100", len(requests))
	}
	for i, r := range requests {
		want := []string{"", "100", "200"}[i]
		if r.Query.Get("continue") != want || r.Query.Get("limit") != "100" || r.Query.Get("labelSelector") != "app=shop" || r.Query.Get("fieldSelector") != "metadata.name!=other" || r.Authorization != "Bearer token-1" {
			t.Errorf("request %d: %+v", i, r)
		}
	}
}

// Listed namespaces are listed each at its own path, in the order named;
// all_namespaces lists the resource across the cluster, which is how a
// cluster-scoped resource is listed; the core group's paths are under
// /api, and an empty listing is a document of no objects.
func TestAKubernetesCollectorListsTheNamespacesItNames(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/apis/apps/v1/namespaces/shop/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("shop", 2)})
	s.Serve("/apis/apps/v1/namespaces/billing/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("billing", 1)})
	s.Serve("/apis/apps/v1/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: append(deployments("billing", 1), deployments("shop", 1)...)})
	s.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{{"metadata": map[string]any{"name": "node-1"}}}})
	s.Serve("/api/v1/namespaces/monitoring/pods", kubetest.List{Kind: "PodList", APIVersion: "v1"})

	doc, err := listDoc(t, kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) { c.Request.Namespaces = []string{"shop", "billing"} }))
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(t, doc); !slices.Equal(got, []string{"shop/app-000", "shop/app-001", "billing/app-000"}) || doc.Count != 3 {
		t.Errorf("listed %v", got)
	}
	doc, err = listDoc(t, kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) { c.Request.AllNamespaces = true }))
	if err != nil || doc.Count != 2 {
		t.Errorf("every namespace: %+v, %v", doc, err)
	}
	doc, err = listDoc(t, kubernetesCollector(t, "", "nodes", func(c *model.Collector) { c.Request.AllNamespaces = true }))
	if err != nil || doc.Kind != "Node" || doc.APIVersion != "v1" || !slices.Equal(itemNames(t, doc), []string{"/node-1"}) {
		t.Errorf("nodes: %+v, %v", doc, err)
	}
	doc, err = listDoc(t, kubernetesCollector(t, "", "pods"))
	if err != nil || doc.Kind != "Pod" || doc.Count != 0 || doc.Items == nil {
		t.Errorf("no pods: %+v, %v", doc, err)
	}
	var paths []string
	for _, r := range s.Requests() {
		paths = append(paths, r.Path)
	}
	want := []string{"/apis/apps/v1/namespaces/shop/deployments", "/apis/apps/v1/namespaces/billing/deployments", "/apis/apps/v1/deployments", "/api/v1/nodes", "/api/v1/namespaces/monitoring/pods"}
	if !slices.Equal(paths, want) {
		t.Errorf("asked %v, want %v", paths, want)
	}
}

// The token is read again at every scrape, so a rotated one is sent.
func TestAKubernetesCollectorReadsTheTokenAgain(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/api/v1/namespaces/monitoring/pods", kubetest.List{Kind: "PodList", APIVersion: "v1"})
	c := kubernetesCollector(t, "", "pods")
	if _, err := listDoc(t, c); err != nil {
		t.Fatal(err)
	}
	s.Write(t, "token", "token-2\n")
	if _, err := listDoc(t, c); err != nil {
		t.Fatal(err)
	}
	requests := s.Requests()
	if requests[0].Authorization != "Bearer token-1" || requests[1].Authorization != "Bearer token-2" {
		t.Errorf("sent %q, then %q", requests[0].Authorization, requests[1].Authorization)
	}
}

// A listing of more objects than max_objects, 10000 when left out, fails
// the scrape as a limit; so does a page larger than the response limit and
// objects that come to more than it, managed fields not counted.
func TestAKubernetesListingIsBounded(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/apis/apps/v1/namespaces/monitoring/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("monitoring", 30)})
	if got := kubernetesMaxObjects(kubernetesCollector(t, "apps", "deployments")); got != 10000 {
		t.Errorf("max_objects left out is %d", got)
	}
	_, err := listDoc(t, kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) { c.Request.MaxObjects = 29 }))
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "more than the 29 objects request.max_objects allows") {
		t.Errorf("30 objects over a bound of 29: %v", err)
	}
	if _, err := listDoc(t, kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) { c.Request.MaxObjects = 30 })); err != nil {
		t.Errorf("30 objects at a bound of 30: %v", err)
	}
	// The page of 30 is about 7 kB with its managed fields, its objects
	// about 4.5 kB without them; a page of 20 is about 4.5 kB, and the
	// objects of two such pages come to about 5.6 kB without their managed
	// fields and 9 kB with them.
	_, err = listDoc(t, kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) { c.Request.MaxResponseBytes = 5000 }))
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "a page of deployments is larger than 5000 bytes, the response limit") {
		t.Errorf("a page over the limit: %v", err)
	}
	s.Serve("/apis/apps/v1/namespaces/a/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("a", 20)})
	s.Serve("/apis/apps/v1/namespaces/b/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("b", 20)})
	_, err = listDoc(t, kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) { c.Request.MaxResponseBytes = 5000; c.Request.Namespaces = []string{"a", "b"} }))
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "come to more than the response limit, 5000 bytes") {
		t.Errorf("two pages under the limit whose objects come to more: %v", err)
	}
	if _, err := listDoc(t, kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) { c.Request.MaxResponseBytes = 6000; c.Request.Namespaces = []string{"a", "b"} })); err != nil {
		t.Errorf("two pages whose objects come to less than the limit without their managed fields: %v", err)
	}
}

// The API server's refusals say what to do: a token it does not take, a
// permission the service account lacks, with the chart's value that grants
// it, and a resource it does not have, with the ways to name one. Outside
// a pod, without api_server, the scrape fails saying so.
func TestAKubernetesListingsFailuresSayWhatToDo(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/api/v1/namespaces/monitoring/pods", kubetest.List{Kind: "PodList", APIVersion: "v1"})
	s.Forbid("/apis/apps/v1/namespaces/monitoring/deployments")
	_, err := listDoc(t, kubernetesCollector(t, "apps", "deployments"))
	if err == nil || !strings.Contains(err.Error(), "listing deployments (apps/v1) in the namespace monitoring answered 403 Forbidden: deployments is forbidden: User") || !strings.Contains(err.Error(), "kubernetesAccess.rules") {
		t.Errorf("forbidden: %v", err)
	}
	_, err = listDoc(t, kubernetesCollector(t, "example.com", "widgets", func(c *model.Collector) { c.Request.AllNamespaces = true }))
	if err == nil || !strings.Contains(err.Error(), "across the cluster answered 404 Not Found") || !strings.Contains(err.Error(), "kubectl api-resources") {
		t.Errorf("not found: %v", err)
	}
	s.Write(t, "token", "stale\n")
	s.RequireToken("token-2")
	_, err = listDoc(t, kubernetesCollector(t, "", "pods"))
	if err == nil {
		t.Fatal("a token the stand-in does not take")
	}
	if !strings.Contains(err.Error(), "answered 401 Unauthorized: Unauthorized; the API server does not take the token") {
		t.Errorf("unauthorized: %v", err)
	}
	if strings.Contains(err.Error(), "stale") {
		t.Errorf("the token is in the error: %v", err)
	}
	s.Write(t, "namespace", "")
	if _, err := listDoc(t, kubernetesCollector(t, "", "pods")); err == nil || !strings.Contains(err.Error(), "own namespace cannot be read") {
		t.Errorf("no namespace: %v", err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := listDoc(t, kubernetesCollector(t, "", "pods")); err == nil || !strings.Contains(err.Error(), "does not run in a Kubernetes pod") {
		t.Errorf("outside a pod: %v", err)
	}
}

// Outside the cluster, api_server names the API server, tls.ca_file its
// CA and bearer_token_file the token, each file read at every scrape; the
// listing is traced for a debug probe, its token never shown by it, and
// --dry-run reads the files.
func TestAKubernetesCollectorReachesTheAPIServerItNames(t *testing.T) {
	s := kubetest.New(t)
	s.Serve("/apis/cert-manager.io/v1/namespaces/shop/certificates", kubetest.List{Kind: "CertificateList", APIVersion: "cert-manager.io/v1", Items: []map[string]any{{"apiVersion": "cert-manager.io/v1", "kind": "Certificate", "metadata": map[string]any{"name": "web", "namespace": "shop"}}}})
	c := kubernetesCollector(t, "cert-manager.io", "certificates", func(c *model.Collector) {
		c.Request.APIServer = s.URL + "/"
		c.Request.TLS.CAFile = filepath.Join(s.Dir, "ca.crt")
		c.Request.BearerTokenFile = filepath.Join(s.Dir, "token")
		c.Request.Namespaces = []string{"shop"}
	})
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, "", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"kind":"Certificate","api_version":"cert-manager.io/v1","count":1,"items":[{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"web","namespace":"shop"}}]}`; string(resp.Body) != want {
		t.Errorf("answered %s, want %s", resp.Body, want)
	}
	requests := trace.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || !strings.HasPrefix(requests[0].URL, s.URL+"/apis/cert-manager.io/v1/namespaces/shop/certificates?limit=100") || requests[0].Header.Get("Authorization") != "Bearer token-1" || requests[0].Outcome != "200 OK" {
		t.Errorf("traced %+v", requests)
	}
	if got := RedactHeaderValue("Authorization", requests[0].Header.Get("Authorization")); strings.Contains(got, "token-1") {
		t.Errorf("a debug report shows the token: %q", got)
	}
	if label, err := RequestLabelFor("", c, RequestOverrides{}); err != nil || label != s.URL+"/apis/cert-manager.io/v1/certificates" {
		t.Errorf("label %q, %v", label, err)
	}
	if method, stage := RequestMethodFor(c, RequestOverrides{}), FetchStage(c); method != "LIST" || stage != "kubernetes" {
		t.Errorf("method %q, stage %q", method, stage)
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	missing := kubernetesCollector(t, "", "pods", func(c *model.Collector) {
		c.Request.APIServer = s.URL
		c.Request.Namespaces = []string{"shop"}
		c.Request.TLS.CAFile = filepath.Join(t.TempDir(), "ca.crt")
		c.Request.BearerTokenFile = filepath.Join(t.TempDir(), "token")
	})
	if warnings := FileWarnings(missing, nil); len(warnings) != 2 || !strings.Contains(warnings[0], "request.bearer_token_file") || !strings.Contains(warnings[1], "request.tls") {
		t.Errorf("warnings %v", warnings)
	}
	if _, err := listDoc(t, missing); err == nil || !strings.Contains(err.Error(), "the token to list pods with") {
		t.Errorf("a missing token file: %v", err)
	}
	if label, _ := RequestLabelFor("", kubernetesCollector(t, "", "pods"), RequestOverrides{}); label != "kubernetes:/api/v1/pods" {
		t.Errorf("the in-cluster label is %q", label)
	}
}

// A kubernetes collector takes no target, from a probe or a static
// target: it lists what its configuration names.
func TestAKubernetesCollectorTakesNoTarget(t *testing.T) {
	c := kubernetesCollector(t, "", "pods")
	if err := CheckTarget(c, "", false); err != nil {
		t.Errorf("no target: %v", err)
	}
	if err := CheckTarget(c, " ", true); err != nil {
		t.Errorf("a static target of blanks: %v", err)
	}
	for _, static := range []bool{false, true} {
		if err := CheckTarget(c, "kube-system", static); err == nil || !strings.Contains(err.Error(), "leave") {
			t.Errorf("static %v: a target taken: %v", static, err)
		}
	}
	if _, err := FetchCollector(context.Background(), "kube-system", c, RequestOverrides{}, nil); err == nil {
		t.Error("a scrape with a target listed")
	}
}

// The keys of a kubernetes collector are checked at load: the resource is
// required and a plural name, the group a group's and never "core", the
// version a version's; the core group's secrets are refused; namespaces
// and all_namespaces exclude each other; the selectors are not blanks
// alone; tls and bearer_token_file go with api_server alone, an https URL
// with a credential and without insecure_skip_verify; and the keys of
// other types are refused.
func TestAKubernetesCollectorsKeysAreCheckedAtLoad(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*model.RequestConfig)
		want string
	}{
		"no resource":                 {func(r *model.RequestConfig) { r.Resource = "" }, "has no request.resource"},
		"a subresource":               {func(r *model.RequestConfig) { r.Resource = "pods/status" }, "subresource"},
		"a kind":                      {func(r *model.RequestConfig) { r.Resource = "Pod" }, "plural name"},
		"group core":                  {func(r *model.RequestConfig) { r.Group = "core" }, "leave group out"},
		"a bad group":                 {func(r *model.RequestConfig) { r.Group = "Apps" }, "not an API group"},
		"a bad version":               {func(r *model.RequestConfig) { r.Version = "V1" }, "not an API version"},
		"secrets":                     {func(r *model.RequestConfig) { r.Resource = "secrets" }, "lists secrets"},
		"both namespace keys":         {func(r *model.RequestConfig) { r.Namespaces, r.AllNamespaces = []string{"a"}, true }, "not both"},
		"a bad namespace":             {func(r *model.RequestConfig) { r.Namespaces = []string{"a_b"} }, "not a namespace's name"},
		"a namespace twice":           {func(r *model.RequestConfig) { r.Namespaces = []string{"a", "a"} }, "twice"},
		"a blank selector":            {func(r *model.RequestConfig) { r.LabelSelector = " " }, "label_selector is written with blanks alone"},
		"negative max_objects":        {func(r *model.RequestConfig) { r.MaxObjects = -1 }, "must not be negative"},
		"tls in the cluster":          {func(r *model.RequestConfig) { r.TLS.CAFile = "/ca" }, "without request.api_server"},
		"a token file in the cluster": {func(r *model.RequestConfig) { r.BearerTokenFile = "/t" }, "without request.api_server"},
		"an http api_server":          {func(r *model.RequestConfig) { r.APIServer, r.BearerTokenFile = "http://k8s.example:6443", "/t" }, "not an https URL"},
		"a user in api_server":        {func(r *model.RequestConfig) { r.APIServer, r.BearerTokenFile = "https://u:p@k8s.example", "/t" }, "has a user"},
		"a query in api_server":       {func(r *model.RequestConfig) { r.APIServer, r.BearerTokenFile = "https://k8s.example/?a=b", "/t" }, "query or fragment"},
		"no credential":               {func(r *model.RequestConfig) { r.APIServer = "https://k8s.example" }, "no credential"},
		"insecure_skip_verify": {func(r *model.RequestConfig) {
			r.APIServer, r.BearerTokenFile, r.TLS.InsecureSkipVerify = "https://k8s.example", "/t", true
		}, "always verified"},
		"a lone cert_file": {func(r *model.RequestConfig) { r.APIServer, r.TLS.CertFile = "https://k8s.example", "/c" }, "only one of cert_file and key_file"},
		"a path":           {func(r *model.RequestConfig) { r.Path = "/x" }, "request.path, which does not apply"},
		"allowed_targets":  {func(r *model.RequestConfig) { r.AllowedTargets = []string{"10.0.0.0/8"} }, "request.allowed_targets, which does not apply"},
	} {
		collector := &model.Collector{Name: "kube", Request: model.RequestConfig{Type: RequestTypeKubernetes, Resource: "pods"}}
		c.edit(&collector.Request)
		if err := ValidateRequest(collector); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to say %q", name, err, c.want)
		}
	}
	for name, edit := range map[string]func(*model.RequestConfig){
		"secrets of another group": func(r *model.RequestConfig) { r.Group, r.Resource = "example.com", "secrets" },
		"a client certificate": func(r *model.RequestConfig) {
			r.APIServer, r.TLS.CertFile, r.TLS.KeyFile = "https://k8s.example:6443/proxy/", "/c", "/k"
		},
		"a version": func(r *model.RequestConfig) { r.Group, r.Version = "example.com", "v1beta2" },
	} {
		collector := &model.Collector{Name: "kube", Request: model.RequestConfig{Type: RequestTypeKubernetes, Resource: "pods"}}
		edit(&collector.Request)
		if err := ValidateRequest(collector); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if server, err := parseKubernetesAPIServer("https://k8s.example:6443/proxy/"); err != nil || server != "https://k8s.example:6443/proxy" {
		t.Errorf("api_server with a path is %q, %v", server, err)
	}
}
