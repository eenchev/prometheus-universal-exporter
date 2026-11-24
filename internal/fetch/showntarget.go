//go:build !select_request_types || request_type_icmp || request_type_ntp || request_type_snmp || request_type_amqp || request_type_nats || request_type_radius || request_type_whois || request_type_sip || request_type_smtp || request_type_modbus || request_type_mqtt || request_type_ftp || request_type_imap || request_type_pop3

package fetch

import "strings"

// shownTarget renders a target that holds no credentials for logs and
// labels: as written, or "<invalid target>" when err says it is not a target
// of its type.
func shownTarget(target string, err error) string {
	if err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}
