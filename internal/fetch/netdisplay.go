//go:build request_type_smtp || request_type_modbus || request_type_mqtt || request_type_ftp || request_type_imap

package fetch

import "strings"

// netDisplay renders a target of one of these types for logs and labels: as
// written, since it holds no credential, when it is a target at all.
func netDisplay(what, target string, ports map[string]int, withRest bool) string {
	if _, err := parseNetTarget(what, target, ports, withRest); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}
