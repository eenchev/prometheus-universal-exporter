package allocator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// addresses are the addresses of found, in order.
func addresses(found []discovered) []string {
	out := make([]string, 0, len(found))
	for _, d := range found {
		out = append(out, d.Address)
	}
	return out
}

// testSource is a source of d, as a load would check it, on an allocator
// of no inputs, with limit as its max_targets.
func testSource(t *testing.T, d DiscoveryConfig, limit int) (*Allocator, *sourceState) {
	t.Helper()
	list := []DiscoveryConfig{d}
	if err := checkDiscovery(list, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	a := New(nil, Options{Logger: testutil.QuietLogger(t)})
	return a, &sourceState{spec: sourceSpec{key: "k", kind: "probe", owner: "job", cfg: list[0]}, limit: limit}
}

// Target groups are read as Prometheus reads them, from JSON and from YAML,
// and a key or a label name Prometheus would refuse is refused.
func TestTargetGroupsAreReadStrictly(t *testing.T) {
	groups, err := parseGroupsJSON([]byte(`[{"targets":["a:1","b:2"],"labels":{"team":"web","__meta_x":"y"}},{"targets":["c:3"]}]`))
	if err != nil || len(groups) != 2 || !slices.Equal(groups[0].Targets, []string{"a:1", "b:2"}) || groups[0].Labels["team"] != "web" {
		t.Fatalf("got %+v, %v", groups, err)
	}
	groups, err = parseGroupsYAML([]byte("- targets: [a:1]\n  labels: {team: web}\n"))
	if err != nil || len(groups) != 1 || groups[0].Labels["team"] != "web" {
		t.Fatalf("got %+v, %v", groups, err)
	}
	for name, raw := range map[string]string{
		"an unknown key":   `[{"targets":["a:1"],"label":{}}]`,
		"a bad label name": `[{"targets":["a:1"],"labels":{"a-b":"c"}}]`,
		"an empty target":  `[{"targets":[""]}]`,
		"no list":          `{"targets":["a:1"]}`,
		"more after it":    `[] []`,
	} {
		if _, err := parseGroupsJSON([]byte(raw)); err == nil {
			t.Errorf("%s: %s was taken", name, raw)
		}
	}
	if _, err := parseGroupsYAML([]byte("- targets: [a:1]\n  extra: 1\n")); err == nil {
		t.Error("a YAML group with an unknown key was taken")
	}
}

// A source's labels mapping sets a label from one of the source's own,
// which the target keeps beside them.
func TestTheLabelsMappingSetsLabelsFromTheSources(t *testing.T) {
	found, err := flatten([]TargetGroup{{Targets: []string{"a:1"}, Labels: map[string]string{"__meta_zone": "eu", "team": "web"}}}, map[string]string{"zone": "__meta_zone", "absent": "__meta_none"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"__meta_zone": "eu", "team": "web", "zone": "eu"}
	if fmt.Sprint(found[0].Labels) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", found[0].Labels, want)
	}
}

// A discovery list is checked at load: exactly one kind per source, names,
// labels mappings, URLs, label selectors, ports and timings.
func TestDiscoveryIsCheckedAtLoad(t *testing.T) {
	ok := func() DiscoveryConfig {
		return DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics", LabelSelector: "app=db,tier in (a, b),!legacy,env!=dev"}}
	}
	if err := checkDiscovery([]DiscoveryConfig{ok()}, "/"); err != nil {
		t.Fatalf("a good source was refused: %v", err)
	}
	for name, c := range map[string]struct {
		mutate func(*DiscoveryConfig)
		want   string
	}{
		"two kinds":            {func(d *DiscoveryConfig) { d.FileSD = &FileSDConfig{Files: []string{"a.json"}} }, "exactly one of"},
		"no kind":              {func(d *DiscoveryConfig) { d.Kubernetes = nil }, "exactly one of"},
		"a bad name":           {func(d *DiscoveryConfig) { d.Name = "-x" }, "has the name"},
		"a __ label":           {func(d *DiscoveryConfig) { d.Labels = map[string]string{"__x": "y"} }, "Prometheus's own"},
		"an instance label":    {func(d *DiscoveryConfig) { d.Labels = map[string]string{"instance": "y"} }, "sets instance"},
		"a bad source label":   {func(d *DiscoveryConfig) { d.Labels = map[string]string{"x": "a-b"} }, "not a label name"},
		"a negative bound":     {func(d *DiscoveryConfig) { d.MaxTargets = -1 }, "max_targets"},
		"a bad selector":       {func(d *DiscoveryConfig) { d.Kubernetes.LabelSelector = "app==" + strings.Repeat("x", 70) }, "label_selector"},
		"a bad selector key":   {func(d *DiscoveryConfig) { d.Kubernetes.LabelSelector = "a b=c" }, "label_selector"},
		"an empty requirement": {func(d *DiscoveryConfig) { d.Kubernetes.LabelSelector = "a=b,,c" }, "empty requirement"},
		"a bad port":           {func(d *DiscoveryConfig) { d.Kubernetes.Port = "Metrics_Port" }, "kubernetes.port"},
		"port 0":               {func(d *DiscoveryConfig) { d.Kubernetes.Port = "0" }, "kubernetes.port"},
		"no port":              {func(d *DiscoveryConfig) { d.Kubernetes.Port = "" }, "kubernetes.port"},
		"a bad namespace":      {func(d *DiscoveryConfig) { d.Kubernetes.Namespaces = []string{"Prod"} }, "namespaces"},
		"both namespace ways":  {func(d *DiscoveryConfig) { d.Kubernetes.Namespaces, d.Kubernetes.AllNamespaces = []string{"a"}, true }, "all_namespaces"},
		"a long timeout":       {func(d *DiscoveryConfig) { d.Kubernetes.Timeout = 31e9 }, "longer than its refresh_interval"},
		"a short refresh":      {func(d *DiscoveryConfig) { d.Kubernetes.RefreshInterval = 1e8 }, "under the least"},
		"an address type":      {func(d *DiscoveryConfig) { d.Kubernetes.AddressType = "ipv4" }, "address_type"},
		"a relative URL": {func(d *DiscoveryConfig) {
			d.Kubernetes, d.HTTPSD = nil, &HTTPSDConfig{URL: "/sd"}
		}, "absolute http or https URL"},
		"a URL with credentials": {func(d *DiscoveryConfig) {
			d.Kubernetes, d.HTTPSD = nil, &HTTPSDConfig{URL: "https://u:p@cmdb.example/sd"}
		}, "credentials"},
		"tls for http": {func(d *DiscoveryConfig) {
			d.Kubernetes, d.HTTPSD = nil, &HTTPSDConfig{URL: "http://cmdb.example/sd"}
			d.HTTPSD.TLS.CAFile = "ca.crt"
		}, "never uses it"},
		"no files": {func(d *DiscoveryConfig) { d.Kubernetes, d.FileSD = nil, &FileSDConfig{} }, "files must not be empty"},
		"a file of another kind": {func(d *DiscoveryConfig) {
			d.Kubernetes, d.FileSD = nil, &FileSDConfig{Files: []string{"targets.txt"}}
		}, "must end in .json"},
		"a bad pattern": {func(d *DiscoveryConfig) { d.Kubernetes, d.FileSD = nil, &FileSDConfig{Files: []string{"[.json"}} }, "not a pattern"},
	} {
		d := ok()
		c.mutate(&d)
		err := checkDiscovery([]DiscoveryConfig{d}, "/")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, c.want)
		}
	}
	twice := []DiscoveryConfig{ok(), ok()}
	twice[0].Name, twice[1].Name = "a", "a"
	if err := checkDiscovery(twice, "/"); err == nil || !strings.Contains(err.Error(), "named twice") {
		t.Errorf("two sources of one name: got %v", err)
	}
	unnamed := []DiscoveryConfig{ok(), {FileSD: &FileSDConfig{Files: []string{"sd/*.json"}}}}
	if err := checkDiscovery(unnamed, "/etc/pue"); err != nil || unnamed[0].Name != "kubernetes_0" || unnamed[1].Name != "file_sd_1" || unnamed[1].FileSD.Files[0] != "/etc/pue/sd/*.json" {
		t.Errorf("got the names %q and %q and the file %q, %v; want kubernetes_0, file_sd_1 and a path from the file's directory", unnamed[0].Name, unnamed[1].Name, unnamed[1].FileSD.Files, err)
	}
}

