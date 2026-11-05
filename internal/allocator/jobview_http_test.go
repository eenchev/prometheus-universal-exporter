//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// jobProbes are probe jobs of every kind the probe jobs table shows: one
// listed with labels and params, a credential among them, one of
// credentials in its targets' URLs, one discovered from a file_sd source
// and one whose source fails.
const jobProbes = `probe_jobs:
  - name: websites
    collector: demo
    labels: {team: web, gone: ""}
    params: {timeout: 10s, api_key: s3cr3t-key}
    targets: [https://a.example/health, https://b.example/health, https://c.example/health, https://d.example/health, https://e.example/health, https://f.example/health]
  - name: apis
    collector: other
    targets: ["https://api1.example/v1?access_token=sekret1", "https://api2.example/v1?access_token=sekret2"]
  - name: pods
    collector: demo
    target_template: http://{{address}}/metrics
    labels: {team: shop}
    discovery:
      - name: pod-files
        file_sd: {files: [sd/pods.json]}
  - name: legacy
    collector: demo
    targets: [https://old.example/health]
    discovery:
      - name: inventory
        file_sd: {files: [sd/broken.json]}
`

// jobAllocator is an allocator of jobProbes over r1, r2 and r3, its
// sources asked once.
func jobAllocator(t *testing.T) *Allocator {
	t.Helper()
	p := writeInputs(t, testConfig, staticTargets(9), jobProbes)
	dir := filepath.Dir(p.ProbeTargets)
	testutil.WriteIn(t, dir, "sd/pods.json", `[{"targets": ["10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080"], "labels": {"namespace": "shop", "team": "theirs"}}]`)
	testutil.WriteIn(t, dir, "sd/broken.json", `{not json`)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1", "r2", "r3")
	t.Cleanup(a.BeginShutdown)
	a.refreshDiscovery(context.Background())
	return a
}

