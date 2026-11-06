//go:build !select_request_types || request_type_http

package allocator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// ask asks h for path, as GET with the header given, "" for none.
func ask(h http.Handler, path, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func statusOf(t *testing.T, h http.Handler, header string) Status {
	t.Helper()
	rec := ask(h, StatusPath, header)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s answered %d: %s", StatusPath, rec.Code, rec.Body)
	}
	var s Status
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The status, as JSON and as the page, shows the instance, the members and
// how the targets are spread over them, counted as /metrics counts them,
// with the totals, the average per member and the imbalance; every value
// the page shows is escaped.
func TestTheStatusShowsTheMembersAndTheSpread(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(30), testProbes)
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t),
		Address: ":9116", Version: "9.9.9", Revision: "abc123"})
	t.Cleanup(a.BeginShutdown)
	odd := `r<img src=x onerror=alert(1)>`
	members := []string{"r1", "r2", odd}
	beat(t, a, members...)
	keepBeating(t, a, clock, 20*time.Second, members...)
	// r1 reports the bundle it is to have, r2 none.
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r1", Advertise: "r1:9115", Generation: a.Bundle("r1").Generation})); err != nil {
		t.Fatal(err)
	}
	h := a.Handler("")
	s := statusOf(t, h, "")
	if s.Instance.Address != ":9116" || s.Instance.Version != "9.9.9" || s.Instance.Revision != "abc123" || !s.Instance.Ready || s.Instance.UptimeSeconds != 20 || s.Peers != nil {
		t.Errorf("the instance is %+v, peers %v", s.Instance, s.Peers)
	}
	if !s.Members.Formed || s.Members.InList != 3 || s.Members.Live != 3 || len(s.Members.List) != 3 {
		t.Fatalf("the members are %+v", s.Members)
	}
	r1 := s.Members.List[0]
	if r1.ID != "r1" || r1.Address != "r1:9115" || !r1.UpToDate || r1.Current != a.Bundle("r1").Generation || !r1.InList || r1.LastHeartbeatAgeSeconds != 0 {
		t.Errorf("r1 is %+v", r1)
	}
	if r2 := s.Members.List[1]; r2.UpToDate || r2.Generation != "" || r2.Current == "" {
		t.Errorf("r2, which reports no bundle, is %+v", r2)
	}

	metrics := metricsOf(t, a)
	sp := s.Spread
	if len(sp.Members) != 3 || len(sp.Jobs) != 1 || sp.Jobs[0].Job != "websites" {
		t.Fatalf("the spread is %+v", sp)
	}
	most := 0.0
	for _, m := range sp.Members {
		for kind, n := range map[string]float64{"static": m.Static, "probe": m.Probe} {
			line := fmt.Sprintf("pue_allocator_assigned{kind=%q,member=%q} %d\n", kind, escape(m.Member), int(n))
			if !strings.Contains(metrics, line) {
				t.Errorf("the status has %s, /metrics not:\n%s", line, metrics)
			}
		}
		if m.Total != m.Static+m.Draining+m.Probe {
			t.Errorf("%s: %+v", m.Member, m.SpreadCounts)
		}
		most = max(most, m.Total)
	}
	if sp.Total.Static != 30 || sp.Total.Probe != 2 || sp.Total.Total != 32 || sp.Jobs[0].Targets != 2 {
		t.Errorf("the total is %+v", sp.Total)
	}
	if sp.Average.Total != 32.0/3 || sp.Imbalance.Total != most/(32.0/3) {
		t.Errorf("the average is %+v and the imbalance %+v", sp.Average, sp.Imbalance)
	}
	if len(s.Sources) != 0 {
		t.Errorf("the sources are %+v", s.Sources)
	}

	rec := ask(h, "/", "")
	page := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("/ answered %d %v", rec.Code, rec.Header())
	}
	requireIn(t, page,
		`<link rel="icon" type="image/svg+xml" href="data:image/svg+xml;base64,`,
		`<script id="theme-head">`, `<select id="theme">`,
		`<option value="off" data-seconds="0" selected>Off</option>`,
		`<td class="num"><strong>`, `class="seg-static"`, `<i class="avg"`,
		`r&lt;img src=x onerror=alert(1)&gt;`, "Version 9.9.9, revision abc123",
		"30 static targets", "Imbalance, most ÷ average")
	if strings.Contains(page, odd) {
		t.Error("the page shows a member's id unescaped")
	}
	fragment := ask(h, "/?format=fragment", "").Body.String()
	if strings.Contains(fragment, "<html") || strings.Contains(fragment, `<header class="site">`) || !strings.Contains(fragment, `id="as-of"`) {
		t.Errorf("the fragment is not the content alone:\n%.400s", fragment)
	}
	if rec := ask(h, "/?refresh=30s", ""); !strings.Contains(rec.Body.String(), `<option value="30s" data-seconds="30" selected>`) {
		t.Error("?refresh=30s is not the interval chosen")
	}
	if rec := ask(h, "/?format=json", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("?format=json answered %d", rec.Code)
	}
}

