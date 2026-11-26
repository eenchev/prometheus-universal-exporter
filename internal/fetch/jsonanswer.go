//go:build !select_request_types || request_type_icmp || request_type_tcp || request_type_amqp || request_type_nats || request_type_kafka || request_type_radius || request_type_sip || request_type_whois

package fetch

import (
	"encoding/json"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// jsonAnswer is the answer of a request type that reports what it found as a
// JSON document: doc written as JSON, as jsonBody answers it.
func jsonAnswer(doc any, target string, c *model.Collector, start time.Time) (*HTTPResponse, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return jsonBody(body, target, c, start), nil
}
