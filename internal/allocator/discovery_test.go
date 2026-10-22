package allocator

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	mrand "math/rand/v2"
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

	"github.com/eenchev/prometheus-universal-exporter/internal/kubetest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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
	// limit, when set, is the only page size answered, as an API server
	// whose answers could not be larger would; delay is how long each
	// answer takes.
	limit string
	delay time.Duration
}

func newKubeAPI(t *testing.T) *kubeAPI {
	t.Helper()
	k := &kubeAPI{dir: t.TempDir(), pages: map[string]string{}}
	k.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.requests = append(k.requests, r)
		if k.limit != "" && r.URL.Query().Get("limit") != k.limit {
			http.Error(w, `{"kind":"Status","code":400,"message":"pages of `+k.limit+` only"}`, http.StatusBadRequest)
			return
		}
		if k.delay > 0 {
			select {
			case <-time.After(k.delay):
			case <-r.Context().Done():
				return
			}
		}
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
	if first.Get("labelSelector") != "app.kubernetes.io/name=pg" || first.Get("limit") != "100" {
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

// The kubernetes source asks for pages of 100 EndpointSlices, which an API
// server answering no other size serves, and follows the continue tokens
// through every page.
func TestKubernetesListsInPagesOf100(t *testing.T) {
	k := newKubeAPI(t)
	k.limit = "100"
	path := "/apis/discovery.k8s.io/v1/namespaces/db/endpointslices"
	tokens := []string{"", "p2", "p3", "p4"}
	for i, token := range tokens {
		next := ""
		if i+1 < len(tokens) {
			next = tokens[i+1]
		}
		k.pages[path+"?"+token] = `{"metadata":{"continue":"` + next + `"},"items":[` + slice(fmt.Sprintf("postgres-%d", i), fmt.Sprintf(`{"addresses":["10.0.0.%d"]}`, i+1)) + `]}`
	}
	a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics"}}, 10)
	if !a.refreshSource(context.Background(), s) {
		t.Fatalf("nothing found: %s", s.lastErr)
	}
	if got := addresses(s.found); !slices.Equal(got, []string{"10.0.0.1:9187", "10.0.0.2:9187", "10.0.0.3:9187", "10.0.0.4:9187"}) {
		t.Errorf("found %v, want the endpoint of each of the 4 pages", got)
	}
	if len(k.requests) != len(tokens) {
		t.Errorf("the listing took %d requests, want one per page, %d", len(k.requests), len(tokens))
	}
}

// worstCaseSlice is an EndpointSlice as the API server sends it, whole,
// with n endpoints, the EndpointSlice controller's 100 unless it was told
// otherwise, and every name at its longest: what a page of a large
// cluster's listing is made of.
func worstCaseSlice(i, n int) map[string]any {
	name := func(prefix string, n int) string { return prefix + strings.Repeat("x", n-len(prefix)) }
	labels := map[string]string{
		"kubernetes.io/service-name":             name("svc", 63),
		"endpointslice.kubernetes.io/managed-by": "endpointslice-controller.k8s.io",
	}
	// The controller copies the Service's labels onto its slices.
	for j := range 8 {
		labels[name(fmt.Sprintf("example.com/label-%d", j), 63)] = name("value", 63)
	}
	endpoints := make([]map[string]any, n)
	for j := range endpoints {
		endpoints[j] = map[string]any{
			"addresses":  []string{fmt.Sprintf("fd00:1234:5678:9abc:def0:%04x:%04x:%04x", i, j, i+j)},
			"conditions": map[string]bool{"ready": true, "serving": true, "terminating": false},
			"hostname":   name(fmt.Sprintf("host-%d-%d", i, j), 63),
			"nodeName":   name(fmt.Sprintf("node-%d-%d", i, j), 253),
			"zone":       "europe-west1-b-zone-name",
			"hints":      map[string]any{"forZones": []map[string]string{{"name": "europe-west1-b-zone-name"}}},
			"targetRef": map[string]string{
				"kind": "Pod", "namespace": name("ns", 63), "name": name(fmt.Sprintf("pod-%d-%d", i, j), 253),
				"uid": "0f6e9c7a-6a5f-4e3c-9b8d-2a1c0e4f5b6d", "resourceVersion": "123456789012",
			},
		}
	}
	return map[string]any{
		"metadata": map[string]any{
			"name": name(fmt.Sprintf("slice-%d-", i), 253), "namespace": name("ns", 63), "generateName": name("svc-", 63),
			"uid": "0f6e9c7a-6a5f-4e3c-9b8d-2a1c0e4f5b6d", "resourceVersion": "123456789012", "generation": 1234,
			"creationTimestamp": "2026-10-21T12:00:00Z", "labels": labels,
			"annotations":     map[string]string{"endpoints.kubernetes.io/last-change-trigger-time": "2026-10-21T12:00:00Z"},
			"ownerReferences": []map[string]any{{"apiVersion": "v1", "kind": "Service", "name": name("svc", 63), "uid": "0f6e9c7a-6a5f-4e3c-9b8d-2a1c0e4f5b6d", "controller": true, "blockOwnerDeletion": true}},
			"managedFields": []map[string]any{{
				"manager": "kube-controller-manager", "operation": "Update", "apiVersion": "discovery.k8s.io/v1", "time": "2026-10-21T12:00:00Z", "fieldsType": "FieldsV1",
				"fieldsV1": map[string]any{"f:addressType": map[string]any{}, "f:endpoints": map[string]any{}, "f:metadata": map[string]any{"f:labels": map[string]any{".": map[string]any{}}, "f:ownerReferences": map[string]any{}}, "f:ports": map[string]any{}},
			}},
		},
		"addressType": "IPv6",
		"ports":       []map[string]any{{"name": "metrics", "port": 9100, "protocol": "TCP"}, {"name": "http", "port": 8080, "protocol": "TCP"}},
		"endpoints":   endpoints,
	}
}

// A page of the kubernetes source's size, of slices of the controller's
// 100 endpoints with every name at its longest, fits in the 16 MiB an
// answer may hold, so a large cluster's listing does not fail at every
// refresh as one of pages of 500 did.
func TestAKubernetesPageOfTheWorstCaseFits(t *testing.T) {
	items := make([]map[string]any, kubernetesPageSize)
	for i := range items {
		items[i] = worstCaseSlice(i, 100)
	}
	raw, err := json.Marshal(map[string]any{"kind": "EndpointSliceList", "apiVersion": "discovery.k8s.io/v1", "metadata": map[string]string{"resourceVersion": "123456789012", "continue": strings.Repeat("c", 200)}, "items": items})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("a page of %d slices is %d bytes, %d per slice", kubernetesPageSize, len(raw), len(raw)/kubernetesPageSize)
	if len(raw) > maxDiscoveryAnswerBytes {
		t.Fatalf("a page of %d slices is %d bytes, more than the %d an answer may hold", kubernetesPageSize, len(raw), maxDiscoveryAnswerBytes)
	}
	page, err := readBounded(bytes.NewReader(raw), "a page of EndpointSlices")
	if err != nil {
		t.Fatal(err)
	}
	var list endpointSliceList
	if err := json.Unmarshal(page, &list); err != nil || len(list.Items) != kubernetesPageSize || len(list.Items[0].Endpoints) != 100 {
		t.Fatalf("the page reads as %d slices: %v", len(list.Items), err)
	}
	if per := len(raw) / kubernetesPageSize; 500*per <= maxDiscoveryAnswerBytes {
		t.Errorf("a slice is %d bytes, so 500 of them would fit too: the case is not the worst a page of 500 met", per)
	}
}

// A cluster that raises --max-endpoints-per-slice makes pages of 100
// slices larger than the 16 MiB an answer may hold: 60 slices of 300
// endpoints, about 18 MiB, are listed in pages of 50, about 15 MiB, after
// the page of 100 was refused, every endpoint found, and the fall to
// smaller pages is logged once at debug level.
func TestAKubernetesListingOfLargeSlicesFallsToSmallerPages(t *testing.T) {
	s := kubetest.New(t)
	s.InCluster(t)
	old := serviceAccountDir
	serviceAccountDir = s.Dir
	t.Cleanup(func() { serviceAccountDir = old })
	items := make([]map[string]any, 60)
	for i := range items {
		items[i] = worstCaseSlice(i, 300)
	}
	s.Serve("/apis/discovery.k8s.io/v1/namespaces/monitoring/endpointslices", kubetest.List{Kind: "EndpointSliceList", APIVersion: "discovery.k8s.io/v1", Items: items})
	a, src := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics", AddressType: "IPv6", RefreshInterval: model.Duration(time.Minute), Timeout: model.Duration(30 * time.Second)}}, 60*300)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if !a.refreshSource(context.Background(), src) {
		t.Fatalf("nothing found: %s", src.lastErr)
	}
	if len(src.found) != 60*300 {
		t.Errorf("found %d targets, want every endpoint of the 60 slices, %d", len(src.found), 60*300)
	}
	var asked []string
	for _, r := range s.Requests() {
		asked = append(asked, r.Query.Get("limit")+"@"+r.Query.Get("continue"))
	}
	if want := []string{"100@", "50@", "50@50"}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	if n := strings.Count(logs.String(), "the listing asked for smaller pages"); n != 1 || !strings.Contains(logs.String(), `"page_size":50`) || !strings.Contains(logs.String(), `"level":"DEBUG"`) {
		t.Errorf("logged %d times, want once at debug level with the page size 50:\n%s", n, logs.String())
	}
}

// A timeout left out is the default, 10s, or the refresh_interval when that
// is shorter, so a source asked every 5s is never asked again while an
// earlier ask still runs; a timeout written is taken as it is.
func TestAnUnsetTimeoutIsCutToTheRefreshInterval(t *testing.T) {
	for _, c := range []struct {
		refresh, timeout model.Duration
		want             time.Duration
	}{
		{0, 0, DefaultDiscoveryTimeout},
		{model.Duration(time.Minute), 0, DefaultDiscoveryTimeout},
		{model.Duration(5 * time.Second), 0, 5 * time.Second},
		{model.Duration(5 * time.Second), model.Duration(3 * time.Second), 3 * time.Second},
	} {
		for _, d := range []DiscoveryConfig{
			{HTTPSD: &HTTPSDConfig{URL: "https://cmdb.example/sd", RefreshInterval: c.refresh, Timeout: c.timeout}},
			{Kubernetes: &KubernetesConfig{Port: "metrics", RefreshInterval: c.refresh, Timeout: c.timeout}},
		} {
			if err := checkDiscovery([]DiscoveryConfig{d}, "/"); err != nil {
				t.Errorf("%s every %s with the timeout %s: refused, %v", d.kind(), time.Duration(c.refresh), time.Duration(c.timeout), err)
			}
			if got := d.timeout(); got != c.want {
				t.Errorf("%s every %s with the timeout %s: the timeout is %s, want %s", d.kind(), time.Duration(c.refresh), time.Duration(c.timeout), got, c.want)
			}
		}
	}

	// An http_sd source asked every second, its timeout left out, gives up
	// on an answer that takes longer.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`[{"targets":["a:1"]}]`))
	}))
	defer server.Close()
	a, s := testSource(t, DiscoveryConfig{HTTPSD: &HTTPSDConfig{URL: server.URL + "/sd", RefreshInterval: model.Duration(time.Second)}}, 3)
	start := time.Now()
	if a.refreshSource(context.Background(), s) || s.failures != 1 {
		t.Errorf("an answer later than the refresh interval was taken")
	}
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("the ask took %s, want it given up after the 1s refresh interval", took)
	}
}

