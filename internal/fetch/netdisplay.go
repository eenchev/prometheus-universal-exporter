//go:build request_type_smtp || request_type_modbus || request_type_mqtt || request_type_ftp || request_type_imap || request_type_pop3

package fetch

// netDisplay renders a target of one of these types for logs and labels: as
// written, since it holds no credential, when it is a target at all.
func netDisplay(what, target string, ports map[string]int, withRest bool) string {
	_, err := parseNetTarget(what, target, ports, withRest)
	return shownTarget(target, err)
}
