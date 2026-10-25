//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// reposTarget answers a GitHub-style list of repositories, two to a page,
// over pages pages, each naming the next in its Link header; the page
// failing, when not 0, answers 503.
func reposTarget(t *testing.T, pages int, failing *atomic.Int32) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		if failing != nil && int(failing.Load()) == page {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"busy"}`))
			return
		}
		if page < pages {
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/repos?page=%d>; rel="next", <%s/orgs/acme/repos?page=%d>; rel="last"`, server.URL, page+1, server.URL, pages))
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `[{"name":"repo%d","stargazers_count":%d},{"name":"repo%d","stargazers_count":%d}]`, 2*page-1, 10*page, 2*page, 10*page+1)
	}))
	t.Cleanup(server.Close)
	return server
}

// reposCollector reads the repositories of every page with jq: their number
// and each one's stars.
func reposCollector(maxPages int) model.Collector {
	return model.Collector{
		Name:    "repos",
		Request: model.RequestConfig{Type: fetch.RequestTypeHTTP, Path: "/orgs/acme/repos", Own: fetch.HTTPConfig{Pagination: &model.PaginationConfig{Type: model.PaginationLink, Items: ".", MaxPages: maxPages}}},
		// The decoder is left out: pagination makes it json.
		Transform: model.TransformConfig{Type: "jq"},
		Limits:    model.Limits{MaxResponseBytes: 1 << 20},
		Metrics: []model.MetricRule{
			{Name: "github_repos", Type: model.GaugeMetricType, Expression: "length"},
			{Name: "github_repo_stars", Type: model.GaugeMetricType, Items: ".[]", Expression: ".stargazers_count", Labels: []model.LabelRule{{Name: "repo", Expression: ".name"}}},
		},
	}
}

// A probe of a collector with request.pagination reads every page, the rules
// written for one page seeing them all as one; the pages are counted in the
// self-metrics. A trip that stops at max_pages with another page to come is
// answered with the pages it fetched, counted, and logged once at warn level
// until a trip fetches every page again. A later page answered 503 fails
// the probe in the http_status stage, naming the page.
func TestAProbeReadsEveryPage(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	var failing atomic.Int32
	target := reposTarget(t, 3, &failing)
	server := modeServer(t, reposCollector(0))
	probeURL := "/probe?collector=repos&target=" + url.QueryEscape(target.URL)
	r := probeOnce(t, server, probeURL, nil)
	body := r.Body.String()
	if r.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", r.Code, body)
	}
	for _, want := range []string{"github_repos 6\n", `github_repo_stars{repo="repo1"} 10` + "\n", `github_repo_stars{repo="repo6"} 31` + "\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{`http_exporter_pages_fetched_total{collector="repos"} 3`, `http_exporter_pagination_max_pages_reached_total{collector="repos"} 0`} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics", want)
		}
	}

	stopping := modeServer(t, reposCollector(2))
	for range 2 {
		if r := probeOnce(t, stopping, probeURL, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "github_repos 4\n") {
			t.Fatalf("answered %d: %s", r.Code, r.Body)
		}
	}
	metrics = selfMetrics(t, stopping)
	for _, want := range []string{`http_exporter_pages_fetched_total{collector="repos"} 4`, `http_exporter_pagination_max_pages_reached_total{collector="repos"} 2`} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("no %s in the self-metrics", want)
		}
	}
	const stopped = `"level":"WARN","msg":"pagination stopped at request.pagination.max_pages with another page to come; the series are of the pages fetched"`
	if n := strings.Count(logs.String(), stopped); n != 1 || !strings.Contains(logs.String(), `"pages":2,"max_pages":2`) {
		t.Fatalf("%d warnings:\n%s", n, logs)
	}

	failing.Store(2)
	r = probeOnce(t, server, probeURL, nil)
	if want := "received HTTP status 503 on page 2 of request.pagination, " + target.URL + "/orgs/acme/repos?page=2"; r.Code != http.StatusBadGateway || !strings.Contains(r.Body.String(), want) {
		t.Fatalf("answered %d: %s\nwant %s", r.Code, r.Body, want)
	}
	if !strings.Contains(logs.String(), `"stage":"http_status"`) {
		t.Errorf("the failure was not logged in the http_status stage:\n%s", logs)
	}
}

// A debug probe's report shows each page asked for, by its number, the
// response as the pages merged, and how many pages the fetch read.
func TestADebugProbeShowsEveryPage(t *testing.T) {
	testutil.CaptureLogs(t)
	target := reposTarget(t, 3, nil)
	server := modeServer(t, reposCollector(2))
	server.SetDebug(DebugFull)
	r := debugProbeGet(t, server, "collector=repos&debug=true&target="+url.QueryEscape(target.URL))
	assertContains(t, r.Body.String(),
		"1. GET "+target.URL+"/orgs/acme/repos (page 1) -> 200 OK",
		"2. GET "+target.URL+"/orgs/acme/repos?page=<redacted> (page 2) -> 200 OK",
		"2 pages of request.pagination merged into one document; the status and headers are the first page's, stopped at request.pagination.max_pages with another page to come",
		`[{"name":"repo1","stargazers_count":10},{"name":"repo2","stargazers_count":11},{"name":"repo3","stargazers_count":20},{"name":"repo4","stargazers_count":21}]`,
		"status 200, 157 bytes, 2 pages, stopped at request.pagination.max_pages with another page to come",
		"pagination stopped at request.pagination.max_pages",
		"github_repos 4",
	)
}

// A collector with request.pagination decodes its pages as JSON: a decoder
// left out is json, and another is refused when the configuration loads.
func TestPaginationNeedsTheJSONDecoder(t *testing.T) {
	c := reposCollector(0)
	server := modeServer(t, c)
	if got := server.manager.Get().Collectors[0].Decoder.Type; got != "json" {
		t.Fatalf("decoder %q", got)
	}
	c = reposCollector(0)
	c.Decoder.Type = "yaml"
	err := config.Validate(&model.Config{Collectors: []model.Collector{c}})
	if want := `collector "repos" decodes with yaml, but request.pagination reads its pages as JSON and merges them into one JSON document; leave decoder.type out, or set json`; err == nil || err.Error() != want {
		t.Fatalf("err=%v", err)
	}
}

// A static target of a collector with request.pagination is scraped page by
// page as a probe is, its own path the base the pages count from.
func TestAStaticTargetReadsEveryPage(t *testing.T) {
	testutil.CaptureLogs(t)
	target := reposTarget(t, 3, nil)
	c := reposCollector(0)
	c.Request.Path = ""
	cfg := &model.Config{Collectors: []model.Collector{c}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "acme", Collector: "repos", Target: target.URL, Request: model.TargetRequestConfig{Path: "/orgs/acme/repos", PathSet: true}},
	}}
	server := newStaticServer(t, cfg, file)
	server.scrapeStaticTargets(context.Background(), 0)
	results := server.staticTargetResults()
	if len(results) != 1 {
		t.Fatalf("results %+v", results)
	}
	var repos float64
	stars := 0
	for _, m := range results[0].set.Metrics {
		switch m.Name {
		case "github_repos":
			repos = m.Value
		case "github_repo_stars":
			stars++
		}
	}
	if repos != 6 || stars != 6 {
		t.Fatalf("repos %v, %d stars series: %+v", repos, stars, results[0].set.Metrics)
	}
	if want := `http_exporter_pages_fetched_total{collector="repos"} 3`; !strings.Contains(selfMetrics(t, server), want+"\n") {
		t.Errorf("no %s in the self-metrics", want)
	}
}
