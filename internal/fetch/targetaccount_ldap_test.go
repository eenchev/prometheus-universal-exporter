//go:build request_type_ldap

package fetch

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An ldap static target's own account and tls block are checked as the
// collector's are: a bind_dn comes with the target's password_file, a
// password_file alone is the collector's bind_dn's, refused for a
// collector that binds anonymously, a bind_dn is a DN, a password_file is
// not blanks alone, a client certificate needs both files, and an ldap://
// target asks for plaintext, which its own tls block contradicts unless
// the collector begins TLS with start_tls. None of them takes a
// placeholder, and username is another type's. The label of a target with
// its own block is ldaps://.
func TestAnLDAPStaticTargetsAccountAndTLSAreChecked(t *testing.T) {
	c := ldapCollector(t, func(c *model.Collector) {
		c.Request.BindDN, c.Request.PasswordFile = "cn=exporter,dc=example,dc=org", "/secrets/exporter"
	})
	ca := &model.TLSConfig{CAFile: "/ca.pem", Set: true}
	checkTargetRequests(t, c, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"an account and a tls block": {"ldaps://dir.internal", model.TargetRequestConfig{BindDN: "cn=tenant,dc=example,dc=org", PasswordFile: "/secrets/tenant", TLS: ca}, ""},
		"a tls block alone":          {"dir.internal", model.TargetRequestConfig{TLS: ca}, ""},
		"a bind_dn alone":            {"dir.internal", model.TargetRequestConfig{BindDN: "cn=tenant,dc=example,dc=org"}, "sets request.bind_dn without request.password_file"},
		"a password file alone":      {"dir.internal", model.TargetRequestConfig{PasswordFile: "/secrets/tenant"}, ""},
		"a bind_dn that is no DN":    {"dir.internal", model.TargetRequestConfig{BindDN: "tenant", PasswordFile: "/p"}, `request.bind_dn "tenant" is not a DN`},
		"a bind_dn of blanks":        {"dir.internal", model.TargetRequestConfig{BindDN: "  ", PasswordFile: "/p"}, "request.bind_dn is blanks alone"},
		"a password file of blanks":  {"dir.internal", model.TargetRequestConfig{BindDN: "cn=a", PasswordFile: " "}, "request.password_file is blanks alone"},
		"a placeholder in the DN":    {"dir.internal", model.TargetRequestConfig{BindDN: "cn={{param_user}}", PasswordFile: "/p"}, "has a placeholder"},
		"half a client certificate":  {"dir.internal", model.TargetRequestConfig{TLS: &model.TLSConfig{KeyFile: "/k.pem", Set: true}}, "only one of cert_file and key_file"},
		"ldap:// with its own tls":   {"ldap://dir.internal", model.TargetRequestConfig{TLS: ca}, "asks for plaintext with ldap://, but sets request.tls of its own"},
		"a redis account":            {"dir.internal", model.TargetRequestConfig{Username: "tenant", PasswordFile: "/p"}, "sets request.username, which does not apply"},
	})
	startTLS := ldapCollector(t, func(c *model.Collector) { c.Request.StartTLS = true })
	checkTargetRequests(t, startTLS, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"ldap:// with its own tls and start_tls":     {"ldap://dir.internal", model.TargetRequestConfig{TLS: ca}, ""},
		"a password file alone, binding anonymously": {"dir.internal", model.TargetRequestConfig{PasswordFile: "/secrets/tenant"}, `sets request.password_file without request.bind_dn, and collector "directory" binds anonymously`},
	})
	target := &model.StaticTarget{Name: "x", Collector: c.Name, Target: "dir.internal", Request: model.TargetRequestConfig{TLS: ca}}
	if label, err := RequestLabelFor(target.Target, c, TargetOverrides(target)); err != nil || label != "ldaps://dir.internal:636" {
		t.Errorf("the label of a target with its own tls block: %q, %v", label, err)
	}
}

// --dry-run reads an ldap static target's own password and tls files, as
// a scrape of it would, under the target's name.
func TestAnLDAPStaticTargetsFilesAreReadByTheDryRun(t *testing.T) {
	checkTargetFilesAreRead(t, ldapCollector(t, func(c *model.Collector) {
		c.Request.BindDN, c.Request.PasswordFile = "cn=exporter,dc=example,dc=org", "/missing/collector"
	}), true)
}
