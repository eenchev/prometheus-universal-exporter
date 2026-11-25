package exporter

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// everyChainState is a report of a chain with a step in each state: one
// that went through, a fan-out cut by max_items whose skipped items
// failed, one that failed, one that timed out and one not reached.
func everyChainState() *fetch.ChainReport {
	return &fetch.ChainReport{Answer: model.ChainAnswerAll, FailedStep: "detail", Failed: `step "detail"`, Steps: []fetch.ChainStepReport{
		{Name: "first", Method: "GET", URL: "https://api.example/jobs?token=%3Credacted%3E", State: fetch.ChainStepOK, Status: 200, Duration: 12 * time.Millisecond, Requests: 1,
			Values: []fetch.ChainValueReport{{Name: "jobs", Text: `["api","web"]`, Found: true}, {Name: "token", Text: "s3cret", Secret: true, Found: true}}},
		{Name: "builds", Method: "GET", URL: "https://api.example/job/api", State: fetch.ChainStepFailed, ForEach: true, OnItemError: model.ChainItemSkip, ItemsTotal: 340, ItemsCut: 240, ItemsFailed: 1, Requests: 100,
			Items: []fetch.ChainItemReport{{Index: 0, Each: "api", Status: 200}, {Index: 2, Each: "web", Error: "received HTTP status 404"}}},
		{Name: "detail", Method: "POST", URL: "https://api.example/detail", State: fetch.ChainStepFailed, Status: 500, Error: "received HTTP status 500"},
		{Name: "slow", Method: "GET", State: fetch.ChainStepTimedOut, Duration: 2100 * time.Millisecond, Deadline: "the trip's deadline", Error: "context deadline exceeded"},
		{Name: "after", Method: "GET", State: fetch.ChainStepNotReached},
	}}
}

// The workflow diagram draws each state with its colour and its word: a
// block that went through green, a failed or timed-out one red with its
// error inside it, a fan-out stacked, amber when its failed items were
// skipped and with an amber note when max_items cut it, and a step not
// reached grey and dashed. Its text equivalent says the same in words, a
// secret value shows as ***, and each block links to its step's details.
func TestTheWorkflowDiagramDrawsEveryState(t *testing.T) {
	html := string(chainDiagram(everyChainState(), chainDiagramOptions{values: true, links: true}))
	for _, want := range []string{
		`<rect class="chain-block chain-ok"`,
		`<rect class="chain-block chain-warned chain-stack"`,
		`<rect class="chain-block chain-failed"`,
		`<rect class="chain-block chain-unreached"`,
		`<a href="#chain-step-detail">`,
		"failed  status 500",
		"received HTTP status 500",
		"timed out after 2.1s, the trip&#39;s deadline ran out",
		"failed items skipped",
		"2 items: 1 went through, 1 failed",
		"item 3 (web): received HTTP status 404",
		`<text class="chain-text chain-note"`, "cut at 100 of 340 (max_items)",
		"not reached",
		"read: jobs=[&#34;api&#34;,&#34;web&#34;], token=***",
		`<ol class="chain-steps"`, "<li>after  GET; not reached</li>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q in\n%s", want, html)
		}
	}
	if strings.Contains(html, "s3cret") || strings.Contains(html, "<script") {
		t.Fatalf("the diagram holds a secret or a script:\n%s", html)
	}
	if strings.Contains(html, `href="#chain-step-after"`) {
		t.Error("a step not reached links to details it has none of")
	}
	names := string(chainDiagram(everyChainState(), chainDiagramOptions{}))
	if !strings.Contains(names, "read: jobs, token") || strings.Contains(names, "<a href") {
		t.Errorf("the status page's diagram shows values or links:\n%s", names)
	}
	cached := string(chainDiagram(everyChainState(), chainDiagramOptions{cached: time.Date(2026, 11, 25, 10, 2, 15, 0, time.UTC)}))
	if strings.Count(cached, "<rect") != 1 || !strings.Contains(cached, "from the cache, fetched 10:02:15") || !strings.Contains(cached, "chain-cached") {
		t.Errorf("a trip from the cache is not one grey block:\n%s", cached)
	}
}

// The text debug report gives the chain one line per step, its state in
// capitals when it is not ok, with what each read and a fan-out's items.
func TestTheChainLinesOfTheTextReport(t *testing.T) {
	lines := strings.Join(chainLines(everyChainState()), "\n")
	for _, want := range []string{
		"  first   ok          GET https://api.example/jobs?token=%3Credacted%3E -> 200 in 12ms, 0 bytes",
		"          read: jobs=[\"api\",\"web\"], token=***",
		"  builds  FAILED      2 items, 1 failed and skipped, in 0s",
		"          cut at 100 of 340 (max_items)",
		"          item 3 (web): GET  -> received HTTP status 404",
		"  detail  FAILED      received HTTP status 500",
		"  slow    TIMED OUT   context deadline exceeded",
		"  after   NOT REACHED",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %q in\n%s", want, lines)
		}
	}
}