// file_sd files are read again at every refresh: a change is what the
// source finds next, and a file that cannot be read keeps what was found.
func TestFileSDReadsItsFilesAgain(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "a.json", `[{"targets":["a:1"],"labels":{"team":"web"}}]`)
	testutil.WriteIn(t, dir, "b.yml", "- targets: [b:2]\n")
	a, s := testSource(t, DiscoveryConfig{FileSD: &FileSDConfig{Files: []string{filepath.Join(dir, "*.json"), filepath.Join(dir, "*.yml")}}}, 10)
	ctx := context.Background()
	if !a.refreshSource(ctx, s) || !slices.Equal(addresses(s.found), []string{"a:1", "b:2"}) || s.found[0].Labels["team"] != "web" {
		t.Fatalf("found %+v", s.found)
	}
	if a.refreshSource(ctx, s) {
		t.Error("files that did not change changed what was found")
	}
	testutil.WriteIn(t, dir, "a.json", `[{"targets":["a:1","c:3"]}]`)
	if !a.refreshSource(ctx, s) || !slices.Equal(addresses(s.found), []string{"a:1", "c:3", "b:2"}) {
		t.Fatalf("after the change found %v", addresses(s.found))
	}
	testutil.WriteIn(t, dir, "a.json", `[{"targets":`)
	if a.refreshSource(ctx, s) || !slices.Equal(addresses(s.found), []string{"a:1", "c:3", "b:2"}) || s.failures != 1 {
		t.Errorf("a broken file: found %v with %d failures, want the last good targets and one failure", addresses(s.found), s.failures)
	}
	a.refreshSource(ctx, s)
	if s.failures != 1 {
		t.Errorf("the same failure again counted %d failures, want 1", s.failures)
	}
}

