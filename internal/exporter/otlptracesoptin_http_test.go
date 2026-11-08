//go:build (!select_request_types || request_type_tcp || request_type_sshexec) && (!select_request_types || request_type_http)

package exporter

import (
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// tracedServerOf is an exporter of the configuration at path whose every
// probe's trace goes to the receiver.
func tracedServerOf(t *testing.T, receiver *logReceiver, path string) *Server {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.OTLP = otlpConfig(receiver.server.URL + "/v1/metrics")
	cfg.OTLP.Compression = model.OTLPCompressionNone
	cfg.OTLP.Traces = model.OTLPTracesConfig{Enabled: true, Threshold: model.Duration(time.Millisecond)}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
}

// clientSpans are the CLIENT spans received, those of the requests sent.
func clientSpans(t *testing.T, receiver *logReceiver) []receivedSpan {
	t.Helper()
	var out []receivedSpan
	for _, span := range receiver.spans(t) {
		if span.Kind == otlpSpanKindClient {
			out = append(out, span)
		}
	}
	return out
}
