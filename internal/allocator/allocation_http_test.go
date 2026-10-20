//go:build !select_request_types || request_type_http

package allocator

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// owned are the static targets of in that member owns among members, by
// name in file order.
func owned(in *Inputs, members []string, member string) []string {
	var names []string
	for i := range in.Targets.Targets {
		if owner, _ := allocapi.Owner(members, in.shardKeys[i]); owner == member {
			names = append(names, in.Targets.Targets[i].Name)
		}
	}
	return names
}

// inFileOrder is the names of a and b together, in the order of the file.
func inFileOrder(in *Inputs, a, b []string) []string {
	var names []string
	for i := range in.Targets.Targets {
		name := in.Targets.Targets[i].Name
		if slices.Contains(a, name) || slices.Contains(b, name) {
			names = append(names, name)
		}
	}
	return names
}

// Every static target is in exactly one member's share, and the shares are
// about even.
func TestEveryStaticTargetIsInOneShare(t *testing.T) {
	in := mustLoad(t, writeInputs(t, testConfig, staticTargets(300), ""))
	members := []string{"r1", "r2", "r3"}
	a, _ := settledAllocator(t, in, members...)
	seen := map[string]string{}
	for _, m := range members {
		got := share(t, a, m)
		if want := owned(in, members, m); !slices.Equal(got, want) {
			t.Errorf("%s's share is %v, want %v", m, got, want)
		}
		if len(got) < 70 || len(got) > 130 {
			t.Errorf("%s's share has %d of 300 targets, far from a third", m, len(got))
		}
		for _, name := range got {
			if other, ok := seen[name]; ok {
				t.Errorf("%s is in the shares of %s and %s", name, other, m)
			}
			seen[name] = m
		}
	}
	if len(seen) != 300 {
		t.Errorf("%d of 300 targets are in a share", len(seen))
	}
}

// A member that joins takes only the targets it now owns, about a quarter
// of them with a fourth member; each one its old owner keeps for one of
// the target's intervals, its own or the file's, while the new owner
// already has it, and then gives up.
func TestAJoiningMemberTakesItsTargetsWhileTheOldOwnersDrainThem(t *testing.T) {
	in := mustLoad(t, writeInputs(t, testConfig, staticTargets(300), ""))
	before := []string{"r1", "r2", "r3"}
	after := []string{"r1", "r2", "r3", "r4"}
	a, clock := settledAllocator(t, in, before...)
	beat(t, a, after...)
	keepBeating(t, a, clock, 15*time.Second, after...)
	if got := effective(a); !slices.Equal(got, after) {
		t.Fatalf("got the member list %v, want %v", got, after)
	}
	if got, want := share(t, a, "r4"), owned(in, after, "r4"); !slices.Equal(got, want) || len(got) < 50 || len(got) > 100 {
		t.Fatalf("the new member's share is %d targets %v, want its own %d %v, about a quarter", len(got), got, len(want), want)
	}
	// The draining targets of each old owner, by the interval they keep.
	moved := map[string][2][]string{}
	for _, m := range before {
		for _, name := range owned(in, before, m) {
			if slices.Contains(owned(in, after, m), name) {
				continue
			}
			pair := moved[m]
			if time.Duration(in.Targets.Targets[in.targetIndex[name]].Interval) == time.Minute {
				pair[1] = append(pair[1], name)
			} else {
				pair[0] = append(pair[0], name)
			}
			moved[m] = pair
		}
	}
	for _, m := range before {
		if got, want := share(t, a, m), inFileOrder(in, owned(in, after, m), append(slices.Clone(moved[m][0]), moved[m][1]...)); !slices.Equal(got, want) {
			t.Errorf("right after the change, %s's share is %v, want its own and its draining targets %v", m, got, want)
		}
	}
	keepBeating(t, a, clock, 30*time.Second, after...)
	for _, m := range before {
		if got, want := share(t, a, m), inFileOrder(in, owned(in, after, m), moved[m][1]); !slices.Equal(got, want) {
			t.Errorf("after 30s, %s's share is %v, want its own and the draining targets of 1m %v", m, got, want)
		}
	}
	keepBeating(t, a, clock, 30*time.Second, after...)
	for _, m := range before {
		if got, want := share(t, a, m), owned(in, after, m); !slices.Equal(got, want) {
			t.Errorf("after 1m, %s's share is %v, want its own %v", m, got, want)
		}
	}
}

