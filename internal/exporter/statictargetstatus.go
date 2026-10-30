package exporter

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The status page, at the static targets path followed by /status, shows
// how each static target's last scrape went, and the probes the exporter
// answered lately (recentprobes.go), for people who have no Prometheus to
// ask. Of a static target: when its last scrape began and how long it took,
// whether it succeeded, came from the cache, failed and served a stale
// result, or was skipped by failure_backoff, the error of the last failed
// scrape, and whether the target is backing off. ?format=json serves the
// same as JSON, and ?format=fragment the page's content section alone,
// which the page fetches to refresh itself (statusrefresh.go).
//
// It shows what the static targets endpoint's series, the self-metrics and
// the exporter's log already show, read by whoever may read the endpoints:
// the same Basic Auth protects it. A target is shown as its health series
// and the log show it (fetch.DisplayTarget), with any credential in it
// withheld, and an error as the log has it, the URLs it quotes redacted
// (fetch.RedactURLErrors, which the fetch has mostly done already), masked
// once more for anything reading as a credential, a URL's userinfo among it
// (fetch.RedactText), and cut to model.MaxFailureBytes. A probe's error is
// shown by the same rule (shownStatusError), and so is a panic's: by a fixed
// text, the panic itself being only in the log. What an error holds for the
// operator alone is shown only with --web.debug=full; below it, as the
// probe's caller is answered it. What is kept of each
// static target is one record, replaced by its next scrape, so the page
// holds no more than the targets are many; what is kept of the probes is
// bounded apart (recentprobes.go). The path is the static targets path's,
// so it needs no flag of its own, and moves with --web.static-targets-path:
// it was the static targets' alone, and both flags and the chart already
// keep the self-metrics off it.
//
// The page is served with or without static targets: the recent probes are
// there either way, and most exporters have no static target. So is its
// OTLP section (otlpstatuspage.go), which says OTLP export is off when it
// is.

// staticTargetsStatusSuffix follows the static targets path to make the
// status page's.
const staticTargetsStatusSuffix = "/status"

// staticTargetsStatusEndpoint is the path of the status page.
func (s *Server) staticTargetsStatusEndpoint() string {
	return s.staticTargetsEndpoint() + staticTargetsStatusSuffix
}

// errScrapePanicked is the failure of a scrape or a probe that panicked, as
// the status page shows it and a probe answers it: the panic and its stack
// are in the log.
var errScrapePanicked = errors.New("the scrape panicked; the exporter's log has the panic")

// staticScrapeStatus is how one scrape of a static target went, as it ends.
type staticScrapeStatus struct {
	// collector and target are the target's definition the scrape was of.
	collector, target string
	started           time.Time
	duration          time.Duration
	ok                bool
	// err is why the scrape failed; skipped says failure_backoff kept it
	// from the target, stale that a stale result was served for it, cached
	// that the result came from the response cache.
	err                    error
	skipped, stale, cached bool
	// backoffKey is the key failure_backoff tells the target apart by,
	// empty without failure_backoff.
	backoffKey string
}

// staticTargetStatus is what the status page keeps of a target: its last
// scrape, and of its failures the run it is in and the last one's error.
type staticTargetStatus struct {
	last staticScrapeStatus
	// errorText is the shown error of the last failed scrape, which
	// failedAt began; failures counts the consecutive failed scrapes up to
	// the last, 0 when it succeeded. A scrape skipped by failure_backoff is
	// neither: it leaves both as they were.
	errorText string
	failedAt  time.Time
	failures  int
}

