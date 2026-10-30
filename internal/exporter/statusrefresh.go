package exporter

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The status page refreshes itself as a Grafana dashboard does: a Refresh
// button, and an auto-refresh of Off or one of Grafana's intervals. The page
// fetches its own content section again, ?format=fragment, and swaps it in,
// so one template renders both the page and what a refresh brings, and the
// fetch is same-origin, so the exporter's Basic Auth and its cross-site
// refusal pass it as they pass the page. The interval chosen is kept in the
// URL, ?refresh=30s, so a bookmark or a shared link keeps it; without it the
// page takes --web.status-refresh, off by default.

// Bounds of an auto-refresh. Below 5s, the least Grafana offers, a page
// left open in a few tabs would rebuild the status many times a second
// between them for nothing a person reads; above a day a browser's timer,
// which holds about 24.8 days, is no longer far off, and nobody waits that
// long for a page anyway.
const (
	minStatusRefresh = 5 * time.Second
	maxStatusRefresh = 24 * time.Hour
)

// statusRefreshChoices are the intervals the page offers, Grafana's.
var statusRefreshChoices = []time.Duration{
	5 * time.Second, 10 * time.Second, 30 * time.Second,
	time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour,
}

// ParseStatusRefresh reads --web.status-refresh: off, or a duration in
// whole seconds from 5s to 24h; off is 0. Anything else is a command-line
// error, as a page that refreshed every few milliseconds, or never though
// it was asked to, would be found out only by whoever opened it.
func ParseStatusRefresh(value string) (time.Duration, error) {
	if d, ok := statusRefreshOf(value); ok {
		return d, nil
	}
	return 0, fmt.Errorf("--web.status-refresh %q must be off or a duration in whole seconds from 5s to 24h, such as 30s or 1m", value)
}

// statusRefreshOf reads an auto-refresh as the flag and ?refresh= take it,
// and says whether it is one.
func statusRefreshOf(value string) (time.Duration, bool) {
	if value == "off" {
		return 0, true
	}
	d, err := time.ParseDuration(value)
	if err != nil || d%time.Second != 0 || d < minStatusRefresh || d > maxStatusRefresh {
		return 0, false
	}
	return d, true
}

// SetStatusRefresh sets the status page's auto-refresh when its URL does not
// say, --web.status-refresh; 0 is off.
func (s *Server) SetStatusRefresh(d time.Duration) {
	s.statusRefresh = d
}

// statusRefreshText is d as the page and its URL write it, as Grafana does:
// 30s, 1m, 1h30m; off for 0.
func statusRefreshText(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	var b strings.Builder
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / unit.size; n > 0 {
			b.WriteString(strconv.FormatInt(int64(n), 10) + unit.name)
			d -= n * unit.size
		}
	}
	return b.String()
}

// statusRefreshControl is the page's refresh controls: the interval in
// force, from ?refresh= or else the flag, and the choices, the one in force
// among them even when it is none of Grafana's.
type statusRefreshControl struct {
	// Interval is the interval in force as the URL writes it, Seconds the
	// same in seconds, 0 when off.
	Interval string
	Seconds  int64
	Choices  []statusRefreshChoice
}

type statusRefreshChoice struct {
	Value    string
	Seconds  int64
	Selected bool
}

// statusRefreshControlFor is the controls of a page asked for with query,
// the value of ?refresh=: one that is no auto-refresh is ignored, as a
// mistyped link still shows the page, and the flag's applies.
func (s *Server) statusRefreshControlFor(query string) statusRefreshControl {
	interval := s.statusRefresh
	if d, ok := statusRefreshOf(query); ok {
		interval = d
	}
	control := statusRefreshControl{Interval: statusRefreshText(interval), Seconds: int64(interval / time.Second)}
	control.Choices = append(control.Choices, statusRefreshChoice{Value: "off", Selected: interval == 0})
	listed := interval == 0
	for _, d := range statusRefreshChoices {
		if !listed && interval < d {
			control.Choices = append(control.Choices, statusRefreshChoice{Value: control.Interval, Seconds: control.Seconds, Selected: true})
			listed = true
		}
		control.Choices = append(control.Choices, statusRefreshChoice{Value: statusRefreshText(d), Seconds: int64(d / time.Second), Selected: d == interval})
		if d == interval {
			listed = true
		}
	}
	if !listed {
		control.Choices = append(control.Choices, statusRefreshChoice{Value: control.Interval, Seconds: control.Seconds, Selected: true})
	}
	return control
}
