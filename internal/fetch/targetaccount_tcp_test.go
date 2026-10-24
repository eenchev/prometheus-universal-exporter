//go:build request_type_tcp

package fetch

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A tcp static target's own tls block is checked as the collector's is: a
// client certificate needs both files, server_name is a host's name, and
// no value takes a placeholder. An account is no tcp target's to set. The
// label of a target with its own block is tls://.
func TestATCPStaticTargetsTLSIsChecked(t *testing.T) {
	c := tcpCollector(t)
	ca := &model.TLSConfig{CAFile: "/ca.pem", Set: true}
	checkTargetRequests(t, c, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"a tls block":                   {"cache.internal:11211", model.TargetRequestConfig{TLS: ca}, ""},
		"an empty tls block":            {"cache.internal:11211", model.TargetRequestConfig{TLS: &model.TLSConfig{Set: true}}, ""},
		"half a client certificate":     {"cache.internal:11211", model.TargetRequestConfig{TLS: &model.TLSConfig{CertFile: "/c.pem", Set: true}}, "only one of cert_file and key_file"},
		"a server name that is no host": {"cache.internal:11211", model.TargetRequestConfig{TLS: &model.TLSConfig{ServerName: "a/b", Set: true}}, `request.tls.server_name "a/b"`},
		"a placeholder in the tls":      {"cache.internal:11211", model.TargetRequestConfig{TLS: &model.TLSConfig{CertFile: "{{param_c}}", KeyFile: "/k", Set: true}}, "has a placeholder"},
		"a password":                    {"cache.internal:11211", model.TargetRequestConfig{PasswordFile: "/p"}, "sets request.password_file, which does not apply"},
	})
	target := &model.StaticTarget{Name: "x", Collector: c.Name, Target: "cache.internal:11211", Request: model.TargetRequestConfig{TLS: ca}}
	if label, err := RequestLabelFor(target.Target, c, TargetOverrides(target)); err != nil || label != "tls://cache.internal:11211" {
		t.Errorf("the label of a target with its own tls block: %q, %v", label, err)
	}
}

// --dry-run reads a tcp static target's own tls files, as a scrape of it
// would, under the target's name.
func TestATCPStaticTargetsFilesAreReadByTheDryRun(t *testing.T) {
	checkTargetFilesAreRead(t, tcpCollector(t), false)
}
