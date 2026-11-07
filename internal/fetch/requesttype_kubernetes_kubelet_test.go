//go:build !select_request_types || request_type_kubernetes

package fetch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/kubetest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// kubeletNodeObject is a Node named name whose kubelet is the stand-in at
// address, its InternalIP and port, an ExternalIP after it; with no
// address, a Node without addresses.
func kubeletNodeObject(t *testing.T, name, address string) map[string]any {
	t.Helper()
	node := map[string]any{"metadata": map[string]any{"name": name, "managedFields": []any{map[string]any{"manager": "kubelet"}}}, "status": map[string]any{}}
	if address == "" {
		return node
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	node["status"] = map[string]any{
		"addresses":       []any{map[string]any{"type": "Hostname", "address": "unresolvable.invalid"}, map[string]any{"type": "InternalIP", "address": u.Hostname()}, map[string]any{"type": "ExternalIP", "address": "192.0.2.1"}},
		"daemonEndpoints": map[string]any{"kubeletEndpoint": map[string]any{"Port": port}},
	}
	return node
}

// summary is the Summary API's answer of node, one pod using used bytes of
// ephemeral storage.
func summary(node string, used int) string {
	return `{"node":{"nodeName":"` + node + `","fs":{"usedBytes":100,"capacityBytes":1000}},"pods":[{"podRef":{"name":"web","namespace":"shop"},"ephemeral-storage":{"usedBytes":` + strconv.Itoa(used) + `}}]}`
}

// kubeletCollector is a collector of the nodes' kubelets' summaries.
func kubeletCollector(t *testing.T, edit ...func(*KubernetesConfig)) *model.Collector {
	t.Helper()
	return kubernetesCollector(t, "", "nodes", func(c *model.Collector) {
		editKubernetes(c, func(r *KubernetesConfig) {
			r.AllNamespaces, r.Kubelet = true, "stats/summary"
			for _, e := range edit {
				e(r)
			}
		})
	})
}

// kubeletDoc is a scrape of c, and its document.
func kubeletDoc(t *testing.T, c *model.Collector) (kubeletDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, "", c, RequestOverrides{}, nil)
	if err != nil {
		return kubeletDocument{}, err
	}
	var doc kubeletDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v\n%s", err, resp.Body)
	}
	return doc, nil
}

// A collector with kubelet: stats/summary lists the nodes, its label
// selector sent with the listing, and asks each node's kubelet at its
// InternalIP and the port its Node names, with the service account's
// token, the kubelet's certificate verified against the cluster's CA: the
// document is each summary as the kubelet answered it, in the order the
// nodes were listed, and the nodes whose kubelet answered none, each with
// why in words that name no address — a refusal, a Node without an
// address, a summary over the response limit.
func TestAKubeletCollectorAsksEachNodesKubelet(t *testing.T) {
	s := kubeAPI(t)
	var kubelets []*kubetest.Server
	for i := range 4 {
		k := kubetest.New(t)
		k.Answer("/stats/summary", summary("node-"+strconv.Itoa(i), 1000*(i+1)))
		kubelets = append(kubelets, k)
	}
	kubelets[1].Forbid("/stats/summary")
	kubelets[3].Answer("/stats/summary", summary("node-3", 1)+strings.Repeat(" ", 20000))
	s.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{
		kubeletNodeObject(t, "node-0", kubelets[0].URL), kubeletNodeObject(t, "node-1", kubelets[1].URL), kubeletNodeObject(t, "node-2", ""),
		kubeletNodeObject(t, "node-3", kubelets[3].URL), kubeletNodeObject(t, "node-4", kubelets[2].URL),
	}})
	c := kubeletCollector(t, func(r *KubernetesConfig) { r.LabelSelector = "pool=web" })
	c.Request.MaxResponseBytes = 15000
	doc, err := kubeletDoc(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "Summary" || doc.APIVersion != "stats/v1alpha1" || doc.Count != 2 || len(doc.Items) != 2 {
		t.Fatalf("document %+v", doc)
	}
	if string(doc.Items[0]) != summary("node-0", 1000) || string(doc.Items[1]) != summary("node-2", 3000) {
		t.Errorf("summaries %s", doc.Items)
	}
	want := []kubeletFailure{
		{Node: "node-1", Error: "the kubelet answered 403 Forbidden"},
		{Node: "node-2", Error: "its Node has no InternalIP, Hostname or ExternalIP address to ask its kubelet at"},
		{Node: "node-3", Error: "the kubelet's summary is larger than the response limit"},
	}
	if !slices.Equal(doc.Failed, want) {
		t.Errorf("failed %+v, want %+v", doc.Failed, want)
	}
	if got := s.Requests()[0]; got.Path != "/api/v1/nodes" || got.Query.Get("labelSelector") != "pool=web" {
		t.Errorf("listed the nodes with %+v", got)
	}
	for i, k := range kubelets {
		if i == 2 {
			continue
		}
		if got := k.Requests(); len(got) != 1 || got[0].Path != "/stats/summary" || got[0].Authorization != "Bearer token-1" || got[0].Accept != "application/json" {
			t.Errorf("kubelet %d was sent %+v", i, got)
		}
	}
}