// An http_sd source asks its URL with the bearer token of its file, read
// again at every refresh; an answer that changes is what it finds next,
// and a failure, an answer over the bound or with more targets than
// max_targets keeps the last good targets.
func TestHTTPSDKeepsItsLastGoodAnswer(t *testing.T) {
	var mu sync.Mutex
	status, body, gotAuth, gotRefresh := http.StatusOK, `[{"targets":["a:1"]}]`, "", ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuth, gotRefresh = r.Header.Get("Authorization"), r.Header.Get("X-Prometheus-Refresh-Interval-Seconds")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	set := func(s int, b string) {
		mu.Lock()
		defer mu.Unlock()
		status, body = s, b
	}
	dir := t.TempDir()
	token := testutil.WriteIn(t, dir, "token", "first\n")
	a, s := testSource(t, DiscoveryConfig{HTTPSD: &HTTPSDConfig{URL: server.URL + "/sd", BearerTokenFile: token}}, 3)
	ctx := context.Background()
	if !a.refreshSource(ctx, s) || !slices.Equal(addresses(s.found), []string{"a:1"}) || gotAuth != "Bearer first" || gotRefresh != "60" {
		t.Fatalf("found %v with Authorization %q and refresh %q", addresses(s.found), gotAuth, gotRefresh)
	}
	testutil.WriteIn(t, dir, "token", "second\n")
	set(http.StatusOK, `[{"targets":["a:1","b:2"]}]`)
	if !a.refreshSource(ctx, s) || !slices.Equal(addresses(s.found), []string{"a:1", "b:2"}) || gotAuth != "Bearer second" {
		t.Fatalf("after the change found %v with Authorization %q", addresses(s.found), gotAuth)
	}
	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"a 500":            {http.StatusInternalServerError, "down"},
		"no JSON":          {http.StatusOK, "<html>"},
		"too many targets": {http.StatusOK, `[{"targets":["a:1","b:2","c:3","d:4"]}]`},
		"too large":        {http.StatusOK, `[{"targets":["` + strings.Repeat("a", maxDiscoveryAnswerBytes) + `"]}]`},
	} {
		set(answer.status, answer.body)
		failures := s.failures
		if a.refreshSource(ctx, s) || !slices.Equal(addresses(s.found), []string{"a:1", "b:2"}) || s.failures != failures+1 {
			t.Errorf("%s: found %v with %d failures, want the last good targets and one more failure", name, addresses(s.found), s.failures)
		}
	}
	set(http.StatusOK, `[{"targets":["a:1","b:2"]}]`)
	if a.refreshSource(ctx, s) || s.lastErr != "" {
		t.Errorf("the same answer as the last good one again changed what was found, or left the failure (%q)", s.lastErr)
	}
}