// The kubernetes timeout bounds the whole listing, every page of it, not
// each request alone: a listing of pages each well within the timeout but
// together longer fails.
func TestAKubernetesListingIsBoundedWhole(t *testing.T) {
	k := newKubeAPI(t)
	k.delay = 400 * time.Millisecond
	path := "/apis/discovery.k8s.io/v1/namespaces/db/endpointslices"
	k.pages[path+"?"] = `{"metadata":{"continue":"p2"},"items":[]}`
	k.pages[path+"?p2"] = `{"metadata":{"continue":"p3"},"items":[]}`
	k.pages[path+"?p3"] = `{"metadata":{"continue":"p4"},"items":[]}`
	k.pages[path+"?p4"] = `{"metadata":{},"items":[` + slice("postgres", `{"addresses":["10.0.0.1"]}`) + `]}`
	a, s := testSource(t, DiscoveryConfig{Kubernetes: &KubernetesConfig{Port: "metrics", RefreshInterval: model.Duration(time.Second)}}, 10)
	if a.refreshSource(context.Background(), s) || !strings.Contains(s.lastErr, "took longer than the timeout, 1s") {
		t.Errorf("a listing of 4 pages of 400ms each with a 1s timeout found %v, failing with %q", addresses(s.found), s.lastErr)
	}
}

