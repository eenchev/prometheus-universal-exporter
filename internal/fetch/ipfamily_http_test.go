//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// recordingDial is a dial that records the networks it was asked for, and
// fails those in refuse with a refused connection, as an unreachable family
// would; the others it dials for real.
type recordingDial struct {
	mu        sync.Mutex
	networks  []string
	deadlines []time.Duration
	refuse    map[string]bool
}

func (d *recordingDial) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.networks = append(d.networks, network)
	if deadline, ok := ctx.Deadline(); ok {
		d.deadlines = append(d.deadlines, time.Until(deadline))
	}
	refused := d.refuse[network]
	d.mu.Unlock()
	if refused {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

// ip4 and ip6 dial their family alone, prefer_ip4 and prefer_ip6 theirs
// first and the other after it when it fails, with half of the time left
// under a deadline, and no ip_family dials as Go does, both families.
func TestTheIPFamilyIsTheNetworkDialed(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	address := listener.Addr().String()
	for _, c := range []struct {
		family string
		refuse []string
		want   []string
		fails  string
	}{
		{"", nil, []string{"tcp"}, ""},
		{"ip4", nil, []string{"tcp4"}, ""},
		{"ip6", nil, []string{"tcp6"}, ""},
		{"prefer_ip4", nil, []string{"tcp4"}, ""},
		{"prefer_ip6", nil, []string{"tcp6"}, ""},
		{"prefer_ip6", []string{"tcp6"}, []string{"tcp6", "tcp4"}, ""},
		{"prefer_ip4", []string{"tcp4"}, []string{"tcp4", "tcp6"}, ""},
		{"prefer_ip4", []string{"tcp4", "tcp6"}, []string{"tcp4", "tcp6"}, "then over IPv6"},
		{"ip6", []string{"tcp6"}, []string{"tcp6"}, "connection refused"},
	} {
		d := &recordingDial{refuse: map[string]bool{}}
		for _, network := range c.refuse {
			d.refuse[network] = true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := familyDialer(d.dial, c.family)(ctx, "tcp", address)
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		if strings.Join(d.networks, ",") != strings.Join(c.want, ",") {
			t.Errorf("ip_family %q, %v refused: dialed %v, want %v", c.family, c.refuse, d.networks, c.want)
		}
		switch {
		case c.fails == "" && err != nil:
			t.Errorf("ip_family %q, %v refused: %v", c.family, c.refuse, err)
		case c.fails != "" && (err == nil || !strings.Contains(err.Error(), c.fails)):
			t.Errorf("ip_family %q, %v refused: %v, want an error saying %q", c.family, c.refuse, err, c.fails)
		}
		if strings.HasPrefix(c.family, "prefer_") && len(d.deadlines) > 0 && d.deadlines[0] > 5*time.Second+100*time.Millisecond {
			t.Errorf("ip_family %q: the first family was given %s of a 10s deadline, want half", c.family, d.deadlines[0])
		}
	}
	// A dial of anything but TCP, as of a Unix socket, is left as it is.
	d := &recordingDial{}
	if conn, err := familyDialer(d.dial, "ip6")(context.Background(), "udp", address); err == nil {
		_ = conn.Close()
	}
	if len(d.networks) != 1 || d.networks[0] != "udp" {
		t.Errorf("a udp dial under ip6 dialed %v", d.networks)
	}
}

// A collector's ip_family is the family its requests connect over: to a
// target on 127.0.0.1, ip4 and prefer_ip6, which falls back to IPv4, are
// answered and connect, and ip6 fails, saying the target has no IPv6
// address; a probe's ip_family and a static target's do the same, and a
// connection the collector kept over IPv4 does not carry a probe that asked
// for IPv6.
func TestARequestConnectsOverItsIPFamily(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	for _, c := range []struct {
		family string
		fails  bool
	}{{"ip4", false}, {"prefer_ip6", false}, {"prefer_ip4", false}, {"ip6", true}} {
		collector := httpCollector(t, func(x *model.Collector) { x.Request.IPFamily = c.family })
		response, err := FetchCollector(context.Background(), server.URL, collector, RequestOverrides{}, nil)
		if c.fails {
			if err == nil || !strings.Contains(err.Error(), "127.0.0.1 has no IPv6 address, and request.ip_family is ip6") {
				t.Errorf("ip_family %s to 127.0.0.1: %v, want the target's want of an IPv6 address", c.family, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ip_family %s to 127.0.0.1: %v", c.family, err)
			continue
		}
		if response.StatusCode != http.StatusOK {
			t.Errorf("ip_family %s: status %d", c.family, response.StatusCode)
		}
	}

	// The collector's kept IPv4 connection does not answer a probe that
	// asks for IPv6 alone, nor a static target's.
	collector := httpCollector(t, nil)
	if _, err := FetchCollector(context.Background(), server.URL, collector, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	overrides, err := ParseRequestOverrides(url.Values{"ip_family": {"ip6"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), server.URL, collector, overrides, nil); err == nil || !strings.Contains(err.Error(), "no IPv6 address") {
		t.Errorf("a probe's ip_family ip6 after a kept IPv4 connection: %v", err)
	}
	ip6 := "ip6"
	target := TargetOverrides(&model.StaticTarget{Request: model.TargetRequestConfig{IPFamily: &ip6}})
	if target.IPFamily != "ip6" {
		t.Fatalf("a static target's ip_family became %q", target.IPFamily)
	}
	if _, err := FetchCollector(context.Background(), server.URL, collector, target, nil); err == nil || !strings.Contains(err.Error(), "no IPv6 address") {
		t.Errorf("a static target's ip_family ip6 after a kept IPv4 connection: %v", err)
	}
}

// Where IPv6 works, ip6 to ::1 is answered over it.
func TestIP6ConnectsOverIPv6(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 here: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.RemoteAddr)) }))
	server.Listener = listener
	server.Start()
	defer server.Close()
	for _, family := range []string{"ip6", "prefer_ip4", "prefer_ip6"} {
		collector := httpCollector(t, func(x *model.Collector) { x.Request.IPFamily = family })
		response, err := FetchCollector(context.Background(), server.URL, collector, RequestOverrides{}, nil)
		if err != nil {
			t.Errorf("ip_family %s to [::1]: %v", family, err)
			continue
		}
		if !strings.HasPrefix(string(response.Body), "[::1]:") {
			t.Errorf("ip_family %s: the server saw the request from %s", family, response.Body)
		}
	}
	collector := httpCollector(t, func(x *model.Collector) { x.Request.IPFamily = "ip4" })
	if _, err := FetchCollector(context.Background(), server.URL, collector, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "has no IPv4 address") {
		t.Errorf("ip_family ip4 to [::1]: %v", err)
	}
}

// ip_family takes the four values and no other, as a collector's key, a
// probe parameter and a static target's key; and it is refused where the
// exporter does not make the connection to the target: beside a unix_socket
// or a proxy_url, when the configuration loads, as a probe's parameter or a
// static target's key of such a collector, and for a request the
// environment's proxy would carry, before the proxy is asked anything.
func TestTheIPFamilyIsCheckedAndRefusedWhereItCannotHold(t *testing.T) {
	for _, family := range IPFamilies {
		httpCollector(t, func(x *model.Collector) { x.Request.IPFamily = family })
		if _, err := ParseRequestOverrides(url.Values{"ip_family": {family}}); err != nil {
			t.Errorf("probe parameter ip_family=%s: %v", family, err)
		}
	}
	bad := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, IPFamily: "ipv6"}}
	if err := ValidateRequest(&bad); err == nil || !strings.Contains(err.Error(), `ip_family "ipv6" is not ip4, ip6, prefer_ip4 or prefer_ip6`) {
		t.Errorf("request.ip_family ipv6: %v", err)
	}
	for _, value := range []string{"", "ipv6", "IP6"} {
		if _, err := ParseRequestOverrides(url.Values{"ip_family": {value}}); err == nil || !strings.Contains(err.Error(), "invalid ip_family override") {
			t.Errorf("probe parameter ip_family=%q: %v", value, err)
		}
	}
	plain := httpCollector(t, nil)
	wrong := "both"
	if err := CheckTargetRequest(&model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{IPFamily: &wrong}}, plain); err == nil || !strings.Contains(err.Error(), `request.ip_family "both" is not`) {
		t.Errorf("a static target's ip_family both: %v", err)
	}

	proxied := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, ProxyURL: "http://proxy.example:3128", IPFamily: "ip6"}}
	if err := ValidateRequest(&proxied); err == nil || !strings.Contains(err.Error(), "ip_family does not apply to a collector whose requests go through request.proxy_url") {
		t.Errorf("ip_family beside proxy_url: %v", err)
	}
	httpCollector(t, func(x *model.Collector) { x.Request.ProxyURL, x.Request.IPFamily = model.ProxyDirect, "ip6" })
	socketed := model.Collector{Name: "docker", Request: model.RequestConfig{Type: RequestTypeHTTP, IPFamily: "ip4", UnixSocket: "/run/docker.sock", Own: HTTPConfig{}}}
	if err := ValidateRequest(&socketed); err == nil || !strings.Contains(err.Error(), "request.ip_family does not apply to a collector whose requests go to request.unix_socket") {
		t.Errorf("ip_family beside unix_socket: %v", err)
	}

	viaProxy := httpCollector(t, func(x *model.Collector) { x.Request.ProxyURL = "http://proxy.example:3128" })
	socket := socketCollector(t, "docker", "/run/docker.sock")
	ip4 := "ip4"
	for _, c := range []*model.Collector{viaProxy, socket} {
		if err := CheckOverrideParams(c, url.Values{"ip_family": {"ip4"}}); err == nil || !strings.Contains(err.Error(), "ip_family does not apply") {
			t.Errorf("collector with proxy %q socket %q, probe parameter ip_family: %v", c.Request.ProxyURL, CollectorPolicy(c).UnixSocket, err)
		}
		if err := CheckTargetRequest(&model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{IPFamily: &ip4}}, c); err == nil || !strings.Contains(err.Error(), "ip_family does not apply") {
			t.Errorf("collector with proxy %q socket %q, static target's ip_family: %v", c.Request.ProxyURL, CollectorPolicy(c).UnixSocket, err)
		}
	}

	environment := proxyEnvironment(t)
	unresolvable(t, nil)
	collector := httpCollector(t, func(x *model.Collector) { x.Request.IPFamily = "prefer_ip6" })
	_, err := FetchCollector(context.Background(), "http://site.example/", collector, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "request.ip_family is prefer_ip6, and the request to site.example goes through a proxy") {
		t.Errorf("ip_family through the environment's proxy: %v", err)
	}
	if asked, _ := environment.seen(); len(asked) != 0 {
		t.Errorf("the environment's proxy was asked %v", asked)
	}
	// Without ip_family the same request goes through it.
	if _, err := FetchCollector(context.Background(), "http://site.example/", httpCollector(t, nil), RequestOverrides{}, nil); err != nil {
		t.Errorf("without ip_family, through the environment's proxy: %v", err)
	}
	if asked, _ := environment.seen(); len(asked) != 1 {
		t.Errorf("the environment's proxy was asked %v, want the request without ip_family", asked)
	}
}

