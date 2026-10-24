//go:build !select_request_types || request_type_grpc || request_type_sql || request_type_redis || request_type_ldap || request_type_tcp

package fetch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkTargetTLS checks a static target's own tls block, which replaces its
// collector's whole for a redis, ldap, tcp, sql or grpc collector, as the
// collector's is checked: a client certificate needs both its files, and
// server_name is a host's name. Its values take no placeholder, as no value
// of a target's own does: there is no probe to fill one.
func checkTargetTLS(t *model.StaticTarget) error {
	s := t.Request.TLS
	if s == nil {
		return nil
	}
	for _, key := range []struct{ name, value string }{{"ca_file", s.CAFile}, {"cert_file", s.CertFile}, {"key_file", s.KeyFile}, {"server_name", s.ServerName}} {
		if strings.Contains(key.value, "{{") {
			return fmt.Errorf("request.tls.%s %q has a placeholder; a static target's tls block is used as it is written, with no probe to fill one, so write the value out", key.name, key.value)
		}
	}
	if (s.CertFile == "") != (s.KeyFile == "") {
		return errors.New("request.tls sets only one of cert_file and key_file; a client certificate needs both")
	}
	if s.ServerName != "" {
		if _, err := canonicalHost(s.ServerName); err != nil {
			return fmt.Errorf("request.tls.server_name %q: %w", s.ServerName, err)
		}
	}
	return nil
}
