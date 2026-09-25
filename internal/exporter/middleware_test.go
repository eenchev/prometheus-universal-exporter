package exporter

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                           false,
		"gzip":                       true,
		"x-gzip":                     true,
		"*":                          true,
		"GZip":                       true,
		"identity":                   false,
		"br":                         false,
		"br, gzip;q=0.5":             true,
		"gzip, deflate, br":          true,
		"gzip;q=0":                   false,
		"gzip; q=0.0":                false,
		"gzip;q=0, *;q=0":            false,
		"identity;q=1, gzip;Q=0.001": true,
		// gzip named overrides *, either way.
		"gzip;q=0, *":   false,
		"*, gzip;q=0":   false,
		"*;q=0, gzip":   true,
		"*;q=0, x-gzip": true,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

// get serves one request through the exporter's handler.
func get(server *Server, method, target, acceptEncoding string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func gunzip(t *testing.T, body []byte) string {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

func TestProbeMetricsAndSelfMetricsAreGzippedWhenAccepted(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("value=42\n"))
	}))
	defer target.Close()
	server, _ := newCacheTestServer(t, testutil.Collector("app", "text"))
	probe := "/probe?collector=app&target=" + url.QueryEscape(target.URL)
	for _, path := range []string{probe, "/self-metrics"} {
		plain := get(server, http.MethodGet, path, "")
		if plain.Code != http.StatusOK || plain.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s without Accept-Encoding: %d, Content-Encoding %q", path, plain.Code, plain.Header().Get("Content-Encoding"))
		}
		if plain.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: Vary %q, want Accept-Encoding", path, plain.Header().Get("Vary"))
		}
		zipped := get(server, http.MethodGet, path, "gzip, deflate, br")
		if zipped.Code != http.StatusOK || zipped.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s with gzip: %d, Content-Encoding %q", path, zipped.Code, zipped.Header().Get("Content-Encoding"))
		}
		if zipped.Header().Get("Vary") != "Accept-Encoding" || zipped.Header().Get("Content-Length") != "" {
			t.Errorf("%s with gzip: Vary %q, Content-Length %q", path, zipped.Header().Get("Vary"), zipped.Header().Get("Content-Length"))
		}
		if got, want := zipped.Header().Get("Content-Type"), plain.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q compressed, %q plain", path, got, want)
		}
		body := gunzip(t, zipped.Body.Bytes())
		if path == probe {
			if body != plain.Body.String() || !strings.Contains(body, "demo_value 42") {
				t.Errorf("the decompressed probe differs from the plain one:\n%s\n---\n%s", body, plain.Body.String())
			}
		} else if !strings.Contains(body, "http_exporter_") {
			t.Errorf("%s decompressed has no self-metrics:\n%s", path, body)
		}
		for _, refused := range []string{"gzip;q=0", "identity"} {
			if r := get(server, http.MethodGet, path, refused); r.Header().Get("Content-Encoding") != "" {
				t.Errorf("%s with Accept-Encoding %q was compressed", path, refused)
			}
		}
		if r := get(server, http.MethodHead, path, "gzip"); r.Header().Get("Content-Encoding") != "" {
			t.Errorf("HEAD %s was compressed", path)
		}
	}
}

func TestErrorAnswersAreGzippedToo(t *testing.T) {
	server, _ := newCacheTestServer(t, testutil.Collector("app", "text"))
	r := get(server, http.MethodGet, "/probe?target=http://127.0.0.1:1", "gzip")
	if r.Code != http.StatusBadRequest || r.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, Content-Encoding %q", r.Code, r.Header().Get("Content-Encoding"))
	}
	if body := gunzip(t, r.Body.Bytes()); !strings.Contains(body, "collector") {
		t.Errorf("the decompressed error is %q", body)
	}
	if !strings.HasPrefix(r.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("Content-Type %q", r.Header().Get("Content-Type"))
	}
}

func TestHealthAndReadyAreNeverGzipped(t *testing.T) {
	server, _ := newCacheTestServer(t, testutil.Collector("app", "text"))
	for _, path := range []string{"/health", "/ready"} {
		r := get(server, http.MethodGet, path, "gzip")
		if r.Code != http.StatusOK || r.Header().Get("Content-Encoding") != "" || r.Header().Get("Vary") != "" {
			t.Errorf("%s: %d, Content-Encoding %q, Vary %q", path, r.Code, r.Header().Get("Content-Encoding"), r.Header().Get("Vary"))
		}
	}
}

// A handler that writes without a Content-Type gets it sniffed from the plain
// bytes, not from the compressed ones; a 204 is passed on without a body.
func TestGzipWriterSniffsAndPassesBodylessAnswers(t *testing.T) {
	handler := compressed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>hi</body></html>"))
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type %q, want text/html", got)
	}
	if gunzip(t, recorder.Body.Bytes()) != "<html><body>hi</body></html>" {
		t.Error("the body did not round-trip")
	}
	empty := compressed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	recorder = httptest.NewRecorder()
	empty(recorder, request)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Encoding") != "" {
		t.Errorf("204: %d, %d bytes, Content-Encoding %q", recorder.Code, recorder.Body.Len(), recorder.Header().Get("Content-Encoding"))
	}
}

