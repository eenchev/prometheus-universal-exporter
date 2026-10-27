package exporter

import (
	"sync/atomic"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The endpoints whose answers are compressed (middleware.go), and counted,
// by the value of their endpoint label.
const (
	answerProbe = iota
	answerSelfMetrics
	answerStaticTargets
	answerEndpoints
)

// answerEndpointLabels are the endpoint labels of the answer counters, in
// the order of the constants above.
var answerEndpointLabels = [answerEndpoints]string{
	answerProbe:         "probe",
	answerSelfMetrics:   "self_metrics",
	answerStaticTargets: "static_targets",
}

// answerCounter adds up the answers of one endpoint: the bytes their
// handler wrote, before compression, and those written on to the client,
// after it. They are atomic, added to once per answer when it is done, so
// that answers written at once take no lock.
type answerCounter struct {
	written atomic.Uint64
	sent    atomic.Uint64
}

// add counts one answer.
func (c *answerCounter) add(written, sent int64) {
	if written > 0 {
		c.written.Add(uint64(written))
	}
	if sent > 0 {
		c.sent.Add(uint64(sent))
	}
}

// answerCounters are the answer counters of every compressed endpoint.
type answerCounters [answerEndpoints]answerCounter

// answerMetrics are http_exporter_answer_bytes_total and
// http_exporter_answer_sent_bytes_total, a series for each endpoint, whose
// handlers are always served: each is there from the start, at 0, whether
// or not the endpoint has answered yet, or the exporter has static targets,
// so a rate of each can be taken from the first scrape.
func (s *Server) answerMetrics() []model.Metric {
	out := make([]model.Metric, 0, 2*answerEndpoints)
	for i := range s.answers {
		out = append(out, model.Metric{Name: "http_exporter_answer_bytes_total", Help: exporterMetricHelp["http_exporter_answer_bytes_total"], Type: model.CounterMetricType, Value: float64(s.answers[i].written.Load()), Labels: map[string]string{"endpoint": answerEndpointLabels[i]}})
	}
	for i := range s.answers {
		out = append(out, model.Metric{Name: "http_exporter_answer_sent_bytes_total", Help: exporterMetricHelp["http_exporter_answer_sent_bytes_total"], Type: model.CounterMetricType, Value: float64(s.answers[i].sent.Load()), Labels: map[string]string{"endpoint": answerEndpointLabels[i]}})
	}
	return out
}

// answerGzipLevelMetric is http_exporter_answer_gzip_level, --web.gzip-level,
// published with the verbose self-metrics only: it is configuration, which
// the command line already says, and only of use to tell the two counters
// above apart.
func (s *Server) answerGzipLevelMetric() model.Metric {
	return model.Metric{Name: "http_exporter_answer_gzip_level", Help: answerGzipLevelHelp, Type: model.GaugeMetricType, Labels: map[string]string{}, Value: float64(s.compression.level)}
}