// A source that never answered contributes no targets, and counts and
// logs its failure.
func TestASourceThatNeverAnsweredHasNoTargets(t *testing.T) {
	a, s := testSource(t, DiscoveryConfig{HTTPSD: &HTTPSDConfig{URL: "http://127.0.0.1:1/sd"}}, 3)
	a.sources["k"] = s
	if a.refreshSource(context.Background(), s) || s.ok || s.failures != 1 {
		t.Fatalf("ok %v, failures %d", s.ok, s.failures)
	}
	if found := a.foundBySource(); len(found) != 0 {
		t.Errorf("a source that never answered found %v", found)
	}
}

// slice is an EndpointSlice of the namespace db, of the Service postgres,
// with a port named metrics, 9187, and another, 5432.
func slice(name string, endpoints string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"db","labels":{"kubernetes.io/service-name":"postgres","app.kubernetes.io/name":"pg"}},
	"addressType":"IPv4","ports":[{"name":"sql","port":5432},{"name":"metrics","port":9187}],"endpoints":[` + endpoints + `]}`
}

// kubeAPI is an API server of the test's own, over TLS, with the service
// account directory a pod would have: the token, the CA and the namespace.
type kubeAPI struct {
	server   *httptest.Server
	dir      string
	mu       sync.Mutex
	requests []*http.Request
	pages    map[string]string
}

func newKubeAPI(t *testing.T) *kubeAPI {
	t.Helper()
	k := &kubeAPI{dir: t.TempDir(), pages: map[string]string{}}
	k.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.requests = append(k.requests, r)
		if r.Header.Get("Authorization") != "Bearer "+strings.TrimSpace(readFile(t, filepath.Join(k.dir, "token"))) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		page, ok := k.pages[r.URL.Path+"?"+r.URL.Query().Get("continue")]
		if !ok {
			http.Error(w, `{"kind":"Status","code":404}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(k.server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: k.server.Certificate().Raw})
	testutil.WriteIn(t, k.dir, "ca.crt", string(ca))
	testutil.WriteIn(t, k.dir, "token", "token-1\n")
	testutil.WriteIn(t, k.dir, "namespace", "db\n")
	u, _ := url.Parse(k.server.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	old := serviceAccountDir
	serviceAccountDir = k.dir
	t.Cleanup(func() { serviceAccountDir = old })
	return k
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Error(err)
	}
	return string(raw)
}

