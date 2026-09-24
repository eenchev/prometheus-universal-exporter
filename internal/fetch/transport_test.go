package fetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestBooleanOverrideParsing(t *testing.T) {
	for _, name := range []string{"follow_redirects", "enable_http2", "insecure_skip_verify"} {
		t.Run(name, func(t *testing.T) {
			absent, err := ParseRequestOverrides(url.Values{})
			if err != nil {
				t.Fatal(err)
			}
			if overrideFor(t, absent, name) != nil {
				t.Fatalf("an absent %s must leave the collector setting in force", name)
			}
			for _, raw := range []string{"true", "false"} {
				parsed, err := ParseRequestOverrides(url.Values{name: {raw}})
				if err != nil {
					t.Fatal(err)
				}
				value := overrideFor(t, parsed, name)
				if value == nil || *value != (raw == "true") {
					t.Fatalf("%s=%s parsed as %v", name, raw, value)
				}
			}
			for _, raw := range []string{"yes", "1", "", "TRUE!"} {
				if _, err := ParseRequestOverrides(url.Values{name: {raw}}); err == nil {
					t.Fatalf("%s=%q was accepted; want a client error", name, raw)
				}
			}
		})
	}
}

func overrideFor(t *testing.T, overrides RequestOverrides, name string) *bool {
	t.Helper()
	switch name {
	case "follow_redirects":
		return overrides.FollowRedirects
	case "enable_http2":
		return overrides.EnableHTTP2
	case "insecure_skip_verify":
		return overrides.InsecureSkipVerify
	}
	t.Fatalf("unknown override %q", name)
	return nil
}

// Requests go through the proxy the environment names, and a host NO_PROXY
// names goes direct. Target requests and OTLP exports both take their client
// from HTTPClient.
func TestClientsUseTheProxyFromTheEnvironment(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		_, _ = w.Write([]byte("value=7\n"))
	}))
	t.Cleanup(proxy.Close)
	asked := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "direct.example")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("no_proxy", "")
	// Transports read the environment when they are built.
	previous := transports
	transports = newTransportCache()
	t.Cleanup(func() { transports = previous })

	client, err := HTTPClient(TransportSettings{}, true, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://target.example:8080/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "value=7\n" {
		t.Fatalf("body=%q", body)
	}
	if got := asked(); len(got) != 1 || got[0] != "http://target.example:8080/status" {
		t.Fatalf("the proxy was asked for %q", got)
	}

	client.Timeout = time.Second
	if resp, err := client.Get("http://direct.example:8080/status"); err == nil {
		_ = resp.Body.Close()
	}
	if got := asked(); len(got) != 1 {
		t.Fatalf("a NO_PROXY host went through the proxy: %q", got)
	}
}
