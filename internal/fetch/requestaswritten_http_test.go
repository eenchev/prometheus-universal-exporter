//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// requestRecorder is a target that remembers the request line of every
// request it is sent, and answers ok.
type requestRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	uris []string
}

func newRequestRecorder(t *testing.T) *requestRecorder {
	t.Helper()
	r := &requestRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.uris = append(r.uris, req.RequestURI)
		r.mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(r.Close)
	return r
}

// requested is what the target was asked for since the last call.
func (r *requestRecorder) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.uris
	r.uris = nil
	return out
}

// everyNameReaches gives c's connection pool a dialer that connects every
// name to backend, as a DNS that knows them all would, and no proxy.
func everyNameReaches(t *testing.T, c *model.Collector, backend string) {
	t.Helper()
	tr, err := transports.get(TransportSettings{policy: policyOf(c)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tr.DialContext = policyDialer(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, backend)
	})
	tr.Proxy = nil
}

// A host written in ASCII is dialed as it is written, so it is checked as
// written: a name with an underscore, with two hyphens where a registered
// domain may not have them, or with a hyphen first is reached like any
// other, and allowed_targets and denied_targets match it by name.
func TestAnASCIIHostIsCheckedAsItIsWritten(t *testing.T) {
	target := newRequestRecorder(t)
	backend := strings.TrimPrefix(target.URL, "http://")
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })

	open := httpCollector(t, nil)
	listed := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedTargets = []string{"my_service", "*.svc_local", "-edge.internal", "db--primary.internal"}
	})
	denied := httpCollector(t, func(c *model.Collector) { c.Request.DeniedTargets = []string{"my_*", "-edge.*"} })
	for _, c := range []*model.Collector{open, listed, denied} {
		everyNameReaches(t, c, backend)
	}
	for _, tc := range []struct {
		host      string
		collector *model.Collector
		refused   bool
	}{
		{"my_service", open, false},
		{"app_1.internal", open, false},
		{"db--primary.internal", open, false},
		{"-edge.internal", open, false},
		{"my_service", listed, false},
		{"MY_Service.", listed, false},
		{"api.svc_local", listed, false},
		{"-edge.internal", listed, false},
		{"db--primary.internal", listed, false},
		{"other_service", listed, true},
		{"my_service", denied, true},
		{"-edge.internal", denied, true},
		{"app_1.internal", denied, false},
	} {
		_, err := FetchCollector(context.Background(), "http://"+tc.host+":8080/metrics", tc.collector, RequestOverrides{}, nil)
		if refused := errors.Is(err, ErrTargetRefused); refused != tc.refused || (err != nil && !refused) {
			t.Errorf("%s with allowed=%v denied=%v: err=%v, want refused=%v", tc.host, tc.collector.Request.AllowedTargets, tc.collector.Request.DeniedTargets, err, tc.refused)
		}
	}
	for host, want := range map[string]string{
		"my_service":       "my_service",
		"MY_Service.":      "my_service",
		"-Edge.Internal":   "-edge.internal",
		"db--primary":      "db--primary",
		"[FD00:EC2::254]":  "fd00:ec2::254",
		"169.254.169.254.": "169.254.169.254",
		"bücher.example":   "xn--bcher-kva.example",
		"１２７.０.０.１":        "127.0.0.1",
	} {
		if got, err := canonicalHost(host); err != nil || got != want {
			t.Errorf("canonicalHost(%q) = %q, %v, want %q", host, got, err, want)
		}
	}
	// A name with other characters still has to have an ASCII form.
	if _, err := canonicalHost("bü_cher.example"); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a name with no ASCII form: %v", err)
	}
}

