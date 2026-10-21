package repository

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// allocatorSeries is an allocator metric's name, not the
	// __meta_pue_allocator_ labels of its service discovery answer.
	allocatorSeries = regexp.MustCompile(`(^|[^_A-Za-z0-9])pue_allocator_[a-z_]+`)
	// maxOfAllocatorSeries is an allocator metric as the argument of max,
	// with or without a by clause.
	maxOfAllocatorSeries = regexp.MustCompile(`max(\s+by\s*\([^)]*\))?\s*\(\s*pue_allocator_[a-z_]+`)
)

// Every allocator copy reports its metrics, one series each, so a query of
// an example that sums them, or takes them as they are, counts every target
// once per copy: an External metric's average doubles with two copies, and
// KEDA refuses an answer of several series. Every query an example of the
// docs or the chart README makes of an allocator metric takes their max,
// and so does every prometheus-adapter rule over one.
func TestExampleQueriesOfAllocatorMetricsTakeTheirMax(t *testing.T) {
	pages, err := filepath.Glob("docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	pages = append(pages, "charts/prometheus-universal-exporter/README.md")
	queries := 0
	for _, page := range pages {
		parts := strings.Split(read(t, page), "\n```")
		// The odd parts are the fenced blocks, each starting with its
		// language.
		for i := 1; i < len(parts); i += 2 {
			block := parts[i]
			for line := range strings.SplitSeq(block, "\n") {
				if !allocatorSeries.MatchString(line) || strings.Contains(line, "seriesQuery:") {
					continue
				}
				queries++
				if len(allocatorSeries.FindAllString(line, -1)) != len(maxOfAllocatorSeries.FindAllString(line, -1)) {
					t.Errorf("%s: an example queries an allocator metric without taking the max of the allocator copies' series: %s", page, strings.TrimSpace(line))
				}
			}
			for _, rule := range strings.Split(block, "- seriesQuery:")[1:] {
				series, _, _ := strings.Cut(rule, "\n")
				if !allocatorSeries.MatchString(series) {
					continue
				}
				queries++
				if !strings.Contains(rule, "metricsQuery: max by (<<.GroupBy>>) (<<.Series>>") {
					t.Errorf("%s: the prometheus-adapter rule over%s does not take the max of the allocator copies' series", page, series)
				}
			}
		}
	}
	if queries < 2 {
		t.Errorf("found %d example queries of allocator metrics, want at least the External metric rule and the KEDA trigger of docs/SELF-METRICS.md", queries)
	}
}
