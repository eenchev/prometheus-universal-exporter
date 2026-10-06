package exporter

import (
	"context"
	"log/slog"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The prometheus decoder leaves out a sample line that is no part of its
// histogram or summary family (decode/promparse.go): Micrometer's
// x{quantile="0.95"} under "# TYPE x histogram", an x_bucket without an le
// label. The rest of the exposition is served, and without a word the series
// such a line stands for would be missing as if nobody wrote them. So the
// lines are counted in the collector's self-metrics with the carbon lines the
// graphite decoder skips, and logged at warn level as those are
// (graphitereport.go): once for a scrape, with the first line and what was
// expected in its place, then sparingly as other repeated failures are
// (failurelog.go), until a read leaves none out.

// notePrometheus counts and logs the sample lines the prometheus decoder left
// out of d. file is the directory's file d was read from, or empty; target is
// the target as logs show it, keyTarget as the failure log tells targets
// apart.
func (s *Server) notePrometheus(ctx context.Context, d *decode.Decoded, c *model.Collector, rec statsRecorder, target, keyTarget, file string) {
	if d.Kind != "prometheus" {
		return
	}
	attrs := []any{"collector", c.Name, "target", target}
	if file != "" {
		attrs = append(attrs, "file", file)
	}
	key := aspectKey(c.Name, keyTarget, file, sampleLinesAspect)
	report := d.Prometheus
	if report == nil {
		s.tripRecovered(ctx, rec.read, key, "every sample line is part of its family again", attrs...)
		return
	}
	rec.update(func(x *serverStats) {
		x.linesSkipped += uint64(report.LeftOutLines) //nolint:gosec // G115: a count, never negative
	})
	s.tripFailed(ctx, rec.read, slog.LevelWarn, key, "sample lines left out", "decode", report.FirstLeftOut, append(attrs, "left_out", report.LeftOutLines)...)
}
