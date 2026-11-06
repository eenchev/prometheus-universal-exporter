package allocator

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// testUnits are n units named t0, t1, ..., of weight 1 each, or, weighted,
// of a weight from 1 to 20 drawn from seed.
func testUnits(n int, weighted bool, seed int64) []unit {
	r := rand.New(rand.NewSource(seed))
	out := make([]unit, n)
	for i := range out {
		w := int64(milli)
		if weighted {
			w = int64(1+r.Intn(20)) * milli
		}
		key := fmt.Sprintf("t%d", i)
		out[i] = unit{key: key, order: unitOrder(key), weight: w}
	}
	return out
}

// testMembers are n members r00, r01, ..., each of capacity 1, or of the
// capacities given, in order.
func testMembers(n int, caps ...float64) ([]string, []int64) {
	names := make([]string, n)
	capacities := make([]int64, n)
	for i := range names {
		names[i] = fmt.Sprintf("r%02d", i)
		capacities[i] = milli
		if i < len(caps) {
			capacities[i] = int64(math.Round(caps[i] * milli))
		}
	}
	return names, capacities
}

var defaultFactor = factorMilli(DefaultBalanceFactor)

// The same units over the same members are placed alike, whatever order
// the units come in: the pass takes them in the order of their hashes, never
// in the order given or of a map. A factor of 1 makes the bounds matter to
// most units, and so the order.
func TestPlacementIsDeterministic(t *testing.T) {
	units := testUnits(500, true, 1)
	members, caps := testMembers(7, 2, 1, 0.5, 3)
	want := place(units, members, caps, milli)
	for seed := int64(0); seed < 5; seed++ {
		shuffled := slices.Clone(units)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := place(shuffled, members, caps, milli)
		for _, u := range units {
			if got.owner[u.key] != want.owner[u.key] {
				t.Fatalf("seed %d: %s is placed on %s, and on %s with the units in another order", seed, u.key, got.owner[u.key], want.owner[u.key])
			}
		}
	}
}

// With every weight and every capacity 1, as when no file sets a weight
// and no replica a capacity, the placement of many targets is rendezvous
// hashing's, allocapi.Owner's: the bound is far above what hashing puts on
// a member by chance.
func TestUnweightedPlacementOfManyTargetsIsRendezvousHashing(t *testing.T) {
	units := testUnits(1000, false, 0)
	members, caps := testMembers(5)
	p := place(units, members, caps, defaultFactor)
	for _, u := range units {
		if want, _ := allocapi.Owner(members, u.key); p.owner[u.key] != want {
			t.Errorf("%s is placed on %s, where rendezvous hashing puts it on %s", u.key, p.owner[u.key], want)
		}
	}
}

// No member's load goes above the balance factor times its fair share when
// no unit is too heavy for that, at any factor and with capacities apart;
// at a factor of 1 the bounds add up to the total at most, so what whole
// units cannot fit goes above them by less than a unit.
func TestThePlacementKeepsEveryMemberWithinItsBound(t *testing.T) {
	units := testUnits(400, true, 2)
	for _, factor := range []int64{1000, 1100, 1250, 2000} {
		members, caps := testMembers(6, 1, 1, 2, 0.5, 4, 1)
		p := place(units, members, caps, factor)
		bounds := boundsOf(members, p.capOf, p.capacity, p.total, factor)
		placed := int64(0)
		for _, m := range members {
			placed += p.load[m]
			slack := int64(0)
			if factor == milli {
				slack = 20 * milli
			}
			if p.load[m] > bounds[m]+slack {
				t.Errorf("factor %d: %s has %d of a bound of %d", factor, m, p.load[m], bounds[m])
			}
		}
		if placed != p.total || len(p.owner) != len(units) {
			t.Errorf("factor %d: %d of %d placed, %d units of %d", factor, placed, p.total, len(p.owner), len(units))
		}
	}
}