// When no kubelet answers, the scrape fails with the first's failure and
// what to do about it: a refusal with the permission and the chart's value
// that grants it; a certificate no CA the exporter has signs with
// kubelet_ca_file and kubelet_via_api_server; a probe's caller is told no
// address. No node at all is an empty document.
func TestAKubeletCollectorWithNoKubeletAnsweringFails(t *testing.T) {
	s := kubeAPI(t)
	k := kubetest.New(t)
	k.Forbid("/stats/summary")
	s.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{kubeletNodeObject(t, "node-0", k.URL)}})
	_, err := kubeletDoc(t, kubeletCollector(t))
	if err == nil || !strings.Contains(err.Error(), "no node's kubelet answered its stats/summary; the first: ") || !strings.Contains(err.Error(), "grant it get on nodes/stats, with the chart's kubernetesAccess.kubelet: stats") {
		t.Errorf("forbidden: %v", err)
	}
	if shown := model.CallerText(err); strings.Contains(shown, "127.0.0.1") || !strings.Contains(shown, "the first: the kubelet answered 403 Forbidden") {
		t.Errorf("a probe's caller is told %q", shown)
	}

	// A CA that did not sign the kubelet's certificate.
	k = kubetest.New(t)
	k.Answer("/stats/summary", summary("node-0", 1))
	s.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{kubeletNodeObject(t, "node-0", k.URL)}})
	other := filepath.Join(t.TempDir(), "other-ca.crt")
	writeOtherCA(t, other)
	_, err = kubeletDoc(t, kubeletCollector(t, func(r *KubernetesConfig) { r.KubeletCAFile = other }))
	if err == nil || !strings.Contains(err.Error(), "give theirs in request.kubelet_ca_file, or set kubelet_via_api_server") {
		t.Errorf("a CA of another: %v", err)
	}
	// The CA that did.
	if doc, err := kubeletDoc(t, kubeletCollector(t, func(r *KubernetesConfig) { r.KubeletCAFile = filepath.Join(k.Dir, "ca.crt") })); err != nil || doc.Count != 1 {
		t.Errorf("the kubelet's CA: %+v, %v", doc, err)
	}

	s.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1"})
	doc, err := kubeletDoc(t, kubeletCollector(t))
	if err != nil || doc.Count != 0 || doc.Items == nil || doc.Failed == nil {
		t.Errorf("no nodes: %+v, %v", doc, err)
	}
}

// With kubelet_via_api_server the kubelets are asked through the API
// server's proxy of each node, with the API server's client; a refusal
// names nodes/proxy.
func TestAKubeletCollectorAsksThroughTheAPIServer(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{kubeletNodeObject(t, "node-0", ""), kubeletNodeObject(t, "node-1", "")}})
	s.Answer("/api/v1/nodes/node-0/proxy/stats/summary", summary("node-0", 5))
	s.Answer("/api/v1/nodes/node-1/proxy/stats/summary", summary("node-1", 6))
	c := kubeletCollector(t, func(r *KubernetesConfig) { r.KubeletViaAPIServer = true })
	doc, err := kubeletDoc(t, c)
	if err != nil || doc.Count != 2 || string(doc.Items[1]) != summary("node-1", 6) || len(doc.Failed) != 0 {
		t.Fatalf("through the API server: %+v, %v", doc, err)
	}
	s.Forbid("/api/v1/nodes/node-0/proxy/stats/summary")
	s.Forbid("/api/v1/nodes/node-1/proxy/stats/summary")
	if _, err := kubeletDoc(t, c); err == nil || !strings.Contains(err.Error(), "grant it get on nodes/proxy, with the chart's kubernetesAccess.kubelet: proxy") {
		t.Errorf("forbidden: %v", err)
	}
}

// writeOtherCA writes a CA certificate of its own to path, which signed no
// stand-in's certificate.
func writeOtherCA(t *testing.T, path string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A kubelet that does not answer is given up on before the scrape's
// deadline: the others' summaries are answered, and its node is among the
// failed, its request having run out of time, rather than the scrape
// failing at its deadline for one node.
func TestAKubeletThatDoesNotAnswerIsAmongTheFailed(t *testing.T) {
	s := kubeAPI(t)
	answering, silent := kubetest.New(t), kubetest.New(t)
	answering.Answer("/stats/summary", summary("node-0", 1))
	silent.HoldFrom(1)
	s.Serve("/api/v1/nodes", kubetest.List{Kind: "NodeList", APIVersion: "v1", Items: []map[string]any{kubeletNodeObject(t, "node-0", answering.URL), kubeletNodeObject(t, "node-1", silent.URL)}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, "", kubeletCollector(t), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc kubeletDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Count != 1 || !slices.Equal(doc.Failed, []kubeletFailure{{Node: "node-1", Error: "the request to the kubelet failed: context deadline exceeded"}}) {
		t.Errorf("document %+v", doc)
	}
	// A tenth of the second is left when the kubelet is given up on.
	if deadline, _ := ctx.Deadline(); time.Until(deadline) < 50*time.Millisecond {
		t.Errorf("the scrape ran to %s before its deadline", time.Until(deadline))
	}
}
