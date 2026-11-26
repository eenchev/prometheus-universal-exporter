//go:build !select_request_types || request_type_dns || request_type_icmp || request_type_kubernetes || request_type_ldap || request_type_ntp || request_type_redis || request_type_snmp || request_type_tcp || request_type_tlscert || request_type_amqp || request_type_imap || request_type_modbus || request_type_mqtt || request_type_nats || request_type_kafka || request_type_pop3 || request_type_radius || request_type_sip || request_type_smtp || request_type_whois

package fetch

import (
	"net/http"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// jsonBody is the answer of a request type whose document is body, already
// JSON: status 200 with no status line of its own, for c's request of target
// that began at start.
func jsonBody(body []byte, target string, c *model.Collector, start time.Time) *HTTPResponse {
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}
}
