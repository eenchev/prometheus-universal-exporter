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
// heartbeat came: dashed and marked for one overdue or shutting down. A
// member's box leads to its view, and every name is escaped.
func TestTheStatusPageDrawsTheMembersAroundTheLeader(t *testing.T) {
	clock := newFakeClock()
	a := New(nil, Options{HeartbeatInterval: 4 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	odd := `r<img src=x>`
	beat(t, a, "r1", "r2", odd)
	clock.Advance(7 * time.Second)
	beat(t, a, "r1")
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: odd, Advertise: "x:1", ShuttingDown: true})); err != nil {
		t.Fatal(err)
	}
	h := a.Handler("")
	s := statusOf(t, h, "")
	page := ask(h, "/", "").Body.String()
	r1, r2 := s.Members.List[0], s.Members.List[1]
	requireIn(t, page, `<h2 id="diagram">Diagram</h2>`,
		`<g class="node hub master">`, `>Target allocator</text>`, `>this instance</text>`,
		`<a href="?member=r1"><g class="node"><title>r1</title>`,
		`<text class="edge-label" x="`, `<title>last heartbeat at `+r1.LastHeartbeat+`</title>0s ago</text>`,
		`<line class="edge late"`, `<title>a heartbeat is overdue; last heartbeat at `+r2.LastHeartbeat+`</title>7s ago, overdue</text>`,
		`<line class="edge down"`, `>shutting down</text>`, `<g class="node down">`,
		`r&lt;img src=x&gt;`, "The filled box is the leader, this instance, which runs without peers.")
	if strings.Contains(page, "<img src=x>") {
		t.Error("a member's name is written unescaped")
	}
}

// Among peers the leader is the filled box: on the leader's page this
// instance, on a follower's the peer that leads. A peer's line says how
// long ago this instance last checked it.
func TestTheDiagramFillsTheLeaderAmongPeers(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	for _, c := range []*peerInstance{leader, follower} {
		beat(t, c.a, "r1")
	}
	leaderPage := ask(leader.a.Handler(""), "/", "").Body.String()
	requireIn(t, leaderPage, `<g class="node hub master">`, `>this instance, leader</text>`,
		`<title>`+follower.addr.String()+`</title>`, `>follower</text>`, `<title>last checked at `)
	followerPage := ask(follower.a.Handler(""), "/", "").Body.String()
	requireIn(t, followerPage, `<g class="node hub">`, `>this instance, follower</text>`,
		`<g class="node master"><title>`+leader.addr.String()+`</title>`, `>leader</text>`)
}
