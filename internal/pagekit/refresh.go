package pagekit

import (
	"strconv"
	"strings"
	"time"
)

// A page that refreshes itself does so as a Grafana dashboard does: a
// Refresh button, and an auto-refresh of Off or one of Grafana's intervals,
// kept in the page's URL, ?refresh=30s, so a bookmark or a shared link keeps
// it. These are its bounds, its choices and its controls (RefreshStyle);
// each page's script fetches its own content again and swaps it in.

// Bounds of an auto-refresh. Below 5s, the least Grafana offers, a page
// left open in a few tabs would rebuild its content many times a second
// between them for nothing a person reads; above a day a browser's timer,
// which holds about 24.8 days, is no longer far off, and nobody waits that
// long for a page anyway.
const (
	MinRefresh = 5 * time.Second
	MaxRefresh = 24 * time.Hour
)

// RefreshChoices are the intervals a page offers, Grafana's.
var RefreshChoices = []time.Duration{
	5 * time.Second, 10 * time.Second, 30 * time.Second,
	time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour,
}

// RefreshOf reads an auto-refresh as a flag and ?refresh= take it, off or
// a duration in whole seconds from MinRefresh to MaxRefresh, off being 0,
// and says whether it is one.
func RefreshOf(value string) (time.Duration, bool) {
	if value == "off" {
		return 0, true
	}
	d, err := time.ParseDuration(value)
	if err != nil || d%time.Second != 0 || d < MinRefresh || d > MaxRefresh {
		return 0, false
	}
	return d, true
}

// RefreshText is d as a page and its URL write it, as Grafana does: 30s,
// 1m, 1h30m; off for 0.
func RefreshText(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	var b strings.Builder
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / unit.size; n > 0 {
			b.WriteString(strconv.FormatInt(int64(n), 10))
			b.WriteString(unit.name)
			d -= n * unit.size
		}
	}
	return b.String()
}

// RefreshControl is a page's refresh controls: the interval in force, from
// ?refresh= or else the page's default, and the choices, the one in force
// among them even when it is none of Grafana's.
type RefreshControl struct {
	// Interval is the interval in force as the URL writes it, Seconds the
	// same in seconds, 0 when off.
	Interval string
	Seconds  int64
	Choices  []RefreshChoice
}

// RefreshChoice is one choice of a RefreshControl.
type RefreshChoice struct {
	Value    string
	Seconds  int64
	Selected bool
}

// NewRefreshControl is the controls of a page whose default interval is
// def, asked for with query, the value of ?refresh=: one that is no
// auto-refresh is ignored, as a mistyped link still shows the page, and the
// default applies.
func NewRefreshControl(def time.Duration, query string) RefreshControl {
	interval := def
	if d, ok := RefreshOf(query); ok {
		interval = d
	}
	control := RefreshControl{Interval: RefreshText(interval), Seconds: int64(interval / time.Second)}
	control.Choices = append(control.Choices, RefreshChoice{Value: "off", Selected: interval == 0})
	listed := interval == 0
	for _, d := range RefreshChoices {
		if !listed && interval < d {
			control.Choices = append(control.Choices, RefreshChoice{Value: control.Interval, Seconds: control.Seconds, Selected: true})
			listed = true
		}
		control.Choices = append(control.Choices, RefreshChoice{Value: RefreshText(d), Seconds: int64(d / time.Second), Selected: d == interval})
		if d == interval {
			listed = true
		}
	}
	if !listed {
		control.Choices = append(control.Choices, RefreshChoice{Value: control.Interval, Seconds: control.Seconds, Selected: true})
	}
	return control
}

// RefreshStyle is the style of a page's refresh controls: a form.refresh
// of a Refresh a.button, an Auto-refresh label around its select, a button
// for a browser without JavaScript, and a line saying how old the content
// is.
const RefreshStyle = `
form.refresh { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; margin: 8px 0 0; }
form.refresh label { display: inline-flex; align-items: center; gap: 6px; }
form.refresh a.button, form.refresh button, form.refresh select { display: inline-flex; align-items: center; height: 30px; font: inherit; line-height: 1; color: var(--fg); background: var(--panel); border: 1px solid var(--line); border-radius: 6px; padding: 0 10px; text-decoration: none; }
form.refresh a.button { color: var(--on-accent); background: var(--accent); border-color: var(--accent); }
`