// noteStaticStatusLocked records status as the last scrape of the target
// name, under staticMu. Only the text of an error is kept, shown as the
// page shows it, so no error value, and nothing it holds on to, outlives
// the scrape.
func (s *Server) noteStaticStatusLocked(name string, status staticScrapeStatus) {
	if s.staticStatus == nil {
		s.staticStatus = map[string]*staticTargetStatus{}
	}
	st := s.staticStatus[name]
	if st == nil || st.last.collector != status.collector || st.last.target != status.target {
		// A target whose collector or address changed starts again: the
		// failures of what it was are not its own.
		st = &staticTargetStatus{}
		s.staticStatus[name] = st
	}
	switch {
	case status.ok:
		st.failures = 0
	case status.skipped:
		// A scrape failure_backoff kept from the target tried nothing: the
		// error shown stays that of the last scrape that did, which is why
		// the target is backed off from, and the run of failures is of the
		// scrapes that failed, which the backoff counts apart.
	default:
		st.failures++
		st.errorText = s.shownStatusError(status.err)
		st.failedAt = status.started
	}
	status.err = nil
	st.last = status
}

// shownStatusError is err as the status page shows it. The page is read by
// whoever may call /probe, behind the same Basic Auth, so what an error
// holds for the operator alone (model.WithOperatorDetail) is shown as the
// probe's caller is answered it (model.CallerText) unless --web.debug is
// full, the level that already shows those callers what a target answers.
// The caller's text is taken before the error is bounded, which keeps only
// its text and with it would lose the mark.
func (s *Server) shownStatusError(err error) string {
	if err == nil {
		return "the scrape failed"
	}
	if s.debug != DebugFull {
		err = &callerShownError{text: model.CallerText(err), err: err}
	}
	return shownFailureText(boundedTripFailure(fetch.RedactURLErrors(err)).Error())
}

// callerShownError is err with the text a probe's caller is answered: what
// it wraps stays readable, so that its URLs are still redacted.
type callerShownError struct {
	text string
	err  error
}

func (e *callerShownError) Error() string { return e.text }

func (e *callerShownError) Unwrap() error { return e.err }

// The results a row shows.
const (
	statusSuccess    = "success"
	statusFailed     = "failed"
	statusStale      = "stale result served"
	statusBackoff    = "skipped by backoff"
	statusNotScraped = "not yet scraped"
)

// staticStatusRow is one target's row, and its object in the JSON.
type staticStatusRow struct {
	Name           string            `json:"name"`
	Collector      string            `json:"collector"`
	Target         string            `json:"target"`
	Labels         map[string]string `json:"labels"`
	Interval       string            `json:"interval"`
	ExportViaOTLP  bool              `json:"export_via_otlp"`
	Result         string            `json:"result"`
	Failing        bool              `json:"failing"`
	LastScrape     string            `json:"last_scrape,omitempty"`
	LastScrapeAge  *float64          `json:"last_scrape_age_seconds,omitempty"`
	Duration       *float64          `json:"duration_seconds,omitempty"`
	Cached         bool              `json:"cached"`
	Stale          bool              `json:"stale"`
	Failures       int               `json:"consecutive_failures"`
	LastError      string            `json:"last_error,omitempty"`
	LastErrorAt    string            `json:"last_error_at,omitempty"`
	Backoff        string            `json:"backoff,omitempty"`
	BackoffUntil   string            `json:"backoff_until,omitempty"`
	BackoffTrial   bool              `json:"backoff_trial"`
	BackoffFailure int               `json:"backoff_failures,omitempty"`
	// LabelText, Age and DurationText are the row's values as the page
	// writes them.
	LabelText    string `json:"-"`
	Age          string `json:"-"`
	DurationText string `json:"-"`
}

