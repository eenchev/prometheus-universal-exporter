package exporter

import (
	"fmt"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
)

// The status page refreshes itself as a Grafana dashboard does: a Refresh
// button, and an auto-refresh of Off or one of Grafana's intervals. The page
// fetches its own content section again, ?format=fragment, and swaps it in,
// so one template renders both the page and what a refresh brings, and the
// fetch is same-origin, so the exporter's Basic Auth and its cross-site
// refusal pass it as they pass the page. The interval chosen is kept in the
// URL, ?refresh=30s, so a bookmark or a shared link keeps it; without it the
// page takes --web.status-refresh, off by default.

// ParseStatusRefresh reads --web.status-refresh: off, or a duration in
// whole seconds from 5s to 24h (pagekit.RefreshOf); off is 0. Anything
// else is a command-line error, as a page that refreshed every few
// milliseconds, or never though it was asked to, would be found out only by
// whoever opened it.
func ParseStatusRefresh(value string) (time.Duration, error) {
	if d, ok := pagekit.RefreshOf(value); ok {
		return d, nil
	}
	return 0, fmt.Errorf("--web.status-refresh %q must be off or a duration in whole seconds from 5s to 24h, such as 30s or 1m", value)
}

// SetStatusRefresh sets the status page's auto-refresh when its URL does not
// say, --web.status-refresh; 0 is off.
func (s *Server) SetStatusRefresh(d time.Duration) {
	s.statusRefresh = d
}

// statusRefreshText is d as the page and its URL write it
// (pagekit.RefreshText).
func statusRefreshText(d time.Duration) string { return pagekit.RefreshText(d) }

// statusRefreshControl is the page's refresh controls (pagekit).
type statusRefreshControl = pagekit.RefreshControl

// statusRefreshControlFor is the controls of a page asked for with query,
// the value of ?refresh=: one that is no auto-refresh is ignored, as a
// mistyped link still shows the page, and the flag's applies.
func (s *Server) statusRefreshControlFor(query string) statusRefreshControl {
	return pagekit.NewRefreshControl(s.statusRefresh, query)
}