// An IPv4 address in one of the older forms a resolver or a proxy turns into
// the address — one number, fewer than four parts, parts in hexadecimal or
// octal — is checked as that address before anything is sent, with or without
// a lookup: it is not a name that happens to resolve nowhere here.
func TestAnAddressInAnOlderFormIsCheckedAsTheAddress(t *testing.T) {
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, errors.New("no such host: " + host)
	}
	loopback, err := compileTargetPolicy(nil, []string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"127.0.0.1", "127.0.0.1.", "2130706433", "127.1", "127.0.1", "0x7f.0.0.1", "0X7F.0.0.1", "0177.0.0.1", "0x7f000001", "017700000001", "0x7f.1"} {
		for _, resolve := range []bool{false, true} {
			if _, err := loopback.check(context.Background(), host, resolve); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "127.0.0.0/8") {
				t.Errorf("%s (resolve=%v) was not refused as a loopback address: %v", host, resolve, err)
			}
		}
	}
	// The cloud metadata address, refused with no lists at all, and behind
	// a proxy too, where a name that does not resolve here is the proxy's.
	none, err := compileTargetPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"2852039166", "169.254.43518", "169.16689662", "0xa9.0xfe.0xa9.0xfe", "0251.0376.0251.0376", "0xa9fea9fe"} {
		if _, err := none.check(context.Background(), host, true); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "cloud metadata") {
			t.Errorf("%s was not refused as the cloud metadata address: %v", host, err)
		}
	}
	// What is no address in any form is a name.
	for _, host := range []string{"1.2.3.4.5", "256.1.1.1", "08.0.0.1", "0x.0.0.1", "1e3", "-1", "1_0", "4294967296", "1.2.3.256", "a.b", "example.0x7f.com", ""} {
		if addr, literal := literalAddr(host); literal {
			t.Errorf("%q was read as the address %s", host, addr)
		}
	}
	for host, want := range map[string]string{"0": "0.0.0.0", "1.2.3.4": "1.2.3.4", "1.2.772": "1.2.3.4", "1.131844": "1.2.3.4", "16909060": "1.2.3.4", "4294967295": "255.255.255.255", "[::1]": "::1"} {
		if addr, literal := literalAddr(host); !literal || addr.String() != want {
			t.Errorf("literalAddr(%q) = %s, %v, want %s", host, addr, literal, want)
		}
	}
}

// What a policy does with a host behind a proxy that the exporter cannot
// look up itself: the proxy resolves it, as for any client behind one, and
// the name rules still hold; only a collector whose own lists name addresses
// or networks fails, since it could not be held to them.
func TestBehindAProxyANameTheExporterCannotResolveIsTheProxys(t *testing.T) {
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	var lookups atomic.Int64
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups.Add(1)
		if host == "metadata.partner.example" {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	for name, tc := range map[string]struct {
		allowed, denied []string
		host            string
		refused         bool
		failure         string
		nameAllowed     bool
	}{
		"no lists":                                 {host: "metrics.partner.example"},
		"allowed by name":                          {allowed: []string{"*.partner.example"}, host: "metrics.partner.example", nameAllowed: true},
		"not among the allowed names":              {allowed: []string{"*.partner.example"}, host: "metrics.elsewhere.example", refused: true},
		"denied by name":                           {denied: []string{"*.partner.example"}, host: "metrics.partner.example", refused: true},
		"another name denied":                      {denied: []string{"bad.example"}, host: "metrics.partner.example"},
		"a denied network":                         {denied: []string{"10.0.0.0/8"}, host: "metrics.partner.example", failure: "request.allowed_targets and denied_targets"},
		"an allowed address":                       {allowed: []string{"203.0.113.7"}, host: "metrics.partner.example", failure: "goes through a proxy"},
		"allowed by name, with a denied network":   {allowed: []string{"*.partner.example"}, denied: []string{"10.0.0.0/8"}, host: "metrics.partner.example", failure: "the lookup failed", nameAllowed: true},
		"a name that resolves here is still held":  {host: "metadata.partner.example", refused: true},
		"the metadata address in an older form":    {host: "2852039166", refused: true},
		"the metadata address allowed by its name": {allowed: []string{"metadata.partner.example"}, host: "metadata.partner.example", refused: true, nameAllowed: true},
	} {
		t.Run(name, func(t *testing.T) {
			policy, err := compileTargetPolicy(tc.allowed, tc.denied)
			if err != nil {
				t.Fatal(err)
			}
			nameAllowed, err := policy.check(context.Background(), tc.host, true)
			switch {
			case tc.refused:
				if !errors.Is(err, ErrTargetRefused) {
					t.Fatalf("err=%v, want a refusal", err)
				}
			case tc.failure != "":
				if err == nil || errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), tc.failure) || !strings.Contains(err.Error(), "no such host") {
					t.Fatalf("err=%v, want a failed lookup saying %q", err, tc.failure)
				}
			case err != nil:
				t.Fatalf("err=%v, want the name left to the proxy", err)
			}
			if err == nil && nameAllowed != tc.nameAllowed {
				t.Fatalf("nameAllowed=%v, want %v", nameAllowed, tc.nameAllowed)
			}
		})
	}
	if lookups.Load() == 0 {
		t.Fatal("no name was looked up, so nothing was shown")
	}
}

