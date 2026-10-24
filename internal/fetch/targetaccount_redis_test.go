//go:build request_type_redis

package fetch

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A redis static target's own account and tls block are checked as the
// collector's are: a username needs the target's password_file, a user's
// name holds no blank, a password_file is not blanks alone, a client
// certificate needs both files, server_name is a host's name, and a
// redis:// target asks for plaintext, which its own tls block contradicts.
// None of them takes a placeholder, and bind_dn is ldap's. The label of a
// target with its own block is rediss://.
func TestARedisStaticTargetsAccountAndTLSAreChecked(t *testing.T) {
	c := redisCollector(t, func(c *model.Collector) { c.Request.Username, c.Request.PasswordFile = "exporter", "/secrets/exporter" })
	ca := &model.TLSConfig{CAFile: "/ca.pem", Set: true}
	checkTargetRequests(t, c, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"an account and a tls block":      {"rediss://cache.internal", model.TargetRequestConfig{Username: "tenant", PasswordFile: "/secrets/tenant", TLS: ca}, ""},
		"the collector's user's password": {"cache.internal", model.TargetRequestConfig{PasswordFile: "/secrets/tenant"}, ""},
		"an empty tls block":              {"cache.internal", model.TargetRequestConfig{TLS: &model.TLSConfig{Set: true}}, ""},
		"a username alone":                {"cache.internal", model.TargetRequestConfig{Username: "tenant"}, "sets request.username without request.password_file"},
		"a username with a blank":         {"cache.internal", model.TargetRequestConfig{Username: "a b", PasswordFile: "/p"}, `request.username "a b" is not an ACL user's name`},
		"a password file of blanks":       {"cache.internal", model.TargetRequestConfig{PasswordFile: "  "}, "request.password_file is blanks alone"},
		"a placeholder in the user":       {"cache.internal", model.TargetRequestConfig{Username: "{{param_user}}", PasswordFile: "/p"}, "request.username \"{{param_user}}\" has a placeholder"},
		"a placeholder in the file":       {"cache.internal", model.TargetRequestConfig{PasswordFile: "/{{param_x}}"}, "request.password_file \"/{{param_x}}\" has a placeholder"},
		"half a client certificate":       {"cache.internal", model.TargetRequestConfig{TLS: &model.TLSConfig{CertFile: "/c.pem", Set: true}}, "only one of cert_file and key_file"},
		"a placeholder in the tls":        {"cache.internal", model.TargetRequestConfig{TLS: &model.TLSConfig{CAFile: "/{{param_ca}}", Set: true}}, "request.tls.ca_file \"/{{param_ca}}\" has a placeholder"},
		"a server name that is no host":   {"cache.internal", model.TargetRequestConfig{TLS: &model.TLSConfig{ServerName: "a b", Set: true}}, `request.tls.server_name "a b"`},
		"redis:// with its own tls":       {"redis://cache.internal", model.TargetRequestConfig{TLS: ca}, "asks for plaintext with redis://, but sets request.tls of its own"},
		"an ldap account":                 {"cache.internal", model.TargetRequestConfig{BindDN: "cn=a", PasswordFile: "/p"}, "sets request.bind_dn, which does not apply"},
		"an ssh key":                      {"cache.internal", model.TargetRequestConfig{PrivateKeyFile: "/k"}, "sets request.private_key_file, which does not apply"},
	})
	target := &model.StaticTarget{Name: "x", Collector: c.Name, Target: "cache.internal", Request: model.TargetRequestConfig{TLS: ca}}
	if label, err := RequestLabelFor(target.Target, c, TargetOverrides(target)); err != nil || label != "rediss://cache.internal:6379/0" {
		t.Errorf("the label of a target with its own tls block: %q, %v", label, err)
	}
}

// --dry-run reads a redis static target's own password and tls files, as
// a scrape of it would, and says what it would fail on under the target's
// name; a target that names none of its own leaves the collector's to be
// warned of as the collector's.
func TestARedisStaticTargetsFilesAreReadByTheDryRun(t *testing.T) {
	checkTargetFilesAreRead(t, redisCollector(t, func(c *model.Collector) { c.Request.PasswordFile = "/missing/collector" }), true)
}