// The status says the heartbeat interval and the member TTL of it, and
// marks a live member late once its last heartbeat is more than one and a
// half of the configured intervals old, as the page shows.
func TestTheStatusMarksAMemberLateByTheConfiguredInterval(t *testing.T) {
	clock := newFakeClock()
	a := New(nil, Options{HeartbeatInterval: 4 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1", "r2")
	clock.Advance(6 * time.Second)
	beat(t, a, "r1")
	h := a.Handler("")
	if s := statusOf(t, h, ""); s.Members.List[1].Late {
		t.Errorf("r2 is late at 1.5 heartbeat intervals: %+v", s.Members.List[1])
	}
	clock.Advance(time.Second)
	s := statusOf(t, h, "")
	if s.Members.HeartbeatIntervalSeconds != 4 || s.Members.MemberTTLSeconds != 12 {
		t.Errorf("the members say a heartbeat interval of %v and a member TTL of %v, want 4 and 12", s.Members.HeartbeatIntervalSeconds, s.Members.MemberTTLSeconds)
	}
	if r1, r2 := s.Members.List[0], s.Members.List[1]; r1.Late || !r2.Late || !r2.Live {
		t.Errorf("want r2 alone late and still live: %+v, %+v", r1, r2)
	}
	requireIn(t, ask(h, "/", "").Body.String(), "The replicas heartbeat every 4s; a member not heard from for 12s stops being live.",
		`<span class="bad" title="a heartbeat is overdue">7s ago</span>`)
}

// metricsOf is a's /metrics text.
func metricsOf(t *testing.T, a *Allocator) string {
	t.Helper()
	var b strings.Builder
	if err := a.WriteMetrics(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func requireIn(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("missing %q", w)
		}
	}
}

// A browser cannot send a bearer token: the page, the status and /metrics
// take the token as Basic auth's password, of any user name, and their 401
// asks for it, so the browser prompts. No other endpoint takes it, nor is
// the token taken from the URL.
func TestTheStatusPageTakesTheTokenAsBasicAuth(t *testing.T) {
	a, _ := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, staticTargets(3), testProbes)), "r1")
	h := a.Handler(peerToken)
	basic := func(user, password string) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth(user, password)
		return req.Header.Get("Authorization")
	}
	for _, path := range []string{"/", StatusPath, "/metrics"} {
		rec := ask(h, path, "")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(strings.Join(rec.Header().Values("WWW-Authenticate"), "\n"), `Basic realm="pue-allocator"`) {
			t.Errorf("%s without the token answered %d, %v", path, rec.Code, rec.Header().Values("WWW-Authenticate"))
		}
		for _, user := range []string{"pue", "", "anyone"} {
			if rec := ask(h, path, basic(user, peerToken)); rec.Code != http.StatusOK {
				t.Errorf("%s with the token as user %q's password answered %d", path, user, rec.Code)
			}
		}
		if rec := ask(h, path, basic("pue", "wrong")); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with another password answered %d", path, rec.Code)
		}
		if rec := ask(h, path, "Bearer "+peerToken); rec.Code != http.StatusOK {
			t.Errorf("%s with the bearer token answered %d", path, rec.Code)
		}
		if rec := ask(h, path+"?token="+peerToken, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s took the token from the URL: %d", path, rec.Code)
		}
	}
	for _, path := range []string{allocapi.BundlePath + "?member=r1", allocapi.SDPathPrefix + "websites", PeerPath, SnapshotPath} {
		rec := ask(h, path, basic("pue", peerToken))
		if rec.Code != http.StatusUnauthorized || strings.Contains(strings.Join(rec.Header().Values("WWW-Authenticate"), "\n"), "Basic") {
			t.Errorf("%s with Basic auth answered %d, %v", path, rec.Code, rec.Header().Values("WWW-Authenticate"))
		}
	}
	req := httptest.NewRequest(http.MethodPost, allocapi.HeartbeatPath, strings.NewReader(`{"member":"evil","advertise":"evil:1","instance":"x","sequence":1}`))
	req.SetBasicAuth("pue", peerToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a heartbeat with Basic auth answered %d", rec.Code)
	}
	for _, path := range []string{"/healthz", "/ready"} {
		if rec := ask(h, path, ""); rec.Code != http.StatusOK {
			t.Errorf("%s answered %d", path, rec.Code)
		}
	}
}

