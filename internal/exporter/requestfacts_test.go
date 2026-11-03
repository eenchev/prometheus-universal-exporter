package exporter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// A request's facts are its span's attributes, each of the value its kind
// is, as OTLP/JSON writes it: an integer as an intValue string, a boolean as
// a boolValue, a floating-point number as a doubleValue and a name as a
// stringValue.
func TestARequestsFactsAreAttributesOfTheirKind(t *testing.T) {
	r := fetch.TracedRequest{Method: "QUERY", URL: "ntp://time.example:123", Facts: []fetch.TraceFact{
		{Key: "ntp.stratum", Kind: fetch.TraceFactInt, Int: 2},
		{Key: "tlscert.verified", Kind: fetch.TraceFactBool, Bool: false},
		{Key: "ntp.offset_seconds", Kind: fetch.TraceFactDouble, Double: -0.0125},
		{Key: "tls.protocol.version", Kind: fetch.TraceFactName, Name: "1.3"},
	}}
	raw, err := json.Marshal(requestAttributes(r, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"key":"ntp.stratum","value":{"intValue":"2"}}`,
		`{"key":"tlscert.verified","value":{"boolValue":false}}`,
		`{"key":"ntp.offset_seconds","value":{"doubleValue":-0.0125}}`,
		`{"key":"tls.protocol.version","value":{"stringValue":"1.3"}}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the attributes are %s, without %s", raw, want)
		}
	}
}
