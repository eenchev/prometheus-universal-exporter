//go:build !select_request_types || request_type_http

package allocator

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// memberViewOf is the member view the JSON gives for path's query.
func memberViewOf(t *testing.T, h http.Handler, query, header string) MemberView {
	t.Helper()
	rec := ask(h, StatusPath+"?"+query, header)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%s?%s answered %d: %s", StatusPath, query, rec.Code, rec.Body)
	}
	var v MemberView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A member's view lists the static targets it owns, those it keeps
// draining with the member each moved to, and the probe targets routed to
// it by job, as many as the spread counts for it; every target is shown
// with its credentials withheld. The status page links each member to its
// view, which has its way back, its filter and its JSON, needs the token
// as the status page does, and is cut to a filter on the allocator's side.
func TestTheMemberViewListsItsTargets(t *testing.T) {
	var static strings.Builder
	static.WriteString("interval: 30s\ntargets:\n")
	for i := range 40 {
		target := fmt.Sprintf("http://t%02d.example/metrics", i)
		if i%4 == 0 {
			target = fmt.Sprintf("https://scraper:hunter2@t%02d.example/metrics?token=tok%02d&tenant=a", i, i)
		}
		fmt.Fprintf(&static, "  - name: t%02d\n    collector: demo\n    target: %q\n    labels: {team: team%d, empty: \"\"}\n", i, target, i%3)
	}
	probes := `probe_jobs:
  - name: websites
    collector: demo
    targets: [https://a.example/health, https://b.example/health, https://c.example/health, https://d.example/health]
  - name: apis
    collector: demo
    targets: ["https://api1.example/v1?access_token=sekret1", "https://api2.example/v1?access_token=sekret2", "https://api3.example/v1?access_token=sekret3"]
`
	p := writeInputs(t, testConfig, static.String(), probes)
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1", "r2")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2")
	// r3 joins: the targets it takes from r1 and r2 drain there.
	keepBeating(t, a, clock, 30*time.Second, "r1", "r2", "r3")
	h := a.Handler("tok")
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("me:tok"))

	for _, path := range []string{"/?member=r1", StatusPath + "?member=r1"} {
		if rec := ask(h, path, ""); rec.Code != http.StatusUnauthorized || !strings.Contains(strings.Join(rec.Header().Values("WWW-Authenticate"), " "), "Basic") {
			t.Errorf("%s without the token answered %d %v", path, rec.Code, rec.Header())
		}
	}
	s := statusOf(t, h, basic)
	var spread SpreadMember
	for _, m := range s.Spread.Members {
		if m.Member == "r1" {
			spread = m
		}
	}
	v := memberViewOf(t, h, "member=r1", basic)
	if v.Member != "r1" || v.Partial || v.Limit != MemberViewLimit || v.Status == nil || v.Status.ID != "r1" || !v.Status.InList {
		t.Fatalf("the view is %+v", v)
	}
	if float64(v.Static.Total) != spread.Static || float64(v.Draining.Total) != spread.Draining || float64(v.Probes.Total) != spread.Probe ||
		v.Static.Total == 0 || v.Draining.Total == 0 || v.Probes.Total == 0 {
		t.Errorf("the view counts %d static, %d draining and %d probe targets, the spread %+v", v.Static.Total, v.Draining.Total, v.Probes.Total, spread.SpreadCounts)
	}
	var want []string
	for i := range 40 {
		if owner, _ := allocapi.Owner([]string{"r1", "r2", "r3"}, fmt.Sprintf("t%02d", i)); owner == "r1" {
			want = append(want, fmt.Sprintf("t%02d", i))
		}
	}
	var got []string
	for _, st := range v.Static.List {
		got = append(got, st.Name)
		if st.Collector != "demo" || st.Interval != "30s" || st.Labels["team"] == "" || len(st.Labels) != 1 || st.MovedTo != "" {
			t.Errorf("the owned target is %+v", st)
		}
	}
	if !slices.Equal(got, want) || v.Static.Matching != len(want) {
		t.Errorf("r1 owns %v (%d matching), want %v", got, v.Static.Matching, want)
	}
	for _, st := range v.Draining.List {
		if owner, _ := allocapi.Owner([]string{"r1", "r2", "r3"}, st.Name); st.MovedTo != "r3" || owner != "r3" || st.DrainsForSeconds <= 0 || st.DrainsForSeconds > 30 {
			t.Errorf("the draining target is %+v", st)
		}
	}
	if len(v.Probes.Jobs) != 2 || v.Probes.Jobs[0].Job != "websites" || v.Probes.Jobs[1].Job != "apis" ||
		float64(v.Probes.Jobs[0].Targets) != spread.ProbeJobs["websites"] || float64(v.Probes.Jobs[1].Targets) != spread.ProbeJobs["apis"] {
		t.Errorf("the probe jobs are %+v, the spread's %v", v.Probes.Jobs, spread.ProbeJobs)
	}
	for i, pt := range v.Probes.List {
		if pt.Collector != "demo" || (i > 0 && v.Probes.List[i-1].Job == pt.Job && v.Probes.List[i-1].Target > pt.Target) {
			t.Errorf("the probe targets are out of order or wrong: %+v", v.Probes.List)
		}
	}

	rec := ask(h, "/?member=r1", basic)
	page := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("the view answered %d %v", rec.Code, rec.Header())
	}
	requireIn(t, page, "<title>Member r1 · pue-allocator</title>", `<a class="home" href="./">`, `<p class="back"><a href="./">← Allocator status</a></p>`,
		`<h1>Member <span class="member-id">r1</span></h1>`, `<a href="v1/status?member=r1">JSON</a>`,
		`<input type="hidden" name="member" value="r1">`, `<input id="target-filter" name="filter" type="search"`,
		`<table class="targets" id="owned">`, `<table class="targets" id="draining">`, `<table class="targets" id="probes">`,
		`<a href="?member=r3">r3</a>`, `<span class="label">team=`, "https://redacted:redacted@t", "/metrics?token=&lt;redacted&gt;&amp;tenant=a",
		fmt.Sprintf("Static targets owned <span class=\"count\">%d</span>", v.Static.Total))
	body := ask(h, StatusPath+"?member=r1", basic).Body.String()
	for _, secret := range []string{"hunter2", "tok00", "sekret"} {
		if strings.Contains(page, secret) || strings.Contains(body, secret) {
			t.Errorf("the view shows the credential %q", secret)
		}
	}
	if !strings.Contains(body, "access_token=\\u003credacted\\u003e") {
		t.Errorf("the probe targets are not shown redacted:\n%s", body)
	}
	fragment := ask(h, "/?member=r1&format=fragment", basic).Body.String()
	if strings.Contains(fragment, "<html") || strings.Contains(fragment, `id="target-filter"`) || !strings.Contains(fragment, `id="owned"`) {
		t.Errorf("the fragment is not the view's content alone:\n%.400s", fragment)
	}
	requireIn(t, ask(h, "/", basic).Body.String(), `<a href="?member=r1" title="the targets of r1"><strong>r1</strong></a>`)

	// The filter, in any case, cuts every list on the allocator's side.
	f := memberViewOf(t, h, "member=r1&filter=T0", basic)
	for _, st := range append(f.Static.List, f.Draining.List...) {
		if !strings.HasPrefix(st.Name, "t0") {
			t.Errorf("the filter T0 let %s in", st.Name)
		}
	}
	if f.Filter != "T0" || f.Static.Total != v.Static.Total || f.Static.Matching != len(f.Static.List) || f.Static.Matching >= v.Static.Total || f.Probes.Matching != 0 {
		t.Errorf("filtered: static %d of %d listed, %d matching; probes %d matching", len(f.Static.List), f.Static.Total, f.Static.Matching, f.Probes.Matching)
	}
	if f := memberViewOf(t, h, "member=r1&filter=apis", basic); f.Probes.Matching != v.Probes.Jobs[1].Targets || f.Static.Matching != 0 {
		t.Errorf("the filter apis matches %d probe and %d static targets", f.Probes.Matching, f.Static.Matching)
	}
	requireIn(t, ask(h, "/?member=r1&filter=nothing-holds-this", basic).Body.String(), `None of the `, `holds “nothing-holds-this”.`)

	for path, code := range map[string]int{"/?member=nobody": http.StatusOK, StatusPath + "?member=nobody": http.StatusNotFound, "/?member=": http.StatusBadRequest, StatusPath + "?member=": http.StatusBadRequest} {
		if rec := ask(h, path, basic); rec.Code != code {
			t.Errorf("%s answered %d, want %d", path, rec.Code, code)
		}
	}
	requireIn(t, ask(h, "/?member=nobody", basic).Body.String(), "The allocator knows no member <strong>nobody</strong>", `<p class="back"><a href="./">`)
}

