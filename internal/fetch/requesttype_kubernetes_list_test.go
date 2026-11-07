//go:build !select_request_types || request_type_kubernetes

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/kubeapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/kubetest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// configMaps are n ConfigMaps of namespace, each with data, managed fields,
// an annotation and kubectl apply's copy of what it applied.
func configMaps(namespace string, n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{
			"metadata": map[string]any{
				"name": "cm-" + string(rune('a'+i)), "namespace": namespace, "creationTimestamp": "2026-11-01T00:00:00Z",
				"annotations":   map[string]string{"team": "shop", lastAppliedAnnotation: `{"data":{"password":"hunter2"}}`},
				"managedFields": []map[string]any{{"manager": "kubectl"}},
			},
			"data": map[string]string{"password": "hunter2"},
		}
	}
	return out
}

// A collector of metadata_only lists the objects' metadata alone, a
// PartialObjectMetadataList: the document's kind is PartialObjectMetadata,
// whether there were objects or not, and each object has its metadata
// without its managed fields and without kubectl's last-applied
// configuration, its other annotations kept, and none of the rest. It is
// how the core group's secrets are listed; an answer that is no
// PartialObjectMetadataList fails the scrape and hands nothing on.
func TestAKubernetesCollectorListsTheMetadataAlone(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/api/v1/namespaces/monitoring/secrets", kubetest.List{Kind: "SecretList", APIVersion: "v1", Items: configMaps("monitoring", 2)})
	s.Serve("/api/v1/namespaces/monitoring/configmaps", kubetest.List{Kind: "ConfigMapList", APIVersion: "v1"})
	metadataOnly := func(c *model.Collector) { editKubernetes(c, func(r *KubernetesConfig) { r.MetadataOnly = true }) }
	doc, err := listDoc(t, kubernetesCollector(t, "", "secrets", metadataOnly))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "PartialObjectMetadata" || doc.APIVersion != "meta.k8s.io/v1" || doc.Count != 2 || !slices.Equal(itemNames(t, doc), []string{"monitoring/cm-a", "monitoring/cm-b"}) {
		t.Fatalf("listed %+v", doc)
	}
	for _, raw := range doc.Items {
		if text := string(raw); strings.Contains(text, "hunter2") || strings.Contains(text, "managedFields") || strings.Contains(text, lastAppliedAnnotation) || !strings.Contains(text, `"team":"shop"`) || !strings.Contains(text, `"creationTimestamp"`) {
			t.Errorf("an object is %s", raw)
		}
	}
	if got := s.Requests()[0].Accept; got != kubeapi.AcceptMetadataList {
		t.Errorf("asked with Accept %q", got)
	}
	doc, err = listDoc(t, kubernetesCollector(t, "", "configmaps", metadataOnly))
	if err != nil || doc.Kind != "PartialObjectMetadata" || doc.APIVersion != "meta.k8s.io/v1" || doc.Count != 0 {
		t.Errorf("none: %+v, %v", doc, err)
	}

	// A PartialObjectMetadataList whose objects carry more than their
	// metadata hands on their metadata alone.
	s.Answer("/api/v1/namespaces/billing/secrets", `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","metadata":{},"items":[{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{"name":"a","namespace":"billing"},"data":{"password":"aHVudGVyMg=="}},{"kind":"PartialObjectMetadata","stringData":{"password":"hunter2"}}]}`)
	doc, err = listDoc(t, kubernetesCollector(t, "", "secrets", metadataOnly, func(c *model.Collector) {
		editKubernetes(c, func(r *KubernetesConfig) { r.Namespaces = []string{"billing"} })
	}))
	if err != nil || doc.Count != 2 || string(doc.Items[0]) != `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"name":"a","namespace":"billing"}}` || string(doc.Items[1]) != `{"kind":"PartialObjectMetadata"}` {
		t.Errorf("objects with more than their metadata: %s, %v", doc.Items, err)
	}

	// What stands before the API server answers the Secrets whole.
	s.Answer("/api/v1/namespaces/shop/secrets", `{"kind":"SecretList","apiVersion":"v1","metadata":{},"items":[{"metadata":{"name":"a"},"data":{"password":"aHVudGVyMg=="}}]}`)
	_, err = listDoc(t, kubernetesCollector(t, "", "secrets", metadataOnly, func(c *model.Collector) {
		editKubernetes(c, func(r *KubernetesConfig) { r.Namespaces = []string{"shop"} })
	}))
	if err == nil || !strings.Contains(err.Error(), "answered a SecretList, not the PartialObjectMetadataList of their metadata alone asked for") || strings.Contains(err.Error(), "aHVudGVy") {
		t.Errorf("the objects whole: %v", err)
	}
}