// The kubernetes source lists the EndpointSlices of its namespace by its
// label selector, page by page, and finds each ready endpoint's address at
// the named port, with the meta labels Prometheus gives such a target.
func TestKubernetesFindsReadyEndpointsAtTheNamedPort(t *testing.T) {
	k := newKubeAPI(t)
	path := "/apis/discovery.k8s.io/v1/namespaces/db/endpointslices"
	k.pages[path+"?"] = `{"metadata":{"continue":"next"},"items":[` + slice("postgres-a",
		`{"addresses":["10.0.0.2"],"conditions":{"ready":true},"nodeName":"node-1","targetRef":{"kind":"Pod","name":"pg-0"}},
		 {"addresses":["10.0.0.3"],"conditions":{"ready":false},"targetRef":{"kind":"Pod","name":"pg-1"}}`) + `]}`
	k.pages[path+"?next"] = `{"metadata":{},"items":[` + slice("postgres-b", `{"addresses":["10.0.0.1"],"conditions":{},"hostname":"pg-2"}`) + `,
		{"metadata":{"name":"other","namespace":"db"},"ports":[{"name":"http","port":80}],"endpoints":[{"addresses":["10.0.0.9"]}]}]}`
	a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics", LabelSelector: "app.kubernetes.io/name=pg"}, Labels: map[string]string{"pod": "__meta_kubernetes_pod_name"}}, 10)
	if !a.refreshSource(context.Background(), s) {
		t.Fatalf("nothing found: %s", s.lastErr)
	}
	if got := addresses(s.found); !slices.Equal(got, []string{"10.0.0.1:9187", "10.0.0.2:9187"}) {
		t.Errorf("found %v, want the ready endpoints, at port metrics, sorted", got)
	}
	labels := s.found[1].Labels
	for name, want := range map[string]string{
		"__meta_kubernetes_namespace":                                  "db",
		"__meta_kubernetes_service_name":                               "postgres",
		"__meta_kubernetes_endpointslice_name":                         "postgres-a",
		"__meta_kubernetes_pod_name":                                   "pg-0",
		"__meta_kubernetes_node_name":                                  "node-1",
		"__meta_kubernetes_endpoint_port_name":                         "metrics",
		"__meta_kubernetes_endpoint_ready":                             "true",
		"__meta_kubernetes_endpointslice_label_app_kubernetes_io_name": "pg",
		"pod": "pg-0",
	} {
		if labels[name] != want {
			t.Errorf("label %s is %q, want %q (labels %v)", name, labels[name], want, labels)
		}
	}
	if s.found[0].Labels["__meta_kubernetes_endpoint_hostname"] != "pg-2" {
		t.Errorf("an endpoint whose readiness is unknown, taken as ready, has the labels %v", s.found[0].Labels)
	}
	first := k.requests[0].URL.Query()
	if first.Get("labelSelector") != "app.kubernetes.io/name=pg" || first.Get("limit") != "500" {
		t.Errorf("the listing asked %v", first)
	}

	// Numbered, with the endpoints that are not ready.
	s.spec.cfg.Kubernetes.Port, s.spec.cfg.Kubernetes.IncludeNotReady = "5432", true
	a.refreshSource(context.Background(), s)
	if got := addresses(s.found); !slices.Equal(got, []string{"10.0.0.1:5432", "10.0.0.2:5432", "10.0.0.3:5432"}) {
		t.Errorf("found %v at the port numbered 5432, endpoints not ready included", got)
	}
	if s.found[2].Labels["__meta_kubernetes_endpoint_ready"] != "false" {
		t.Errorf("an endpoint not ready has the labels %v", s.found[2].Labels)
	}
}

// A dual-stack Service has an EndpointSlice of each address family, the
// same pods in both: a source takes the IPv4 slices by default, the IPv6
// ones with address_type IPv6, and every slice, FQDN ones included, with
// any, each target telling its slice's family in a meta label.
func TestKubernetesTakesTheSlicesOfItsAddressType(t *testing.T) {
	k := newKubeAPI(t)
	path := "/apis/discovery.k8s.io/v1/namespaces/db/endpointslices"
	ipv6 := strings.Replace(slice("postgres-v6", `{"addresses":["fd00::2"],"targetRef":{"kind":"Pod","name":"pg-0"}}`), `"IPv4"`, `"IPv6"`, 1)
	fqdn := strings.Replace(slice("postgres-dns", `{"addresses":["pg.example"]}`), `"IPv4"`, `"FQDN"`, 1)
	k.pages[path+"?"] = `{"metadata":{},"items":[` + slice("postgres-v4", `{"addresses":["10.0.0.2"],"targetRef":{"kind":"Pod","name":"pg-0"}}`) + "," + ipv6 + "," + fqdn + `]}`
	for addressType, want := range map[string][]string{
		"":     {"10.0.0.2:9187"},
		"IPv4": {"10.0.0.2:9187"},
		"IPv6": {"[fd00::2]:9187"},
		"any":  {"10.0.0.2:9187", "[fd00::2]:9187", "pg.example:9187"},
	} {
		a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics", AddressType: addressType}}, 10)
		if !a.refreshSource(context.Background(), s) {
			t.Fatalf("address_type %q: nothing found: %s", addressType, s.lastErr)
		}
		if got := addresses(s.found); !slices.Equal(got, want) {
			t.Errorf("address_type %q found %v, want %v", addressType, got, want)
		}
	}
	a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics", AddressType: "any"}, Labels: map[string]string{"family": "__meta_kubernetes_endpointslice_address_type"}}, 10)
	a.refreshSource(context.Background(), s)
	var families []string
	for _, d := range s.found {
		families = append(families, d.Labels["family"])
	}
	if want := []string{"IPv4", "IPv6", "FQDN"}; !slices.Equal(families, want) {
		t.Errorf("the targets' slices are of the families %v, want %v", families, want)
	}
}