// Behind HTTP_PROXY a target only the proxy can resolve — a host with no
// outside DNS of its own — is requested through the proxy; with an address
// rule of the collector's own it fails, saying which lists need the lookup,
// and the proxy is not asked.
func TestATargetOnlyTheProxyResolvesIsRequestedThroughIt(t *testing.T) {
	proxy := newRequestRecorder(t)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	// Transports read the environment when they are built.
	previous, restore := transports, resolveHost
	transports = newTransportCache()
	t.Cleanup(func() { transports, resolveHost = previous, restore })
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := FetchCollector(ctx, "http://metrics.partner.invalid/metrics?x=1", httpCollector(t, nil), RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != "ok" {
		t.Fatalf("resp=%+v err=%v, want the proxy's answer", resp, err)
	}
	if got := proxy.requested(); len(got) != 1 || got[0] != "http://metrics.partner.invalid/metrics?x=1" {
		t.Fatalf("the proxy was asked for %q", got)
	}

	byName := httpCollector(t, func(c *model.Collector) { c.Request.AllowedTargets = []string{"*.partner.invalid"} })
	if _, err := FetchCollector(ctx, "http://metrics.partner.invalid/", byName, RequestOverrides{}, nil); err != nil {
		t.Fatalf("a name allowed by name: %v", err)
	}
	if _, err := FetchCollector(ctx, "http://metrics.elsewhere.invalid/", byName, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "request.allowed_targets") {
		t.Fatalf("a name that is not allowed: %v", err)
	}
	if got := proxy.requested(); len(got) != 1 {
		t.Fatalf("the proxy was asked for %q, want the allowed name only", got)
	}

	byAddress := httpCollector(t, func(c *model.Collector) { c.Request.DeniedTargets = []string{"10.0.0.0/8"} })
	_, err = FetchCollector(ctx, "http://metrics.partner.invalid/", byAddress, RequestOverrides{}, nil)
	if err == nil || errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "goes through a proxy") || !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("with a denied network: %v", err)
	}
	if got := proxy.requested(); len(got) != 0 {
		t.Fatalf("the proxy was asked for %q although the target could not be checked", got)
	}
}

