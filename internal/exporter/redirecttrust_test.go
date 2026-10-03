//go:build !select_request_types || request_type_http

package exporter

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a followed redirect carries, as a probe sees it
// (fetch/redirecttrust.go).

// leavingTarget answers /start with a redirect to /end on the same server
// under its other name, localhost, which is another host and so another
// origin. /end answers the document, or, once a status is set, that status
// to a request without a credential. It records the Authorization and the
// API key each path was sent.
type leavingTarget struct {
	*httptest.Server
	elsewhere string
	mu        sync.Mutex
	sent      map[string]string
	status    int
}

func newLeavingTarget(t *testing.T) *leavingTarget {
	t.Helper()
	target := &leavingTarget{sent: map[string]string{}}
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.mu.Lock()
		target.sent[r.URL.Path] = r.Header.Get("Authorization") + "|" + r.Header.Get("X-Api-Key")
		status := target.status
		target.mu.Unlock()
		switch {
		case r.URL.Path == "/start":
			http.Redirect(w, r, target.elsewhere+"/end", http.StatusFound)
		case status != 0 && r.Header.Get("Authorization") == "":
			http.Error(w, "who are you", status)
		default:
			_, _ = w.Write([]byte(`{"up":1,"latency":0.2}`))
		}
	}))
	t.Cleanup(target.Close)
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	target.elsewhere = "http://" + net.JoinHostPort("localhost", u.Port())
	return target
}

// answers sets the status /end answers a request without a credential.
func (r *leavingTarget) answers(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// sentTo is the Authorization and the API key path was last sent, joined
// by |.
func (r *leavingTarget) sentTo(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[path]
}

// redirectCollector is debugCollector, which sends a bearer token and an API
// key, following redirects and trusting the hosts given.
func redirectCollector(name string, trusted ...string) model.Collector {
	c := debugCollector(name)
	c.Request.Query = nil
	c.Request.FollowRedirects = true
	c.Request.RedirectTrustedHosts = trusted
	return c
}

// A status that fails the probe, answered by a host a redirect led to and
// that was not sent the collector's headers, is reported with that: the host,
// that it was sent neither headers nor credentials, and the setting that
// trusts it. With the host listed the probe succeeds, and a 401 the target
// itself answers is reported as it always was.
func TestAStatusAfterAnUntrustedRedirectSaysWhatWasNotSent(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newLeavingTarget(t)
	server := modeServer(t, redirectCollector("wary"), redirectCollector("trusting", "localhost"))
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		target.answers(status)
		got := probeOnce(t, server, probePath("wary", target.URL+"/start", ""), nil)
		want := "collector wary http_status failed: received HTTP status " + strconv.Itoa(status) + " from localhost, where a redirect led: the collector's headers and credentials were not sent to that host, which is not the origin the request was made to; list it in request.redirect_trusted_hosts if it is to be sent them"
		if got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), want) {
			t.Errorf("%d: answered %d %q, want %q in it", status, got.Code, got.Body.String(), want)
		}
		if sent := target.sentTo("/end"); sent != "|" {
			t.Errorf("%d: the redirect was sent %q", status, sent)
		}
	}
	if got := probeOnce(t, server, probePath("trusting", target.URL+"/start", ""), nil); got.Code != http.StatusOK || target.sentTo("/end") != "Bearer s3cret-bearer|s3cret-key" {
		t.Errorf("the listed host: answered %d %q, and was sent %q", got.Code, got.Body.String(), target.sentTo("/end"))
	}
	// The target's own status says nothing of redirects.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(refusing.Close)
	got := probeOnce(t, server, probePath("wary", refusing.URL, ""), nil)
	if body := strings.TrimSpace(got.Body.String()); got.Code != http.StatusBadGateway || !strings.HasSuffix(body, "http_status failed: received HTTP status 401") {
		t.Errorf("the target's own 401: answered %d %q", got.Code, body)
	}
}

// No probe parameter sets or extends the list: a probe naming
// redirect_trusted_hosts is answered as though it had not, and the host it
// names is sent nothing of the collector's. Following switched on by the
// probe keeps to the collector's own list.
func TestAProbeCannotTrustARedirectsHost(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newLeavingTarget(t)
	wary, trusting := redirectCollector("wary"), redirectCollector("trusting", "localhost")
	wary.Request.FollowRedirects, trusting.Request.FollowRedirects = false, false
	server := modeServer(t, wary, trusting)
	for _, extra := range []string{"&follow_redirects=true&redirect_trusted_hosts=localhost", "&follow_redirects=true&redirect_trusted_hosts=*"} {
		got := probeOnce(t, server, probePath("wary", target.URL+"/start", extra), nil)
		if got.Code != http.StatusOK || target.sentTo("/start") != "Bearer s3cret-bearer|s3cret-key" || target.sentTo("/end") != "|" {
			t.Errorf("%s: answered %d %q; the target was sent %q and the redirect %q", extra, got.Code, got.Body.String(), target.sentTo("/start"), target.sentTo("/end"))
		}
	}
	got := probeOnce(t, server, probePath("trusting", target.URL+"/start", "&follow_redirects=true"), nil)
	if got.Code != http.StatusOK || target.sentTo("/end") != "Bearer s3cret-bearer|s3cret-key" {
		t.Errorf("the collector's own list: answered %d, the redirect was sent %q", got.Code, target.sentTo("/end"))
	}
}

