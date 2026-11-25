package allocator

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
)

// The diagram draws nothing without a peer or a member, and at most 50
// members, saying how many more there are. Every box is inside the
// diagram, the columns do not overlap, and the lines' texts are far
// enough apart to read. A peer that failed one check says so in the
// singular; a live peer whose last check failed is marked, as a member
// shutting down still says when its last heartbeat came.
func TestTheDiagramFitsItsNodes(t *testing.T) {
	if v := topologyOf(Status{Members: &MembersStatus{}}); v != nil {
		t.Errorf("a diagram of nothing: %+v", v)
	}
	members := make([]MemberStatus, 60)
	for i := range members {
		members[i] = MemberStatus{ID: "exporter-" + strconv.Itoa(i), Live: true, InList: true}
	}
	peers := []PeerStatus{{Address: "a:1", Self: true, Leader: true, Live: true}, {Address: "b:1", Live: true, LastCheck: "2026-01-01T00:00:00Z"}}
	v := topologyOf(Status{Members: &MembersStatus{List: members}, Peers: &PeersStatus{Role: "leader", Instances: peers}})
	if len(v.Members) != 50 || v.MoreMembers != 10 || len(v.Peers) != 1 || !v.Hub.Master {
		t.Fatalf("%d members drawn, %d more, %d peers, hub %+v", len(v.Members), v.MoreMembers, len(v.Peers), v.Hub)
	}
	if v.Width > 860 {
		t.Errorf("the diagram is %dpx wide, wider than the page", v.Width)
	}
	all := append(append([]topologyNode{v.Hub}, v.Peers...), v.Members...)
	for _, n := range all {
		if n.X < 0 || n.Y < 0 || n.X+n.W > v.Width || n.Y+n.H > v.Height {
			t.Errorf("%s at %d,%d %dx%d is outside %dx%d", n.Name, n.X, n.Y, n.W, n.H, v.Width, v.Height)
		}
	}
	if v.Peers[0].X+v.Peers[0].W >= v.Hub.X || v.Hub.X+v.Hub.W >= v.Members[0].X {
		t.Errorf("the columns overlap: peer %+v, hub %+v, member %+v", v.Peers[0], v.Hub, v.Members[0])
	}
	if v.MoreX != v.Width-topologyMargin {
		t.Errorf("the more text ends at %d, not at the diagram's right edge, %d", v.MoreX, v.Width)
	}
	if v.MoreY <= v.Members[49].Y+v.Members[49].H || v.MoreY > v.Height {
		t.Errorf("the more text is at %d, the last member ends at %d", v.MoreY, v.Members[49].Y+v.Members[49].H)
	}
	// A line's text ends short of the box it leads to, so a long one does
	// not run into it.
	for _, n := range v.Members {
		if n.Edge.Anchor != "end" || n.Edge.LX >= n.X {
			t.Errorf("the text of %s's line ends at %d, its box begins at %d", n.Name, n.Edge.LX, n.X)
		}
	}
	for _, n := range v.Peers {
		if n.Edge.Anchor != "start" || n.Edge.LX <= n.X+n.W {
			t.Errorf("the text of %s's line begins at %d, its box ends at %d", n.Name, n.Edge.LX, n.X+n.W)
		}
	}
	for i := 1; i < len(v.Members); i++ {
		if gap := v.Members[i].Edge.LY - v.Members[i-1].Edge.LY; gap < 14 {
			t.Errorf("the texts of lines %d and %d are %dpx apart", i-1, i, gap)
		}
	}
	if long := topologyOf(Status{Members: &MembersStatus{List: []MemberStatus{{ID: "a-very-long-member-name-that-goes-on"}}}}); long.Members[0].Name != "a-very-long-member-na…" || long.Members[0].FullName != "a-very-long-member-name-that-goes-on" {
		t.Errorf("a long name is drawn as %q", long.Members[0].Name)
	}
	if _, tip, class := peerEdgeText(PeerStatus{LastCheck: "2026-01-01T00:00:00Z", Failures: 1}); tip != ", the last check; 1 failed check in a row" || class != "down" {
		t.Errorf("a peer down after one failed check says %q, %q", tip, class)
	}
	if text, _, class := peerEdgeText(PeerStatus{LastCheck: "2026-01-01T00:00:00Z", LastCheckAgeSeconds: 3, Live: true, Failures: 2}); text != "3s ago, failed" || class != "late" {
		t.Errorf("a live peer whose last check failed says %q, %q", text, class)
	}
	if text, _, _ := memberEdgeText(MemberStatus{ShuttingDown: true, Live: true, LastHeartbeatAgeSeconds: 4}); text != "shutting down, 4s ago" {
		t.Errorf("a member shutting down says %q", text)
	}
}