// The target's own query is sent byte for byte as it was written: a bare
// key gains no =, a pair with a ; in it is not dropped, and the pairs are
// neither sorted nor escaped anew. request.query is added after it, encoded,
// and a name both carry is sent twice, the target's first.
func TestATargetsQueryIsSentAsWritten(t *testing.T) {
	target := newRequestRecorder(t)
	host := strings.TrimPrefix(target.URL, "http://")
	for _, tc := range []struct {
		name, target string
		query        map[string]string
		want         string
	}{
		{"a bare key", target.URL + "/x?debug", nil, "/x?debug"},
		{"a pair with a semicolon", target.URL + "/x?a=1;b=2&c=3", nil, "/x?a=1;b=2&c=3"},
		{"order and escaping", target.URL + "/x?z=a%20b&a=1+2&e=%7e", nil, "/x?z=a%20b&a=1+2&e=%7e"},
		{"an empty query", target.URL + "/x?", nil, "/x?"},
		{"what cannot be sent is escaped where it stands", target.URL + `/x?q=a b&n=é&j={"a":1}&lt=<>&p=100%&e=%zz`, nil, "/x?q=a%20b&n=%C3%A9&j={%22a%22:1}&lt=%3C%3E&p=100%&e=%zz"},
		{"an empty value and a repeated name", target.URL + "/x?a=&a=2&&b", nil, "/x?a=&a=2&&b"},
		{"request.query goes after it", target.URL + "/x?z=1&debug", map[string]string{"format": "json", "a": "b c&d"}, "/x?z=1&debug&a=b+c%26d&format=json"},
		{"request.query alone", target.URL + "/x", map[string]string{"format": "json"}, "/x?format=json"},
		{"a name both carry", target.URL + "/x?format=xml&v=1", map[string]string{"format": "json"}, "/x?format=xml&v=1&format=json"},
		{"a placeholder in request.query", target.URL + "/x?debug", map[string]string{"region": "{{param_region:eu west}}"}, "/x?debug&region=eu+west"},
		{"no scheme, a URL in the query", host + "/x?next=http://other/", nil, "/x?next=http://other/"},
	} {
		c := httpCollector(t, func(c *model.Collector) { c.Request.Query = tc.query })
		if _, err := FetchCollector(context.Background(), tc.target, c, RequestOverrides{}, nil); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := target.requested(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: requested %q, want %s", tc.name, got, tc.want)
		}
	}
}

// A target has a scheme only when its :// comes before its path, query and
// fragment begin; one further on belongs to those, and the target is http.
func TestASchemeIsLookedForBeforeThePath(t *testing.T) {
	for raw, want := range map[string]string{
		"h:8080/login?next=http://other/": "http://h:8080/login?next=http://other/",
		"h:8080?next=https://other/":      "http://h:8080?next=https://other/",
		"h/a://b":                         "http://h/a://b",
		"h#at://x":                        "http://h#at://x",
		"10.0.0.5:8080":                   "http://10.0.0.5:8080",
		"https://h/x?next=http://other/":  "https://h/x?next=http://other/",
		"HTTPS://h":                       "HTTPS://h",
		"ftp://h/x":                       "ftp://h/x",
		"":                                "",
	} {
		if got := normalizeTarget(raw); got != want {
			t.Errorf("normalizeTarget(%q) = %q, want %q", raw, got, want)
		}
	}
	c := httpCollector(t, nil)
	u, err := resolveRequestURL("h:8080/login?next=http://other/", c, RequestOverrides{})
	if err != nil || u.Scheme != "http" || u.Host != "h:8080" || u.Path != "/login" || u.RawQuery != "next=http://other/" {
		t.Fatalf("url=%v err=%v", u, err)
	}
}

