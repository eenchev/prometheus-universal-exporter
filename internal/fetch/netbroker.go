//go:build request_type_amqp || request_type_nats

package fetch

import (
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What the amqp and nats request types share beyond nettypes.go: each
// logs in to a broker with the collector's account, over TLS as its tls
// block says, and shows what the broker said in a failure.

// brokerTLSConfig is the tls block's configuration.
func brokerTLSConfig(c *model.Collector) (*tls.Config, error) {
	cfg, err := tlsConfig(c.Request.TLS)
	if err != nil {
		return nil, fmt.Errorf("request.tls: %w", err)
	}
	return cfg, nil
}

// checkBrokerAccount holds a collector's tls block and account to the rules
// of the types that take them: a client certificate with both its files, a
// server_name that is a host's name, and an account that is whole, with no
// placeholder or control character in it.
func checkBrokerAccount(x *model.Collector) error {
	r, t := &x.Request, x.Request.TLS
	switch {
	case (t.CertFile == "") != (t.KeyFile == ""):
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	case (r.Username == "") != (strings.TrimSpace(r.PasswordFile) == "") && !x.RequestNotMade:
		return fmt.Errorf("collector %q sets only one of request.username and password_file; an account is the two together", x.Name)
	case HasPathParams(r.Username) || HasPathParams(r.PasswordFile):
		return fmt.Errorf("collector %q request.username or password_file has a placeholder; the account is used as it is written", x.Name)
	case strings.ContainsFunc(r.Username, func(c rune) bool { return c < 0x20 || c == 0x7f }):
		return fmt.Errorf("collector %q request.username holds a control character", x.Name)
	}
	if t.ServerName != "" {
		if _, err := canonicalHost(t.ServerName); err != nil {
			return fmt.Errorf("collector %q request.tls.server_name %q: %w", x.Name, t.ServerName, err)
		}
	}
	return nil
}

// brokerSaid is text a broker sent, as an error shows it: its control
// characters written as ?, what reads as a credential masked, and no longer
// than a failure's line.
func brokerSaid(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, strings.ToValidUTF8(text, "?"))
	return model.CutTo(RedactText(text), 200, false)
}

// brokerFileWarnings are what --dry-run says of the files a collector of
// these types reads at a scrape: its password file, and its tls files when
// secure says a scrape would read them.
func brokerFileWarnings(c *model.Collector, secure bool) []string {
	const later = "a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)"
	var warnings []string
	if _, err := netPassword(c); err != nil {
		warnings = append(warnings, fmt.Sprintf("collector %q: %v; %s", c.Name, err, later))
	}
	if secure {
		if _, err := brokerTLSConfig(c); err != nil {
			warnings = append(warnings, fmt.Sprintf("collector %q: %v; %s", c.Name, err, later))
		}
	}
	return warnings
}