// Among peers the status shows the leader, every instance with its liveness,
// its last check and its failures, the snapshot's age and whether the instance
// holds the leader's targets, and each source's origin: the leader's own
// discovery, the follower's from the snapshot. Its metrics are /metrics'
// lines of the peers' families, pue_allocator_peer_up{peer} among them,
// which goes to 0 for an instance that stops answering.
func TestThePeersStatusAgreesWithTheMetrics(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	ls := statusOf(t, leader.a.Handler(""), "")
	fs := statusOf(t, follower.a.Handler(""), "")
	if ls.Peers.Role != "leader" || ls.Peers.Leader != leader.addr.String() || !ls.Peers.HoldsLeaderState || ls.Instance.Address != leader.addr.String() {
		t.Errorf("the leader's peers are %+v", ls.Peers)
	}
	if fs.Peers.Role != "follower" || fs.Peers.Leader != leader.addr.String() || !fs.Peers.HoldsLeaderState || !fs.Peers.SnapshotTaken || fs.Peers.LiveInstances != 2 {
		t.Errorf("the follower's peers are %+v", fs.Peers)
	}
	if len(fs.Peers.Instances) != 2 || fs.Peers.Instances[0].Address != leader.addr.String() || !fs.Peers.Instances[0].Leader || !fs.Peers.Instances[0].Live ||
		!fs.Peers.Instances[0].Discovering || fs.Peers.Instances[0].LastCheck == "" || !fs.Peers.Instances[1].Self || fs.Peers.Instances[1].Leader {
		t.Errorf("the follower's instances are %+v", fs.Peers.Instances)
	}
	for name, s := range map[string]Status{"leader": ls, "follower": fs} {
		want := map[string]string{"leader": "own", "follower": "snapshot"}[name]
		if len(s.Sources) != 2 {
			t.Fatalf("the %s's sources are %+v", name, s.Sources)
		}
		for _, src := range s.Sources {
			if src.From != want || src.Result != "ok" || src.Targets != 1 || src.LastRefresh == "" || src.Type != "http_sd" {
				t.Errorf("the %s's source is %+v, want it from %s", name, src, want)
			}
		}
	}
	samples := func(c *peerInstance, s Status) {
		t.Helper()
		metrics := c.metrics(t)
		for _, m := range s.Peers.Metrics {
			if !strings.Contains(metrics, m.Series+" "+m.Value+"\n") {
				t.Errorf("the status has %s %s, /metrics not:\n%s", m.Series, m.Value, metrics)
			}
		}
		if len(s.Peers.Metrics) < 8 {
			t.Errorf("the status has %d series of the peers", len(s.Peers.Metrics))
		}
	}
	samples(follower, fs)
	hasLine(t, follower, fmt.Sprintf("pue_allocator_peer_up{peer=%q} 1\n", leader.addr.String()))
	hasLine(t, leader, fmt.Sprintf("pue_allocator_peer_up{peer=%q} 1\n", follower.addr.String()))

	leader.failPeer.Store(true)
	for range DefaultPeerFailureThreshold {
		follower.round()
	}
	hasLine(t, follower, fmt.Sprintf("pue_allocator_peer_up{peer=%q} 0\n", leader.addr.String()))
	fs = statusOf(t, follower.a.Handler(""), "")
	if fs.Peers.Role != "leader" || fs.Peers.Instances[0].Live || fs.Peers.Instances[0].Failures != DefaultPeerFailureThreshold || fs.Peers.CheckFailures != DefaultPeerFailureThreshold {
		t.Errorf("after the leader's checks failed the follower's peers are %+v", fs.Peers)
	}
	samples(follower, fs)
	page := ask(follower.a.Handler(""), "/", "").Body.String()
	requireIn(t, page, "Leader election metrics", "pue_allocator_peer_up{peer=&#34;"+leader.addr.String()+"&#34;}", `<span class="bad">down</span>`)
}