// A member's lists are bounded: the first MemberViewLimit of each are
// listed, and the view says how many more there are; the filter looks
// through them all.
func TestAMemberViewIsBounded(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(MemberViewLimit+205), "")
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1")
	keepBeating(t, a, clock, 20*time.Second, "r1")
	h := a.Handler("")
	v := memberViewOf(t, h, "member=r1", "")
	if v.Static.Total != MemberViewLimit+205 || v.Static.Matching != MemberViewLimit+205 || len(v.Static.List) != MemberViewLimit {
		t.Errorf("the view has %d of %d static targets, %d matching", len(v.Static.List), v.Static.Total, v.Static.Matching)
	}
	requireIn(t, ask(h, "/?member=r1", "").Body.String(), "Only the first 1000 are listed</span>; 205 more are left out.")
	// t120 to t129 and t1200 to t1204, the last beyond the bound.
	f := memberViewOf(t, h, "member=r1&filter=t12", "")
	if f.Static.Matching != 15 || len(f.Static.List) != 15 || !slices.ContainsFunc(f.Static.List, func(st MemberStaticTarget) bool { return st.Name == "t1204" }) {
		t.Errorf("the filter t12 lists %d of %d matching", len(f.Static.List), f.Static.Matching)
	}
}

// Under --peers.leader-only a follower leaves the member views to the
// leader, as it leaves it the members: the page names the leader, and the
// JSON is partial; the leader serves the view.
func TestALeaderOnlyFollowerLeavesTheMemberViewToTheLeader(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	leaderOnly(leader, follower)
	for _, c := range []*peerCopy{leader, follower} {
		if _, err := c.a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r1", Advertise: "r1:9115"})); err != nil {
			t.Fatal(err)
		}
	}
	v := memberViewOf(t, follower.a.Handler(""), "member=r1", "")
	if !v.Partial || v.Leader != leader.addr.String() || v.Status != nil || v.Static != nil || v.Probes != nil {
		t.Errorf("the follower's view is %+v", v)
	}
	rec := ask(follower.a.Handler(""), "/?member=r1", "")
	page := rec.Body.String()
	requireIn(t, page, `<p class="notice">`, "follows the leader, <strong>"+leader.addr.String()+"</strong>", `<code class="flag">--peers.leader-only</code>`)
	if rec.Code != http.StatusOK || strings.Contains(page, `id="target-filter"`) || strings.Contains(page, "Static targets owned") {
		t.Errorf("the follower's view answered %d with the member's targets", rec.Code)
	}
	if lv := memberViewOf(t, leader.a.Handler(""), "member=r1", ""); lv.Partial || lv.Status == nil || lv.Static == nil {
		t.Errorf("the leader's view is %+v", lv)
	}
}