// staticStatusPage is the page, and the JSON: the static targets, then the
// recent probes, then the OTLP export.
type staticStatusPage struct {
	GeneratedAt string `json:"generated_at"`
	// Allocator says the targets are those the allocator gave this
	// replica, in allocator mode, and not every target there is.
	Allocator  bool              `json:"allocator_mode"`
	Total      int               `json:"targets"`
	Failing    int               `json:"failing"`
	NotScraped int               `json:"not_yet_scraped"`
	Rows       []staticStatusRow `json:"static_targets"`
	// ProbeKeys is how many keys the recent probes have, FailingProbes how
	// many of them failed their latest probe.
	ProbeKeys     int              `json:"probe_keys"`
	FailingProbes int              `json:"failing_probe_keys"`
	Probes        []probeStatusRow `json:"probes"`
	// OTLP is how OTLP exports are going.
	OTLP otlpStatusSection `json:"otlp"`
	// PerKey and MaxKeys are the bounds of the recent probes, as the page
	// says them.
	PerKey  int `json:"-"`
	MaxKeys int `json:"-"`
	// StaticTargetsPath is the static targets endpoint.
	StaticTargetsPath string `json:"-"`
	// Chrome is the page's bar (pagechrome.go); the content section, which
	// a refresh swaps in, has none.
	Chrome pageChrome `json:"-"`
	// Refresh is the page's refresh controls (statusrefresh.go).
	Refresh statusRefreshControl `json:"-"`
}

// staticStatusPageFor builds the page of targets, those in force with cfg,
// as of now.
func (s *Server) staticStatusPageFor(cfg *model.Config, targets []model.StaticTarget, now time.Time) staticStatusPage {
	collectors := make(map[string]*model.Collector, len(cfg.Collectors))
	for i := range cfg.Collectors {
		collectors[cfg.Collectors[i].Name] = &cfg.Collectors[i]
	}
	page := staticStatusPage{
		GeneratedAt:       now.UTC().Format(time.RFC3339),
		Allocator:         s.allocatorStatus != nil,
		Total:             len(targets),
		StaticTargetsPath: s.staticTargetsEndpoint(),
		PerKey:            recentProbesPerKey,
		MaxKeys:           recentProbeKeys,
	}
	page.Probes = s.probeRowsAt(now)
	page.ProbeKeys = len(page.Probes)
	for i := range page.Probes {
		if page.Probes[i].Failing {
			page.FailingProbes++
		}
	}
	// The records are copied under the lock and read after it: the backoff
	// state is under a lock of its own, which is never taken under this one.
	statuses := make([]*staticTargetStatus, len(targets))
	s.staticMu.Lock()
	for i := range targets {
		if st := s.staticStatus[targets[i].Name]; st != nil && st.last.collector == targets[i].Collector && st.last.target == targets[i].Target {
			kept := *st
			statuses[i] = &kept
		}
	}
	s.staticMu.Unlock()
	exported := 0
	for i := range targets {
		if targets[i].ExportViaOTLP {
			exported++
		}
	}
	page.OTLP = s.otlpStatusSectionFor(cfg.OTLP, exported, now)
	page.Rows = make([]staticStatusRow, 0, len(targets))
	for i := range targets {
		target := &targets[i]
		row := staticStatusRow{
			Name: target.Name, Collector: target.Collector, Target: target.Target,
			Labels:        map[string]string{},
			Interval:      time.Duration(target.Interval).String(),
			ExportViaOTLP: target.ExportViaOTLP,
			Result:        statusNotScraped,
		}
		if c := collectors[target.Collector]; c != nil {
			row.Target = fetch.DisplayTarget(c, target.Target)
		} else {
			row.Target = fetch.DisplayTarget(&model.Collector{}, target.Target)
		}
		var labels []string
		for _, name := range model.SortedKeys(target.Labels) {
			// A label written "" is the label left out (withTargetLabels).
			if value := target.Labels[name]; value != "" {
				row.Labels[name] = value
				labels = append(labels, name+"="+value)
			}
		}
		row.LabelText = strings.Join(labels, ", ")
		if st := statuses[i]; st != nil {
			last := st.last
			age := max(now.Sub(last.started), 0).Seconds()
			duration := last.duration.Seconds()
			row.LastScrape = last.started.UTC().Format(time.RFC3339)
			row.LastScrapeAge, row.Duration = &age, &duration
			row.Age = shortDuration(time.Duration(age * float64(time.Second)))
			row.DurationText = shownScrapeDuration(last.duration)
			row.Cached, row.Stale = last.cached, last.stale
			row.Failures = st.failures
			row.LastError = st.errorText
			if !st.failedAt.IsZero() {
				row.LastErrorAt = st.failedAt.UTC().Format(time.RFC3339)
			}
			switch {
			case last.ok:
				row.Result = statusSuccess
			case last.skipped:
				row.Result = statusBackoff
			case last.stale:
				row.Result = statusStale
			default:
				row.Result = statusFailed
			}
			row.Failing = !last.ok
			if b := s.backoffs.state(target.Collector, last.backoffKey); b.backingOff {
				row.BackoffFailure = b.failures
				row.BackoffTrial = b.trial
				if b.trial {
					row.Backoff = "next try in progress"
				} else {
					row.BackoffUntil = b.until.UTC().Format(time.RFC3339)
					row.Backoff = "backing off until " + row.BackoffUntil
				}
			}
		}
		if row.Failing {
			page.Failing++
		}
		if row.Result == statusNotScraped {
			page.NotScraped++
		}
		page.Rows = append(page.Rows, row)
	}
	// Failing targets first, the rest after them, each by name.
	sort.SliceStable(page.Rows, func(i, j int) bool {
		if page.Rows[i].Failing != page.Rows[j].Failing {
			return page.Rows[i].Failing
		}
		return page.Rows[i].Name < page.Rows[j].Name
	})
	return page
}

