package allocator

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"slices"
	"sort"
	"strconv"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// Weighted placement (docs/ALLOCATOR.md, "Weights and capacity"). Every
// target weighs something, 1 unless its file says otherwise, and every
// member has a capacity, 1 unless its heartbeat says otherwise. The
// targets that share a shard key, static or probe, are one unit, which
// weighs what they weigh together and goes to one member. The units are
// taken in the order of a hash of their keys, and each goes to the member
// of the highest weighted rendezvous score for its key that has room for
// it: whose load with the unit stays within the balance factor times its
// fair share, the total weight times its part of the total capacity. When
// no member has room, the unit goes to the member that is least loaded for
// its capacity. No unit is ever left out.
//
// Weights, capacities and the balance factor are integer thousandths
// (milli-units), every sum and comparison is integer arithmetic, and every
// tie is broken by a hash and then a name, never by the order of a map:
// every instance of the allocator places the same inputs and the same
// members alike, whatever machine it runs on.

// The bounds of a weight, in the files and from a discovery label.
const (
	// MaxWeight is the most a target may weigh.
	MaxWeight = 1000
	// milli is one, in thousandths.
	milli = 1000
)

// The balance factor, --allocation.balance-factor: how far above its fair
// share a member's load may go before a unit that prefers it goes to the
// next member. 1 holds every member to its fair share, as near as whole
// units allow, and moves the most units when the members change; 10 lets
// a member of a list of 10 or fewer take everything its hash gives it,
// which is rendezvous hashing weighted by capacity alone.
const (
	DefaultBalanceFactor = 1.25
	MinBalanceFactor     = 1.0
	MaxBalanceFactor     = 10.0
)

// unit is targets that share a shard key: placed together.
type unit struct {
	key string
	// order is where the unit comes in the pass: a hash of its key.
	order  uint64
	weight int64
}

// placement is where every unit goes for one set of inputs and one member
// list with its capacities.
type placement struct {
	// in, members and caps are what it was made from (planFor).
	in      *Inputs
	members []string
	caps    []int64
	factor  int64
	// owner is each unit's member, by key; empty without members.
	owner map[string]string
	// load is each member's weight, static and probe, and total the weight
	// of every unit, in thousandths.
	load  map[string]int64
	total int64
	// capOf is each member's capacity, and capacity their sum.
	capOf    map[string]int64
	capacity int64
	// heavy are the units heavier than the fair share of the member of the
	// largest capacity, by key: such a unit fills the member it goes to
	// whatever the others hold.
	heavy []heavyUnit
}

// heavyUnit is a unit heavier than any member's fair share.
type heavyUnit struct {
	key    string
	weight int64
	member string
}

// units are the inputs' units: every static
// target by its shard key and every probe target of every job by its
// (shardKeyOf), the weights of the targets of one key added up.
func (in *Inputs) units() []unit {
	weights := map[string]int64{}
	var keys []string
	add := func(key string, w int64) {
		if _, ok := weights[key]; !ok {
			keys = append(keys, key)
		}
		weights[key] += w
	}
	if in.Targets != nil {
		for i := range in.Targets.Targets {
			add(in.shardKeys[i], in.staticWeight(i))
		}
	}
	if in.Probes != nil {
		for i := range in.Probes.Jobs {
			j := &in.Probes.Jobs[i]
			for _, target := range j.Targets {
				add(shardKeyOf(j, target), j.weightOfTarget(target))
			}
		}
	}
	out := make([]unit, len(keys))
	for i, key := range keys {
		out[i] = unit{key: key, order: unitOrder(key), weight: weights[key]}
	}
	return out
}

// unitOrder is where the unit of key comes in the pass: the first 8 bytes
// of SHA-256 of a prefix of its own and the key, so the order is not that of
// any member's score.
func unitOrder(key string) uint64 {
	sum := sha256.Sum256([]byte("unit\x00" + key))
	return binary.BigEndian.Uint64(sum[:8])
}

