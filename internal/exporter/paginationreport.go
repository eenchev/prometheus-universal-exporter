package exporter

import (
	"context"
	"log/slog"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An http collector with request.pagination fetches every page of its
// answer, up to request.pagination.max_pages (fetch/pagination.go). A trip
// that stops there while the target has another page is no failure: its
// answer is the pages it fetched. But its series then describe part of
// what the target holds, which looks the same as all of it, so the pages a
// trip fetched are counted in the collector's self-metrics, a trip stopped
// at max_pages is counted too, and it is logged at warn level, first in
// full and then sparingly as other repeated failures are (failurelog.go),
// until a trip fetches every page again.

// notePages counts the pages of response in the collector's self-metrics,
// and the trip when it stopped at max_pages; and when the pages were all
// accepted, logs that stop, or that the trip fetched every page again.
// target is the target as logs show it, keyTarget as the failure log tells
// targets apart.
func (s *Server) notePages(ctx context.Context, response *fetch.HTTPResponse, accepted bool, c *model.Collector, rec statsRecorder, target, keyTarget string) {
	read := response.Pages
	if read == nil {
		return
	}
	rec.update(func(x *serverStats) {
		x.pagesFetched += uint64(len(read.Pages))
		if read.MoreLeft {
			x.pagesStopped++
		}
	})
	if !accepted {
		return
	}
	attrs := []any{"collector", c.Name, "target", target, "pages", len(read.Pages), "max_pages", c.Request.Pagination.MaxPages}
	key := aspectKey(c.Name, keyTarget, "", pagesAspect)
	if read.MoreLeft {
		s.tripFailed(ctx, rec.read, slog.LevelWarn, key, "pagination stopped at request.pagination.max_pages with another page to come; the series are of the pages fetched", "pagination", nil, attrs...)
		return
	}
	s.tripRecovered(ctx, rec.read, key, "pagination fetched every page again", attrs...)
}