// unescapeAddress reads back what escapeAddress wrote, left to right: _
// followed by a digit and an upper-case letter is an escaped byte, any
// other _ a dot. That it exists is what makes escapeAddress injective.
func unescapeAddress(t *testing.T, escaped string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		switch {
		case c != '_':
			b.WriteByte(c)
		case i+2 < len(escaped) && escaped[i+1] >= '0' && escaped[i+1] <= '9' && escaped[i+2] >= 'A' && escaped[i+2] <= 'F':
			var v byte
			if _, err := fmt.Sscanf(escaped[i+1:i+3], "%02X", &v); err != nil {
				t.Fatalf("%q: %v", escaped, err)
			}
			b.WriteByte(v)
			i += 2
		default:
			b.WriteByte('.')
		}
	}
	return b.String()
}

// A discovered target's name is <entry>_ and its address escaped, readably
// for IPv4 and IPv6 addresses and lower-case host names, or, for any other
// address or a long one, the address cut and followed by _X and a digest.
func TestDiscoveredNames(t *testing.T) {
	for address, want := range map[string]string{
		"10.0.0.5:9100":          "node_10_0_0_5_3A9100",
		"[fd00::1]:9100":         "node__5Bfd00_3A_3A1_5D_3A9100",
		"node-1.example.com:443": "node_node_2D1_example_com_3A443",
		"a.b.example:9100":       "node_a_b_example_3A9100",
		"a-b.example:9100":       "node_a_2Db_example_3A9100",
		"Host.example:9100":      "node_host_example_9100_X" + hashHex("Host.example:9100"),
		"a_b.example:9100":       "node_a_b_example_9100_X" + hashHex("a_b.example:9100"),
		"https://a.example/x":    "node_https___a_example_x_X" + hashHex("https://a.example/x"),
		strings.Repeat("a", 129): "node_" + strings.Repeat("a", hashedAddressPrefix) + "_X" + hashHex(strings.Repeat("a", 129)),
	} {
		got := discoveredName("node", address)
		if got != want {
			t.Errorf("%q is named %q, want %q", address, got, want)
		}
		if !targetName.MatchString(got) {
			t.Errorf("%q is named %q, which is no target's name", address, got)
		}
	}
}