// A page the API server's priority and fairness refused with 429 is asked
// for again after the wait its Retry-After asks for, a debug trace showing
// the wait; refused more than three times, or with a wait past the
// scrape's deadline, the scrape fails saying so.
func TestAThrottledKubernetesListingIsAskedForAgain(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/apis/apps/v1/namespaces/monitoring/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("monitoring", 3)})
	c := kubernetesCollector(t, "apps", "deployments")
	s.Throttle("/apis/apps/v1/namespaces/monitoring/deployments", 2, "0")
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, "", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc kubernetesDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil || doc.Count != 3 {
		t.Fatalf("listed %s, %v", resp.Body, err)
	}
	var outcomes, waits []string
	for _, r := range trace.Requests() {
		outcomes, waits = append(outcomes, r.Outcome), append(waits, r.Wait)
	}
	if !slices.Equal(outcomes, []string{"429 Too Many Requests", "429 Too Many Requests", "200 OK"}) || !slices.Equal(waits, []string{"then waited 0s, as the answer's Retry-After asked", "then waited 0s, as the answer's Retry-After asked", ""}) {
		t.Errorf("traced %q, waits %q", outcomes, waits)
	}

	s.Throttle("/apis/apps/v1/namespaces/monitoring/deployments", 5, "0")
	_, err = listDoc(t, c)
	if err == nil || !strings.Contains(err.Error(), "answered 429 Too Many Requests") || !strings.Contains(err.Error(), "asked for again 3 times") {
		t.Errorf("throttled four times: %v", err)
	}

	s.Throttle("/apis/apps/v1/namespaces/monitoring/deployments", 1, "60")
	before := len(s.Requests())
	_, err = listDoc(t, c)
	if err == nil || !strings.Contains(err.Error(), "asked for again 0 times") || len(s.Requests()) != before+1 {
		t.Errorf("a Retry-After past the deadline: %v, %d requests", err, len(s.Requests())-before)
	}
}