// noAddressOf tells a dial to a host with no address of the family, by an
// address written of the other or a lookup that found none, from any other
// failure.
func TestNoAddressOfTheFamily(t *testing.T) {
	_, err := (&net.Dialer{}).DialContext(context.Background(), "tcp6", "127.0.0.1:1")
	if !noAddressOf(err) {
		t.Errorf("tcp6 to 127.0.0.1: %v is not a want of an address", err)
	}
	if !noAddressOf(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "v4only.example", IsNotFound: true}}) {
		t.Error("a lookup that found nothing is a want of an address")
	}
	if noAddressOf(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}) || noAddressOf(errors.New("timeout")) {
		t.Error("a refused connection is no want of an address")
	}
}

// prefer_ip6 to a target whose IPv6 address never answers falls back to
// IPv4 within the probe, through the transport the request is made with:
// net/http dials with a context that has dropped the request's deadline,
// so the deadline reaches the dial as a value, and IPv6 is given half of
// what is left of it, under the probe's deadline and under the request's
// own timeout alike. A blackholed IPv6 address is a dial that waits for
// its context, as one to an address that never answers waits for the
// Dialer's timeout.
func TestPreferIP6FallsBackWithinTheProbeWhenIPv6NeverAnswers(t *testing.T) {
	tier.Slow(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	collector := httpCollector(t, func(x *model.Collector) { x.Request.IPFamily = "prefer_ip6"; x.Request.NewConnection = true })
	if _, err := FetchCollector(context.Background(), server.URL, collector, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var dialed []string
	var ip6For []time.Duration
	blackhole := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, network)
		mu.Unlock()
		if network == "tcp6" {
			start := time.Now()
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second): // the transport's Dialer.Timeout
			}
			mu.Lock()
			ip6For = append(ip6For, time.Since(start))
			mu.Unlock()
			return nil, &net.OpError{Op: "dial", Net: network, Err: context.DeadlineExceeded}
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	transports.mu.Lock()
	var replaced []*http.Transport
	for settings, entry := range transports.entries {
		if settings.ipFamily == "prefer_ip6" {
			entry.transport.CloseIdleConnections()
			entry.transport.DialContext = policyDialer(familyDialer(blackhole, "prefer_ip6"))
			replaced = append(replaced, entry.transport)
		}
	}
	transports.mu.Unlock()
	if len(replaced) == 0 {
		t.Fatal("no prefer_ip6 transport was made")
	}
	defer func() {
		transports.mu.Lock()
		defer transports.mu.Unlock()
		for settings, entry := range transports.entries {
			if settings.ipFamily == "prefer_ip6" {
				delete(transports.entries, settings)
				entry.transport.CloseIdleConnections()
			}
		}
	}()
	for _, c := range []struct {
		name      string
		deadline  time.Duration
		overrides RequestOverrides
	}{
		{"the probe's deadline", 4 * time.Second, RequestOverrides{}},
		{"the request's timeout", time.Minute, RequestOverrides{Timeout: 4 * time.Second}},
	} {
		mu.Lock()
		dialed, ip6For = nil, nil
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), c.deadline)
		start := time.Now()
		response, err := FetchCollector(ctx, server.URL, collector, c.overrides, nil)
		took := time.Since(start)
		cancel()
		mu.Lock()
		if err != nil || response.StatusCode != http.StatusOK || strings.Join(dialed, ",") != "tcp6,tcp4" {
			t.Errorf("prefer_ip6 to a blackholed IPv6 address under %s of 4s: dialed %v, took %s: %v", c.name, dialed, took, err)
		}
		if len(ip6For) != 1 || ip6For[0] > 2500*time.Millisecond {
			t.Errorf("under %s of 4s IPv6 was given %v, want about half", c.name, ip6For)
		}
		mu.Unlock()
	}
}
