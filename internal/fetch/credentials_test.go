//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// credentialTarget answers every request and reports the basic auth
// credentials it was sent, so a swapped username and password shows.
func credentialTarget(t *testing.T) (*httptest.Server, chan [2]string) {
	t.Helper()
	sent := make(chan [2]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			username, password = "<none>", r.Header.Get("Authorization")
		}
		sent <- [2]string{username, password}
		_, _ = w.Write([]byte("value 1\n"))
	}))
	t.Cleanup(server.Close)
	return server, sent
}

// Inline basic_auth reaches the target as the username and password it
// names, both a collector's and a static target's.
func TestInlineBasicAuthIsSentAsConfigured(t *testing.T) {
	server, sent := credentialTarget(t)
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.BasicAuth = &model.BasicAuth{Username: "collector-user", Password: "collector-password"}
	})
	if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != [2]string{"collector-user", "collector-password"} {
		t.Fatalf("a collector's basic_auth arrived as %q", got)
	}
	target := &model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{BasicAuth: &model.BasicAuth{Username: "target-user", Password: "target-password"}}}
	headers, err := TargetHeaders(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCollector(context.Background(), server.URL, httpCollector(t, nil), TargetOverrides(target), headers); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != [2]string{"target-user", "target-password"} {
		t.Fatalf("a static target's basic_auth arrived as %q", got)
	}
}

// An empty credential file is refused rather than sent: a basic auth
// username or password left empty, or a bearer token file holding nothing,
// is a secret that has not been mounted yet, and sending the request anyway
// would present half a credential or none. A file holding only whitespace is
// empty too, since the value is trimmed.
func TestAnEmptyCredentialFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	full, empty := write("full", "secret\n"), write("empty", " \n")
	for name, test := range map[string]struct {
		basic  *model.BasicAuthFile
		bearer string
		want   string
	}{
		"empty username file": {basic: &model.BasicAuthFile{Username: empty, Password: full}, want: "basic auth credential files must not be empty"},
		"empty password file": {basic: &model.BasicAuthFile{Username: full, Password: empty}, want: "basic auth credential files must not be empty"},
		"empty bearer file":   {bearer: empty, want: "bearer token file " + empty + " is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			server, sent := credentialTarget(t)
			c := httpCollector(t, func(c *model.Collector) {
				c.Request.BasicAuthFile, c.Request.BearerTokenFile = test.basic, test.bearer
			})
			if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("collector: err=%v, want %q", err, test.want)
			}
			target := &model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{BasicAuthFile: test.basic, BearerTokenFile: test.bearer}}
			if _, err := TargetHeaders(target); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("static target: err=%v, want %q", err, test.want)
			}
			select {
			case got := <-sent:
				t.Errorf("the request was sent with %q", got)
			default:
			}
		})
	}
}

// A username read from a basic_auth_file that holds a colon is refused at
// the request and nothing is sent, a collector's and a static target's: a
// Basic credential ends the username at the first colon (RFC 7617), so the
// target read a shorter username and a password that started with the rest,
// and refused every scrape. The message names the file and never the value.
// A password with a colon is sent as it is.
func TestABasicUsernameFileWithAColonIsRefused(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	colon, plain, password := write("colon", "metrics:reader\n"), write("plain", "metrics\n"), write("password", "se:cret\n")
	server, sent := credentialTarget(t)
	refused := &model.BasicAuthFile{Username: colon, Password: password}
	want := "basic auth username file " + colon + ` holds a colon (":")`
	c := httpCollector(t, func(c *model.Collector) { c.Request.BasicAuthFile = refused })
	if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "reader") {
		t.Errorf("collector: err=%v, want %q", err, want)
	}
	if _, err := TargetHeaders(&model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{BasicAuthFile: refused}}); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "reader") {
		t.Errorf("static target: err=%v, want %q", err, want)
	}
	select {
	case got := <-sent:
		t.Fatalf("the request was sent with %q", got)
	default:
	}
	c = httpCollector(t, func(c *model.Collector) {
		c.Request.BasicAuthFile = &model.BasicAuthFile{Username: plain, Password: password}
	})
	if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != [2]string{"metrics", "se:cret"} {
		t.Fatalf("the credential arrived as %q", got)
	}
}