// staticWeight is the weight of the static target at i, in thousandths.
func (in *Inputs) staticWeight(i int) int64 {
	if i < len(in.weights) && in.weights[i] > 0 {
		return in.weights[i]
	}
	return milli
}

// weightOfTarget is the weight of target of j, in thousandths: a
// discovered target's own, from its weight_label, or else the job's.
func (j *ProbeJob) weightOfTarget(target string) int64 {
	if w := j.weightOf[target]; w > 0 {
		return w
	}
	if j.weight > 0 {
		return j.weight
	}
	return milli
}

// planFor is the placement of in over members of caps, the one made last
// when it was made from the same.
func (a *Allocator) planFor(in *Inputs, members []string, caps []int64) *placement {
	if p := a.plan; p != nil && p.in == in && p.factor == a.balanceFactor && slices.Equal(p.members, members) && slices.Equal(p.caps, caps) {
		return p
	}
	var units []unit
	if in != nil {
		units = in.units()
	}
	p := place(units, members, caps, a.balanceFactor)
	p.in = in
	a.plan = p
	if in == a.inputs && slices.Equal(members, a.effective) {
		a.noteHeavy(p)
	}
	return p
}

// currentPlan is the placement of the inputs in force over the member list.
func (a *Allocator) currentPlan() *placement {
	return a.planFor(a.inputs, a.effective, a.effectiveCaps)
}

// place places units over members, caps their capacities, in the order of
// the pass, whatever the order they are given in.
func place(units []unit, members []string, caps []int64, factor int64) *placement {
	units = slices.Clone(units)
	sortUnits(units)
	p := &placement{members: slices.Clone(members), caps: slices.Clone(caps), factor: factor,
		owner: make(map[string]string, len(units)), load: make(map[string]int64, len(members)), capOf: make(map[string]int64, len(members))}
	for _, u := range units {
		p.total += u.weight
	}
	if len(members) == 0 {
		return p
	}
	for i, m := range members {
		p.capOf[m] = caps[i]
		p.capacity += caps[i]
		p.load[m] = 0
	}
	bounds := boundsOf(members, p.capOf, p.capacity, p.total, factor)
	most := int64(0)
	for _, c := range caps {
		most = max(most, c)
	}
	heavy := fairShare(p.total, most, p.capacity)
	for _, u := range units {
		owner := pick(u, members, p.capOf, p.load, bounds)
		p.owner[u.key] = owner
		p.load[owner] += u.weight
		if len(members) > 1 && u.weight > heavy {
			p.heavy = append(p.heavy, heavyUnit{key: u.key, weight: u.weight, member: owner})
		}
	}
	return p
}

// boundsOf is each member's bound: factor (in thousandths) times its fair
// share, total times its capacity over capacity, rounded down.
func boundsOf(members []string, capOf map[string]int64, capacity, total, factor int64) map[string]int64 {
	out := make(map[string]int64, len(members))
	den := new(big.Int).Mul(big.NewInt(capacity), big.NewInt(milli))
	for _, m := range members {
		n := new(big.Int).Mul(big.NewInt(factor), big.NewInt(total))
		n.Mul(n, big.NewInt(capOf[m]))
		n.Quo(n, den)
		out[m] = n.Int64()
	}
	return out
}

// fairShare is total times c over capacity, rounded down.
func fairShare(total, c, capacity int64) int64 {
	if capacity <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(total), big.NewInt(c))
	return n.Quo(n, big.NewInt(capacity)).Int64()
}

// pick is the member u goes to among members: the first in score order
// with room for it, or else the least loaded for its capacity, the one of
// the higher score of those alike.
func pick(u unit, members []string, capOf, load, bounds map[string]int64) string {
	scores := make([]memberScore, len(members))
	for i, m := range members {
		scores[i] = memberScore{name: m, hash: allocapi.Score(m, u.key), capacity: capOf[m]}
	}
	// The member of the best score is taken most often: it is looked for
	// first, and the others ordered only when it has no room.
	best := 0
	for i := 1; i < len(scores); i++ {
		if scores[i].better(&scores[best]) {
			best = i
		}
	}
	if b := scores[best]; load[b.name]+u.weight <= bounds[b.name] {
		return b.name
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].better(&scores[j]) })
	for _, s := range scores {
		if load[s.name]+u.weight <= bounds[s.name] {
			return s.name
		}
	}
	least := scores[0]
	for _, s := range scores[1:] {
		if lessLoaded(load[s.name], s.capacity, load[least.name], least.capacity) {
			least = s
		}
	}
	return least.name
}

