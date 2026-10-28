//go:build !select_request_types || request_type_http

package fetch

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector's request.proxy_url (proxy.go).

// proxiedCollector is an http collector, validated, through proxyURL.
func proxiedCollector(t *testing.T, name, proxyURL string) *model.Collector {
	t.Helper()
	c := testutil.Collector(name, "text")
	c.Request.ProxyURL = proxyURL
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// A collector's proxy carries its requests, in place of the environment's:
// an http target's request whole, with the credentials its files hold as
// Proxy-Authorization, and an https target's through a CONNECT tunnel, with
// TLS end to end with the target. The environment's proxy is asked nothing.
func TestACollectorsProxyCarriesItsRequestsInPlaceOfTheEnvironments(t *testing.T) {
	environment := proxyEnvironment(t)
	unresolvable(t, nil)
	proxy := newTestProxy(t)
	c := proxiedCollector(t, "api", proxy.URL)
	c.Request.ProxyBasicAuthFile = proxyCredentials(t, "scraper", "s3cret")
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}

	response, err := FetchCollector(context.Background(), "http://metrics.partner.example:8080/status", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Body), "url=http://metrics.partner.example:8080/status") {
		t.Fatalf("the answer is %q, want the proxy's", response.Body)
	}

	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "value=9\n")
	}))
	t.Cleanup(target.Close)
	c.Request.TLS = model.TLSConfig{InsecureSkipVerify: true}
	response, err = FetchCollector(context.Background(), target.URL+"/status", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Body) != "value=9\n" {
		t.Fatalf("the answer is %q, want the target's through the tunnel", response.Body)
	}

	asked, auth := proxy.seen()
	want := []string{"GET http://metrics.partner.example:8080/status", "CONNECT " + strings.TrimPrefix(target.URL, "https://")}
	if strings.Join(asked, "|") != strings.Join(want, "|") {
		t.Fatalf("the proxy was asked %q, want %q", asked, want)
	}
	credential := "Basic " + base64.StdEncoding.EncodeToString([]byte("scraper:s3cret"))
	for i, got := range auth {
		if got != credential {
			t.Errorf("request %d carried Proxy-Authorization %q, want %q", i, got, credential)
		}
	}
	if asked, _ := environment.seen(); len(asked) != 0 {
		t.Fatalf("the environment's proxy was asked %q", asked)
	}
}

// proxy_url: direct reaches the target itself, though the environment names
// a proxy: a name only the proxy would resolve fails to resolve here, and
// the environment's proxy, which a collector without proxy_url is sent
// through, is asked nothing.
func TestProxyURLDirectReachesTheTargetPastTheEnvironmentsProxy(t *testing.T) {
	environment := proxyEnvironment(t)
	unresolvable(t, nil)
	c := testutil.Collector("api", "text")
	if _, err := FetchCollector(context.Background(), "http://api.example/status", &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if asked, _ := environment.seen(); len(asked) != 1 {
		t.Fatalf("without proxy_url the environment's proxy was asked %q, want the request", asked)
	}

	direct := proxiedCollector(t, "direct", model.ProxyDirect)
	_, err := FetchCollector(context.Background(), "http://api.example/status", direct, RequestOverrides{}, nil)
	var lookup *net.DNSError
	if !errors.As(err, &lookup) || lookup.Name != "api.example" {
		t.Fatalf("err = %v, want the target's name looked up here", err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "value=3\n")
	}))
	t.Cleanup(target.Close)
	response, err := FetchCollector(context.Background(), target.URL, direct, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Body) != "value=3\n" {
		t.Fatalf("the answer is %q, want the target's", response.Body)
	}
	if asked, _ := environment.seen(); len(asked) != 1 {
		t.Fatalf("with proxy_url: direct the environment's proxy was asked %q", asked)
	}
}