// A member's view left open, refreshing itself, of a member that has gone
// shows that it has: the fragment of a member the allocator no longer knows
// is answered 200, as the page is, with the notice in place of the lists,
// so the refresh swaps it in rather than failing over the gone member's
// targets; the page hides its filter then, and the JSON answers 404.
func TestAMemberViewOfAGoneMemberSaysSo(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(10), "")
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1", "r2")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2")
	h := a.Handler("")
	if rec := ask(h, "/?member=r2&format=fragment", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="owned"`) {
		t.Fatalf("r2's fragment answered %d: %s", rec.Code, rec.Body)
	}
	// r2 stops: it leaves the live members after the TTL, the member list
	// the settle time after, and is forgotten then.
	keepBeating(t, a, clock, allocapi.MemberTTL(allocapi.DefaultHeartbeatInterval)+20*time.Second, "r1")
	rec := ask(h, "/?member=r2&format=fragment", "")
	fragment := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(fragment, `id="owned"`) || strings.Contains(fragment, "<html") {
		t.Errorf("the fragment of the gone r2 answered %d: %s", rec.Code, fragment)
	}
	requireIn(t, fragment, `<p class="meta" id="as-of">`, `<p class="notice" id="unknown-member">The allocator knows no member <strong>r2</strong>`)
	page := ask(h, "/?member=r2", "")
	if page.Code != http.StatusOK {
		t.Errorf("the page of the gone r2 answered %d", page.Code)
	}
	requireIn(t, page.Body.String(), `<form class="filter" id="filter" method="get" role="search" hidden>`, `$("filter").hidden = content.querySelector("#unknown-member") !== null;`)
	if rec := ask(h, StatusPath+"?member=r2", ""); rec.Code != http.StatusNotFound {
		t.Errorf("the JSON of the gone r2 answered %d, want 404", rec.Code)
	}
	if page := ask(h, "/?member=r1", "").Body.String(); !strings.Contains(page, `<form class="filter" id="filter" method="get" role="search">`) {
		t.Error("r1's view has no filter shown")
	}
}

// The filter box matches what the allocator's filter matches: each row
// carries those texts, a line each, in data-match, which the box matches
// in place of the row's text, so text the allocator does not match, as an
// interval, or running across two cells, matches no row in the box either.
// A filter of spaces alone is no filter.
func TestTheFilterBoxMatchesWhatTheAllocatorMatches(t *testing.T) {
	static := "interval: 30s\ntargets:\n  - name: alpha\n    collector: demo\n    target: http://a.example/m\n    labels: {team: ops, zone: eu}\n"
	probes := "probe_jobs:\n  - name: websites\n    collector: demo\n    targets: [https://w.example/health]\n"
	p := writeInputs(t, testConfig, static, probes)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	h := a.Handler("")
	page := ask(h, "/?member=r1", "").Body.String()
	requireIn(t, page,
		"<tr data-match=\"alpha\nhttp://a.example/m\ndemo\n\nteam=ops\nzone=eu\">",
		"<tr data-match=\"https://w.example/health\nwebsites\ndemo\">",
		`(row.dataset.match || "").toLowerCase().includes(needle)`,
		"Both match a target's name, target, collector, labels, probe job and the member it moved to.")
	if strings.Contains(page, "row.textContent") {
		t.Error("the filter box matches a row's text")
	}
	// What the allocator matches, the box matches: of each, the texts the
	// row's data-match holds.
	for _, q := range []string{"30s", "alphahttp", "demoteam"} {
		if v := memberViewOf(t, h, "member=r1&filter="+q, ""); v.Static.Matching != 0 {
			t.Errorf("the allocator's filter %q matches %d static targets", q, v.Static.Matching)
		}
	}
	for _, q := range []string{"%20", "%20%20"} {
		v := memberViewOf(t, h, "member=r1&filter="+q, "")
		if v.Filter != "" || v.Static.Matching != 1 || v.Probes.Matching != 1 {
			t.Errorf("?filter=%s: filter %q, %d and %d matching", q, v.Filter, v.Static.Matching, v.Probes.Matching)
		}
		if page := ask(h, "/?member=r1&filter="+q, "").Body.String(); strings.Contains(page, "Only those holding") || strings.Contains(page, `value=" `) {
			t.Errorf("?filter=%s is echoed as a filter", q)
		}
	}
}

// On a phone a named cell is a grid of its name and its value on one
// baseline, the value's first: a label is an inline flex box, whose
// baseline is its first line's, so a label long enough to wrap keeps the
// cell's name beside its first line; an inline block's baseline is its
// last line's. Measured in Chromium at 320px, the name of a cell whose
// first label wraps is beside its first line with it and beside its second
// without; at 1280px nothing moves but a label that wraps there too.
func TestAWrappedLabelKeepsTheNameOnItsFirstLine(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(4), "")
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1")
	keepBeating(t, a, clock, 20*time.Second, "r1")
	page := ask(a.Handler(""), "/?member=r1", "").Body.String()
	requireIn(t, page, `span.label { display: inline-flex;`, `table.targets td[data-label] { display: grid; grid-template-columns: 92px minmax(0, 1fr); align-items: baseline; }`)
	if strings.Contains(page, `span.label { display: inline-block;`) {
		t.Error("a label is an inline block, whose baseline is its last line's")
	}
}