// A unit heavier than any member's room goes to the member least loaded for
// its capacity, and is never left out, and is reported heavy; the others
// are still placed within their bounds where they fit.
func TestAHeavyUnitGoesToTheLeastLoadedMemberAndIsKept(t *testing.T) {
	units := testUnits(30, false, 0)
	units = append(units, unit{key: "heavy", order: unitOrder("heavy"), weight: 100 * milli})
	members, caps := testMembers(3, 1, 1, 2)
	p := place(units, members, caps, defaultFactor)
	owner := p.owner["heavy"]
	if owner == "" {
		t.Fatal("the heavy unit was left out")
	}
	if len(p.heavy) != 1 || p.heavy[0].key != "heavy" || p.heavy[0].member != owner {
		t.Errorf("heavy units %+v, want the unit heavy on %s", p.heavy, owner)
	}
	// Placed again without it, the members' loads say which was the least
	// loaded for its capacity when it came: it comes in the order of its
	// hash, so compare with the loads of the units before it.
	before := map[string]int64{}
	for _, u := range units {
		if u.key != "heavy" && u.order < unitOrder("heavy") {
			before[p.owner[u.key]] += u.weight
		}
	}
	for i, m := range members {
		j := slices.Index(members, owner)
		if lessLoaded(before[m], caps[i], before[owner], caps[j]) {
			t.Errorf("the heavy unit went to %s (%d for capacity %d), but %s was less loaded (%d for capacity %d)", owner, before[owner], caps[j], m, before[m], caps[i])
		}
	}
	for _, u := range units {
		if p.owner[u.key] == "" {
			t.Errorf("%s was left out", u.key)
		}
	}
}

// A member of capacity 2 is given about twice the weight of one of
// capacity 1, and one of 0.5 about half.
func TestAMembersWeightFollowsItsCapacity(t *testing.T) {
	units := testUnits(3000, true, 3)
	members, caps := testMembers(4, 2, 1, 1, 0.5)
	p := place(units, members, caps, defaultFactor)
	unit := float64(p.load[members[1]]+p.load[members[2]]) / 2
	for i, want := range []float64{2, 1, 1, 0.5} {
		got := float64(p.load[members[i]]) / unit
		if math.Abs(got-want) > 0.1*want {
			t.Errorf("%s of capacity %v has %.2f times a capacity 1 member's weight", members[i], want, got)
		}
	}
	// Without the bound, the weighted scores alone still give the
	// capacities their part, as hashing gives it.
	free := place(units, members, caps, factorMilli(MaxBalanceFactor))
	for i, want := range []float64{2, 1, 1, 0.5} {
		got := float64(free.load[members[i]]) / (float64(free.total) / 4.5)
		if math.Abs(got-want) > 0.15*want {
			t.Errorf("unbounded, %s of capacity %v has %.2f times a fair capacity 1 share", members[i], want, got)
		}
	}
}

// No member places nothing, one member takes everything, and two members
// share what they can within their bounds, which with heavy units leaves
// little room: everything is still placed.
func TestFewMembers(t *testing.T) {
	units := testUnits(50, true, 4)
	if p := place(units, nil, nil, defaultFactor); len(p.owner) != 0 || p.total == 0 {
		t.Errorf("with no member, %d units are placed and the total is %d", len(p.owner), p.total)
	}
	one, caps := testMembers(1)
	p := place(units, one, caps, defaultFactor)
	if p.load[one[0]] != p.total || len(p.heavy) != 0 {
		t.Errorf("one member has %d of %d, heavy %v", p.load[one[0]], p.total, p.heavy)
	}
	two, caps := testMembers(2)
	p = place(units, two, caps, defaultFactor)
	bounds := boundsOf(two, p.capOf, p.capacity, p.total, defaultFactor)
	if p.load[two[0]]+p.load[two[1]] != p.total || p.load[two[0]] > bounds[two[0]] || p.load[two[1]] > bounds[two[1]] {
		t.Errorf("two members have %d and %d of %d, bounds %v", p.load[two[0]], p.load[two[1]], p.total, bounds)
	}
}