// A percent-escape written in request.path, or in the path probe parameter
// or a static target's path, is sent as it was written, not escaped a second
// time; a % that begins no escape is a percent sign; and a value bound into
// a placeholder is still escaped whole, its % included.
func TestEscapesInARequestPathAreSentAsWritten(t *testing.T) {
	target := newRequestRecorder(t)
	for _, tc := range []struct {
		name, target, path string
		override           bool
		want               string
	}{
		{"an escaped slash", "", "/api/v4/projects/group%2Fproject", false, "/api/v4/projects/group%2Fproject"},
		{"an escaped space", "", "/a%20b", false, "/a%20b"},
		{"lower-case digits", "", "/a%2fb%c3%a9", false, "/a%2fb%c3%a9"},
		{"an escape beside what needs escaping", "", "/a b/c%2Fd/é", false, "/a%20b/c%2Fd/%C3%A9"},
		{"a percent sign", "", "/100%/x", false, "/100%25/x"},
		{"a % with one digit", "", "/50%2", false, "/50%252"},
		{"a % with no digits", "", "/50%zz/%2F", false, "/50%25zz/%2F"},
		{"an escaped percent sign", "", "/50%25", false, "/50%25"},
		{"escaped dots are not a parent", "", "/a/%2E%2E/b", false, "/a/%2E%2E/b"},
		{"onto a target's path", "/base%2Fone/", "/x%2Fy/", false, "/base%2Fone/x%2Fy/"},
		{"a placeholder's value", "", "/p/{{param_t}}/x%2Fy", false, "/p/50%25%2Foff/x%2Fy"},
		{"a placeholder's default", "", "/p/{{param_u:a%2Fb}}/x", false, "/p/a%252Fb/x"},
		{"a written NUL beside a placeholder", "", "/%000%00/{{param_t}}", false, "/%000%00/50%25%2Foff"},
		{"the path parameter", "", "/api/group%2Fproject", true, "/api/group%2Fproject"},
		{"the path parameter, with a percent sign", "", "/api/100%/a b", true, "/api/100%25/a%20b"},
	} {
		c := httpCollector(t, func(c *model.Collector) {
			if !tc.override {
				c.Request.Path = tc.path
			}
		})
		overrides := RequestOverrides{Params: map[string]string{"param_t": "50%/off"}}
		if !HasPathParams(tc.path) {
			overrides.Params = nil
		}
		if tc.override {
			overrides.PathSet, overrides.Path = true, tc.path
		}
		if _, err := FetchCollector(context.Background(), target.URL+tc.target, c, overrides, nil); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := target.requested(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: requested %q, want %s", tc.name, got, tc.want)
		}
	}
	// The verbose url label shows the path as it is requested.
	c := httpCollector(t, func(c *model.Collector) { c.Request.Path = "/api/{{param_t}}/group%2Fproject" })
	if label, err := RequestLabelFor("http://h", c, RequestOverrides{}); err != nil || label != "http://h/api/{{param_t}}/group%2Fproject" {
		t.Fatalf("label=%q err=%v", label, err)
	}
}

// The largest limit that can be written, as someone writes "no limit", reads
// the whole answer: the one byte read past the limit does not wrap round to
// a read of nothing.
func TestTheLargestResponseLimitReadsTheAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // no Content-Length
		_, _ = w.Write([]byte(`{"up": 1}`))
	}))
	defer server.Close()
	for name, edit := range map[string]func(*model.Collector){
		"request.max_response_bytes": func(c *model.Collector) { c.Request.MaxResponseBytes = math.MaxInt64 },
		"limits.max_response_bytes":  func(c *model.Collector) { c.Limits.MaxResponseBytes = math.MaxInt64 },
	} {
		c := httpCollector(t, edit)
		if limit := responseLimit(c); limit != math.MaxInt64-1 {
			t.Errorf("%s: the limit is %d, want one under the largest number", name, limit)
		}
		resp, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil)
		if err != nil || string(resp.Body) != `{"up": 1}` {
			t.Errorf("%s: resp=%+v err=%v, want the answer whole", name, resp, err)
		}
	}
	if limit := responseLimit(httpCollector(t, func(c *model.Collector) { c.Request.MaxResponseBytes = 4096 })); limit != 4096 {
		t.Fatalf("an ordinary limit became %d", limit)
	}
}

