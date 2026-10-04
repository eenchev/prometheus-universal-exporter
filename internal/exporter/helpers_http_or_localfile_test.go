//go:build !select_request_types || request_type_http || request_type_localfile

package exporter

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func regexCollector(name string) model.Collector {
	return model.Collector{
		Name:      name,
		Request:   model.RequestConfig{Type: fetch.RequestTypeHTTP},
		Transform: model.TransformConfig{Type: "regex"},
		Metrics:   []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Expression: `v=(\d+) (?P<who>\S+)`, Labels: []model.LabelRule{{Name: "who", Expression: "who"}}}},
	}
}

func landingServer(t *testing.T, cfg *model.Config) *Server {
	t.Helper()
	server := newStaticServer(t, cfg, nil)
	server.logger = testutil.QuietLogger(t)
	return server
}

func getPath(server *Server, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// getPage fetches a page, which must be HTML that is not cached.
func getPage(t *testing.T, server *Server, path string) string {
	t.Helper()
	response := getPath(server, http.MethodGet, path)
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d", path, response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("%s Content-Type=%q", path, got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("%s Cache-Control=%q", path, got)
	}
	// No other site may frame the pages: the collectors page takes target
	// credentials.
	if got := response.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("%s X-Frame-Options=%q", path, got)
	}
	if got := response.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") || !strings.Contains(got, "connect-src 'self'") {
		t.Fatalf("%s Content-Security-Policy=%q", path, got)
	}
	return response.Body.String()
}

func requireContains(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
}

func seriesValue(t *testing.T, exposition, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(exposition, "\n") {
		if value, found := strings.CutPrefix(line, series+" "); found {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("no series %s", series)
	return 0
}

// inForce is the configuration and the static target file in force, with
// the generation they are followed at, as the scrape loop reads them for the
// scrapes it starts (followedInForce).
func (s *Server) inForce() (*model.Config, *model.StaticTargetFile, uint64) {
	followed := s.followedInForce()
	return followed.config, followed.targets, followed.generation
}

// rememberedOf is what the failure log remembers under the collector's name:
// each failure's stage and how many times it was reported, sorted.
func rememberedOf(server *Server, collector string) []string {
	server.failures.mu.Lock()
	defer server.failures.mu.Unlock()
	var out []string
	for key, st := range server.failures.entries {
		if keyCollector(key) == collector {
			out = append(out, fmt.Sprintf("%s x%d", st.stage, st.failures))
		}
	}
	sort.Strings(out)
	return out
}

// lateScrape is a server on which a static target's scrape can be made
// late: with the configuration read before reloads, each followed, removed
// its collector and brought it back as it was. The collector in force is
// then another stay of the same definition, so what the late scrape writes
// to the failure log would be written under the very keys of the collector
// in force. The target is in force throughout, in a static target file no
// reload replaces.
type lateScrape struct {
	server *Server
	logs   *bytes.Buffer
	static model.StaticTarget
	read   *followedConfig
}

func newLateScrape(t *testing.T, c model.Collector, static model.StaticTarget) *lateScrape {
	t.Helper()
	logs := testutil.CaptureLogs(t)
	beside := c
	beside.Name = c.Name + "_beside"
	server, manager := newCacheTestServer(t, beside, c)
	file := &model.StaticTargetFile{Targets: []model.StaticTarget{static}}
	manager.SetTargets("", file)
	l := &lateScrape{server: server, logs: logs, static: static, read: server.reconcile()}
	for _, collectors := range [][]model.Collector{{beside}, {beside, c}} {
		cfg := &model.Config{Collectors: collectors}
		if err := config.Validate(cfg); err != nil {
			t.Fatal(err)
		}
		server.manager = config.NewManager(cfg, "", server.logger)
		server.manager.SetTargets("", file)
		server.reconcile()
	}
	return l
}

// late makes the scrape that read the collector before the reloads.
func (l *lateScrape) late(ctx context.Context) {
	l.server.scrapeTargetSince(ctx, l.read.config, l.read.generation, l.static)
}

// inForce makes a scrape with the configuration in force.
func (l *lateScrape) inForce(ctx context.Context) {
	cfg, _, generation := l.server.inForce()
	l.server.scrapeTargetSince(ctx, cfg, generation, l.static)
}

// logged is how many lines of the message, or beginning with it, the log has
// above debug level.
func (l *lateScrape) logged(msg string) int {
	return strings.Count(l.logs.String(), `"msg":"`+msg)
}

// lateSite is one place where a trip writes to the failure log: a failure
// of some kind, and its end. fail makes the scrapes that follow fail there
// and mend makes them pass; stage is what the failure log remembers the
// failure as, failure its message and recovery that of its end, empty when
// the site has none of its own. ctx is what the scrapes run under,
// context.Background when it is nil.
type lateSite struct {
	name                     string
	collector                model.Collector
	static                   model.StaticTarget
	fail, mend               func(l *lateScrape)
	stage, failure, recovery string
	ctx                      func() context.Context
}

// checkLateSite shows that what a late scrape writes at the site goes
// nowhere: its failure is not remembered nor logged above debug level, where
// the same scrape with the configuration in force has its failure remembered
// and logged once; and its success is no recovery of the collector in force,
// whose failure stays remembered until a scrape of its own passes, which is
// logged as the recovery.
func checkLateSite(t *testing.T, site lateSite) {
	t.Helper()
	ctx := context.Background
	if site.ctx != nil {
		ctx = site.ctx
	}
	name := site.collector.Name
	t.Run(site.name+", a late failure", func(t *testing.T) {
		l := newLateScrape(t, site.collector, site.static)
		site.fail(l)
		l.late(ctx())
		if remembered := rememberedOf(l.server, name); len(remembered) != 0 || l.logged(site.failure) != 0 {
			t.Errorf("the failure of a scrape of a stay that ended is remembered as %v, or logged above debug level:\n%s", remembered, l.logs)
		}
		l.inForce(ctx())
		if remembered := rememberedOf(l.server, name); !slices.Contains(remembered, site.stage+" x1") || l.logged(site.failure) != 1 {
			t.Errorf("the failure of a scrape of the collector in force is remembered as %v, want %q among them and logged once:\n%s", remembered, site.stage+" x1", l.logs)
		}
	})
	if site.recovery == "" {
		return
	}
	t.Run(site.name+", a late recovery", func(t *testing.T) {
		l := newLateScrape(t, site.collector, site.static)
		site.fail(l)
		l.inForce(ctx())
		before := rememberedOf(l.server, name)
		if !slices.Contains(before, site.stage+" x1") {
			t.Fatalf("the failure of the collector in force is remembered as %v, want %q among them:\n%s", before, site.stage+" x1", l.logs)
		}
		site.mend(l)
		l.late(ctx())
		if after := rememberedOf(l.server, name); !slices.Equal(after, before) || l.logged(site.recovery) != 0 {
			t.Errorf("after the success of a scrape of a stay that ended the collector in force has %v remembered, want %v as before, and no recovery logged:\n%s", after, before, l.logs)
		}
		l.inForce(ctx())
		if after := rememberedOf(l.server, name); slices.Contains(after, site.stage+" x1") || l.logged(site.recovery) != 1 {
			t.Errorf("after a success of its own the collector in force has %v remembered, want recovered and the recovery logged once:\n%s", after, l.logs)
		}
	})
}
