//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A graphite request whose expressions are too long for a URL is posted as a
// form, and that form is a request body like any other: a 307 on the render
// API's own origin has it sent again, and one to a host the collector does
// not trust is refused, the host receiving nothing; listed in
// request.redirect_trusted_hosts, the host is sent the form.
func TestAGraphiteFormIsNotPostedToAnUntrustedHost(t *testing.T) {
	type posted struct{ host, path, method, body string }
	var mu sync.Mutex
	var seen []posted
	location := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, posted{r.Host, r.URL.Path, r.Method, string(body)})
		redirect := location
		mu.Unlock()
		if r.URL.Path == "/render" {
			w.Header().Set("Location", redirect)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, "[]")
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := "http://" + net.JoinHostPort("localhost", u.Port())
	requests := func(to string) []posted {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen, location = nil, to
		return out
	}
	long := "sumSeries(" + strings.Repeat("app.requests.count,", 150) + "app.errors)"
	c := graphiteCollector(long)
	c.Request.FollowRedirects = true
	c.Request.Retry.Attempts = 2
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}

	requests("/moved")
	if _, err := FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	got := requests(elsewhere + "/moved")
	if len(got) != 2 || got[0].method != http.MethodPost || got[1].method != http.MethodPost || got[1].path != "/moved" || got[1].body != got[0].body || !strings.Contains(got[1].body, "target=sumSeries") {
		t.Fatalf("on the origin: %+v", got)
	}

	_, err = FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil)
	if want := "redirect to " + elsewhere + "/moved refused: it would send the request body to localhost, which is not the origin the request was made to and is not listed in request.redirect_trusted_hosts"; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("err=%v, want an error ending in %q", err, want)
	}
	// The form is retried as a GET would be, and the refusal is not.
	if got := requests(elsewhere + "/moved"); len(got) != 1 || got[0].path != "/render" {
		t.Fatalf("to an untrusted host: %+v", got)
	}

	c.Request.RedirectTrustedHosts = []string{"localhost"}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := requests(""); len(got) != 2 || got[1].host != strings.TrimPrefix(elsewhere, "http://") || got[1].body != got[0].body {
		t.Fatalf("to a listed host: %+v", got)
	}
}