// lessLoaded reports whether a load of la for a capacity of ca is less, for
// its capacity, than lb for cb: la/ca < lb/cb, as la·cb < lb·ca in 128 bits.
func lessLoaded(la, ca, lb, cb int64) bool {
	xh, xl := bits.Mul64(uint64(la), uint64(cb)) //nolint:gosec // G115: loads and capacities are never negative
	yh, yl := bits.Mul64(uint64(lb), uint64(ca)) //nolint:gosec // G115: as above
	return xh < yh || (xh == yh && xl < yl)
}

// memberScore is a member's weighted rendezvous score for a unit: its
// capacity over -ln h, h in (0, 1) the member's rendezvous hash for the
// unit's key (allocapi.Score) as a fraction. Of members of one capacity
// the one of the higher hash has the higher score, so with every capacity
// alike the order is allocapi.Owner's; a higher capacity wins more keys,
// in proportion to it.
type memberScore struct {
	name     string
	hash     uint64
	capacity int64
	// log is -log2 h in fixed point, 32 bits of fraction, worked out when
	// first compared with a member of another capacity; logSet says it is.
	log    uint64
	logSet bool
}

// better reports whether s ranks above o: the higher score, then the higher
// hash, then the lower name. Scores are compared as fractions, capacity
// over -log2 h, crossed into products of integers, so the comparison is
// exact for the logarithms as computed, and the same on every machine.
func (s *memberScore) better(o *memberScore) bool {
	if s.capacity != o.capacity {
		ls, lo := s.negLog(), o.negLog()
		// s.capacity/ls > o.capacity/lo, a log of 0 being the best score.
		switch {
		case ls == 0 && lo != 0:
			return true
		case lo == 0 && ls != 0:
			return false
		case ls != 0:
			xh, xl := bits.Mul64(uint64(s.capacity), lo) //nolint:gosec // G115: a capacity is 100 to 100000
			yh, yl := bits.Mul64(uint64(o.capacity), ls) //nolint:gosec // G115: as above
			if xh != yh || xl != yl {
				return xh > yh || (xh == yh && xl > yl)
			}
		}
	}
	if s.hash != o.hash {
		return s.hash > o.hash
	}
	return s.name < o.name
}

// negLog is -log2 h for the score's hash, h being (hash/2 + 1) / 2^63, in
// (0, 1], in fixed point of 32 bits of fraction.
func (s *memberScore) negLog() uint64 {
	if !s.logSet {
		s.log = 63<<32 - log2Fixed(s.hash>>1+1)
		s.logSet = true
	}
	return s.log
}

// log2Fixed is log2 v for v of 1 to 2^63, in fixed point of 32 bits of
// fraction, rounded down: the whole part from the highest bit set, and each
// bit of the fraction by squaring what is left, in integers alone.
func log2Fixed(v uint64) uint64 {
	n := uint64(bits.Len64(v) - 1) //nolint:gosec // G115: v is at least 1, so 0 to 63
	// y holds v / 2^n in [1, 2) with 62 bits of fraction.
	var y uint64
	if n <= 62 {
		y = v << (62 - n)
	} else {
		y = v >> (n - 62)
	}
	result := n << 32
	for i := 31; i >= 0; i-- {
		hi, lo := bits.Mul64(y, y)
		y = hi<<2 | lo>>62
		if y >= 1<<63 {
			y >>= 1
			result |= 1 << uint(i)
		}
	}
	return result
}

// factorMilli is a balance factor in thousandths, DefaultBalanceFactor for
// 0.
func factorMilli(f float64) int64 {
	if f == 0 {
		f = DefaultBalanceFactor
	}
	return int64(math.Round(f * milli))
}