func hashHex(address string) string {
	sum := sha256.Sum256([]byte(address))
	return hex.EncodeToString(sum[:8])
}

// escapeAddress is read back whole for every address of its characters it
// takes, so no two of them share a name; checked over every address of up
// to 5 of those characters, and random longer ones.
func TestEscapedAddressesReadBack(t *testing.T) {
	const alphabet = "a9.:-[]"
	names := map[string]string{}
	check := func(address string) {
		escaped, ok := escapeAddress(address)
		if !ok {
			return
		}
		if back := unescapeAddress(t, escaped); back != address {
			t.Fatalf("%q is escaped %q, read back as %q", address, escaped, back)
		}
		if other, seen := names[escaped]; seen && other != address {
			t.Fatalf("%q and %q are both escaped %q", other, address, escaped)
		}
		names[escaped] = address
	}
	var all func(prefix string, n int)
	all = func(prefix string, n int) {
		check(prefix)
		if n == 0 {
			return
		}
		for i := range len(alphabet) {
			all(prefix+alphabet[i:i+1], n-1)
		}
	}
	all("", 5)
	r := mrand.New(mrand.NewPCG(1, 2))
	for range 100000 {
		b := make([]byte, 6+r.IntN(40))
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		check(string(b))
	}
}

// No two addresses an entry finds share a name, whatever they are.
func FuzzDiscoveredNamesAreInjective(f *testing.F) {
	for _, pair := range [][2]string{
		{"a.b.example:9100", "a-b.example:9100"},
		{"a.b.example:9100", "a_b.example:9100"},
		{"a..b", "a._b"},
		{"[::1]:80", "::1:80"},
		{"10.0.0.5:9100", "10.0.0.5.9100"},
		{"x", "X"},
		{strings.Repeat("a", 200), strings.Repeat("a", 201)},
		{"a.3a", "a:"},
	} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		x, y := discoveredName("node", a), discoveredName("node", b)
		if !targetName.MatchString(x) {
			t.Fatalf("%q is named %q, which is no target's name", a, x)
		}
		if a != b && x == y {
			t.Fatalf("%q and %q are both named %q", a, b, x)
		}
		if a == b && x != y {
			t.Fatalf("%q is named both %q and %q", a, x, y)
		}
	})
}