// A response's headers are bounded apart from its body, at 1 MiB: more fails
// the request as a limit, in words that say what was too large, and is not
// retried; headers under the bound are read whatever max_response_bytes is.
func TestResponseHeadersAreBounded(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		pads := 8 // 64 KiB
		if r.URL.Path == "/huge" {
			pads = 256 // 2 MiB
		}
		for i := range pads {
			w.Header().Set("X-Pad-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), strings.Repeat("a", 8000))
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.MaxResponseBytes = 100
		c.Request.Retry.Attempts = 2
	})
	resp, err := FetchCollector(context.Background(), server.URL+"/modest", c, RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != "ok" || len(resp.Headers.Get("X-Pad-a")) != 8000 {
		t.Fatalf("64 KiB of headers under a 100 byte body limit: err=%v", err)
	}
	hits.Store(0)
	_, err = FetchCollector(context.Background(), server.URL+"/huge", c, RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "response headers are larger than 1048576 bytes") {
		t.Fatalf("2 MiB of headers: err=%v, want a limit error naming the headers", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the target was asked %d times, want once: headers too large are too large again", n)
	}
}

// request.allowed_schemes takes http and https, in any case, and nothing
// else: a mistyped scheme, one with a space or :// around it, or an empty
// entry would refuse every target, and stops the load instead.
func TestAllowedSchemesAreCheckedAtLoad(t *testing.T) {
	for _, schemes := range [][]string{{"htps"}, {"https "}, {" http"}, {"ftp"}, {"https://"}, {""}, {"https", "file"}} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: schemes}}
		err := ValidateRequest(&c)
		if err == nil || !strings.Contains(err.Error(), `collector "web" request.allowed_schemes[`) || !strings.Contains(err.Error(), "http or https") {
			t.Errorf("allowed_schemes %q: %v", schemes, err)
		}
	}
	for _, schemes := range [][]string{nil, {"https"}, {"http", "https"}, {"HTTPS"}} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedSchemes: schemes}}
		if err := ValidateRequest(&c); err != nil {
			t.Errorf("allowed_schemes %q: %v", schemes, err)
		}
	}
	upper := httpCollector(t, func(c *model.Collector) { c.Request.AllowedSchemes = []string{"HTTPS"} })
	if _, err := resolveRequestURL("https://h/x", upper, RequestOverrides{}); err != nil {
		t.Fatalf("an https target under allowed_schemes [HTTPS]: %v", err)
	}
}

// A placeholder's default that could never be written where it stands — a
// word under |number, a line break in a header, . or .. in the path — is
// refused when the configuration loads, naming the collector, the field and
// the parameter, rather than by every probe that leaves the parameter out.
func TestAnUnusableDefaultIsRefusedAtLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*model.Collector)
		want []string
	}{
		"a word as a number": {
			func(c *model.Collector) {
				c.Request.Method, c.Request.Body = "POST", `{"n": {{param_limit:ten|number}}}`
			},
			[]string{`collector "web"`, "request.body", "param_limit", `"ten"`, "default"},
		},
		"beside a placeholder without a default": {
			func(c *model.Collector) {
				c.Request.Method, c.Request.Body = "POST", `{"s": {{param_service|json}}, "n": {{param_limit:1.|number}}}`
			},
			[]string{`collector "web"`, "request.body", "param_limit", "default"},
		},
		"an empty default as a number": {
			func(c *model.Collector) { c.Request.Method, c.Request.Body = "POST", `{"n": {{param_limit:|number}}}` },
			[]string{`collector "web"`, "request.body", "param_limit", "default"},
		},
		"a line break in a header": {
			func(c *model.Collector) { c.Request.Headers = map[string]string{"X-Tenant": "{{param_tenant:a\nb}}"} },
			[]string{`collector "web"`, "request.headers.X-Tenant", "param_tenant", "default"},
		},
		"a parent directory in the path": {
			func(c *model.Collector) { c.Request.Path = "/api/{{param_tenant:..}}/status" },
			[]string{`collector "web"`, "request.path", "param_tenant", `".."`, "default"},
		},
	} {
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
		tc.edit(&c)
		err := ValidateRequest(&c)
		if err == nil {
			t.Errorf("%s: the configuration loaded", name)
			continue
		}
		for _, fragment := range tc.want {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("%s: %v, want it to say %s", name, err, fragment)
			}
		}
	}
	// Defaults that can be written, and placeholders without one, load.
	good := httpCollector(t, func(c *model.Collector) {
		c.Request.Method = "POST"
		c.Request.Path = "/api/{{param_tenant:acme}}/{{param_suffix:}}"
		c.Request.Headers = map[string]string{"X-Tenant": "{{param_tenant:a\tb}}"}
		c.Request.Query = map[string]string{"region": "{{param_region:eu&west}}"}
		c.Request.Body = `{"s": {{param_service|json}}, "n": {{param_limit:10|number}}, "q": {{param_q:a "b"|json}}, "r": {{param_r:}}}`
	})
	if _, err := CheckRequestParams(good, RequestOverrides{Params: map[string]string{"param_service": "checkout"}}); err != nil {
		t.Fatalf("the defaults that loaded do not render: %v", err)
	}
}

