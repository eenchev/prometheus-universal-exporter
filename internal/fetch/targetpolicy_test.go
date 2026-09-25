//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// request.allowed_targets and denied_targets (targetpolicy.go).

func policyCollector(t *testing.T, allowed, denied []string, follow bool) *model.Collector {
	t.Helper()
	return httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedTargets, c.Request.DeniedTargets, c.Request.FollowRedirects = allowed, denied, follow
	})
}

func fetchRefused(t *testing.T, target string, c *model.Collector) error {
	t.Helper()
	_, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
	return err
}

// Each entry is a host, a glob of one, an address or a network; anything
// else is refused when the configuration loads.
func TestTargetPolicyEntries(t *testing.T) {
	for _, bad := range []string{"http://api.example.com", "api.example.com:8080", "10.0.0.0/33", "", "api/v1", "a b"} {
		c := model.Collector{Name: "p", Request: model.RequestConfig{Type: RequestTypeHTTP, AllowedTargets: []string{bad}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "request.allowed_targets") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	good := []string{"api.example.com", "*.example.com", "API-?.Internal.", "10.0.0.0/8", "192.168.1.7", "::1", "[fd00::1]", "fd00::/8"}
	c := model.Collector{Name: "p", Request: model.RequestConfig{Type: RequestTypeHTTP, DeniedTargets: good}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	files := model.Collector{Name: "f", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: t.TempDir(), DeniedTargets: []string{"x"}}}
	if RequestTypes[RequestTypeLocalFile] != nil {
		if err := ValidateRequest(&files); err == nil || !strings.Contains(err.Error(), "does not apply") {
			t.Fatalf("localfile: %v", err)
		}
	}
}

// fakeResolve is a resolver for the policy tests.
func fakeResolve(host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	switch host {
	case "private.example":
		return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
	case "mixed.example":
		return []netip.Addr{netip.MustParseAddr("203.0.113.5"), netip.MustParseAddr("10.1.2.3")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
}

// Which hosts and addresses a policy lets through.
func TestTargetPolicyDecisions(t *testing.T) {
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) { return fakeResolve(host) }
	for name, tc := range map[string]struct {
		allowed, denied []string
		host            string
		refused         bool
	}{
		"no allowed list lets anything through":           {denied: []string{"bad.example"}, host: "good.example"},
		"a denied name":                                   {denied: []string{"bad.example"}, host: "bad.example", refused: true},
		"a denied glob":                                   {denied: []string{"*.internal"}, host: "db.prod.internal", refused: true},
		"a glob needs something before the dot":           {denied: []string{"*.internal"}, host: "internal"},
		"names ignore case and a final dot":               {denied: []string{"bad.example"}, host: "BAD.Example.", refused: true},
		"a denied network the name resolves into":         {denied: []string{"10.0.0.0/8"}, host: "private.example", refused: true},
		"one bad address of several":                      {denied: []string{"10.0.0.0/8"}, host: "mixed.example", refused: true},
		"an allowed name":                                 {allowed: []string{"*.example"}, host: "public.example"},
		"not on the allowed list":                         {allowed: []string{"*.example"}, host: "elsewhere.test", refused: true},
		"an allowed network":                              {allowed: []string{"203.0.113.0/24"}, host: "public.example"},
		"every address must be allowed":                   {allowed: []string{"203.0.113.0/24"}, host: "mixed.example", refused: true},
		"denied wins over allowed":                        {allowed: []string{"*.example"}, denied: []string{"10.0.0.0/8"}, host: "private.example", refused: true},
		"an address target against a network":             {allowed: []string{"203.0.113.0/24"}, host: "203.0.113.9"},
		"an IPv4-mapped address is the IPv4 address":      {denied: []string{"10.0.0.0/8"}, host: "::ffff:10.1.2.3", refused: true},
		"an address entry is that one address":            {denied: []string{"203.0.113.9"}, host: "203.0.113.10"},
		"an IPv6 network":                                 {denied: []string{"fd00::/8"}, host: "fd00::5", refused: true},
		"a name allowed by name needs no allowed address": {allowed: []string{"private.example"}, host: "private.example"},
	} {
		t.Run(name, func(t *testing.T) {
			policy, err := compileTargetPolicy(tc.allowed, tc.denied)
			if err != nil {
				t.Fatal(err)
			}
			_, err = policy.check(context.Background(), tc.host, true)
			if refused := errors.Is(err, ErrTargetRefused); refused != tc.refused || (err != nil && !refused) {
				t.Fatalf("err=%v, want refused=%v", err, tc.refused)
			}
		})
	}
}

// A refused probe is refused before anything is sent, and an allowed one
// goes through.
func TestARefusedTargetIsNotContacted(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	if err := fetchRefused(t, server.URL, policyCollector(t, nil, []string{"127.0.0.0/8"}, false)); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "request.denied_targets") {
		t.Fatalf("err=%v", err)
	}
	if err := fetchRefused(t, server.URL, policyCollector(t, []string{"*.example.com"}, nil, false)); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "request.allowed_targets") {
		t.Fatalf("err=%v", err)
	}
	if hits != 0 {
		t.Fatalf("a refused target was contacted %d times", hits)
	}
	if err := fetchRefused(t, server.URL, policyCollector(t, []string{"127.0.0.1"}, nil, false)); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
}

// A redirect to a host the policy refuses is refused, and the host it
// names is not contacted.
func TestARedirectIsCheckedToo(t *testing.T) {
	var reached bool
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte("secret"))
	}))
	defer forbidden.Close()
	forbiddenURL, _ := url.Parse(forbidden.URL)
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:"+forbiddenURL.Port()+"/", http.StatusFound)
	}))
	defer allowed.Close()
	allowedURL, _ := url.Parse(allowed.URL)
	// The first host is allowed by its name, localhost; the redirect leads to
	// an address the list does not hold.
	c := policyCollector(t, []string{"localhost"}, nil, true)
	err := fetchRefused(t, "http://localhost:"+allowedURL.Port()+"/", c)
	if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("err=%v", err)
	}
	if reached {
		t.Fatal("the redirect's host was contacted")
	}
}

