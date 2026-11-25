package allocator

import (
	"strconv"
)

// The status page draws this instance, its peers and the members that
// heartbeat to it as a diagram, laid out here (topologyOf) and drawn as SVG
// by the page's template. This instance is in the middle, its peers on the
// left and the members on the right, each joined to it by a line. The
// leader, the instance that asks the discovery sources, is drawn filled in
// the accent colour and says "leader"; so is this instance when it runs
// without --peers. A member's line says how long ago its last heartbeat
// came, and a peer's how long ago this instance last checked it; a line is
// dashed, and its text says why, for an overdue heartbeat, a member silent,
// gone or shutting down, and a peer down or never checked. The members
// table and the peers table say the same in text.

// The diagram's sizes, in pixels: a node's box, the hub's, the gap a line
// spans between two columns, and between the hub and the members when there
// are no peers (topologyAlone), a row's height and the margin. Three columns
// fit the page's 860px. topologyMost is the most members drawn, and
// topologyNameMax the most characters of a box's line.
const (
	topologyNodeW   = 170
	topologyNodeH   = 36
	topologyHubW    = 180
	topologyHubH    = 64
	topologyGap     = 150
	topologyAlone   = 250
	topologyRowH    = 44
	topologyMargin  = 8
	topologyMost    = 50
	topologyNameMax = 22
	// topologyLabelGap is the room between a line's text and the box it
	// leads to.
	topologyLabelGap = 8
)

// topologyView is the diagram: its size, its hub (this instance), and a
// node and a line for each peer and member. MoreMembers are the members
// beyond the first topologyMost, which the table lists.
type topologyView struct {
	Width, Height int
	Hub           topologyNode
	Peers         []topologyNode
	Members       []topologyNode
	MoreMembers   int
	// MoreX and MoreY are where the text saying how many more members
	// there are goes.
	MoreX, MoreY int
	// Label is the diagram's text alternative. HasMembers says the page
	// has the members, which a follower under --peers.leader-only leaves
	// to the leader.
	Label      string
	HasMembers bool
	// HasLeader says a box is the leader's: none is before the instances
	// have checked each other.
	HasLeader bool
}

// topologyNode is a box of the diagram and the line that joins it to the
// hub. Name is its first line, cut to topologyNameMax characters, and
// FullName what it was cut from, the hub's its whole address. Sub is its second line, and Note the
// hub's third, this instance's address. Member is the member's ID, whose
// view the box opens, "" for the hub and a peer. Master marks the leader,
// and Down a peer or member not live, or shutting down.
type topologyNode struct {
	X, Y, W, H     int
	Name, FullName string
	Sub, Note      string
	Member         string
	Master, Down   bool
	Edge           *topologyEdge
}

// topologyEdge is a line from the hub to a node, and its text, put along
// it near the node, where the lines are furthest apart (edgeOf). LX and LY
// are where the text is anchored and Anchor how, start or end, so that it
// ends short of the node's box. Class is "", late or down. At is the moment of the last heartbeat or check, RFC 3339 in
// UTC, "" for a peer never checked, and Tip what the tooltip says after
// it; the page's script writes the moment in the browser's time
// (pagekit.LocalTimesScript).
type topologyEdge struct {
	X1, Y1, X2, Y2 int
	LX, LY         int
	Anchor         string
	Text, Tip      string
	At             string
	Class          string
}

// NameY and SubY are the baselines of a node's two lines, and MidX its
// middle, where they are centred.
func (n topologyNode) NameY() int { return n.Y + 15 }
func (n topologyNode) SubY() int  { return n.Y + 29 }
func (n topologyNode) MidX() int  { return n.X + n.W/2 }