// jobViewOf is the probe job view the JSON gives for query.
func jobViewOf(t *testing.T, h http.Handler, query string) JobView {
	t.Helper()
	rec := ask(h, StatusPath+"?"+query, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%s?%s answered %d: %s", StatusPath, query, rec.Code, rec.Body)
	}
	var v JobView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The status's spread has a row of each probe job: its collector, its
// targets, the members it is routed among with the targets of each, the
// fewest, the mean and the most a member has, the imbalance, marked uneven
// where hashing would seldom put as many on one member, and its sources;
// the members' rows count each job's targets among their probe targets.
// The page's targets table has fixed columns, a probe job no longer one of
// its own, and the probe jobs have a table of their own, sortable and
// filtered as it is typed, the order and the filter kept over a refresh.
func TestTheStatusPageHasAProbeJobsTable(t *testing.T) {
	a := jobAllocator(t)
	h := a.Handler("")
	s := statusOf(t, h, "")
	sp := s.Spread
	var names []string
	for _, j := range sp.Jobs {
		names = append(names, j.Job)
	}
	if !slices.Equal(names, []string{"websites", "apis", "pods", "legacy"}) {
		t.Fatalf("the spread's jobs are %v", names)
	}
	probe := map[string]int{}
	for _, j := range sp.Jobs {
		if len(j.Members) != 3 || j.Members[0].Member != "r1" || j.Members[2].Member != "r3" {
			t.Errorf("%s is routed among %+v", j.Job, j.Members)
		}
		sum, least, most := 0, j.Targets, 0
		for _, m := range j.Members {
			sum += m.Targets
			least, most = min(least, m.Targets), max(most, m.Targets)
			probe[m.Member] += m.Targets
		}
		avg := float64(j.Targets) / 3
		above := float64(most) - avg
		if sum != j.Targets || j.Min != least || j.Max != most || j.Average != avg || j.Imbalance != float64(most)/avg ||
			j.Uneven != (above >= 3 && above >= 3*math.Sqrt(float64(j.Targets)*2/9)) {
			t.Errorf("%s: %+v", j.Job, j)
		}
	}
	for _, m := range sp.Members {
		if m.Probe != float64(probe[m.Member]) {
			t.Errorf("%s has %v probe targets, its jobs' rows %d", m.Member, m.Probe, probe[m.Member])
		}
	}
	jobs := map[string]JobSpread{}
	for _, j := range sp.Jobs {
		jobs[j.Job] = j
	}
	if j := jobs["websites"]; j.Collector != "demo" || j.Targets != 6 || len(j.Sources) != 0 || j.Sources == nil {
		t.Errorf("websites is %+v", j)
	}
	if j := jobs["pods"]; j.Targets != 3 || !slices.Equal(j.Sources, []JobSource{{Name: "pod-files", Type: "file_sd", Result: "ok"}}) {
		t.Errorf("pods is %+v", j)
	}
	if j := jobs["legacy"]; j.Targets != 1 || !slices.Equal(j.Sources, []JobSource{{Name: "inventory", Type: "file_sd", Result: "failing"}}) {
		t.Errorf("legacy is %+v", j)
	}
	// One target over three members is as even as it can be.
	if j := jobs["legacy"]; j.Uneven || j.Max != 1 || j.Imbalance != 3 {
		t.Errorf("legacy is marked uneven: %+v", j)
	}

	page := ask(h, "/", "").Body.String()
	requireIn(t, page,
		`<thead><tr><th>Member</th><th class="num">Static</th><th class="num">Draining</th><th class="num">Probe</th><th class="num">Total</th><th>Share</th></tr></thead>`,
		"<h2>Probe jobs</h2>", `<table class="spread sortable" id="probe-jobs">`,
		`<th class="num" aria-sort="none" data-sort="num"><button type="button">Imbalance</button></th>`,
		`<a href="?job=websites" title="the targets of the probe job websites"><strong>websites</strong></a>`,
		"<tr data-order=\"2\" data-match=\"pods\ndemo\npod-files\">",
		`<span class="source">pod-files <span class="good">ok</span></span>`,
		`<span class="source">inventory <span class="bad">failing</span></span>`,
		`<td class="sources"><span class="muted">listed</span></td>`,
		`<input id="jobs-filter" type="search"`, `<form class="filter jobs" id="jobs" role="search" hidden>`,
		// The script puts the order and the filter back on the table a
		// refresh swaps in, and the caret in the filter being typed in.
		"      filterRows();\n      arrangeJobs();\n", `$("jobs-filter").setSelectionRange(caret[0], caret[1]);`,
		`table.sortable th.num button::before { content: "\2195 ";`)
	if strings.Contains(page, `title="probe job `) {
		t.Error("the targets table still has a column of each probe job")
	}
	for _, j := range sp.Jobs {
		mark := fmt.Sprintf(`<span class="bad" title="Max is further above Avg than hashing puts a member&#39;s targets by chance">%s</span>`, imbalance(j.Imbalance))
		_, row, _ := strings.Cut(page, `<a href="?job=`+j.Job+`"`)
		row, _, _ = strings.Cut(row, "</tr>")
		if strings.Contains(row, mark) != j.Uneven {
			t.Errorf("%s, uneven %v, has the row %s", j.Job, j.Uneven, row)
		}
	}
}

// A probe job's view, at /?job=<name>, as the JSON at
// /v1/status?job=<name>, shows the job: its collector, its labels, its
// params with a credential withheld, its sources, the targets each member
// it is routed among has, the most first, as the spread counts them, and
// each target, credentials withheld, with the member service discovery
// routes it to and the labels its source gave it, cut to a filter on the
// allocator's side. A job the file does not have is a page that says so,
// and the JSON's 404; no name is 400.
func TestTheJobViewListsItsTargets(t *testing.T) {
	a := jobAllocator(t)
	h := a.Handler("")
	s := statusOf(t, h, "")

	v := jobViewOf(t, h, "job=websites")
	if v.Job != "websites" || v.Partial || v.Limit != MemberViewLimit || v.ShardKey != ShardKeyTarget || v.Spread == nil || v.Spread.Collector != "demo" ||
		len(v.Labels) != 1 || v.Labels["team"] != "web" || v.Params["timeout"] != "10s" || v.Params["api_key"] != "<redacted>" || len(v.Sources) != 0 {
		t.Fatalf("the view is %+v", v)
	}
	counts := map[string]int{}
	for _, m := range s.Spread.Jobs[0].Members {
		counts[m.Member] = m.Targets
	}
	for i, m := range v.Spread.Members {
		if m.Targets != counts[m.Member] || (i > 0 && v.Spread.Members[i-1].Targets < m.Targets) {
			t.Errorf("the members are %+v, the spread's %v", v.Spread.Members, counts)
		}
	}
	groups, err := a.SD("websites")
	if err != nil {
		t.Fatal(err)
	}
	routed := map[string]string{}
	for _, g := range groups {
		routed[g.Labels["__param_target"]] = g.Labels[MemberMetaLabel]
	}
	if v.Targets.Total != 6 || v.Targets.Matching != 6 || len(v.Targets.List) != 6 {
		t.Fatalf("the targets are %+v", v.Targets)
	}
	for i, target := range v.Targets.List {
		if target.Member != routed[target.Target] || target.Labels != nil || (i > 0 && v.Targets.List[i-1].Target > target.Target) {
			t.Errorf("the targets are %+v, service discovery routes %v", v.Targets.List, routed)
		}
	}

	pods := jobViewOf(t, h, "job=pods")
	if len(pods.Sources) != 1 || pods.Sources[0].Name != "pod-files" || pods.Sources[0].Result != "ok" || pods.Targets.Total != 3 {
		t.Fatalf("pods is %+v", pods)
	}
	for _, target := range pods.Targets.List {
		// team is the job's own, which the source's gives way to.
		if len(target.Labels) != 1 || target.Labels["namespace"] != "shop" || target.Member == "" {
			t.Errorf("the discovered target is %+v", target)
		}
	}
	apis := ask(h, StatusPath+"?job=apis", "").Body.String()
	if strings.Contains(apis, "sekret") || !strings.Contains(apis, "access_token=\\u003credacted\\u003e") {
		t.Errorf("the targets are not shown redacted:\n%s", apis)
	}

	rec := ask(h, "/?job=websites", "")
	page := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("the view answered %d %v", rec.Code, rec.Header())
	}
	requireIn(t, page, "<title>Probe job websites · pue-allocator</title>", `<a class="home" href="./">`, `<p class="back"><a href="./">← Allocator status</a></p>`,
		`<h1>Probe job <span class="member-id">websites</span></h1>`, `<a href="v1/status?job=websites">JSON</a>`,
		`<input type="hidden" name="job" value="websites">`, `<label for="target-filter">Filter: target, member or label</label>`,
		"looks through every target of the job, beyond the 1000 a list shows. Both match a target, the member it is routed to and the labels shown.",
		`<tr><th>Collector</th><td>demo</td></tr>`, `<span class="label">team=web</span>`, `<span class="label">api_key=&lt;redacted&gt;</span>`, `<span class="label">timeout=10s</span>`,
		"none: the probe targets file lists every target", `<table class="spread job-members">`, `<table class="targets" id="job-targets">`,
		fmt.Sprintf(`<a href="?member=%s">%s</a>`, v.Targets.List[0].Member, v.Targets.List[0].Member), "Targets <span class=\"count\">6</span>")
	if strings.Contains(page, "s3cr3t") || strings.Contains(page, "gone=") {
		t.Error("the page shows the credential param, or a label written empty")
	}
	requireIn(t, ask(h, "/?job=legacy", "").Body.String(), `<strong>inventory</strong> <span class="muted">file_sd</span> <span class="bad">failing</span>`)
	fragment := ask(h, "/?job=websites&format=fragment", "").Body.String()
	if strings.Contains(fragment, "<html") || strings.Contains(fragment, `id="target-filter"`) || !strings.Contains(fragment, `id="job-targets"`) {
		t.Errorf("the fragment is not the view's content alone:\n%.400s", fragment)
	}

	// The filter, in any case, cuts the list on the allocator's side.
	member := v.Targets.List[0].Member
	f := jobViewOf(t, h, "job=websites&filter="+strings.ToUpper(member))
	if f.Targets.Total != 6 || f.Targets.Matching != counts[member] || len(f.Targets.List) != counts[member] {
		t.Errorf("the filter %s lists %+v", member, f.Targets)
	}
	if f := jobViewOf(t, h, "job=pods&filter=namespace=shop"); f.Targets.Matching != 3 {
		t.Errorf("the filter namespace=shop matches %d", f.Targets.Matching)
	}

	for path, code := range map[string]int{"/?job=nope": http.StatusOK, StatusPath + "?job=nope": http.StatusNotFound, "/?job=": http.StatusBadRequest, StatusPath + "?job=": http.StatusBadRequest} {
		if rec := ask(h, path, ""); rec.Code != code {
			t.Errorf("%s answered %d, want %d", path, rec.Code, code)
		}
	}
	gone := ask(h, "/?job=nope", "").Body.String()
	requireIn(t, gone, `<p class="notice" id="unknown-job">The probe targets file has no probe job <strong>nope</strong>`,
		`<form class="filter" id="filter" method="get" role="search" hidden>`, `content.querySelector("#unknown-member, #unknown-job")`)
	if rec := ask(h, "/?job=nope&format=fragment", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="unknown-job"`) {
		t.Errorf("the fragment of an unknown job answered %d", rec.Code)
	}

	// A member's view links each probe job to its view.
	requireIn(t, ask(h, "/?member="+member, "").Body.String(), `<a href="?job=websites" title="the probe job websites">websites</a>`, `<span class="v"><a href="?job=websites">websites</a></span>`)
}

// A probe job's list is bounded as a member's is: the first
// MemberViewLimit targets are listed, the view says how many more there
// are, and the filter looks through them all.
func TestAJobViewIsBounded(t *testing.T) {
	var probes strings.Builder
	probes.WriteString("probe_jobs:\n  - name: many\n    collector: demo\n    targets:\n")
	for i := range MemberViewLimit + 5 {
		fmt.Fprintf(&probes, "      - https://h%04d.example/\n", i)
	}
	a, _ := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, "", probes.String())), "r1", "r2")
	t.Cleanup(a.BeginShutdown)
	h := a.Handler("")
	v := jobViewOf(t, h, "job=many")
	if v.Targets.Total != MemberViewLimit+5 || len(v.Targets.List) != MemberViewLimit {
		t.Errorf("the view lists %d of %d", len(v.Targets.List), v.Targets.Total)
	}
	requireIn(t, ask(h, "/?job=many", "").Body.String(), "Only the first 1000 are listed</span>; 5 more are left out.")
	if f := jobViewOf(t, h, "job=many&filter=h1004"); f.Targets.Matching != 1 || len(f.Targets.List) != 1 {
		t.Errorf("the filter finds %+v", f.Targets)
	}
}

// Under --peers.leader-only a follower leaves a probe job's view to the
// leader, as it leaves it the members: the page names the leader, and the
// JSON is partial.
func TestALeaderOnlyFollowerLeavesTheJobViewToTheLeader(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	leaderOnly(leader, follower)
	for _, c := range []*peerInstance{leader, follower} {
		if _, err := c.a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r1", Advertise: "r1:9115"})); err != nil {
			t.Fatal(err)
		}
	}
	job := leader.a.inputs.Probes.Jobs[0].Name
	v := jobViewOf(t, follower.a.Handler(""), "job="+job)
	if !v.Partial || v.Leader != leader.addr.String() || v.Spread != nil || v.Targets != nil {
		t.Errorf("the follower's view is %+v", v)
	}
	rec := ask(follower.a.Handler(""), "/?job="+job, "")
	page := rec.Body.String()
	requireIn(t, page, `<p class="notice">`, "follows the leader, <strong>"+leader.addr.String()+"</strong>", `<code class="flag">--peers.leader-only</code>`, "this job's at <code>/?job=…</code> there")
	if rec.Code != http.StatusOK || strings.Contains(page, `id="target-filter"`) || strings.Contains(page, `id="job-targets"`) {
		t.Errorf("the follower's view answered %d with the job's targets", rec.Code)
	}
	if lv := jobViewOf(t, leader.a.Handler(""), "job="+job); lv.Partial || lv.Spread == nil || lv.Targets == nil {
		t.Errorf("the leader's view is %+v", lv)
	}
}
