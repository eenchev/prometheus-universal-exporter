//go:build !select_request_types || request_type_grpc || request_type_redis || request_type_ldap || request_type_tcp || request_type_smtp || request_type_mqtt || request_type_websocket || request_type_ftp || request_type_imap || request_type_pop3

package fetch

import (
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A static target of a redis, ldap, tcp or grpc collector may name its own
// account and tls block. This lets one collector serve servers that each
// take their own password or client certificate. The types read these from
// the collector's request in their checks, labels and fetches alike. So a
// scrape of such a target uses a copy of the collector with the target's
// account and block in place of the collector's (withTargetAccount). No
// probe parameter sets them (RequestOverrides).

// withTargetAccount returns c as a scrape with overrides uses it. A static
// target's tls block replaces the collector's whole block, and its
// password_file replaces the collector's. Its username replaces the
// collector's too; it comes only with its own password_file, or alone under
// a collector that logs in with a token. An ldap target's bind_dn, which also
// comes with its own password_file, is ldap's own (withLDAPTargetAccount).
// A target that names only a password file logs in as the collector's
// account with that password. This is for servers that each keep their own
// password for one account. c itself belongs to the configuration and is never written.
func withTargetAccount(c *model.Collector, o RequestOverrides) *model.Collector {
	// A collector that logs in with a token logs in to the target's
	// mailbox with it, which the target names with its username alone.
	named := o.Username != "" && c.Request.XOAuth2 != nil
	if o.PasswordFile == "" && o.TLS == nil && !named {
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
// block's files that a scrape of c reads. Files a static target t names of
// its own are read in place of the collector's, and the collector's are
// warned of as the collector's. An empty password file is warned of when the
// type refuses one.
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
