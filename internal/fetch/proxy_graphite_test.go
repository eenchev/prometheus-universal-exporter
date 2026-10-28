//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"strings"
	"testing"
)

// A graphite collector's request.proxy_url carries its render requests, as
// an http collector's does, in place of the environment's proxy.
func TestAGraphiteCollectorsProxyCarriesItsRequests(t *testing.T) {
	environment := proxyEnvironment(t)
	unresolvable(t, nil)
	proxy := newTestProxy(t)
	c := graphiteCollector("app.requests")
	c.Request.ProxyURL = proxy.URL
	c.Request.ProxyBasicAuthFile = proxyCredentials(t, "scraper", "s3cret")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), "http://graphite.example:8080", &c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	asked, auth := proxy.seen()
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "GET http://graphite.example:8080/render?") || auth[0] == "" {
		t.Fatalf("the proxy was asked %q with %q", asked, auth)
	}
	if asked, _ := environment.seen(); len(asked) != 0 {
		t.Fatalf("the environment's proxy was asked %q", asked)
	}
}