// A member joining takes about its fair share and moves little else, and
// one leaving moves about its own share: the weight that moves stays within
// half again what must move at the least.
func TestAMemberJoiningOrLeavingMovesLittle(t *testing.T) {
	units := testUnits(1000, true, 5)
	five, caps5 := testMembers(5)
	six, caps6 := testMembers(6)
	p5 := place(units, five, caps5, defaultFactor)
	p6 := place(units, six, caps6, defaultFactor)
	moved := int64(0)
	for _, u := range units {
		if p5.owner[u.key] != p6.owner[u.key] {
			moved += u.weight
		}
	}
	if share := float64(moved) / float64(p5.total); share > 1.5/6 {
		t.Errorf("a sixth member moved %.1f%% of the weight; at the least it takes %.1f%%", share*100, 100.0/6)
	}
	// r05 leaving the six is the five again: what moves then is what it
	// had and what its going sets in motion.
	if left := float64(p6.load["r05"]) / float64(p6.total); float64(moved)/float64(p6.total) > 1.5*left {
		t.Errorf("r05 leaving moved %.1f%% of the weight, and had %.1f%%", 100*float64(moved)/float64(p6.total), 100*left)
	}
}

// Of members of one capacity, the best score is the highest rendezvous
// hash, allocapi.Owner's choice; of members of capacities apart, it is
// worked out from logarithms in integers, which agree with math.Log2.
func TestTheWeightedScore(t *testing.T) {
	members := []string{"a", "b", "c", "d"}
	for i := range 200 {
		key := fmt.Sprintf("k%d", i)
		best := memberScore{name: members[0], hash: allocapi.Score(members[0], key), capacity: milli}
		for _, m := range members[1:] {
			s := memberScore{name: m, hash: allocapi.Score(m, key), capacity: milli}
			if s.better(&best) {
				best = s
			}
		}
		if want, _ := allocapi.Owner(members, key); best.name != want {
			t.Errorf("%s: the best score is %s's, allocapi.Owner chooses %s", key, best.name, want)
		}
	}
	for _, v := range []uint64{1, 2, 3, 1000, 1 << 40, 1<<63 - 25, 1 << 63} {
		got := float64(log2Fixed(v)) / (1 << 32)
		if want := math.Log2(float64(v)); math.Abs(got-want) > 1e-8 {
			t.Errorf("log2 %d is %v, want %v", v, got, want)
		}
	}
	// A member of twice the capacity beats one whose -log2 h is less than
	// twice its own.
	strong := memberScore{name: "x", hash: 1 << 62, capacity: 2 * milli} // h = 1/4: -log2 h = 2
	weak := memberScore{name: "y", hash: 1 << 63, capacity: milli}       // h = 1/2: -log2 h = 1
	if strong.better(&weak) || !weak.better(&strong) {
		// 2/2 = 1 against 1/1 = 1: a tie of scores, broken by the hash.
		t.Errorf("of equal scores, the higher hash ranks first")
	}
	strong.capacity = 3 * milli
	if !strong.better(&weak) {
		t.Errorf("capacity 3 at -log2 h 2 does not beat capacity 1 at -log2 h 1")
	}
}

// The balance factor is from 1 to 10 with at most three decimals.
func TestTheBalanceFactorsBounds(t *testing.T) {
	for _, f := range []float64{1, 1.25, 1.001, 10} {
		if err := CheckBalanceFactor(f); err != nil {
			t.Errorf("%v is refused: %v", f, err)
		}
	}
	for _, f := range []float64{0, 0.999, 10.001, 1.0005, math.NaN(), math.Inf(1)} {
		if err := CheckBalanceFactor(f); err == nil {
			t.Errorf("%v is taken", f)
		}
	}
}
