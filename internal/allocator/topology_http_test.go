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
		`<a href="?member=r1"><g class="node"><title>r1: 0 targets, and no member holds any weight</title>`, `class="share">0 targets</text>`,
		`<a href="?member=a%26b%23c">`,
		`<text class="edge-label" x="`, `<title data-at="`+r1.LastHeartbeat+`">`+r1.LastHeartbeat+`, the last heartbeat</title>0s ago</text>`,
		`<line class="edge late"`, `<title data-at="`+r2.LastHeartbeat+`">`+r2.LastHeartbeat+`, the last heartbeat; a heartbeat is overdue</title>7s ago, overdue</text>`,
		`const tip = mark.localName === "title" ? "textContent" : "title";`,
		`<line class="edge down"`, `>shutting down, 0s ago</text>`, `<g class="node down">`,
		`r&lt;img src=x&gt;`, "<figcaption>This instance leads; the grey boxes are the exporters. Each line gives the time since the last heartbeat. Dashed red needs attention; the line says why. An exporter's last line is its targets and its share of their weight, in red when it is overloaded.</figcaption>")
	if strings.Contains(page, "<img src=x>") {
		t.Error("a member's name is written unescaped")
	}
}

// Among peers the leader is the filled box: on the leader's page this
// instance, on a follower's the peer that leads. The other allocator
// instances are drawn apart from the members, in the allocator's colour,
// and the caption says each instance draws the heartbeats it receives,
// every exporter heartbeating every instance. A peer's line says how
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
		`<g class="node allocator"><title>`+follower.addr.String()+`</title>`, `>follower</text>`, `, the last check</title>`,
		"<figcaption>The filled box leads; purple boxes are the other allocator instances, grey ones the exporters. Each exporter heartbeats every allocator instance; the exporters' lines are the heartbeats this instance receives. Each line gives the time since the last heartbeat or check. Dashed red needs attention; the line says why. An exporter's last line is its targets and its share of their weight, in red when it is overloaded.</figcaption>",
		"Diagram: this instance, leader, 1 peer and 2 members;")
	followerPage := ask(follower.a.Handler(""), "/", "").Body.String()
	requireIn(t, followerPage, `<g class="node hub allocator">`, `>this instance, follower</text>`,
		`<g class="node allocator master"><title>`+leader.addr.String()+`</title>`, `>leader</text>`)
	leaderOnly(leader, follower)
	partial := ask(follower.a.Handler(""), "/", "").Body.String()
	requireIn(t, partial, `aria-label="Diagram: this instance, follower, 1 peer; the tables below list the same."`,
		"<figcaption>The filled box leads; purple boxes are the other allocator instances. Each line gives the time since the last check; the exporters are on the leader's page. Dashed red needs attention; the line says why.</figcaption>")
	if _, caption, _ := strings.Cut(partial, "<figcaption>"); strings.Contains(strings.SplitN(caption, "</figcaption>", 2)[0], "heartbeat") {
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

// The diagram is drawn in a window of a fixed height that scrolls, and
// zooms: its buttons are hidden until the page's script shows them, so a
// page without JavaScript has no dead buttons. The script scales the
// drawing from the size the SVG keeps in data-width and data-height, by
// steps or around the pointer under Ctrl and the wheel, a wheel's lines
// counted in pixels, or Safari's pinch, which its wheel events, if any,
// do not zoom a second time, and keeps the zoom and the scroll over a
// refresh. At first and on Reset the window is scrolled to the
// leader's box, whose middle the SVG gives.
func TestTheDiagramScrollsAndZooms(t *testing.T) {
	a, _ := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, staticTargets(3), "")), "r1")
	t.Cleanup(a.BeginShutdown)
	page := ask(a.Handler(""), "/", "").Body.String()
	requireIn(t, page,
		`.topology-window { max-height: 480px; overflow: auto;`,
		// An overloaded member's box and its share line are red.
		".topology .node.over rect { stroke: var(--bad); stroke-width: 2; }", ".topology .node.over text.share { fill: var(--bad); font-weight: 600; }",
		`<div class="topology-zoom" role="group" aria-label="Zoom" hidden><button type="button" data-zoom="out" aria-label="Zoom out"`,
		`<button type="button" data-zoom="reset">Reset</button><span class="muted">or Ctrl and the mouse wheel, or a trackpad's pinch</span></div>`,
		// On a phone the hint wraps, and "Reset" is not broken in two.
		".topology-zoom { display: flex; flex-wrap: wrap;", ".topology-zoom button { flex: none; white-space: nowrap; overflow-wrap: normal;",
		`<div class="topology-window" tabindex="0" role="region" aria-label="The diagram, which scrolls">`,
		`data-width="616" data-height="109" data-focus-x="98" data-focus-y="54">`,
		`<title>r1: 3 targets, 100% of the weight; its fair share is 100%</title>`, `class="share">3 targets · 100%</text>`,
		"const zoomSteps = [0.5, 0.67, 0.8, 1, 1.25, 1.5, 2];",
		`svg.setAttribute("width", Number(svg.getAttribute("data-width")) * zoom);`,
		"box.scrollLeft = at[0] * zoom - x;",
		`const box = event.ctrlKey && event.target.closest(".topology-window");`,
		"}, {passive: false});",
		"arrangeJobs(() => keepDiagram(() => {",
		"const wheelPixels = (event, box) => event.deltaY * [1, 16, box.clientHeight][event.deltaMode];",
		"zoomDiagram(zoom * Math.exp(-wheelPixels(event, box) * 0.002)",
		`content.addEventListener("gesturechange", (event) => {`, "zoomDiagram(pinchFrom * event.scale,",
		"if (pinchFrom) return;",
		`if (step === "reset") {`+"\n      zoomDiagram(1);\n      onLeader(content.querySelector(\".topology-window\"));",
		"if (!at) return onLeader(after);", "    zoomDiagram(1);\n    onLeader(first);")
}