// A member that leaves gives its targets to the others, and only its: the
// others keep theirs, and nothing drains, since the old owner is gone.
func TestALeavingMembersTargetsMoveAndNoOthers(t *testing.T) {
	in := mustLoad(t, writeInputs(t, testConfig, staticTargets(300), ""))
	before := []string{"r1", "r2", "r3"}
	a, clock := settledAllocator(t, in, before...)
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r3", Advertise: "r3:9115", ShuttingDown: true})); err != nil {
		t.Fatal(err)
	}
	keepBeating(t, a, clock, 15*time.Second, "r1", "r2")
	after := []string{"r1", "r2"}
	if got := effective(a); !slices.Equal(got, after) {
		t.Fatalf("got the member list %v, want %v", got, after)
	}
	for _, m := range after {
		got := share(t, a, m)
		if want := owned(in, after, m); !slices.Equal(got, want) {
			t.Errorf("%s's share is %v, want %v", m, got, want)
		}
		for _, name := range owned(in, before, m) {
			if !slices.Contains(got, name) {
				t.Errorf("%s lost %s, which was not the leaving member's", m, name)
			}
		}
	}
	if !strings.Contains(metrics(t, a), "pue_allocator_member_changes_total 1\n") {
		t.Errorf("the change was not counted:\n%s", metrics(t, a))
	}
}

// Static targets that share a shard_key share a member.
func TestStaticTargetsOfOneShardKeyShareAMember(t *testing.T) {
	var b strings.Builder
	b.WriteString("interval: 30s\ntargets:\n")
	for i := range 30 {
		fmt.Fprintf(&b, "  - name: t%d\n    collector: demo\n    target: http://t%d.example\n    shard_key: rack-%d\n", i, i, i%2)
	}
	in := mustLoad(t, writeInputs(t, testConfig, b.String(), ""))
	a, _ := settledAllocator(t, in, "r1", "r2", "r3", "r4", "r5")
	for _, m := range []string{"r1", "r2", "r3", "r4", "r5"} {
		got := share(t, a, m)
		if len(got) != 0 && len(got) != 15 && len(got) != 30 {
			t.Errorf("%s's share %v splits a shard key", m, got)
		}
	}
}

// shardKeyed is a static target file of n targets, each with the shard key
// keys gives it.
func shardKeyed(n int, key func(i int) string) string {
	var b strings.Builder
	b.WriteString("interval: 30s\ntargets:\n")
	for i := range n {
		fmt.Fprintf(&b, "  - name: t%d\n    collector: demo\n    target: http://t%d.example\n    shard_key: %s\n", i, i, key(i))
	}
	return b.String()
}

// A static target a reload moves to another member, by a changed shard_key,
// stays with the member that had it for one of its intervals, while its new
// owner already has it, as one a change of the member list moves does. A
// target the reload removes is in no share at once.
func TestAReloadThatMovesATargetHandsItOver(t *testing.T) {
	members := []string{"r1", "r2", "r3"}
	p := writeInputs(t, testConfig, shardKeyed(10, func(i int) string { return fmt.Sprintf("k%d", i) }), "")
	a, clock := settledAllocator(t, mustLoad(t, p), members...)
	was, _ := allocapi.Owner(members, "k0")
	moved := ""
	for i := range 100 {
		if owner, _ := allocapi.Owner(members, fmt.Sprintf("x%d", i)); owner != was {
			moved = fmt.Sprintf("x%d", i)
			break
		}
	}
	is, _ := allocapi.Owner(members, moved)
	gone, _ := allocapi.Owner(members, "k9")
	// t0 gets the shard key that moves it, and t9 is removed.
	text := shardKeyed(9, func(i int) string {
		if i == 0 {
			return moved
		}
		return fmt.Sprintf("k%d", i)
	})
	testutil.WriteIn(t, filepath.Dir(p.Config), filepath.Base(p.StaticTargets), text)
	if !a.Reload(p) {
		t.Fatal("the changed file was not put in force")
	}
	if got := share(t, a, is); !slices.Contains(got, "t0") {
		t.Errorf("t0's new owner %s does not have it: %v", is, got)
	}
	if got := share(t, a, was); !slices.Contains(got, "t0") {
		t.Errorf("t0's old owner %s gave it up at once: %v", was, got)
	}
	for _, m := range members {
		if slices.Contains(share(t, a, m), "t9") {
			t.Errorf("the removed t9 is still in %s's share (it was %s's)", m, gone)
		}
	}
	keepBeating(t, a, clock, 30*time.Second, members...)
	if got := share(t, a, was); slices.Contains(got, "t0") {
		t.Errorf("t0's old owner %s still has it after its interval: %v", was, got)
	}
	if got := share(t, a, is); !slices.Contains(got, "t0") {
		t.Errorf("t0's new owner %s lost it: %v", is, got)
	}
}
