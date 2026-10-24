package exporter

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The static targets' status page, at the static targets path followed by
// /status, shows how each static target's last scrape went, for people who
// have no Prometheus to ask: when it began and how long it took, whether it
// succeeded, came from the cache, failed and served a stale result, or was
// skipped by failure_backoff, the error of the last failed scrape, and
// whether the target is backing off. ?format=json serves the same as JSON.
//
// It shows what the static targets endpoint's series and the exporter's log
// already show, read by whoever may read the endpoint: the same Basic Auth
// protects it. The target is shown as its health series and the log show it
// (fetch.DisplayTarget), with any credential in it withheld, and an error as
// the log has it, its URLs already redacted where the fetch made it, masked
// once more for anything reading as a credential (fetch.RedactText) and cut
// to model.MaxFailureBytes. What is kept of each target is one record,
// replaced by its next scrape, so the page holds no more than the targets
// are many. The path is the static targets path's, so it needs no flag of
// its own, and moves with --web.static-targets-path.
//
// With no static target in force the page is not found: there is nothing to
// show, and the targets may come with a reload, so the path stays routed.

// staticTargetsStatusSuffix follows the static targets path to make the
// status page's.
const staticTargetsStatusSuffix = "/status"

// staticTargetsStatusEndpoint is the path of the status page.
func (s *Server) staticTargetsStatusEndpoint() string {
	return s.staticTargetsEndpoint() + staticTargetsStatusSuffix
}

// errScrapePanicked is the failure of a scrape that panicked, as the status
// page shows it: the panic and its stack are in the log.
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
	// the last, 0 when it succeeded.
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
	if status.ok {
		st.failures = 0
	} else {
		st.failures++
		st.errorText = shownStatusError(status.err)
		st.failedAt = status.started
	}
	status.err = nil
	st.last = status
}

// shownStatusError is err as the status page shows it.
func shownStatusError(err error) string {
	if err == nil {
		return "the scrape failed"
	}
	text := fetch.RedactText(strings.Join(strings.Fields(boundedTripFailure(err).Error()), " "))
	return model.CutTo(text, model.MaxFailureBytes, false)
}

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

// staticStatusPage is the page, and the JSON.
type staticStatusPage struct {
	GeneratedAt string `json:"generated_at"`
	// Allocator says the targets are those the allocator gave this
	// replica, in allocator mode, and not every target there is.
	Allocator  bool              `json:"allocator_mode"`
	Total      int               `json:"targets"`
	Failing    int               `json:"failing"`
	NotScraped int               `json:"not_yet_scraped"`
	Rows       []staticStatusRow `json:"static_targets"`
	// StaticTargetsPath is the static targets endpoint; HomeHref the
	// landing page, as a link relative to this page, so it holds behind a
	// reverse proxy that serves the exporter under a path of its own.
	StaticTargetsPath string `json:"-"`
	HomeHref          string `json:"-"`
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
		HomeHref:          rootLink(s.staticTargetsStatusEndpoint()),
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
	if !readOnly(w, r, "use GET or HEAD to read the static targets' status") {
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "html" && format != "json" {
		http.Error(w, "the format parameter is html or json", http.StatusBadRequest)
		return
	}
	cfg, file := s.manager.InForce()
	targets := staticTargetsOf(file)
	if len(targets) == 0 {
		http.Error(w, "no static targets are configured", http.StatusNotFound)
		return
	}
	page := s.staticStatusPageFor(cfg, targets, time.Now())
	if format == "json" {
		body, err := json.Marshal(page)
		if err != nil {
			s.logger.Error("rendering the static targets' status failed", "error", err)
			http.Error(w, "the status could not be rendered", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(append(body, '\n'))
		return
	}
	s.renderPage(w, staticStatusTemplate, page)
}

var staticStatusTemplate = template.Must(template.New("static-status").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Static targets · Prometheus Universal Exporter</title>
<style>` + pageStyle + `
main { max-width: 1200px; }
.scroll { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; font-size: 14px; }
th, td { text-align: left; vertical-align: top; padding: 6px 8px; border-bottom: 1px solid var(--line); }
th { background: var(--panel); }
td.err { font-family: ui-monospace, monospace; font-size: 13px; max-width: 420px; white-space: pre-wrap; }
.bad { color: var(--bad); font-weight: 600; }
.good { color: var(--good); }
.muted { color: var(--muted); }
</style>
</head>
<body>
<main>
<h1>Static targets</h1>
<p class="meta">{{.Total}} static target{{if ne .Total 1}}s{{end}}, {{.Failing}} failing{{if .NotScraped}}, {{.NotScraped}} not yet scraped{{end}}. As of {{.GeneratedAt}}; reload the page to see it again. <a href="{{.HomeHref}}">Home</a></p>
{{- if .Allocator}}
<p>This exporter runs in allocator mode: these are the static targets the allocator gave this replica, not every target the allocator holds.</p>
{{- end}}
<p>The targets' series are served at <code>{{.StaticTargetsPath}}</code>; this page is also served as JSON with <code>?format=json</code>.</p>
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
</main>
</body>
</html>
`))
