//go:build !select_request_types || request_type_http

package allocator

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The status page draws this instance, which leads when it runs without
// peers, filled, and a line to each member saying how long ago its last
// heartbeat came: dashed and marked for one overdue or shutting down. Its
// tooltip begins with the moment, which the page's script writes in the
// browser's time. A member's box leads to its view, its ID escaped as a
// query value, and every name is escaped.
func TestTheStatusPageDrawsTheMembersAroundTheLeader(t *testing.T) {
	clock := newFakeClock()
	a := New(nil, Options{HeartbeatInterval: 4 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	odd := `r<img src=x>`
	beat(t, a, "r1", "r2", odd, "a&b#c")
	clock.Advance(7 * time.Second)
	beat(t, a, "r1", "a&b#c")
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: odd, Advertise: "x:1", ShuttingDown: true})); err != nil {
		t.Fatal(err)
	}
	h := a.Handler("")
	s := statusOf(t, h, "")
	page := ask(h, "/", "").Body.String()
	r1, r2 := memberOf(t, s, "r1"), memberOf(t, s, "r2")
	requireIn(t, page, `<h2 id="diagram">Diagram</h2>`,
		`<g class="node hub master">`, `>Target allocator</text>`, `>this instance</text>`,
		`<a href="?member=r1"><g class="node"><title>r1</title>`,
		`<a href="?member=a%26b%23c">`,
		`<text class="edge-label" x="`, `<title data-at="`+r1.LastHeartbeat+`">`+r1.LastHeartbeat+`, the last heartbeat</title>0s ago</text>`,
		`<line class="edge late"`, `<title data-at="`+r2.LastHeartbeat+`">`+r2.LastHeartbeat+`, the last heartbeat; a heartbeat is overdue</title>7s ago, overdue</text>`,
		`const tip = mark.localName === "title" ? "textContent" : "title";`,
		`<line class="edge down"`, `>shutting down</text>`, `<g class="node down">`,
		`r&lt;img src=x&gt;`, "The filled box is the leader, this instance, which runs without peers.")
	if strings.Contains(page, "<img src=x>") {
		t.Error("a member's name is written unescaped")
	}
}

// Among peers the leader is the filled box: on the leader's page this
// instance, on a follower's the peer that leads. A peer's line says how
// long ago this instance last checked it. A follower under
// --peers.leader-only draws itself and its peers alone, and says the
// members are on the leader's page.
func TestTheDiagramFillsTheLeaderAmongPeers(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	for _, c := range []*peerInstance{leader, follower} {
		beat(t, c.a, "r1")
	}
	leaderPage := ask(leader.a.Handler(""), "/", "").Body.String()
	requireIn(t, leaderPage, `<g class="node hub master">`, `>this instance, leader</text>`,
		`<title>`+follower.addr.String()+`</title>`, `>follower</text>`, `, the last check</title>`,
		"Diagram: this instance, leader, 1 peer and 2 members;")
	followerPage := ask(follower.a.Handler(""), "/", "").Body.String()
	requireIn(t, followerPage, `<g class="node hub">`, `>this instance, follower</text>`,
		`<g class="node master"><title>`+leader.addr.String()+`</title>`, `>leader</text>`)
	leaderOnly(leader, follower)
	partial := ask(follower.a.Handler(""), "/", "").Body.String()
	requireIn(t, partial, `aria-label="Diagram: this instance, follower, 1 peer; the tables below list the same."`,
		"the members are on the leader's page. A dashed line is a peer down or not yet checked.")
	if strings.Contains(partial, "member's line") {
		t.Error("the follower's caption speaks of members' lines it does not draw")
	}
}

// memberOf is the member id of the status s.
func memberOf(t *testing.T, s Status, id string) MemberStatus {
	t.Helper()
	for _, m := range s.Members.List {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no member %q", id)
	return MemberStatus{}
}