// Before the instances have chosen their leader no box is filled, and the
// caption says so rather than pointing at a filled box; the window opens
// on this instance, and on a follower's page on the leader's box. The diagram is a
// group, not an image, so a screen reader reaches the members' links in
// it; the hub's tooltip is this instance's whole address.
func TestTheDiagramSaysWhenNoInstanceLeads(t *testing.T) {
	address := "allocator-0.allocator.monitoring.svc:9116"
	s := Status{Instance: InstanceStatus{Address: address}, Peers: &PeersStatus{Instances: []PeerStatus{{Address: "a:1", Self: true}, {Address: "b:1"}}},
		Members: &MembersStatus{List: []MemberStatus{{ID: "r1", Live: true}}}}
	v := topologyOf(s)
	if v.HasLeader || v.Hub.Master || v.Peers[0].Master {
		t.Fatalf("a box is filled before a leader is chosen: %+v", v)
	}
	if v.FocusX != v.Hub.MidX() || v.FocusY != v.Hub.Y+v.Hub.H/2 {
		t.Errorf("with no leader the window opens on %d,%d, not on this instance", v.FocusX, v.FocusY)
	}
	var b strings.Builder
	if err := statusContentTemplate.Execute(&b, statusPageOf(s)); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, want := range []string{"<figcaption>No instance leads yet; purple boxes are allocator instances, grey ones the exporters. Each exporter heartbeats every allocator instance;", `<svg role="group" aria-label="Diagram: this instance, choosing, 1 peer and 1 member;`,
		`<g class="node hub allocator"><title>` + address + `</title>`} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(page, "The filled box leads") {
		t.Error("the caption points at a filled box no instance has")
	}
	s.Peers.Role, s.Peers.Instances[1].Leader = "follower", true
	if f := topologyOf(s); f.FocusX != f.Peers[0].MidX() || f.FocusY != f.Peers[0].Y+f.Peers[0].H/2 {
		t.Errorf("a follower's window opens on %d,%d, not on the leader's box %+v", f.FocusX, f.FocusY, f.Peers[0])
	}
}

// The longest text a line can have, at an age of 59m59s and 6.5px a
// character (Chromium draws "shutting down, 59m59s ago" in the page's 12px
// sans-serif 158px wide), fits between the hub and the box it ends short
// of, beside peers and without them.
func TestTheLongestLineTextFitsItsGap(t *testing.T) {
	const old = 3599
	member := []MemberStatus{{ShuttingDown: true, LastHeartbeatAgeSeconds: old}, {InList: true, LastHeartbeatAgeSeconds: old}, {LastHeartbeatAgeSeconds: old}, {Live: true, Late: true, LastHeartbeatAgeSeconds: old}, {Live: true, SameName: true, LastHeartbeatAgeSeconds: old}}
	peer := []PeerStatus{{LastCheck: "x", LastCheckAgeSeconds: old}, {LastCheck: "x", Live: true, Failures: 1, LastCheckAgeSeconds: old}, {}}
	for _, gap := range []int{topologyMemberGap, topologyAlone, topologyPeerGap} {
		var all []string
		if gap == topologyPeerGap {
			for _, p := range peer {
				text, _, _ := peerEdgeText(p)
				all = append(all, text)
			}
		} else {
			for _, m := range member {
				text, _, _ := memberEdgeText(m)
				all = append(all, text)
			}
		}
		for _, text := range all {
			if width := 13 * len([]rune(text)) / 2; width > gap-2*topologyLabelGap {
				t.Errorf("%q is about %dpx, wider than the %dpx gap leaves it", text, width, gap)
			}
		}
	}
}

