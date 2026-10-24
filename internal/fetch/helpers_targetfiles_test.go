//go:build !select_request_types || request_type_grpc || request_type_redis || request_type_ldap || request_type_tcp || request_type_http || request_type_graphite

package fetch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkTargetFilesAreRead checks FileWarnings of static targets of c, a
// collector whose own files, if any, are missing: one with its own
// password file, when password says the type takes one, missing, and one
// empty, one with a tls block naming a CA that is not there, one with
// files that are all there and one with none of its own.
func checkTargetFilesAreRead(t *testing.T, c *model.Collector, password bool) {
	t.Helper()
	dir := t.TempDir()
	empty, full := filepath.Join(dir, "empty"), filepath.Join(dir, "full")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	account := func(file string) model.TargetRequestConfig {
		if c.Request.Type == RequestTypeLDAP {
			return model.TargetRequestConfig{BindDN: "cn=x", PasswordFile: file}
		}
		return model.TargetRequestConfig{PasswordFile: file}
	}
	cases := map[string]struct {
		request model.TargetRequestConfig
		want    string
	}{
		"a CA that is not there": {model.TargetRequestConfig{TLS: &model.TLSConfig{CAFile: filepath.Join(dir, "ca.pem"), Set: true}}, `target "x": request.tls: open ` + filepath.Join(dir, "ca.pem")},
		"an empty tls block":     {model.TargetRequestConfig{TLS: &model.TLSConfig{Set: true}}, ""},
		"none of its own":        {model.TargetRequestConfig{}, ""},
	}
	if password {
		cases["a password file that is not there"] = struct {
			request model.TargetRequestConfig
			want    string
		}{account(filepath.Join(dir, "none")), `target "x": request.password_file: open ` + filepath.Join(dir, "none")}
		cases["an empty password file"] = struct {
			request model.TargetRequestConfig
			want    string
		}{account(empty), `target "x": request.password_file ` + empty + " is empty"}
		cases["a password file that is there"] = struct {
			request model.TargetRequestConfig
			want    string
		}{account(full), ""}
	}
	for name, tc := range cases {
		warnings := FileWarnings(c, &model.StaticTarget{Name: "x", Collector: c.Name, Target: "db.internal", Request: tc.request})
		switch {
		case tc.want == "" && len(warnings) > 0:
			t.Errorf("%s: %q, want none", name, warnings)
		case tc.want != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], tc.want)):
			t.Errorf("%s: %q, want one holding %q", name, warnings, tc.want)
		}
	}
}