// allocationFile is the bundle's allocapi.AllocationFile for a balance
// factor of factor thousandths.
func allocationFile(factor int64) string {
	return "# The settings pue-allocator placed the targets by that are not its defaults.\nbalance_factor: " + formatMilli(factor) + "\n"
}

// formatMilli writes a number of thousandths as a decimal: 1250 is 1.25.
func formatMilli(v int64) string {
	return strconv.FormatFloat(float64(v)/milli, 'f', -1, 64)
}

// noteHeavy logs each unit of p heavier than a fair share once, while it
// stays so: it fills the member it goes to, which then holds little else,
// and a member list of more members cannot help it.
func (a *Allocator) noteHeavy(p *placement) {
	now := make(map[string]bool, len(p.heavy))
	for _, h := range p.heavy {
		now[h.key] = true
		if !a.heavy[h.key] {
			a.logger.Warn("a target, or the targets of one shard key, weigh more than a member's fair share of the weight; they go to the member least loaded for its capacity, which then holds little else", "shard_key", h.key, "weight", formatMilli(h.weight), "member", h.member, "total_weight", formatMilli(p.total), "members", len(p.members))
		}
	}
	a.heavy = now
}

// routeJob is the member each target of j is routed to, by target: the
// member of the placement when it is among the members j is routed among
// (routing), and otherwise one of those, the targets of each shard key
// placed as a unit, in order, over the loads of the placement, as the
// placement places them. A probe so goes elsewhere only while the member
// placed for it is not live or has no bundle with the job's collector yet.
// Empty while j is routed among none. Called under a.mu.
func (a *Allocator) routeJob(j *ProbeJob) map[string]string {
	pool := a.routing(j)
	out := make(map[string]string, len(j.Targets))
	if len(pool) == 0 {
		return out
	}
	plan := a.currentPlan()
	weights := map[string]int64{}
	for _, target := range j.Targets {
		key := shardKeyOf(j, target)
		if owner := plan.owner[key]; owner != "" && slices.Contains(pool, owner) {
			out[target] = owner
			continue
		}
		weights[key] += j.weightOfTarget(target)
	}
	if len(weights) == 0 {
		return out
	}
	displaced := make([]unit, 0, len(weights))
	for key, w := range weights {
		displaced = append(displaced, unit{key: key, order: unitOrder(key), weight: w})
	}
	sortUnits(displaced)
	capOf := make(map[string]int64, len(pool))
	load := make(map[string]int64, len(pool))
	capacity := int64(0)
	for _, name := range pool {
		c, ok := plan.capOf[name]
		if !ok {
			c = a.members[name].capacity
		}
		capOf[name], load[name] = c, plan.load[name]
		capacity += c
	}
	bounds := boundsOf(pool, capOf, capacity, plan.total, a.balanceFactor)
	owners := make(map[string]string, len(displaced))
	for _, u := range displaced {
		owner := pick(u, pool, capOf, load, bounds)
		owners[u.key] = owner
		load[owner] += u.weight
	}
	for _, target := range j.Targets {
		if _, done := out[target]; !done {
			out[target] = owners[shardKeyOf(j, target)]
		}
	}
	return out
}

// CheckBalanceFactor refuses a --allocation.balance-factor outside
// MinBalanceFactor and MaxBalanceFactor, or of more than three decimals,
// which the placement, in thousandths, would not keep.
func CheckBalanceFactor(f float64) error {
	m := f * milli
	if !(f >= MinBalanceFactor && f <= MaxBalanceFactor) || math.Abs(m-math.Round(m)) > 1e-6 {
		return fmt.Errorf("--allocation.balance-factor must be from 1 to 10 with at most three decimals, got %s", strconv.FormatFloat(f, 'g', -1, 64))
	}
	return nil
}

// sortUnits puts units in the order of the pass.
func sortUnits(units []unit) {
	sort.Slice(units, func(i, j int) bool {
		if units[i].order != units[j].order {
			return units[i].order < units[j].order
		}
		return units[i].key < units[j].key
	})
}