// leaderOnly sets --peers.leader-only on each instance.
func leaderOnly(instances ...*peerInstance) {
	for _, c := range instances {
		c.a.peers.o.LeaderOnly = true
	}
}

// familiesOf are the names of the families in a /metrics text.
func familiesOf(metrics string) map[string]bool {
	out := map[string]bool{}
	for line := range strings.SplitSeq(metrics, "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, _, _ = strings.Cut(name, " ")
			out[name] = true
		}
	}
	return out
}

// With --peers.leader-only only the leader serves the whole of /metrics and
// of the status: a follower's /metrics has the peers' families alone, so
// instances scraped together have one instance's series of the allocation, and
// its status and page leave the members, the spread and the sources out,
// naming the leader. A follower that comes to lead serves the whole.
func TestALeaderOnlyFollowerServesThePeersPartAlone(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	leaderOnly(leader, follower)
	peerFamilies := map[string]bool{"pue_allocator_peers": true, "pue_allocator_peer_up": true, "pue_allocator_is_leader": true, "pue_allocator_leader": true,
		"pue_allocator_leader_changes_total": true, "pue_allocator_snapshot_age_seconds": true, "pue_allocator_peer_failures_total": true}
	lf, ff := familiesOf(leader.metrics(t)), familiesOf(follower.metrics(t))
	for name := range peerFamilies {
		if !lf[name] || !ff[name] {
			t.Errorf("%s: on the leader %v, on the follower %v", name, lf[name], ff[name])
		}
	}
	for name := range ff {
		if !peerFamilies[name] {
			t.Errorf("the follower's /metrics has %s", name)
		}
	}
	for _, name := range []string{"pue_allocator_members", "pue_allocator_assigned", "pue_allocator_discovered_targets", "pue_allocator_generation_info"} {
		if !lf[name] {
			t.Errorf("the leader's /metrics has no %s", name)
		}
	}
	hasLine(t, follower, "pue_allocator_is_leader 0\n")
	hasLine(t, follower, fmt.Sprintf("pue_allocator_leader{peer=%q} 1\n", leader.addr.String()))

	rec := ask(follower.a.Handler(""), StatusPath, "")
	if !strings.Contains(rec.Body.String(), `"partial":true`) || !strings.Contains(rec.Body.String(), `"members":null`) {
		t.Errorf("the follower's status is not partial:\n%s", rec.Body)
	}
	fs := statusOf(t, follower.a.Handler(""), "")
	if !fs.Partial || fs.Members != nil || fs.Spread != nil || fs.Sources != nil || !fs.Peers.LeaderOnly || fs.Peers.Leader != leader.addr.String() {
		t.Errorf("the follower's status is %+v", fs)
	}
	if ls := statusOf(t, leader.a.Handler(""), ""); ls.Partial || ls.Members == nil || ls.Spread == nil || len(ls.Sources) != 2 {
		t.Errorf("the leader's status is %+v", ls)
	}
	page := ask(follower.a.Handler(""), "/", "").Body.String()
	requireIn(t, page, `<p class="notice">`, "follows the leader, <strong>"+leader.addr.String()+"</strong>", `<code class="flag">--peers.leader-only</code>`, "<h2>Peers</h2>", "Leader election metrics")
	for _, full := range []string{"<h2>Members</h2>", "<h2>Targets</h2>", "<h2>Discovery sources</h2>"} {
		if strings.Contains(page, full) {
			t.Errorf("the follower's page has %s", full)
		}
	}
	if page := ask(leader.a.Handler(""), "/", "").Body.String(); strings.Contains(page, `<p class="notice">`) || !strings.Contains(page, "<h2>Targets</h2>") {
		t.Error("the leader's page is not the whole")
	}

	leader.failPeer.Store(true)
	for range DefaultPeerFailureThreshold {
		follower.round()
	}
	if ff := familiesOf(follower.metrics(t)); !ff["pue_allocator_assigned"] || !ff["pue_allocator_peer_up"] {
		t.Error("the follower that came to lead does not serve the whole of /metrics")
	}
	if fs := statusOf(t, follower.a.Handler(""), ""); fs.Partial || fs.Members == nil {
		t.Errorf("the follower that came to lead gives %+v", fs)
	}
}