// The token is read again at every listing, so a rotated token is sent.
func TestKubernetesReadsTheTokenAgain(t *testing.T) {
	k := newKubeAPI(t)
	k.pages["/apis/discovery.k8s.io/v1/namespaces/db/endpointslices?"] = `{"items":[` + slice("a", `{"addresses":["10.0.0.1"]}`) + `]}`
	a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics"}}, 10)
	if !a.refreshSource(context.Background(), s) {
		t.Fatalf("nothing found: %s", s.lastErr)
	}
	testutil.WriteIn(t, k.dir, "token", "token-2\n")
	k.pages["/apis/discovery.k8s.io/v1/namespaces/db/endpointslices?"] = `{"items":[` + slice("a", `{"addresses":["10.0.0.5"]}`) + `]}`
	if !a.refreshSource(context.Background(), s) || !slices.Equal(addresses(s.found), []string{"10.0.0.5:9187"}) {
		t.Fatalf("after the token rotated found %v (%s)", addresses(s.found), s.lastErr)
	}
	if got := k.requests[len(k.requests)-1].Header.Get("Authorization"); got != "Bearer token-2" {
		t.Errorf("sent %q after the rotation", got)
	}
}

// The API server's certificate must be signed by the service account's
// CA; listed namespaces and all of them are asked at their own paths; and
// more targets than max_targets are refused.
func TestKubernetesVerifiesTheAPIServer(t *testing.T) {
	k := newKubeAPI(t)
	k.pages["/apis/discovery.k8s.io/v1/namespaces/a/endpointslices?"] = `{"items":[` + slice("a", `{"addresses":["10.0.0.1"]},{"addresses":["10.0.0.2"]}`) + `]}`
	k.pages["/apis/discovery.k8s.io/v1/namespaces/b/endpointslices?"] = `{"items":[` + slice("b", `{"addresses":["10.0.0.3"]}`) + `]}`
	k.pages["/apis/discovery.k8s.io/v1/endpointslices?"] = `{"items":[` + slice("c", `{"addresses":["10.0.0.4"]}`) + `]}`
	a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics", Namespaces: []string{"a", "b"}}}, 10)
	if !a.refreshSource(context.Background(), s) || !slices.Equal(addresses(s.found), []string{"10.0.0.1:9187", "10.0.0.2:9187", "10.0.0.3:9187"}) {
		t.Fatalf("found %v (%s)", addresses(s.found), s.lastErr)
	}
	s.limit = 2
	if a.refreshSource(context.Background(), s) || !strings.Contains(s.lastErr, "max_targets") {
		t.Errorf("three targets over a bound of two: %q", s.lastErr)
	}
	s.limit = 10
	s.spec.cfg.Kubernetes.Namespaces, s.spec.cfg.Kubernetes.AllNamespaces = nil, true
	if !a.refreshSource(context.Background(), s) || !slices.Equal(addresses(s.found), []string{"10.0.0.4:9187"}) {
		t.Errorf("every namespace: found %v (%s)", addresses(s.found), s.lastErr)
	}

	testutil.WriteIn(t, k.dir, "ca.crt", otherCA(t))
	if a.refreshSource(context.Background(), s) || !strings.Contains(s.lastErr, "certificate") {
		t.Errorf("an API server whose certificate another CA signed: %q", s.lastErr)
	}
}

// otherCA is the PEM of a CA of its own, which signed no server's
// certificate. Every httptest server has the same certificate, so another
// server's would not do.
func otherCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Without the environment of a pod the source fails, saying why.
func TestKubernetesOutsideAPod(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics"}}, 10)
	if a.refreshSource(context.Background(), s) || !strings.Contains(s.lastErr, "KUBERNETES_SERVICE_HOST") {
		t.Errorf("got %q", s.lastErr)
	}
}