// The exporter's Basic Auth credential from files (webauth.go).

func webAuthServer(t *testing.T, auth *model.ExporterBasicAuth) (*Server, error) {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, Web: model.WebConfig{BasicAuth: auth}}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	return NewServer(config.NewManager(cfg, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t)), nil
}

func selfMetricsAs(t *testing.T, server *Server, username, password string) int {
	t.Helper()
	header := http.Header{}
	if username != "" || password != "" {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.SetBasicAuth(username, password)
		header = r.Header
	}
	return probeOnce(t, server, "/self-metrics", header).Code
}

func TestExporterCredentialsFromFiles(t *testing.T) {
	dir := t.TempDir()
	username := testutil.WriteIn(t, dir, "username", "exporter\n")
	password := testutil.WriteIn(t, dir, "password", "  s3cret\n")
	server, err := webAuthServer(t, &model.ExporterBasicAuth{Enabled: true, UsernameFile: username, PasswordFile: password})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user, password string
		want           int
	}{
		{"exporter", "s3cret", http.StatusOK},
		{"exporter", "wrong", http.StatusUnauthorized},
		{"other", "s3cret", http.StatusUnauthorized},
		{"", "", http.StatusUnauthorized},
	} {
		if got := selfMetricsAs(t, server, tc.user, tc.password); got != tc.want {
			t.Errorf("%q/%q: %d, want %d", tc.user, tc.password, got, tc.want)
		}
	}
	// /health and /ready stay open.
	if code := probeOnce(t, server, "/health", nil).Code; code != http.StatusOK {
		t.Fatalf("/health: %d", code)
	}

	// A rotated Secret is picked up by the next request.
	if err := os.WriteFile(password, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(password, later, later); err != nil {
		t.Fatal(err)
	}
	if got := selfMetricsAs(t, server, "exporter", "rotated"); got != http.StatusOK {
		t.Fatalf("the rotated password was refused: %d", got)
	}
	if got := selfMetricsAs(t, server, "exporter", "s3cret"); got != http.StatusUnauthorized {
		t.Fatalf("the old password still works: %d", got)
	}

	// A file that goes away refuses every request rather than letting it in.
	if err := os.Remove(password); err != nil {
		t.Fatal(err)
	}
	if got := selfMetricsAs(t, server, "exporter", "rotated"); got != http.StatusInternalServerError {
		t.Fatalf("a missing password file answered %d", got)
	}
}

// An inline username with a password file, as the chart's Secret mount is used.
func TestExporterPasswordFileWithInlineUsername(t *testing.T) {
	password := testutil.WriteIn(t, t.TempDir(), "password", "s3cret")
	server, err := webAuthServer(t, &model.ExporterBasicAuth{Enabled: true, Username: "exporter", PasswordFile: password})
	if err != nil {
		t.Fatal(err)
	}
	if got := selfMetricsAs(t, server, "exporter", "s3cret"); got != http.StatusOK {
		t.Fatalf("%d", got)
	}
}

func TestExporterCredentialValidation(t *testing.T) {
	dir := t.TempDir()
	password := testutil.WriteIn(t, dir, "password", "s3cret")
	empty := testutil.WriteIn(t, dir, "empty", "\n")
	missing := filepath.Join(dir, "missing")
	for name, tc := range map[string]struct {
		auth model.ExporterBasicAuth
		want string
	}{
		"both passwords":   {model.ExporterBasicAuth{Username: "u", Password: "p", PasswordFile: password}, "both password and password_file"},
		"both usernames":   {model.ExporterBasicAuth{Username: "u", UsernameFile: password, Password: "p"}, "both username and username_file"},
		"no password":      {model.ExporterBasicAuth{Username: "u"}, "requires a password or password_file"},
		"no username":      {model.ExporterBasicAuth{Password: "p"}, "requires a username or username_file"},
		"missing file":     {model.ExporterBasicAuth{Username: "u", PasswordFile: missing}, "web.basic_auth.password_file " + missing + " cannot be read"},
		"empty file":       {model.ExporterBasicAuth{Username: "u", PasswordFile: empty}, "web.basic_auth.password_file " + empty + " is empty"},
		"missing username": {model.ExporterBasicAuth{UsernameFile: missing, Password: "p"}, "web.basic_auth.username_file"},
	} {
		t.Run(name, func(t *testing.T) {
			tc.auth.Enabled = true
			_, err := webAuthServer(t, &tc.auth)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	// Disabled, nothing is required or read.
	if _, err := webAuthServer(t, &model.ExporterBasicAuth{PasswordFile: missing}); err != nil {
		t.Fatalf("a disabled basic_auth was checked: %v", err)
	}
}
