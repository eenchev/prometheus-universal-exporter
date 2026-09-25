package exporter

import (
	"context"
	"errors"
	"log/slog"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The graphite decoder leaves series out before the metric rules see them —
// with no point that has a value, older than response.graphite.max_age,
// answered twice — and, under response.graphite.invalid_lines: skip, carbon
// lines it cannot read (decode/graphite.go). Without a word, a metric missing
// for one of those reasons looks the same as one nobody wrote. So they are
// counted in the collector's self-metrics, the series left out are logged at
// debug level with why, and skipped lines at warn level, first in full and
// then sparingly as other repeated failures are (failurelog.go), until a
// read skips none.

// noteGraphite counts and logs what the graphite decoder left out of d. file
// is the directory's file d was read from, or empty; target is the target as
// logs show it, keyTarget as the failure log tells targets apart.
func (s *Server) noteGraphite(ctx context.Context, d *decode.Decoded, c *model.Collector, rec statsRecorder, target, keyTarget, file string) {
	report := d.Graphite
	if report == nil {
		return
	}
	rec.update(func(x *serverStats) {
		x.seriesLeftOut += uint64(report.LeftOut())   //nolint:gosec // G115: a count, never negative
		x.linesSkipped += uint64(report.SkippedLines) //nolint:gosec // G115: a count, never negative
	})
	attrs := []any{"collector", c.Name, "target", target}
	if file != "" {
		attrs = append(attrs, "file", file)
	}
	if report.LeftOut() > 0 {
		s.tripDebug(ctx, "graphite series left out", append(attrs, "no_points", report.NoPoints, "older_than_max_age", report.Stale, "duplicates", report.Duplicates)...)
	}
	key := failureKey(c.Name, keyTarget, file) + "\x00carbon lines"
	if report.SkippedLines > 0 {
		s.tripFailed(ctx, slog.LevelWarn, key, "carbon lines skipped", "decode", errors.New(report.FirstSkipped), append(attrs, "skipped", report.SkippedLines)...)
		return
	}
	s.tripRecovered(ctx, key, "carbon lines read whole again", attrs...)
}
