//go:build !select_request_types || request_type_grpc || request_type_redis || request_type_ldap || request_type_tcp

package fetch

import (
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A static target of a redis, ldap, tcp or grpc collector may name its own
// account and tls block, so one collector serves servers that each take
// their own password or client certificate. The types read these from the
// collector's request, in their checks, labels and fetches alike, so a
// scrape of such a target is made with the collector as the target makes it
// (withTargetAccount): a copy with the target's account and block in the
// collector's place. No probe parameter sets them (RequestOverrides).

// withTargetAccount is c as a scrape with overrides makes it: a static
// target's tls block in place of the collector's whole, its password_file
// in place of the collector's, and its username, which comes only with its
// own password_file, in place of the collector's; an ldap target's bind_dn,
// which comes so too, is ldap's own (withLDAPTargetAccount). A target that
// names a password file alone logs in as the collector's account with that
// password: servers that each keep their own password for one account are
// what it is for. c itself is never written, being the configuration's.
func withTargetAccount(c *model.Collector, o RequestOverrides) *model.Collector {
	if o.PasswordFile == "" && o.TLS == nil {
		return c
	}
	x := *c
	if o.PasswordFile != "" {
		x.Request.PasswordFile = o.PasswordFile
	}
	if o.Username != "" {
		x.Request.Username = o.Username
	}
	if o.TLS != nil {
		x.Request.TLS = *o.TLS
	}
	return &x
}

// accountFileWarnings reads, for --dry-run, the password file and the tls
// block's files a scrape of c reads, or those a static target t names of its
// own, which are read in place of the collector's; the collector's are
// warned of as the collector's. An empty password file is warned of when
// the type refuses one.
func accountFileWarnings(c *model.Collector, t *model.StaticTarget) []string {
	who, passwordFile, settings := fmt.Sprintf("collector %q", c.Name), c.Request.PasswordFile, &c.Request.TLS
	if t != nil {
		who, passwordFile, settings = fmt.Sprintf("target %q", t.Name), t.Request.PasswordFile, t.Request.TLS
	}
	var warnings []string
	if passwordFile != "" {
		password, err := ReadCredentialFile(passwordFile)
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("%s: request.password_file: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", who, err))
		case password == "":
			warnings = append(warnings, fmt.Sprintf("%s: request.password_file %s is empty; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", who, passwordFile))
		}
	}
	if settings != nil && *settings != (model.TLSConfig{}) {
		if _, err := tlsConfig(*settings); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: request.tls: %v; a scrape would fail on it now (the files are read at each connection, so one put in place later is used then)", who, err))
		}
	}
	return warnings
}