// A static target with a credential of its own sends that one, so the
// collector's credential is not read for it: a missing file of the
// collector's fails only the targets that would have sent it. The same
// holds for an Authorization a probe forwards.
func TestATargetsOwnCredentialDoesNotNeedTheCollectors(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	// sent is the Authorization of every request since the last call.
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen = nil
		return out
	}
	for name, edit := range map[string]func(*model.Collector){
		"bearer_token_file": func(c *model.Collector) { c.Request.BearerTokenFile = "/nonexistent/default-token" },
		"basic_auth_file": func(c *model.Collector) {
			c.Request.BasicAuthFile = &model.BasicAuthFile{Username: "/nonexistent/user", Password: "/nonexistent/password"}
		},
	} {
		c := httpCollector(t, edit)
		own := model.StaticTarget{Name: "own", Collector: c.Name, Target: server.URL}
		own.Request.BearerToken = "target-token"
		if err := CheckTargetRequest(&own, c); err != nil {
			t.Fatal(err)
		}
		headers, err := TargetHeaders(&own)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := FetchCollector(context.Background(), server.URL, c, TargetOverrides(&own), headers); err != nil {
			t.Errorf("%s: a target with its own credential: %v", name, err)
		}
		if got := sent(); len(got) != 1 || got[0] != "Bearer target-token" {
			t.Errorf("%s: the target was sent %q", name, got)
		}
		// An Authorization the probe forwards is sent instead as well.
		if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, http.Header{"Authorization": {"Bearer forwarded"}}); err != nil {
			t.Errorf("%s: a forwarded Authorization: %v", name, err)
		}
		if got := sent(); len(got) != 1 || got[0] != "Bearer forwarded" {
			t.Errorf("%s: the target was sent %q", name, got)
		}
		// A target without one sends the collector's, whose file is missing.
		plain := model.StaticTarget{Name: "plain", Collector: c.Name, Target: server.URL}
		plain.Request.Headers = map[string]string{"X-Tenant": "acme"}
		headers, err = TargetHeaders(&plain)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := FetchCollector(context.Background(), server.URL, c, TargetOverrides(&plain), headers); err == nil || !strings.Contains(err.Error(), "/nonexistent/") {
			t.Errorf("%s: a target without a credential of its own: err=%v, want the collector's missing file", name, err)
		}
		if got := sent(); len(got) != 0 {
			t.Errorf("%s: a request was sent without the collector's credential: %q", name, got)
		}
	}
}

// The pairs of a target's query that name a parameter the request type sets
// itself are dropped, by name in any case and however the name is escaped;
// every other pair is kept as it was written.
func TestATypesOwnParametersReplaceTheTargets(t *testing.T) {
	own := url.Values{"target": {"a.b"}, "format": {"json"}}
	for raw, want := range map[string]string{
		"":                                    "",
		"debug":                               "debug",
		"tenant=a%20b&debug&x=1;y=2":          "tenant=a%20b&debug&x=1;y=2",
		"target=secret.*&tenant=a%20b":        "tenant=a%20b",
		"tenant=a&TARGET=x&format=csv&debug":  "tenant=a&debug",
		"t%61rget=x&v=1":                      "v=1",
		"v=1;target=x&w=2":                    "w=2",
		"target&format":                       "",
		"targets=x&reformat=y&target_=z&=1&&": "targets=x&reformat=y&target_=z&=1&&",
	} {
		if got := withoutQueryKeys(raw, own); got != want {
			t.Errorf("withoutQueryKeys(%q) = %q, want %q", raw, got, want)
		}
	}
}
