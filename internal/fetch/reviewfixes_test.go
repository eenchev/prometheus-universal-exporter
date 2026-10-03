//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Header names are checked when the configuration loads: one Go would refuse
// to send, and two that are one header whatever their case.
func TestHeaderNamesAreChecked(t *testing.T) {
	for _, test := range []struct {
		headers map[string]string
		want    string
	}{
		{map[string]string{"X Tenant": "a"}, `"X Tenant" is not a header name`},
		{map[string]string{"X-Tenant": "a", "x-tenant": "b"}, `"X-Tenant" and "x-tenant" are the same header`},
		{map[string]string{"Host": "a", "host": "b"}, "are the same header"},
	} {
		headers, want := test.headers, test.want
		c := model.Collector{Name: "h", Request: model.RequestConfig{Type: RequestTypeHTTP, Headers: headers}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v, want %q", headers, err, want)
		}
		target := &model.StaticTarget{Name: "t", Target: "http://h", Request: model.TargetRequestConfig{Headers: headers}}
		plain := model.Collector{Name: "h", Request: model.RequestConfig{Type: RequestTypeHTTP}}
		if err := ValidateRequest(&plain); err != nil {
			t.Fatal(err)
		}
		if err := CheckTargetRequest(target, &plain); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("target %v: %v, want %q", headers, err, want)
		}
	}
}

// A client certificate without its key, or the reverse, is refused at load.
func TestHalfAClientCertificateIsRefused(t *testing.T) {
	for _, tlsConfig := range []model.TLSConfig{{CertFile: "c.pem"}, {KeyFile: "k.pem"}} {
		c := model.Collector{Name: "t", Request: model.RequestConfig{Type: RequestTypeHTTP, TLS: tlsConfig}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "only one of cert_file and key_file") {
			t.Errorf("%+v: %v", tlsConfig, err)
		}
	}
}

// request.path / asks for the root: on a target without a path of its own,
// and with a trailing slash of the target's, it is /, never //, which a Go
// server such as Filebeat's answers with a redirect or a 404.
func TestARootPathIsOneSlash(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.RequestURI)
	}))
	defer server.Close()
	for _, test := range []struct{ target, path, want string }{
		{server.URL, "/", "/"},
		{server.URL + "/", "/", "/"},
		{server.URL + "/api", "/", "/api/"},
		{server.URL + "/api/", "/", "/api/"},
		{server.URL, "/stats/", "/stats/"},
	} {
		got = nil
		c := model.Collector{Name: "root", Request: model.RequestConfig{Type: RequestTypeHTTP, Path: test.path}}
		if err := ValidateRequest(&c); err != nil {
			t.Fatal(err)
		}
		if _, err := fetch(context.Background(), test.target, &c, RequestOverrides{}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != test.want {
			t.Errorf("target %s path %q: requested %v, want %s", test.target, test.path, got, test.want)
		}
	}
}

// An escape in the target's path, such as %2F inside a segment, is sent as
// written when request.path is joined onto it, with path parameters too.
func TestATargetsEscapedPathIsKept(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.RequestURI)
	}))
	defer server.Close()
	for _, test := range []struct{ path, want string }{
		{"/x", "/a%2Fb/x"},
		{"/x/", "/a%2Fb/x/"},
		{"/{{param_t}}/x", "/a%2Fb/c%2Fd/x"},
		{"", "/a%2Fb"},
	} {
		got = nil
		c := model.Collector{Name: "e", Request: model.RequestConfig{Type: RequestTypeHTTP, Path: test.path}}
		if err := ValidateRequest(&c); err != nil {
			t.Fatal(err)
		}
		if _, err := fetch(context.Background(), server.URL+"/a%2Fb", &c, RequestOverrides{Params: map[string]string{"param_t": "c/d"}}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != test.want {
			t.Errorf("path %q: requested %v, want %s", test.path, got, test.want)
		}
	}
}
