//go:build request_type_smtp || request_type_modbus || request_type_mqtt || request_type_websocket || request_type_ftp || request_type_imap

package fetch

import "testing"

// A target of these types is host, host:port, an IPv6 address in brackets
// or one of the type's schemes, its port the scheme's when it names none;
// a user, a fragment, another scheme, a port that is none and, for a type
// with no use for one, a path are refused.
func TestANetTargetIsParsed(t *testing.T) {
	ports := map[string]int{"": 25, "smtp": 25, "smtps": 465}
	for target, want := range map[string]string{
		"mail.example":           "mail.example:25",
		"Mail.Example:587":       "mail.example:587",
		"smtps://mail.example":   "mail.example:465",
		"SMTP://mail.example/":   "mail.example:25",
		"[2001:db8::1]":          "[2001:db8::1]:25",
		"smtp://[2001:db8::1]:2": "[2001:db8::1]:2",
	} {
		if a, err := parseNetTarget("smtp", target, ports, false); err != nil || a.hostPort != want {
			t.Errorf("%q: %q, %v; want %s", target, a.hostPort, err, want)
		}
	}
	for _, target := range []string{"", "imap://mail.example", "mail.example/inbox", "user@mail.example", "mail.example:0", "mail.example:port", "mail.example:070", "2001:db8::1:25x", "smtp://", "mail.example#x", "://mail.example"} {
		if a, err := parseNetTarget("smtp", target, ports, false); err == nil {
			t.Errorf("%q is taken, as %+v", target, a)
		}
	}
	// Blanks around a target are not part of it.
	if a, err := parseNetTarget("smtp", "\t192.0.2.7:2525 ", ports, false); err != nil || a.hostPort != "192.0.2.7:2525" {
		t.Errorf("%+v, %v", a, err)
	}
	if a, err := parseNetTarget("websocket", "ws://host.example:8080/feed?token=x", map[string]int{"ws": 80}, true); err != nil || a.rest != "/feed?token=x" || a.scheme != "ws" {
		t.Errorf("%+v, %v", a, err)
	}
}
