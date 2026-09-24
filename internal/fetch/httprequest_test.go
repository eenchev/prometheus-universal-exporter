//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func httpCollector(t *testing.T, edit func(*model.Collector)) *model.Collector {
	t.Helper()
	c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}
	if edit != nil {
		edit(&c)
	}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// A Host header, the collector's or a static target's, is the Host the
// request is sent with, so a virtual host is reached by an IP address.
func TestAHostHeaderIsTheRequestsHost(t *testing.T) {
	hosts := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hosts <- r.Host }))
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) { c.Request.Headers = map[string]string{"host": "vhost.example"} })
	if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-hosts; got != "vhost.example" {
		t.Fatalf("Host %q", got)
	}
	target := &model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{Headers: map[string]string{"Host": "other.example"}}}
	headers, err := TargetHeaders(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), server.URL, httpCollector(t, nil), TargetOverrides(target), headers); err != nil {
		t.Fatal(err)
	}
	if got := <-hosts; got != "other.example" {
		t.Fatalf("a static target's Host %q", got)
	}
}

// request.tls.server_name is what the certificate is checked against: the
// test server's certificate names example.com, reached here by an IP address.
func TestTLSServerName(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"example.com": true, "wrong.example": false} {
		c := httpCollector(t, func(c *model.Collector) { c.Request.TLS = model.TLSConfig{CAFile: ca, ServerName: name} })
		_, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil)
		if (err == nil) != want {
			t.Errorf("server_name %s: err=%v", name, err)
		}
		if !want && (err == nil || !strings.Contains(err.Error(), "wrong.example")) {
			t.Errorf("server_name %s: the error does not name it: %v", name, err)
		}
	}
}

// A path with a query or a fragment is refused, in a probe's path parameter
// too; a localfile path may hold either, being a file's name.
func TestAPathHoldsNoQuery(t *testing.T) {
	c := httpCollector(t, nil)
	if err := CheckOverrideParams(c, url.Values{"path": {"/s?a=1"}}); err == nil || !strings.Contains(err.Error(), `probe parameter path "/s?a=1" has a ? in it`) {
		t.Fatalf("err=%v", err)
	}
	if err := CheckOverrideParams(c, url.Values{"path": {"/s"}}); err != nil {
		t.Fatal(err)
	}
	file := model.Collector{Name: "f", Request: model.RequestConfig{Type: RequestTypeLocalFile, Root: t.TempDir(), Path: "odd?name#1.prom"}}
	if err := ValidateRequest(&file); err != nil {
		t.Fatalf("a localfile path with ? and #: %v", err)
	}
}

// A static target's retry replaces each key it sets and keeps the
// collector's for the rest.
func TestAStaticTargetsRetryOverridesKeyByKey(t *testing.T) {
	attempts := 3
	target := &model.StaticTarget{Request: model.TargetRequestConfig{Retry: &model.TargetRetryConfig{Attempts: &attempts}}}
	overrides := TargetOverrides(target)
	if overrides.RetryAttempts == nil || *overrides.RetryAttempts != 3 || overrides.RetryBackoff != nil || overrides.RetryNonIdempotent != nil {
		t.Fatalf("%+v", overrides)
	}
	collector := model.RetryConfig{Attempts: 1, Backoff: model.Duration(2 * time.Second), NonIdempotent: true}
	if got := target.Request.Retry.Over(collector); !reflect.DeepEqual(got, model.RetryConfig{Attempts: 3, Backoff: model.Duration(2 * time.Second), NonIdempotent: true}) {
		t.Fatalf("over the collector's: %+v", got)
	}
	var none *model.TargetRetryConfig
	if got := none.Over(collector); !reflect.DeepEqual(got, collector) {
		t.Fatalf("no retry of its own: %+v", got)
	}
}