// Behind a proxy the connection is the proxy's, so the check at dial time
// cannot catch a redirect: the redirect's host is refused by the redirect
// check itself, before anything is sent to it. proxyOverride stands in for
// the proxy, which the environment's proxy settings would never use for
// the loopback addresses a test can serve on.
func TestARedirectBehindAProxyIsChecked(t *testing.T) {
	t.Cleanup(func() { proxyOverride = nil })
	proxyOverride = func(*url.URL) bool { return true }
	var reached atomic.Bool
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		_, _ = w.Write([]byte("secret"))
	}))
	defer forbidden.Close()
	forbiddenURL, _ := url.Parse(forbidden.URL)
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:"+forbiddenURL.Port()+"/", http.StatusFound)
	}))
	defer allowed.Close()
	allowedURL, _ := url.Parse(allowed.URL)
	c := policyCollector(t, []string{"localhost"}, nil, true)
	err := fetchRefused(t, "http://localhost:"+allowedURL.Port()+"/", c)
	if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "allowed_targets") {
		t.Fatalf("err=%v, want the redirect refused by request.allowed_targets", err)
	}
	if reached.Load() {
		t.Fatal("the redirect's host was contacted")
	}
}

// Each connection is checked against the address it was made to, so a name
// that resolves elsewhere after the check is still caught; a connection to
// a host the request did not check, a proxy, is not.
func TestEveryConnectionIsChecked(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	policy, err := compileTargetPolicy([]string{"10.0.0.0/8"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// As if localhost had resolved into 10.0.0.0/8 when it was checked.
	guard := &policyGuard{policy: policy, hosts: map[string]bool{"localhost": false}}
	ctx := context.WithValue(context.Background(), policyGuardKey{}, guard)
	dial := policyDialer((&net.Dialer{}).DialContext)
	if _, err := dial(ctx, "tcp", "localhost:"+port); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("a connection to a refused address was kept: %v", err)
	}
	// A host the request did not check is refused, unless something the
	// request checked goes through a proxy, which it then is.
	if _, err := dial(ctx, "tcp", "127.0.0.1:"+port); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("a connection to an unchecked host without a proxy was kept: %v", err)
	}
	guard.proxied = true
	conn, err := dial(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("a connection to an unchecked host, a proxy, was refused: %v", err)
	}
	_ = conn.Close()
}