// visibleText is the text a reader of page sees: its markup, scripts,
// styles and code left out, so a JSON key or a flag shown as code does not
// count.
func visibleText(page string) string {
	for _, el := range []string{"script", "style", "code"} {
		page = regexp.MustCompile(`(?s)<`+el+`\b.*?</`+el+`>`).ReplaceAllString(page, " ")
	}
	return regexp.MustCompile(`(?s)<[^>]*>`).ReplaceAllString(page, " ")
}

// The pages call an allocator process an instance, never a copy, and its
// random process id the instance ID: on the leader's page and fragment,
// on a page with no --peers, on a --peers.leader-only follower's page and
// on the member views of both.
func TestTheAllocatorPagesSayInstance(t *testing.T) {
	alone := New(mustLoad(t, writeInputs(t, testConfig, staticTargets(3), testProbes)), Options{Settle: time.Second, Clock: newFakeClock(), Logger: testutil.QuietLogger(t)})
	t.Cleanup(alone.BeginShutdown)
	leader, follower, _ := leaderAndFollower(t)
	leaderOnly(leader, follower)
	for _, c := range []*peerInstance{leader, follower} {
		if _, err := c.a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r1", Advertise: "r1:9115"})); err != nil {
			t.Fatal(err)
		}
	}
	pages := map[string]string{
		"alone":             ask(alone.Handler(""), "/", "").Body.String(),
		"leader":            ask(leader.a.Handler(""), "/", "").Body.String(),
		"leader's fragment": ask(leader.a.Handler(""), "/?format=fragment", "").Body.String(),
		"follower":          ask(follower.a.Handler(""), "/", "").Body.String(),
		"leader's view":     ask(leader.a.Handler(""), "/?member=r1", "").Body.String(),
		"follower's view":   ask(follower.a.Handler(""), "/?member=r1", "").Body.String(),
	}
	for name, page := range pages {
		if text := strings.ToLower(visibleText(page)); strings.Contains(text, "copy") || strings.Contains(text, "copies") {
			t.Errorf("the %s page says copy:\n%s", name, text)
		}
	}
	for _, name := range []string{"alone", "leader", "leader's fragment", "follower"} {
		requireIn(t, pages[name], "<h2>This instance</h2>", `<tr><th class="wraps">Instance ID</th>`)
	}
	requireIn(t, pages["alone"], "off: this instance asks its discovery sources itself")
	// On a phone the longer label goes on two lines rather than pushing the
	// ID's last character onto a line of its own.
	requireIn(t, pages["alone"], `@media (max-width: 400px) { table.facts th.wraps { white-space: normal; overflow-wrap: normal; width: 1px; } }`)
	requireIn(t, pages["leader"], "<th>Instance</th><th>Role</th>", `<span class="muted">(this instance)</span>`, `<br><span class="muted">ID</span> <code>`,
		"This instance leads: it asks the discovery sources, and the other instances take what it finds.", "2 live instances", "leader</span> of 2 live instances")
	requireIn(t, pages["follower"], `<p class="notice">This instance follows the leader`, "Below are this instance and its peers.", "This instance follows "+leader.addr.String()+":")
	requireIn(t, pages["follower's view"], `<p class="notice">This instance follows the leader`)
	// The bar names the allocator by its full name, on the status page and
	// on a member's view alike.
	requireIn(t, pages["leader"], `<span class="home" aria-current="page">Prometheus Universal Exporter · Target Allocator</span>`)
	requireIn(t, pages["leader's view"], `<a class="home" href="./">Prometheus Universal Exporter · Target Allocator</a>`)
}