// Of more than 50 members, the diagram draws those not well first, in the
// members' order, and says how many more there are and how many of those
// are not well. Two exporters under one member name are not well.
func TestTheDiagramDrawsTheMembersNotWellFirst(t *testing.T) {
	members := func(notWell int) []MemberStatus {
		list := make([]MemberStatus, 60)
		for i := range list {
			list[i] = MemberStatus{ID: fmt.Sprintf("exporter-%02d", i), Live: true, InList: true}
			if i >= 60-notWell {
				list[i].Live = false
			}
		}
		list[3].SameName = true
		return list
	}
	v := topologyOf(Status{Members: &MembersStatus{List: members(5)}})
	if len(v.Members) != 50 || v.MoreMembers != 10 || v.MoreNotWell != 0 {
		t.Fatalf("%d drawn, %d more, %d of them not well", len(v.Members), v.MoreMembers, v.MoreNotWell)
	}
	var drawn []string
	for _, n := range v.Members {
		drawn = append(drawn, n.Member)
	}
	if drawn[3] != "exporter-03" || drawn[45] != "exporter-55" || drawn[49] != "exporter-59" || v.Members[3].Edge.Text != "0s ago, two exporters" || v.Members[3].Edge.Class != "late" {
		t.Errorf("drawn %v, exporter-03's line %+v", drawn, v.Members[3].Edge)
	}
	var b strings.Builder
	if err := statusContentTemplate.Execute(&b, statusPageOf(Status{Members: &MembersStatus{List: members(55)}})); err != nil {
		t.Fatal(err)
	}
	if want := ">and 10 more in the table below, 6 of them not well</text>"; !strings.Contains(b.String(), want) {
		t.Errorf("missing %q", want)
	}
}

// The diagram reads in both themes: its lines, plain or red, its boxes'
// outlines, the allocator instances' purple among them, and the window's
// stand out from the page at 3:1 or more, as graphics that carry meaning
// must (WCAG 1.4.11), and its text, on the page, on a box, on a purple box
// and on the leader's filled box, at 4.5:1 or more (WCAG 1.4.3).
func TestTheDiagramReadsInBothThemes(t *testing.T) {
	colour := func(theme, name string) string {
		m := regexp.MustCompile(`--` + name + `: (#[0-9a-f]{6});`).FindStringSubmatch(theme)
		if m == nil {
			t.Fatalf("no --%s in %s", name, theme)
		}
		return m[1]
	}
	for _, rule := range []string{".topology line.edge { stroke: var(--muted);", ".topology line.edge.late, .topology line.edge.down { stroke: var(--bad);",
		".topology .node rect { fill: var(--panel); stroke: var(--field);", ".topology .node.master rect { fill: var(--accent);",
		".topology .node.master text, .topology .node.master text.sub { fill: var(--on-accent); }", ".topology .node.allocator rect { fill: var(--node-allocator-bg); stroke: var(--node-allocator);", "border: 1px solid var(--field);"} {
		if !strings.Contains(statusStyle, rule) {
			t.Errorf("the diagram's style lacks %q", rule)
		}
	}
	// The allocator's own colours are the status page's, light and then
	// dark twice (statusStyle), each read beside the page's of its theme.
	own := regexp.MustCompile(`--node-allocator: (#[0-9a-f]{6}); --node-allocator-bg: (#[0-9a-f]{6});`).FindAllStringSubmatch(statusStyle, -1)
	if len(own) != 3 || own[1][0] != own[2][0] {
		t.Fatalf("the allocator boxes' colours are %v; want light, then dark twice", own)
	}
	for i, theme := range []string{pagekit.LightColours, pagekit.DarkColours} {
		stroke, fill := own[i][1], own[i][2]
		for _, c := range []struct {
			fg, bg string
			least  float64
		}{{stroke, colour(theme, "bg"), 3}, {stroke, fill, 3}, {colour(theme, "fg"), fill, 4.5}, {colour(theme, "muted"), fill, 4.5}} {
			if r := barContrast(c.fg, c.bg); r < c.least {
				t.Errorf("theme %d: %s on %s is %.2f:1, under %.1f:1", i, c.fg, c.bg, r, c.least)
			}
		}
	}
	for name, theme := range map[string]string{"light": pagekit.LightColours, "dark": pagekit.DarkColours} {
		for _, c := range []struct {
			fg, bg string
			least  float64
		}{{"field", "bg", 3}, {"accent", "bg", 3},
			{"muted", "bg", 4.5}, {"bad", "bg", 4.5}, {"fg", "panel", 4.5}, {"muted", "panel", 4.5}, {"bad", "panel", 4.5}, {"on-accent", "accent", 4.5}} {
			if r := barContrast(colour(theme, c.fg), colour(theme, c.bg)); r < c.least {
				t.Errorf("%s theme: --%s on --%s is %.2f:1, under %.1f:1", name, c.fg, c.bg, r, c.least)
			}
		}
	}
}