// request.accept_status: numbers and classes, from YAML numbers or strings;
// an accepted status is the answer, not retried.
func TestAcceptStatus(t *testing.T) {
	for _, bad := range []string{"600", "99", "6xx", "2x", "ok"} {
		c := model.Collector{Name: "a", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptStatus: []string{bad}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "request.accept_status") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AcceptStatus = []string{"2XX", "503"}
		c.Request.Retry.Attempts = 3
	})
	for status, want := range map[int]bool{200: true, 204: true, 503: true, 500: false, 404: false, 301: false} {
		if got := AcceptedStatus(c, RequestOverrides{}, status); got != want {
			t.Errorf("%d: %v", status, got)
		}
	}
	if plain := httpCollector(t, nil); !AcceptedStatus(plain, RequestOverrides{}, 299) || AcceptedStatus(plain, RequestOverrides{}, 503) {
		t.Error("without accept_status every 2xx, and only those, is accepted")
	}
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"state": "maintenance"}`))
	}))
	defer server.Close()
	response, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || hits != 1 {
		t.Fatalf("err=%v status=%v hits=%d: an accepted 503 was retried", err, response, hits)
	}
}

// A target reached directly is resolved once, by the connection, whose
// address is what is checked; one reached through a proxy is resolved
// before the request, since the connection is the proxy's. A refused
// connection is not retried.
func TestAddressesAreCheckedWithoutASecondLookup(t *testing.T) {
	var lookups atomic.Int64
	restoreResolve := resolveHost
	t.Cleanup(func() { resolveHost, proxyOverride = restoreResolve, nil })
	resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		lookups.Add(1)
		return restoreResolve(ctx, host)
	}
	var connections atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	// By name, so the address is only known once looked up.
	byName := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.DeniedTargets = []string{"127.0.0.0/8"}
		c.Request.Retry.Attempts = 3
	})
	if err := fetchRefused(t, byName, c); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("err=%v", err)
	}
	if n := lookups.Load(); n != 0 {
		t.Fatalf("a direct target was looked up %d times before connecting", n)
	}
	// The server sees a connection after the client has closed it.
	for deadline := time.Now().Add(2 * time.Second); connections.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := connections.Load(); n != 1 {
		t.Fatalf("%d connections for a refused target, want the one refused, not retried", n)
	}
	proxyOverride = func(*url.URL) bool { return true }
	if err := fetchRefused(t, byName, c); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("err=%v", err)
	}
	if n := lookups.Load(); n != 1 {
		t.Fatalf("a target behind a proxy was looked up %d times, want once, before the request", n)
	}
	if n := connections.Load(); n != 1 {
		t.Fatal("a target refused before the request was connected to")
	}
}

// Every collector refuses the cloud metadata addresses, written as the target
// or resolved from a name, unless its allowed_targets lists them by address
// or network; an address target is refused before anything is sent.
func TestCloudMetadataIsRefusedByDefault(t *testing.T) {
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "metadata.google.internal" {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return fakeResolve(host)
	}
	for name, tc := range map[string]struct {
		allowed, denied []string
		host            string
		resolve         bool
		refused         bool
	}{
		"the address, with no lists":           {host: "169.254.169.254", refused: true},
		"the IPv6 address":                     {host: "fd00:ec2::254", refused: true},
		"an IPv4-mapped address":               {host: "::ffff:169.254.169.254", refused: true},
		"a name for it, looked up":             {host: "metadata.google.internal", resolve: true, refused: true},
		"with other denied targets":            {denied: []string{"10.0.0.0/8"}, host: "169.254.169.254", refused: true},
		"allowed by name only":                 {allowed: []string{"metadata.google.internal"}, host: "metadata.google.internal", resolve: true, refused: true},
		"allowed by its address":               {allowed: []string{"169.254.169.254"}, host: "169.254.169.254"},
		"allowed by a network":                 {allowed: []string{"169.254.0.0/16"}, host: "169.254.169.254"},
		"the IPv4 allowed leaves IPv6 refused": {allowed: []string{"169.254.169.254"}, host: "fd00:ec2::254", refused: true},
		"another link-local address":           {host: "169.254.1.1"},
		"an ordinary target":                   {host: "public.example", resolve: true},
	} {
		t.Run(name, func(t *testing.T) {
			policy, err := compileTargetPolicy(tc.allowed, tc.denied)
			if err != nil {
				t.Fatal(err)
			}
			_, err = policy.check(context.Background(), tc.host, tc.resolve)
			if refused := errors.Is(err, ErrTargetRefused); refused != tc.refused || (err != nil && !refused) {
				t.Fatalf("err=%v, want refused=%v", err, tc.refused)
			}
			if tc.refused && len(tc.allowed) == 0 && len(tc.denied) == 0 && !strings.Contains(err.Error(), "cloud metadata") {
				t.Fatalf("the refusal does not say why: %v", err)
			}
		})
	}
	// A probe of the address is refused at once, without a connection.
	start := time.Now()
	err := fetchRefused(t, "http://169.254.169.254/latest/meta-data/", httpCollector(t, nil))
	if !errors.Is(err, ErrTargetRefused) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

// An IPv6 address with a zone, fe80::1%eth0, is held to the address rules
// as the address itself: a zone names the interface, not the address, and
// a network never contains a zoned address as Go compares them.
func TestAZonedAddressIsCheckedAsItsAddress(t *testing.T) {
	denied, err := compileTargetPolicy(nil, []string{"fe80::/10"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := denied.check(context.Background(), "fe80::1%eth0", false); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("denied: %v", err)
	}
	metadata, err := compileTargetPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.check(context.Background(), "fd00:ec2::254%eth0", false); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("metadata: %v", err)
	}
	allowed, err := compileTargetPolicy([]string{"fe80::/10"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allowed.check(context.Background(), "fe80::1%eth0", false); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	// So is a target naming one, refused before anything is sent.
	c := policyCollector(t, nil, []string{"fe80::/10"}, false)
	if err := fetchRefused(t, "http://[fe80::1%25eth0]:9100/", c); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("fetch: %v", err)
	}
}