// A debug probe lists a redirect with the headers it was sent, not the ones
// the first request had, and under a hop that was not trusted the names of
// the headers that were not sent and why; their values appear nowhere under
// it.
func TestADebugProbeShowsWhatARedirectWasSent(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newLeavingTarget(t)
	server := modeServer(t, redirectCollector("wary"), redirectCollector("trusting", "localhost"))
	server.SetProbeDebug(true)
	query := "&debug=true&target=" + url.QueryEscape(target.URL+"/start")

	body := debugProbeGet(t, server, "collector=wary"+query).Body.String()
	assertContains(t, body,
		"  1. GET "+target.URL+"/start -> 302 Found",
		"     Accept: application/json\n     Authorization: <redacted>\n     X-Api-Key: <redacted>\n  2. GET "+target.elsewhere+"/end (redirect) -> 200 OK",
		"     Accept: application/json\n     not sent: Authorization, X-Api-Key — the redirect leads to a host that is not the origin the request was made to and is not in request.redirect_trusted_hosts\n\nResponse\n",
	)

	body = debugProbeGet(t, server, "collector=trusting"+query).Body.String()
	_, second, found := strings.Cut(body, "/end (redirect)")
	if !found || strings.Contains(body, "not sent:") {
		t.Fatalf("a trusted hop:\n%s", body)
	}
	if hop, _, _ := strings.Cut(second, "\nResponse\n"); !strings.Contains(hop, "     Accept: application/json\n     Authorization: <redacted>\n     X-Api-Key: <redacted>") || strings.Contains(hop, "Referer") {
		t.Fatalf("a trusted hop is listed as\n%s", hop)
	}
}

// The target's own host on another port is another origin, and the host
// alone would not say why: the status that fails the probe, and the debug
// report's line under the redirect, name both origins, with the port that
// tells them apart, and the host to list.
func TestARedirectToTheSameHostOnAnotherPortNamesBothOrigins(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newLeavingTarget(t)
	other := httptest.NewServer(target.Config.Handler)
	t.Cleanup(other.Close)
	target.elsewhere = other.URL
	target.answers(http.StatusUnauthorized)
	server := modeServer(t, redirectCollector("wary"))
	got := probeOnce(t, server, probePath("wary", target.URL+"/start", ""), nil)
	want := "collector wary http_status failed: received HTTP status 401 from " + other.URL + ", where a redirect led: the collector's headers and credentials were not sent there, since " + other.URL + " is not the origin the request was made to, " + target.URL + "; list 127.0.0.1 in request.redirect_trusted_hosts if it is to be sent them"
	if got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), want) {
		t.Errorf("answered %d %q, want %q in it", got.Code, got.Body.String(), want)
	}
	if sent := target.sentTo("/end"); sent != "|" {
		t.Errorf("the redirect was sent %q", sent)
	}
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=wary&debug=true&target="+url.QueryEscape(target.URL+"/start")).Body.String()
	assertContains(t, body, "     not sent: Authorization, X-Api-Key — the redirect leads to "+other.URL+", which is not the origin the request was made to, "+target.URL+", and whose host is not in request.redirect_trusted_hosts\n")
}

// A credential written into the target URL is one the redirect's host is not
// sent either. Where it is the collector's only one, the status that fails
// the probe still says that credentials were not sent, and the debug report
// names it under the redirect.
func TestCredentialsOfTheTargetURLAreNamedAsNotSent(t *testing.T) {
	testutil.CaptureLogs(t)
	target := newLeavingTarget(t)
	target.answers(http.StatusUnauthorized)
	bare := redirectCollector("bare")
	bare.Request.BearerToken, bare.Request.Headers = "", nil
	server := modeServer(t, bare)
	withUser := strings.Replace(target.URL, "http://", "http://scraper:pass@", 1) + "/start"
	got := probeOnce(t, server, probePath("bare", withUser, ""), nil)
	if want := "received HTTP status 401 from localhost, where a redirect led: the collector's headers and credentials were not sent to that host"; got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), want) {
		t.Errorf("answered %d %q, want %q in it", got.Code, got.Body.String(), want)
	}
	if target.sentTo("/start") != "Basic c2NyYXBlcjpwYXNz|" || target.sentTo("/end") != "|" {
		t.Errorf("the target was sent %q and the redirect %q", target.sentTo("/start"), target.sentTo("/end"))
	}
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=bare&debug=true&target="+url.QueryEscape(withUser)).Body.String()
	assertContains(t, body, "     not sent: Authorization (the target URL's credentials) — the redirect leads to a host that is not the origin the request was made to and is not in request.redirect_trusted_hosts\n")
	if strings.Contains(body, "pass@") {
		t.Errorf("the report shows the password:\n%s", body)
	}
}