// rootLink is the landing page as a link relative to the page at path.
func rootLink(path string) string {
	if up := strings.Count(path, "/") - 1; up > 0 {
		return strings.Repeat("../", up)
	}
	return "./"
}

// shownScrapeDuration is d to the millisecond, or to the microsecond under
// one, which a scrape answered from the cache takes.
func shownScrapeDuration(d time.Duration) string {
	if d < time.Millisecond {
		return d.Round(time.Microsecond).String()
	}
	return d.Round(time.Millisecond).String()
}

// shortDuration is d in whole seconds, minutes and hours, as an age reads.
func shortDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	return d.Truncate(time.Second).String()
}

func (s *Server) staticTargetsStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r, "use GET or HEAD to read the status page") {
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "html" && format != "json" && format != "fragment" {
		http.Error(w, "the format parameter is html, json or fragment", http.StatusBadRequest)
		return
	}
	cfg, file := s.manager.InForce()
	targets := staticTargetsOf(file)
	page := s.staticStatusPageFor(cfg, targets, time.Now())
	if format == "json" {
		body, err := json.Marshal(page)
		if err != nil {
			s.logger.Error("rendering the status page failed", "error", err)
			http.Error(w, "the status could not be rendered", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(append(body, '\n'))
		return
	}
	if format == "fragment" {
		// What a refresh swaps in: the content section, rendered by the
		// page's own template, with the page's headers, so it is never
		// cached either.
		s.renderFragment(w, staticStatusContentTemplate, page)
		return
	}
	page.Chrome = s.chromeFor(s.staticTargetsStatusEndpoint())
	page.Refresh = s.statusRefreshControlFor(r.URL.Query().Get("refresh"))
	s.renderPage(w, staticStatusTemplate, page)
}

var staticStatusTemplate = pageTemplate("static-status", `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Status · Prometheus Universal Exporter</title>
<style>`+pageStyle+`
main { max-width: 1200px; }
.scroll { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; font-size: 14px; }
th, td { text-align: left; vertical-align: top; padding: 6px 8px; border-bottom: 1px solid var(--line); }
th { background: var(--panel); }
td.err { font-family: ui-monospace, monospace; font-size: 13px; max-width: 420px; white-space: pre-wrap; }
.bad { color: var(--bad); font-weight: 600; }
.good { color: var(--good); }
.muted { color: var(--muted); }
.strip { font-family: ui-monospace, monospace; letter-spacing: 2px; white-space: nowrap; }
table.facts { width: auto; }
table.facts th { background: none; font-weight: 600; white-space: nowrap; }
form.refresh { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; margin: 8px 0 0; }
form.refresh a.button, form.refresh button, form.refresh select { font: inherit; color: var(--fg); background: var(--panel); border: 1px solid var(--line); border-radius: 6px; padding: 2px 10px; text-decoration: none; }
form.refresh a.button { color: var(--on-accent); background: var(--accent); border-color: var(--accent); }
</style>
{{- if .Refresh.Seconds}}
<noscript><meta http-equiv="refresh" content="{{.Refresh.Seconds}}"></noscript>
{{- end}}
</head>
<body>
<main>
{{template "chrome" .Chrome}}
<h1>Status</h1>
<form class="refresh" id="refresh" method="get">
<a class="button" id="refresh-now" href="">Refresh</a>
<label>Auto-refresh <select name="refresh" id="refresh-every">
{{- range .Refresh.Choices}}
<option value="{{.Value}}" data-seconds="{{.Seconds}}"{{if .Selected}} selected{{end}}>{{if eq .Value "off"}}Off{{else}}{{.Value}}{{end}}</option>
{{- end}}
</select></label>
<noscript><button type="submit">Apply</button></noscript>
<span id="refresh-state" class="muted" role="status" aria-live="polite"></span>
</form>
<div id="status-content">
{{- template "content" .}}
</div>
<script>
"use strict";
// The Refresh button and the auto-refresh fetch this page's content section
// again, ?format=fragment, and swap it in: one template renders both, and
// the fetch, same-origin, carries the Basic Auth the page was opened with
// and passes the cross-site refusal. Without JavaScript the button reloads
// the page, the form puts the interval chosen in the URL, and the meta
// refresh above reloads the page on it.
(() => {
  const $ = (id) => document.getElementById(id);
  const content = $("status-content"), every = $("refresh-every"), state = $("refresh-state");
  let timer = 0, inFlight = false, refreshedAt = Date.now(), dataAt = "";
  const clock = (ms) => new Date(ms).toLocaleTimeString();
  const seconds = () => Number(every.selectedOptions[0].dataset.seconds) || 0;
  // The content's own as-of line, in UTC, is what a page without
  // JavaScript reads. With it, the line beside the controls says the same
  // in the browser's time, so the content's line is hidden rather than
  // said twice; its UTC time is the line's tooltip.
  function asOf() {
    const line = content.querySelector("#as-of"), time = line && line.querySelector("time");
    if (!time) return;
    line.hidden = true;
    dataAt = time.getAttribute("datetime");
    state.title = dataAt;
  }
  const dataClock = () => clock(Date.parse(dataAt) || refreshedAt);
  asOf();
  state.textContent = "As of " + dataClock();
  async function refresh() {
    // A refresh still waiting for its answer is not overlapped: a tick
    // that finds one is skipped.
    if (inFlight) return;
    inFlight = true;
    const url = new URL(location.href);
    url.searchParams.set("format", "fragment");
    url.hash = "";
    try {
      const response = await fetch(url, {credentials: "same-origin", cache: "no-store"});
      if (!response.ok) throw new Error("HTTP " + response.status);
      content.innerHTML = await response.text();
      refreshedAt = Date.now();
      state.className = "muted";
      asOf();
      state.textContent = "As of " + dataClock();
    } catch (error) {
      // The last data stays, and the state says how old it is.
      state.className = "bad";
      state.textContent = "Refresh failed at " + clock(Date.now()) + " (" + error.message + "); showing the data as of " + dataClock();
    } finally {
      inFlight = false;
    }
  }
  // A hidden tab is not refreshed; back in view, it refreshes at once if a
  // tick fell due meanwhile, and the ticks start again from there.
  function schedule() {
    clearInterval(timer);
    timer = 0;
    const s = seconds();
    if (!s || document.visibilityState === "hidden") return;
    if (Date.now() - refreshedAt >= s * 1000) refresh();
    timer = setInterval(refresh, s * 1000);
  }
  $("refresh-now").addEventListener("click", (event) => {
    event.preventDefault();
    refresh();
  });
  every.addEventListener("change", () => {
    // The URL keeps the interval, so a bookmark or a shared link has it.
    const url = new URL(location.href);
    url.searchParams.set("refresh", every.value);
    history.replaceState(null, "", url);
    schedule();
  });
  $("refresh").addEventListener("submit", (event) => event.preventDefault());
  document.addEventListener("visibilitychange", schedule);
  schedule();
})();
</script>
</main>
</body>
</html>
{{define "content"}}
<p class="meta" id="as-of">As of <time datetime="{{.GeneratedAt}}">{{.GeneratedAt}}</time>.</p>
{{- if .Allocator}}
<p>This exporter runs in allocator mode: these are the static targets the allocator gave this replica, not every target the allocator holds, and the probes this replica answered.</p>
{{- end}}
{{- if .OTLP.Enabled}}{{if .OTLP.Failing}}
<p class="bad">OTLP export is failing: the last {{.OTLP.Failures}} export{{if ne .OTLP.Failures 1}}s{{end}} failed{{if eq .OTLP.Readiness "unready"}}, and the exporter is unready{{end}}. See <a href="#otlp">OTLP export</a>.</p>
{{- end}}{{end}}
<p>This page is also served as JSON with <code>?format=json</code>.</p>

<h2>Static targets</h2>
{{- if .Total}}
<p>{{.Total}} static target{{if ne .Total 1}}s{{end}}, {{.Failing}} failing{{if .NotScraped}}, {{.NotScraped}} not yet scraped{{end}}. Their series are served at <code>{{.StaticTargetsPath}}</code>.</p>
<div class="scroll">
<table>
<thead><tr><th>Target</th><th>Interval</th><th>Last scrape</th><th>Result</th><th>Failures</th><th>Last error</th></tr></thead>
<tbody>
{{- range .Rows}}
<tr>
<td><strong>{{.Name}}</strong><br><span class="muted">collector</span> {{.Collector}}{{if .Target}}<br><span class="muted">target</span> {{.Target}}{{end}}{{if .LabelText}}<br><span class="muted">labels</span> {{.LabelText}}{{end}}{{if .ExportViaOTLP}}<br><span class="muted">exported over OTLP</span>{{end}}</td>
<td>{{.Interval}}</td>
<td>{{if .LastScrape}}{{.LastScrape}}<br><span class="muted">{{.Age}} ago, took {{.DurationText}}</span>{{else}}<span class="muted">none yet</span>{{end}}</td>
<td>{{if .Failing}}<span class="bad">{{.Result}}</span>{{else if eq .Result "success"}}<span class="good">{{.Result}}</span>{{else}}<span class="muted">{{.Result}}</span>{{end}}{{if .Cached}}<br><span class="muted">from the cache</span>{{end}}</td>
<td>{{.Failures}}{{if .Backoff}}<br><span class="bad">{{.Backoff}}</span>{{end}}</td>
<td class="err">{{if .LastError}}{{.LastError}}<br><span class="muted">at {{.LastErrorAt}}</span>{{end}}</td>
</tr>
{{- end}}
</tbody>
</table>
</div>
{{- else}}
<p class="muted">No static targets are configured.</p>
{{- end}}

<h2>Recent probes</h2>
{{- if .ProbeKeys}}
<p>{{.ProbeKeys}} collector and target{{if ne .ProbeKeys 1}}s{{end}} probed, {{.FailingProbes}} failing their latest probe. The last {{.PerKey}} probes of each are kept, oldest first in the strip (✓ success or cached, ✗ failed, refused or rejected, ~ stale result served, … backing off), and of no more than {{.MaxKeys}}, the one probed least recently forgotten first.</p>
<div class="scroll">
<table>
<thead><tr><th>Probe</th><th>Last probe</th><th>Result</th><th>Recent</th><th>Last error</th></tr></thead>
<tbody>
{{- range .Probes}}
<tr>
<td><span class="muted">collector</span> <strong>{{.Collector}}</strong>{{if .Target}}<br><span class="muted">target</span> {{.Target}}{{end}}{{if .Params}}<br><span class="muted">parameters</span> {{.Params}}{{end}}</td>
<td>{{.LastProbe}}<br><span class="muted">{{.Age}} ago, took {{.DurationText}}</span></td>
<td>{{if .Failing}}<span class="bad">{{.Result}}</span>{{else}}<span class="good">{{.Result}}</span>{{end}}{{if .Shared}} <span class="muted">(shared)</span>{{end}}<br><span class="muted">HTTP {{.Status}}, {{.Series}} series</span></td>
<td class="strip">{{range .Probes}}<span class="{{if .Good}}good{{else}}bad{{end}}" title="{{.At}} {{.Result}}{{if .Shared}} (shared){{end}}, HTTP {{.Status}}">{{.Mark}}</span>{{end}}</td>
<td class="err">{{if .LastError}}{{.LastError}}<br><span class="muted">at {{.LastErrorAt}}</span>{{end}}</td>
</tr>
{{- end}}
</tbody>
</table>
</div>
{{- else}}
<p class="muted">No probe has been answered since the exporter started{{if .Allocator}} on this replica{{end}}, or since a reload changed the collectors probed.</p>
{{- end}}

<h2 id="otlp">OTLP export</h2>
{{- with .OTLP}}
{{- if .Enabled}}
<p>Exported {{.Protocol}} every {{.Interval}}: the probes' results, {{if .Exported.StaticTargets}}{{.Exported.StaticTargets}} static target{{if ne .Exported.StaticTargets 1}}s{{end}} with <code>export_via_otlp</code>, {{end}}and the exporter's own metrics.{{if .Allocator}} These are the exports of this replica.{{end}}</p>
<div class="scroll">
<table class="facts">
<tbody>
<tr><th>Endpoint</th><td>{{.Endpoint}}</td></tr>
<tr><th>Last export</th><td>{{with .LastExport}}{{.Started}}<br><span class="muted">{{.AgeText}} ago, took {{.DurationText}}</span><br>{{if eq .Result "success"}}<span class="good">{{.Result}}</span>{{else}}<span class="bad">{{.Result}}</span>{{end}}{{if .Rejected}}, {{.Rejected}} data point{{if ne .Rejected 1}}s{{end}} rejected{{end}}<br><span class="muted">{{.Requests}} request{{if ne .Requests 1}}s{{end}}, {{.DataPoints}} data point{{if ne .DataPoints 1}}s{{end}}, {{.Retries}} retr{{if eq .Retries 1}}y{{else}}ies{{end}}</span>{{else}}<span class="muted">none yet</span>{{end}}</td></tr>
<tr><th>Last success</th><td>{{if .LastSuccess}}{{.LastSuccess}} <span class="muted">({{.LastSuccessAge}} ago)</span>{{else}}<span class="muted">never</span>{{end}}</td></tr>
<tr><th>Failures in a row</th><td>{{if .Failing}}<span class="bad">{{.Failures}}</span>{{else}}0{{end}}</td></tr>
{{- if .UnreadyAfter}}
<tr><th>Readiness</th><td>{{if eq .Readiness "unready"}}<span class="bad">unready</span>{{if .UnreadySince}} since {{.UnreadySince}}{{end}}: {{range $i, $r := .UnreadyReasons}}{{if $i}}; {{end}}{{$r}}{{end}}{{else}}<span class="good">ready</span>{{end}} <span class="muted">(unready after {{.UnreadyAfter}} failed export{{if ne .UnreadyAfter 1}}s{{end}} in a row)</span></td></tr>
{{- end}}
<tr><th>Last error</th><td class="err">{{if .LastError}}{{.LastError}}<br><span class="muted">at {{.LastErrorAt}}</span>{{else}}<span class="muted">none</span>{{end}}</td></tr>
<tr><th>Pending</th><td>{{.Pending}} data point{{if ne .Pending 1}}s{{end}} of at most {{.MaxPending}}</td></tr>
<tr><th>Dropped since start</th><td>{{.Dropped}} data point{{if ne .Dropped 1}}s{{end}}{{if .Dropped}}<br><span class="muted">{{.DroppedBy.Refused}} refused, {{.DroppedBy.Rejected}} rejected, {{.DroppedBy.PendingCap}} over the pending bound, {{.DroppedBy.Shutdown}} of the last export at shutdown</span>{{end}}</td></tr>
<tr><th>Since start</th><td>{{.Succeeded}} export{{if ne .Succeeded 1}}s{{end}} succeeded, {{.FailedAll}} failed, {{.Retries}} retr{{if eq .Retries 1}}y{{else}}ies{{end}}</td></tr>
</tbody>
</table>
</div>
<h3 id="otlp-logs">Log export</h3>
{{- with .Logs}}
{{- if .Enabled}}
<p>The exporter's own log lines at {{.Level}} and above, exported every {{.Interval}}. Failing log exports do not make the exporter unready.</p>
<div class="scroll">
<table class="facts">
<tbody>
<tr><th>Endpoint</th><td>{{.Endpoint}}</td></tr>
<tr><th>Last export</th><td>{{if .LastExport}}{{.LastExport}}<br>{{if eq .LastResult "success"}}<span class="good">{{.LastResult}}</span>{{else}}<span class="bad">{{.LastResult}}</span>{{end}}, {{.LastRecords}} log record{{if ne .LastRecords 1}}s{{end}}{{else}}<span class="muted">none yet</span>{{end}}</td></tr>
<tr><th>Last success</th><td>{{if .LastSuccess}}{{.LastSuccess}}{{else}}<span class="muted">never</span>{{end}}</td></tr>
<tr><th>Failures in a row</th><td>{{if .Failing}}<span class="bad">{{.Failures}}</span>{{else}}0{{end}}</td></tr>
<tr><th>Last error</th><td class="err">{{if .LastError}}{{.LastError}}<br><span class="muted">at {{.LastErrorAt}}</span>{{else}}<span class="muted">none</span>{{end}}</td></tr>
<tr><th>Pending</th><td>{{.Pending}} log record{{if ne .Pending 1}}s{{end}} of at most {{.MaxPending}}</td></tr>
<tr><th>Dropped since start</th><td>{{.Dropped}} log record{{if ne .Dropped 1}}s{{end}}{{if .Dropped}}<br><span class="muted">{{.DroppedBy.PendingCap}} over the pending bound, {{.DroppedBy.Refused}} refused or rejected, {{.DroppedBy.Shutdown}} of the last export at shutdown</span>{{end}}</td></tr>
<tr><th>Since start</th><td>{{.Sent}} log record{{if ne .Sent 1}}s{{end}} sent; {{.Succeeded}} export{{if ne .Succeeded 1}}s{{end}} succeeded, {{.FailedAll}} failed</td></tr>
</tbody>
</table>
</div>
{{- else}}
<p class="muted">Log export is off.</p>
{{- end}}
{{- end}}
{{- else}}
<p class="muted">OTLP export is off.</p>
{{- end}}
{{- end}}
{{end}}`)

// staticStatusContentTemplate is the page's content section alone, which a
// refresh swaps in.
var staticStatusContentTemplate = staticStatusTemplate.Lookup("content")