// A listing whose continue token expired goes on with the token the 410
// carries, or, without one, starts over, the objects of the pages before
// put away, so each is listed once.
func TestAnExpiredKubernetesListingGoesOn(t *testing.T) {
	s := kubeAPI(t)
	s.Serve("/apis/apps/v1/namespaces/monitoring/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments("monitoring", 250)})
	c := kubernetesCollector(t, "apps", "deployments")
	for _, inconsistent := range []bool{true, false} {
		s.Expire("/apis/apps/v1/namespaces/monitoring/deployments", inconsistent)
		doc, err := listDoc(t, c)
		if err != nil {
			t.Fatal(err)
		}
		if names := itemNames(t, doc); doc.Count != 250 || len(names) != 250 || names[0] != "monitoring/app-000" || names[249] != "monitoring/app-249" || !slices.IsSorted(names) {
			t.Errorf("inconsistent %v: listed %d, %v ... ", inconsistent, doc.Count, names[:2])
		}
	}
}

// The namespaces of a collector are listed four at a time: of six, with
// the API server holding every answer, four are asked for before the
// scrape's time runs out; the document keeps the order request.namespaces
// names them in, and a namespace that fails fails the scrape, naming it.
func TestAKubernetesCollectorsNamespacesAreListedAtOnce(t *testing.T) {
	s := kubeAPI(t)
	namespaces := []string{"a", "b", "c", "d", "e", "f"}
	for _, ns := range namespaces {
		s.Serve("/apis/apps/v1/namespaces/"+ns+"/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments(ns, 1)})
	}
	c := kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) {
		editKubernetes(c, func(r *KubernetesConfig) { r.Namespaces = slices.Clone(namespaces) })
	})
	s.HoldFrom(1)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := FetchCollector(ctx, "", c, RequestOverrides{}, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || len(s.Requests()) != 4 {
		t.Errorf("with every answer held: %v, %d requests sent, want 4", err, len(s.Requests()))
	}
	s.HoldFrom(0)
	doc, err := listDoc(t, c)
	if got := itemNames(t, doc); err != nil || !slices.Equal(got, []string{"a/app-000", "b/app-000", "c/app-000", "d/app-000", "e/app-000", "f/app-000"}) {
		t.Errorf("listed %v, %v", got, err)
	}
	s.Forbid("/apis/apps/v1/namespaces/e/deployments")
	if _, err := listDoc(t, c); err == nil || !strings.Contains(err.Error(), "in the namespace e answered 403 Forbidden") {
		t.Errorf("one namespace forbidden: %v", err)
	}
}

// Each request names the exporter in its User-Agent (kubeapi.UserAgent);
// and of an exported trace, each records its phases, its own though the
// namespaces are listed at once: the first request on a connection its
// connection, handshake and first byte.
func TestAKubernetesListingNamesItselfAndIsTimed(t *testing.T) {
	s := kubeAPI(t)
	for _, ns := range []string{"a", "b"} {
		s.Serve("/apis/apps/v1/namespaces/"+ns+"/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: deployments(ns, 1)})
	}
	old := kubeapi.UserAgent
	kubeapi.UserAgent = "prometheus-universal-exporter/9.9.9"
	t.Cleanup(func() { kubeapi.UserAgent = old })
	c := kubernetesCollector(t, "apps", "deployments", func(c *model.Collector) {
		editKubernetes(c, func(r *KubernetesConfig) { r.Namespaces = []string{"a", "b"} })
	})
	ctx, trace := WithSpanTrace(context.Background())
	if _, err := FetchCollector(ctx, "", c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Requests() {
		if r.UserAgent != "prometheus-universal-exporter/9.9.9" {
			t.Errorf("sent the User-Agent %q", r.UserAgent)
		}
	}
	requests := trace.Requests()
	if len(requests) != 2 {
		t.Fatalf("traced %+v", requests)
	}
	for _, r := range requests {
		var names []string
		for _, p := range r.Phases {
			if p.Start.Before(r.Start()) || p.End.Before(p.Start) {
				t.Errorf("%s of %s runs from %s to %s, the request from %s", p.Name, r.URL, p.Start, p.End, r.Start())
			}
			names = append(names, p.Name)
		}
		if got := strings.Join(names, " "); got != "connect tls first_byte" && got != "first_byte" {
			t.Errorf("%s has the phases %q", r.URL, got)
		}
		if r.Outcome != "200 OK" {
			t.Errorf("%s ended %q", r.URL, r.Outcome)
		}
	}
	if !slices.ContainsFunc(requests, func(r TracedRequest) bool { return len(r.Phases) == 3 }) {
		t.Errorf("no request made its connection: %+v", requests)
	}
}

// kubernetesEach runs each, four at a time, and is the first error; a
// context that ends after every one ran is no error, and one that ended
// before they all began is its cause.
func TestKubernetesEachRunsEachFourAtATime(t *testing.T) {
	var mu sync.Mutex
	running, most := 0, 0
	ran := make([]bool, 10)
	err := kubernetesEach(context.Background(), 10, func(_ context.Context, i int) error {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		running--
		ran[i] = true
		mu.Unlock()
		return nil
	})
	if err != nil || most != kubernetesConcurrency || slices.Contains(ran, false) {
		t.Errorf("%v, %d at once, ran %v", err, most, ran)
	}

	ctx, cancel := context.WithCancel(context.Background())
	err = kubernetesEach(ctx, 3, func(_ context.Context, i int) error {
		if i == 2 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Errorf("a context that ended once every one began: %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := kubernetesEach(ctx, 3, func(context.Context, int) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("a context ended before: %v", err)
	}
	boom := errors.New("boom")
	if err := kubernetesEach(context.Background(), 6, func(_ context.Context, i int) error {
		if i == 1 {
			return boom
		}
		return nil
	}); !errors.Is(err, boom) {
		t.Errorf("the first error is %v", err)
	}
}