// jsonKeys are the keys of every object in the JSON document body, at any
// depth.
func jsonKeys(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	keys := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				keys[k] = true
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(doc)
	return keys
}

// saysCopy matches copy or copies as a word, whatever its case.
var saysCopy = regexp.MustCompile(`(?i)\bcop(y|ies)\b`)

// The allocator's answers call an allocator process an instance, as its
// pages do, and the random id its process takes its id: /v1/status has
// instance.id, instances with an id each and live_instances, /v1/peer and
// /v1/peer/snapshot an id, and no key of them says copy; no HELP of
// /metrics says copy either, alone or among peers.
func TestTheAllocatorsAnswersSayInstance(t *testing.T) {
	alone := New(mustLoad(t, writeInputs(t, testConfig, staticTargets(3), testProbes)), Options{Settle: time.Second, Clock: newFakeClock(), Logger: testutil.QuietLogger(t)})
	t.Cleanup(alone.BeginShutdown)
	leader, follower, _ := leaderAndFollower(t)
	bearer := "Bearer " + peerToken
	idOf := func(c *peerInstance, path string) (any, []byte) {
		t.Helper()
		body := ask(c.a.Handler(peerToken), path, bearer).Body.Bytes()
		var answer map[string]any
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("%s: %v: %s", path, err, body)
		}
		return answer["id"], body
	}
	leaderID, peerBody := idOf(leader, PeerPath)
	followerID, _ := idOf(follower, PeerPath)
	snapshotID, snapshotBody := idOf(leader, SnapshotPath)
	if id, ok := leaderID.(string); !ok || id == "" || followerID == leaderID || snapshotID != leaderID {
		t.Errorf("%s answered %s, the follower's id %v, and %s %s; want an id each, the snapshot's the leader's", PeerPath, peerBody, followerID, SnapshotPath, snapshotBody)
	}
	status := ask(follower.a.Handler(""), StatusPath, "").Body.Bytes()
	var s struct {
		Instance map[string]any `json:"instance"`
		Peers    struct {
			LiveInstances any              `json:"live_instances"`
			Instances     []map[string]any `json:"instances"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(status, &s); err != nil {
		t.Fatal(err)
	}
	if s.Instance["id"] != followerID || s.Peers.LiveInstances != 2.0 || len(s.Peers.Instances) != 2 ||
		s.Peers.Instances[0]["id"] != leaderID || s.Peers.Instances[1]["id"] != followerID {
		t.Errorf("%s answered %s; want instance.id %v, live_instances 2 and instances with the ids %v and %v", StatusPath, status, followerID, leaderID, followerID)
	}
	if keys := jsonKeys(t, peerBody); len(keys) != 3 || !keys["id"] || !keys["discovering"] || !keys["address_known"] {
		t.Errorf("%s has the keys %v, want id, discovering and address_known", PeerPath, keys)
	}
	for path, body := range map[string][]byte{StatusPath: status, PeerPath: peerBody, SnapshotPath: snapshotBody} {
		for k := range jsonKeys(t, body) {
			if strings.Contains(strings.ToLower(k), "cop") {
				t.Errorf("%s has the key %s", path, k)
			}
		}
	}
	for name, metrics := range map[string]string{"alone": metricsOf(t, alone), "leader": leader.metrics(t), "follower": follower.metrics(t)} {
		for line := range strings.SplitSeq(metrics, "\n") {
			if strings.HasPrefix(line, "# HELP ") && saysCopy.MatchString(line) {
				t.Errorf("the %s's /metrics says copy: %s", name, line)
			}
		}
	}
}
