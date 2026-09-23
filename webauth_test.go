package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The exporter's Basic Auth credential from files (webauth.go).

func webAuthServer(t *testing.T, auth *ExporterBasicAuth) (*Server, error) {
	t.Helper()
	cfg := &Config{Collectors: []Collector{testCollector("text", "text")}, Web: WebConfig{BasicAuth: auth}}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return NewServer(NewConfigManager(cfg, "", quietLogger(t)), "python3", quietLogger(t)), nil
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
	username := writeIn(t, dir, "username", "exporter\n")
	password := writeIn(t, dir, "password", "  s3cret\n")
	server, err := webAuthServer(t, &ExporterBasicAuth{Enabled: true, UsernameFile: username, PasswordFile: password})
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
	password := writeIn(t, t.TempDir(), "password", "s3cret")
	server, err := webAuthServer(t, &ExporterBasicAuth{Enabled: true, Username: "exporter", PasswordFile: password})
	if err != nil {
		t.Fatal(err)
	}
	if got := selfMetricsAs(t, server, "exporter", "s3cret"); got != http.StatusOK {
		t.Fatalf("%d", got)
	}
}

func TestExporterCredentialValidation(t *testing.T) {
	dir := t.TempDir()
	password := writeIn(t, dir, "password", "s3cret")
	empty := writeIn(t, dir, "empty", "\n")
	missing := filepath.Join(dir, "missing")
	for name, tc := range map[string]struct {
		auth ExporterBasicAuth
		want string
	}{
		"both passwords":   {ExporterBasicAuth{Username: "u", Password: "p", PasswordFile: password}, "both password and password_file"},
		"both usernames":   {ExporterBasicAuth{Username: "u", UsernameFile: password, Password: "p"}, "both username and username_file"},
		"no password":      {ExporterBasicAuth{Username: "u"}, "requires a password or password_file"},
		"no username":      {ExporterBasicAuth{Password: "p"}, "requires a username or username_file"},
		"missing file":     {ExporterBasicAuth{Username: "u", PasswordFile: missing}, "web.basic_auth.password_file " + missing + " cannot be read"},
		"empty file":       {ExporterBasicAuth{Username: "u", PasswordFile: empty}, "web.basic_auth.password_file " + empty + " is empty"},
		"missing username": {ExporterBasicAuth{UsernameFile: missing, Password: "p"}, "web.basic_auth.username_file"},
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
	if _, err := webAuthServer(t, &ExporterBasicAuth{PasswordFile: missing}); err != nil {
		t.Fatalf("a disabled basic_auth was checked: %v", err)
	}
}
