package allocator

import (
	"strconv"
	"testing"
)

// The diagram draws nothing without a peer or a member, and at most 50
// members, saying how many more there are. Every box is inside the
// diagram, the columns do not overlap, and the lines' texts are far
// enough apart to read. A peer that failed one check says so in the
// singular.
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
	if v.MoreY <= v.Members[49].Y+v.Members[49].H || v.MoreY > v.Height {
		t.Errorf("the more text is at %d, the last member ends at %d", v.MoreY, v.Members[49].Y+v.Members[49].H)
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
}
