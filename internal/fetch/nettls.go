//go:build request_type_smtp || request_type_mqtt || request_type_websocket || request_type_ftp

package fetch

import (
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What the smtp, mqtt, websocket and ftp request types share beyond
// nettypes.go, which modbus, with neither TLS nor an account, has no use
// for.

// tlsState is the connection's TLS state, nil over plaintext.
func (c *netConn) tlsState() *tls.ConnectionState {
	if secured, ok := c.Conn.(*tls.Conn); ok {
		state := secured.ConnectionState()
		return &state
	}
	return nil
}

// netTLSConfig is the tls block's configuration when the scrape goes over
// TLS, nil otherwise.
func netTLSConfig(c *model.Collector, secure bool) (*tls.Config, error) {
	if !secure {
		return nil, nil
	}
	cfg, err := tlsConfig(c.Request.TLS)
	if err != nil {
		return nil, fmt.Errorf("request.tls: %w", err)
	}
	return cfg, nil
}

// checkNetAccount holds a collector's tls block and account to what every
// one of these types holds them to: a client certificate with both its
// files, a server_name that is a host's name, and no placeholder in the
// account, which is used as it is written.
func checkNetAccount(x *model.Collector) error {
	r := &x.Request
	t := r.TLS
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	if t.ServerName != "" {
		if _, err := canonicalHost(t.ServerName); err != nil {
			return fmt.Errorf("collector %q request.tls.server_name %q: %w", x.Name, t.ServerName, err)
		}
	}
	for _, key := range []struct{ name, value string }{{"username", r.Username}, {"password_file", r.PasswordFile}} {
		if HasPathParams(key.value) {
			return fmt.Errorf("collector %q request.%s %q has a placeholder; the account is used as it is written", x.Name, key.name, key.value)
		}
		if key.value != "" && strings.TrimSpace(key.value) == "" {
			return fmt.Errorf("collector %q request.%s is blanks alone; write it, or take the key out", x.Name, key.name)
		}
	}
	if strings.ContainsFunc(r.Username, func(c rune) bool { return c < ' ' || c == 0x7f }) {
		return fmt.Errorf("collector %q request.username holds a control character", x.Name)
	}
	return nil
}

// checkNetTargetAccount holds a static target's own account and tls block
// to the same.
func checkNetTargetAccount(t *model.StaticTarget) error {
	r := &t.Request
	for _, key := range []struct{ name, value string }{{"request.username", r.Username}, {"request.password_file", r.PasswordFile}} {
		if strings.Contains(key.value, "{{") {
			return fmt.Errorf("%s %q has a placeholder; a static target's account and credential files are used as they are written, with no probe to fill one, so write the value out", key.name, key.value)
		}
		if key.value != "" && strings.TrimSpace(key.value) == "" {
			return fmt.Errorf("%s is blanks alone; write it, or take the key out", key.name)
		}
	}
	return checkTargetTLS(t)
}

// netShown is text a server sent, as an error or a debug report shows it:
// control characters written as ?, what reads as a credential masked, and
// no longer than any failure is shown.
func netShown(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, strings.TrimRight(text, "\r\n"))
	return model.CutTo(RedactText(text), model.MaxFailureBytes, false)
}
