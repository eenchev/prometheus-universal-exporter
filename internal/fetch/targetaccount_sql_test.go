//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A sql static target's own tls block replaces the collector's whole: a
// collector that connects in plaintext connects with TLS to a target that
// writes one, verified as the target's block says, and targets of one
// server whose blocks differ share no pool, the block being part of the
// pool's key; the target that writes none keeps the collector's plaintext.
func TestASQLStaticTargetConnectsWithItsOwnTLS(t *testing.T) {
	f := newSQLFixture(t)
	f.db.Results["q"] = sqltest.Result{Columns: []string{"v"}, Rows: [][]driver.Value{{int64(1)}}}
	c := f.collector(t, "db", SQLDriverPostgres, "q")
	for _, request := range []model.TargetRequestConfig{
		{},
		{TLS: &model.TLSConfig{Set: true}},
		{TLS: &model.TLSConfig{ServerName: "db.internal", Set: true}},
	} {
		target := &model.StaticTarget{Name: "t", Collector: "db", Target: "db.example", Request: request}
		if _, err := FetchCollector(context.Background(), target.Target, c, TargetOverrides(target), nil); err != nil {
			t.Fatal(err)
		}
	}
	specs := f.madeWith()
	if len(specs) != 3 {
		t.Fatalf("%d connections made, want one for each tls block", len(specs))
	}
	if specs[0].tls != nil {
		t.Errorf("the target without a block of its own connected with TLS: %+v", specs[0].tls)
	}
	for i, name := range []string{"db.example", "db.internal"} {
		if got := specs[i+1].tls; got == nil || got.ServerName != name || got.InsecureSkipVerify {
			t.Errorf("target %d connected with %+v, want TLS verified for %s", i+1, got, name)
		}
	}
}

// A sql static target's own tls block is checked as the collector's is,
// against the collector's options: TLS a driver option switches off, a
// verifying key a mode that verifies nothing ignores and half a client
// certificate are refused, as is a placeholder. --dry-run reads the
// target's own TLS files under its name.
func TestASQLStaticTargetsTLSIsChecked(t *testing.T) {
	f := newSQLFixture(t)
	postgres := f.collector(t, "db", SQLDriverPostgres, "q")
	ca := &model.TLSConfig{CAFile: "/ca.pem", Set: true}
	checkTargetRequests(t, postgres, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"a tls block":               {"db.example", model.TargetRequestConfig{TLS: ca}, ""},
		"an account and a block":    {"db.example", model.TargetRequestConfig{Username: "backup", PasswordFile: "/p", TLS: ca}, ""},
		"half a client certificate": {"db.example", model.TargetRequestConfig{TLS: &model.TLSConfig{KeyFile: "/k", Set: true}}, "only one of cert_file and key_file"},
		"a placeholder":             {"db.example", model.TargetRequestConfig{TLS: &model.TLSConfig{CAFile: "{{param_ca}}", Set: true}}, "has a placeholder"},
	})
	disabled := f.collector(t, "off", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Options = map[string]string{"sslmode": "disable"} })
	checkTargetRequests(t, disabled, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"TLS the options switch off": {"db.example", model.TargetRequestConfig{TLS: ca}, `sets request.tls, but request.options.sslmode is "disable"`},
	})
	unverified := f.collector(t, "require", SQLDriverPostgres, "q", func(r *model.RequestConfig) { r.Options = map[string]string{"sslmode": "require"} })
	checkTargetRequests(t, unverified, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"a CA a mode that verifies nothing ignores": {"db.example", model.TargetRequestConfig{TLS: ca}, "sets request.tls.ca_file or server_name"},
		"a client certificate alone":                {"db.example", model.TargetRequestConfig{TLS: &model.TLSConfig{CertFile: "/c", KeyFile: "/k", Set: true}}, ""},
	})

	notCA := testutil.WriteIn(t, t.TempDir(), "ca.pem", "no certificate\n")
	target := &model.StaticTarget{Name: "t", Collector: "db", Target: "db.example", Request: model.TargetRequestConfig{TLS: &model.TLSConfig{CAFile: notCA, Set: true}}}
	if warnings := FileWarnings(postgres, target); len(warnings) != 1 || !strings.Contains(warnings[0], `target "t": request.tls: no certificates found in `+notCA) {
		t.Errorf("a target's own CA holding no certificate: %q", warnings)
	}
	target.Request.TLS = &model.TLSConfig{CAFile: filepath.Join(t.TempDir(), "missing.pem"), Set: true}
	if warnings := FileWarnings(postgres, target); len(warnings) != 1 || !strings.Contains(warnings[0], `target "t": request.tls: open `) {
		t.Errorf("a target's own missing CA: %q", warnings)
	}
}