// Two collectors with different proxies, or one proxy with different
// credentials, never share a connection pool: each request reaches the proxy
// its own collector names, with its own credentials.
func TestCollectorsWithDifferentProxiesShareNoConnection(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, nil)
	first, second := newTestProxy(t), newTestProxy(t)
	a := proxiedCollector(t, "a", first.URL)
	b := proxiedCollector(t, "b", second.URL)
	c := proxiedCollector(t, "c", first.URL)
	c.Request.ProxyBasicAuthFile = proxyCredentials(t, "other", "secret")
	for range 2 {
		for _, collector := range []*model.Collector{a, b, c} {
			if _, err := FetchCollector(context.Background(), "http://shared.example/status", collector, RequestOverrides{}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	firstAsked, firstAuth := first.seen()
	secondAsked, _ := second.seen()
	if len(firstAsked) != 4 || len(secondAsked) != 2 {
		t.Fatalf("the first proxy was asked %q and the second %q, want 4 and 2 requests", firstAsked, secondAsked)
	}
	withCredential := 0
	for _, auth := range firstAuth {
		if auth != "" {
			withCredential++
		}
	}
	if withCredential != 2 {
		t.Fatalf("the first proxy saw credentials %q, want them on c's two requests alone", firstAuth)
	}
	if got := transports.size(); got != 3 {
		t.Fatalf("%d connection pools, want one for each proxy and credential", got)
	}
}

// A login goes through the collector's proxy, as its requests do.
func TestALoginGoesThroughTheCollectorsProxy(t *testing.T) {
	environment := proxyEnvironment(t)
	unresolvable(t, nil)
	freshLogins(t)
	proxy := newTestProxy(t)
	proxy.forward = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = io.WriteString(w, `{"access_token":"tok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "value=5\n")
	}
	c := testutil.Collector("api", "text")
	c.Request.ProxyURL = proxy.URL
	c.Request.Login = &model.LoginConfig{Type: "json", URL: "http://auth.example/login", Token: ".access_token", FieldFiles: secretFiles(t, map[string]string{"password": "p"})}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	response, err := FetchCollector(context.Background(), "http://api.example/status", &c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Body) != "value=5\n" {
		t.Fatalf("the answer is %q", response.Body)
	}
	asked, _ := proxy.seen()
	if strings.Join(asked, "|") != "POST http://auth.example/login|GET http://api.example/status" {
		t.Fatalf("the proxy was asked %q, want the login and the request", asked)
	}
	if asked, _ := environment.seen(); len(asked) != 0 {
		t.Fatalf("the environment's proxy was asked %q", asked)
	}
}

// allowed_targets and denied_targets apply to the target through a
// collector's proxy, by name and by the addresses the exporter resolves it
// to, and the proxy is asked nothing for a refused one; the proxy's own
// address is the configuration's, and is not held to them.
func TestTheTargetPolicyAppliesToTheTargetBehindACollectorsProxy(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, map[string]string{"inside.example": "10.1.2.3", "public.example": "192.0.2.10"})
	proxy := newTestProxy(t)
	for name, c := range map[string]struct {
		allowed, denied []string
		target          string
		refused         bool
	}{
		"a denied name":                      {denied: []string{"*.blocked.example"}, target: "http://api.blocked.example/", refused: true},
		"a name resolving in a denied net":   {denied: []string{"10.0.0.0/8"}, target: "http://inside.example/", refused: true},
		"an address in a denied network":     {denied: []string{"10.0.0.0/8"}, target: "http://10.9.9.9/", refused: true},
		"the metadata address":               {target: "http://169.254.169.254/", refused: true},
		"a name not allowed":                 {allowed: []string{"api.example"}, target: "http://other.example/", refused: true},
		"an allowed name, the proxy is not":  {allowed: []string{"api.example"}, target: "http://api.example/"},
		"a denied loopback, the proxy's own": {denied: []string{"127.0.0.0/8"}, target: "http://public.example/"},
	} {
		collector := testutil.Collector("api", "text")
		collector.Request.ProxyURL = proxy.URL
		collector.Request.AllowedTargets, collector.Request.DeniedTargets = c.allowed, c.denied
		if err := ValidateRequest(&collector); err != nil {
			t.Fatal(err)
		}
		before, _ := proxy.seen()
		_, err := FetchCollector(context.Background(), c.target, &collector, RequestOverrides{}, nil)
		after, _ := proxy.seen()
		if refused := errors.Is(err, ErrTargetRefused); refused != c.refused {
			t.Errorf("%s: err = %v, want refused %v", name, err, c.refused)
		}
		if asked := len(after) - len(before); asked != map[bool]int{true: 0, false: 1}[c.refused] {
			t.Errorf("%s: the proxy was asked %d times", name, asked)
		}
	}
}

// What a probe's caller is told of a failure at a collector's proxy leaves
// out the proxy: its address when it cannot be reached, its name when it
// does not resolve, and the words it refused a tunnel with; the error keeps
// them for the log.
func TestTheCallerIsNotToldOfTheCollectorsProxy(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, nil)
	refusing := newTestProxy(t)
	refusing.refusal = "403 Blocked by rule 12 at proxy-7.corp"
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for name, c := range map[string]struct{ proxyURL, kept string }{
		"a refused tunnel":   {refusing.URL, "proxy-7.corp"},
		"an unreachable one": {closedURL, strings.TrimPrefix(closedURL, "http://")},
		"an unknown name":    {"http://proxy.corp.example:3128", "proxy.corp.example"},
	} {
		collector := proxiedCollector(t, "api", c.proxyURL)
		collector.Request.TLS = model.TLSConfig{InsecureSkipVerify: true}
		_, err := FetchCollector(context.Background(), "https://api.example/status", collector, RequestOverrides{}, nil)
		if err == nil {
			t.Fatalf("%s: the fetch succeeded", name)
		}
		if !strings.Contains(err.Error(), c.kept) {
			t.Errorf("%s: the error %q does not keep %q for the log", name, err, c.kept)
		}
		told := model.CallerText(err)
		if strings.Contains(told, c.kept) || !strings.Contains(told, model.WithheldMark) || !strings.Contains(told, "api.example") {
			t.Errorf("%s: the caller is told %q", name, told)
		}
	}
}

// An https:// proxy is greeted over TLS verified against the proxy's roots,
// without the target's client certificate, CA or server name, which are
// presented and used to the target through the tunnel.
func TestAnHTTPSProxyIsGreetedWithoutTheTargetsTLSSettings(t *testing.T) {
	proxyEnvironment(t)
	proxyCert, proxyKey := selfSigned(t, "proxy", "localhost")
	cert, err := tls.LoadX509KeyPair(proxyCert, proxyKey)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &testProxy{}
	var presented []string
	var mu sync.Mutex
	proxy.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		presented = append(presented, strconv.Itoa(len(r.TLS.PeerCertificates)))
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS12}
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(proxyCert)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(pem)
	previous := proxyTLSRoots
	proxyTLSRoots = roots
	t.Cleanup(func() { proxyTLSRoots = previous })

	targetURL, targetCA := identityServer(t, func(w http.ResponseWriter, _ *http.Request, presented string) {
		_, _ = io.WriteString(w, "value=1\npresented="+presented+"\n")
	})
	clientCert, clientKey := selfSigned(t, "scraper")
	c := proxiedCollector(t, "api", strings.Replace(proxy.URL, "127.0.0.1", "localhost", 1))
	c.Request.TLS = model.TLSConfig{CAFile: targetCA, CertFile: clientCert, KeyFile: clientKey}
	response, err := FetchCollector(context.Background(), targetURL+"/status", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Body), "presented=scraper") {
		t.Fatalf("the target answered %q, want the client certificate presented to it", response.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(presented, ",") != "0" {
		t.Fatalf("the proxy saw client certificates %q, want none", presented)
	}
}

// A socks5:// proxy is asked to connect to the target by name, with the
// credentials its files hold.
func TestASOCKS5ProxyConnectsToTheTargetByName(t *testing.T) {
	proxyEnvironment(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "value=2\n")
	}))
	t.Cleanup(target.Close)
	socks, seen := socks5Proxy(t)
	c := proxiedCollector(t, "api", "socks5://"+socks)
	c.Request.ProxyBasicAuthFile = proxyCredentials(t, "scraper", "s3cret")
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	targetURL := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	response, err := FetchCollector(context.Background(), targetURL+"/status", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Body) != "value=2\n" {
		t.Fatalf("the answer is %q", response.Body)
	}
	if got, want := seen(), "scraper:s3cret localhost:"+target.URL[strings.LastIndex(target.URL, ":")+1:]; got != want {
		t.Fatalf("the SOCKS proxy saw %q, want %q", got, want)
	}
}

// socks5Proxy is a SOCKS5 proxy (RFC 1928) taking a username and a password
// (RFC 1929), which connects where it is asked, and reports the credentials
// and the address of the last request.
func socks5Proxy(t *testing.T) (addr string, seen func() string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	last := ""
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				read := func(n int) []byte {
					b := make([]byte, n)
					if _, err := io.ReadFull(r, b); err != nil {
						return nil
					}
					return b
				}
				head := read(2)
				if head == nil || read(int(head[1])) == nil {
					return
				}
				_, _ = conn.Write([]byte{5, 2})
				sub := read(2)
				if sub == nil {
					return
				}
				user := read(int(sub[1]))
				passLen := read(1)
				if passLen == nil {
					return
				}
				pass := read(int(passLen[0]))
				_, _ = conn.Write([]byte{1, 0})
				req := read(4)
				if req == nil {
					return
				}
				var host string
				switch req[3] {
				case 1:
					host = net.IP(read(4)).String()
				case 3:
					host = string(read(int(read(1)[0])))
				case 4:
					host = net.IP(read(16)).String()
				}
				port := binary.BigEndian.Uint16(read(2))
				dest := net.JoinHostPort(host, strconv.Itoa(int(port)))
				mu.Lock()
				last = string(user) + ":" + string(pass) + " " + dest
				mu.Unlock()
				upstream, err := net.Dial("tcp", dest)
				if err != nil {
					_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer func() { _ = upstream.Close() }()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				pipe(conn, r, upstream)
			}()
		}
	}()
	return listener.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// request.proxy_url is checked when the configuration loads: a scheme a
// proxy is reached by, a host, nothing after the port, no placeholder, and
// no credentials, which go in proxy_basic_auth_file and are never quoted in
// the refusal; and proxy_basic_auth_file needs a proxy and both its files.
func TestAProxyURLIsCheckedWhenItLoads(t *testing.T) {
	for name, c := range map[string]struct {
		proxyURL string
		files    *model.ProxyBasicAuthFile
		want     string
	}{
		"credentials":           {proxyURL: "http://scraper:s3cret@proxy.example:3128", want: "carries credentials; write it without them, and give them in request.proxy_basic_auth_file"},
		"a username alone":      {proxyURL: "http://s3cret@proxy.example:3128", want: "carries credentials"},
		"an unknown scheme":     {proxyURL: "ftp://proxy.example", want: `has the scheme "ftp"`},
		"no scheme":             {proxyURL: "proxy.example:3128", want: `has the scheme "proxy.example"`},
		"no host":               {proxyURL: "http://:3128", want: "names no host"},
		"a path":                {proxyURL: "http://proxy.example:3128/s3cret", want: "has more than a scheme, a host and a port"},
		"a query":               {proxyURL: "http://proxy.example:3128?token=s3cret", want: "has more than a scheme, a host and a port"},
		"a port out of range":   {proxyURL: "http://proxy.example:70000", want: "a port is from 1 to 65535"},
		"a placeholder":         {proxyURL: "http://{{param_proxy}}:3128", want: "has a placeholder"},
		"DIRECT":                {proxyURL: "DIRECT", want: `has the scheme ""`},
		"credentials, no proxy": {files: &model.ProxyBasicAuthFile{Username: "/u", Password: "/p"}, want: "is set without a request.proxy_url"},
		"credentials, direct":   {proxyURL: model.ProxyDirect, files: &model.ProxyBasicAuthFile{Username: "/u", Password: "/p"}, want: "is set without a request.proxy_url"},
		"half the credentials":  {proxyURL: "http://proxy.example:3128", files: &model.ProxyBasicAuthFile{Username: "/u"}, want: "requires username and password paths"},
	} {
		collector := testutil.Collector("api", "text")
		collector.Request.ProxyURL, collector.Request.ProxyBasicAuthFile = c.proxyURL, c.files
		err := ValidateRequest(&collector)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: the refusal %q quotes the secret", name, err)
		}
	}
	for _, accepted := range []string{"", model.ProxyDirect, "http://proxy.example:3128", "https://proxy.example", "socks5://10.0.0.5:1080", "socks5h://proxy.example:1080", "http://[fd00::5]:3128/", "HTTP://proxy.example:3128", "Socks5H://proxy.example:1080"} {
		collector := testutil.Collector("api", "text")
		collector.Request.ProxyURL = accepted
		if err := ValidateRequest(&collector); err != nil {
			t.Errorf("%q: %v", accepted, err)
		}
	}
}

// The credentials of a proxy are read from their files on every request,
// and what they hold appears in no error: neither one of a credential file
// that cannot be read nor one of the proxy refusing them.
func TestAProxysCredentialsAreReadFromFilesAndNeverShown(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, nil)
	proxy := newTestProxy(t)
	proxy.refusal = "407 Proxy Authentication Required"
	c := proxiedCollector(t, "api", proxy.URL)
	c.Request.ProxyBasicAuthFile = proxyCredentials(t, "scraper", "first-s3cret")
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	c.Request.TLS = model.TLSConfig{InsecureSkipVerify: true}
	_, err := FetchCollector(context.Background(), "https://api.example/", c, RequestOverrides{}, nil)
	if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(model.CallerText(err), "s3cret") {
		t.Fatalf("err = %v", err)
	}
	if err := os.WriteFile(c.Request.ProxyBasicAuthFile.Password, []byte("second-s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = FetchCollector(context.Background(), "https://api.example/", c, RequestOverrides{}, nil)
	_, auth := proxy.seen()
	if len(auth) != 2 || auth[1] != "Basic "+base64.StdEncoding.EncodeToString([]byte("scraper:second-s3cret")) {
		t.Fatalf("the proxy saw %q, want the rotated password at the second request", auth)
	}
	if err := os.Remove(c.Request.ProxyBasicAuthFile.Password); err != nil {
		t.Fatal(err)
	}
	_, err = FetchCollector(context.Background(), "https://api.example/", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "reading request.proxy_basic_auth_file password file") {
		t.Fatalf("err = %v", err)
	}
}

// A proxy's credential file that cannot be read fails the request at once,
// as a basic_auth_file that cannot be read does: a retry would read it again
// after the backoff, to fail the same way.
func TestAProxysUnreadableCredentialsAreNotRetried(t *testing.T) {
	proxyEnvironment(t)
	unresolvable(t, nil)
	proxy := newTestProxy(t)
	c := proxiedCollector(t, "api", proxy.URL)
	c.Request.ProxyBasicAuthFile = proxyCredentials(t, "scraper", "s3cret")
	c.Request.Retry.Attempts = 3
	c.Request.Retry.Backoff = model.Duration(time.Minute)
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.Request.ProxyBasicAuthFile.Password); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := FetchCollector(ctx, "http://metrics.partner.example:8080/status", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "reading request.proxy_basic_auth_file password file") || strings.Contains(err.Error(), "cut short") {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the request took %s, want it to fail at once without waiting out the retries' backoff", took)
	}
	if asked, _ := proxy.seen(); len(asked) != 0 {
		t.Fatalf("the proxy was asked %q", asked)
	}
}

// A SOCKS proxy's failure, its address and what its handshake said, is
// withheld from a probe's caller whole, as a proxyconnect failure is; the log
// has it.
func TestASOCKSProxysFailureIsWithheldFromTheCaller(t *testing.T) {
	proxyEnvironment(t)
	socks, _ := socks5Proxy(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	c := proxiedCollector(t, "api", "socks5://"+socks)
	c.Request.ProxyBasicAuthFile = proxyCredentials(t, "scraper", "s3cret")
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	_, err = FetchCollector(context.Background(), "http://localhost:"+strconv.Itoa(port)+"/status", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "socks connect tcp "+socks+"->localhost:") {
		t.Fatalf("err = %v, want the SOCKS proxy's failure", err)
	}
	told := model.CallerText(err)
	if !strings.Contains(told, "socks connect tcp: "+model.WithheldMark) || strings.Contains(told, socks) || strings.Contains(told, "refused") {
		t.Fatalf("the caller is told %q", told)
	}
}