// topologyOf lays out the diagram of s, nil when there is neither a peer
// nor a member to draw.
func topologyOf(s Status) *topologyView {
	var members []MemberStatus
	if s.Members != nil {
		members = s.Members.List
	}
	var peers []PeerStatus
	if s.Peers != nil {
		for _, p := range s.Peers.Instances {
			if !p.Self {
				peers = append(peers, p)
			}
		}
	}
	if len(members) == 0 && len(peers) == 0 {
		return nil
	}
	v := &topologyView{}
	shown := members
	if len(shown) > topologyMost {
		shown, v.MoreMembers = shown[:topologyMost], len(shown)-topologyMost
	}
	memberRows := len(shown)
	if v.MoreMembers > 0 {
		memberRows++
	}
	rows := max(memberRows, len(peers), 1)
	inner := max(rows*topologyRowH, topologyHubH+topologyRowH/2)
	v.Height = inner + 2*topologyMargin

	hubX, membersX := topologyMargin, topologyMargin+topologyHubW+topologyAlone
	if len(peers) > 0 {
		hubX = topologyMargin + topologyNodeW + topologyGap
		membersX = hubX + topologyHubW + topologyGap
	}
	v.Width = membersX + topologyNodeW + topologyMargin
	if len(members) == 0 {
		v.Width = hubX + topologyHubW + topologyMargin
	}
	hubY := topologyMargin + (inner-topologyHubH)/2
	v.Hub = topologyNode{X: hubX, Y: hubY, W: topologyHubW, H: topologyHubH, Name: "Target allocator", FullName: s.Instance.Address}
	v.Hub.Sub, v.Hub.Master = hubRole(s.Peers)
	v.Hub.Note = cutName(s.Instance.Address)
	hubMidY := hubY + topologyHubH/2

	// rowY is the top of a node in row i of a column of n rows, the column
	// centred beside the hub.
	rowY := func(i, n int) int {
		top := topologyMargin + (inner-n*topologyRowH)/2
		return top + i*topologyRowH + (topologyRowH-topologyNodeH)/2
	}
	for i, p := range peers {
		y := rowY(i, len(peers))
		n := topologyNode{X: topologyMargin, Y: y, W: topologyNodeW, H: topologyNodeH, Master: p.Leader, Down: !p.Live}
		n.Name, n.FullName = cutName(p.Address), p.Address
		n.Sub = "follower"
		if p.Leader {
			n.Sub = "leader"
		}
		if !p.Live {
			n.Sub += ", down"
		}
		n.Edge = edgeOf(hubX, hubMidY, n.X+n.W, y+topologyNodeH/2)
		n.Edge.Text, n.Edge.Tip, n.Edge.Class = peerEdgeText(p)
		n.Edge.At = p.LastCheck
		v.Peers = append(v.Peers, n)
	}
	for i, m := range shown {
		y := rowY(i, memberRows)
		n := topologyNode{X: membersX, Y: y, W: topologyNodeW, H: topologyNodeH, Down: !m.Live || m.ShuttingDown}
		n.Name, n.FullName, n.Sub = cutName(m.ID), m.ID, cutName(m.Address)
		n.Member = m.ID
		n.Edge = edgeOf(hubX+topologyHubW, hubMidY, membersX, y+topologyNodeH/2)
		n.Edge.Text, n.Edge.Tip, n.Edge.Class = memberEdgeText(m)
		n.Edge.At = m.LastHeartbeat
		v.Members = append(v.Members, n)
	}
	if v.MoreMembers > 0 {
		v.MoreX, v.MoreY = membersX+8, rowY(len(shown), memberRows)+topologyNodeH/2+4
	}
	v.Label = topologyLabel(s, len(peers), len(members))
	v.HasMembers = s.Members != nil
	v.HasLeader = v.Hub.Master
	for _, p := range v.Peers {
		v.HasLeader = v.HasLeader || p.Master
	}
	return v
}

// hubRole is this instance's second line, and whether it leads: alone,
// without --peers, it does.
func hubRole(p *PeersStatus) (string, bool) {
	switch {
	case p == nil:
		return "this instance", true
	case p.Role == "leader":
		return "this instance, leader", true
	case p.Role == "follower":
		return "this instance, follower", false
	default:
		return "this instance, choosing", false
	}
}

// edgeOf is the line from the hub's (x1, y1) to a node's (x2, y2). Its
// text ends topologyLabelGap short of the node and runs back along the
// line towards the hub: above a line that rises towards the node, below
// one that falls, so the line, which nears the hub's height as the text
// goes, does not cross it.
func edgeOf(x1, y1, x2, y2 int) *topologyEdge {
	e := &topologyEdge{X1: x1, Y1: y1, X2: x2, Y2: y2, LX: x2 - topologyLabelGap, Anchor: "end"}
	if x2 < x1 {
		e.LX, e.Anchor = x2+topologyLabelGap, "start"
	}
	at := y1 + (y2-y1)*(e.LX-x1)/(x2-x1)
	if y2 > y1 {
		e.LY = at + 13
	} else {
		e.LY = at - 5
	}
	return e
}

// memberEdgeText is a member's line's text, what its tooltip says after
// the moment of the last heartbeat, and its class.
func memberEdgeText(m MemberStatus) (text, tip, class string) {
	ago := age(m.LastHeartbeatAgeSeconds) + " ago"
	tip = ", the last heartbeat"
	switch {
	case m.ShuttingDown:
		return "shutting down", tip + "; the member said it is shutting down", "down"
	case !m.Live && m.InList:
		return "silent, " + ago, tip + "; the member is silent and leaves the member list", "down"
	case !m.Live:
		return "gone, " + ago, tip + "; the member has gone", "down"
	case m.Late:
		return ago + ", overdue", tip + "; a heartbeat is overdue", "late"
	}
	return ago, tip, ""
}

// peerEdgeText is a peer's line's text, what its tooltip says after the
// moment of the last check, and its class.
func peerEdgeText(p PeerStatus) (text, tip, class string) {
	if p.LastCheck == "" {
		return "never checked", "this instance has not checked it yet", "down"
	}
	ago := age(p.LastCheckAgeSeconds) + " ago"
	tip = ", the last check"
	if !p.Live {
		return "down, " + ago, tip + "; " + strconv.Itoa(p.Failures) + " failed check" + plural(p.Failures) + " in a row", "down"
	}
	return ago, tip, ""
}

// cutName is name cut to topologyNameMax characters, with an ellipsis.
func cutName(name string) string {
	r := []rune(name)
	if len(r) <= topologyNameMax {
		return name
	}
	return string(r[:topologyNameMax-1]) + "…"
}

// topologyLabel is the diagram's text alternative.
func topologyLabel(s Status, peers, members int) string {
	role, _ := hubRole(s.Peers)
	label := "Diagram: " + role
	if peers > 0 {
		label += ", " + strconv.Itoa(peers) + " peer" + plural(peers)
	}
	if s.Members != nil {
		label += " and " + strconv.Itoa(members) + " member" + plural(members)
	}
	return label + "; the tables below list the same."
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
