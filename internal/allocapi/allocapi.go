// Package allocapi is the protocol between the target allocator
// (cmd/pue-allocator) and an exporter in allocator mode: the heartbeat that
// a replica sends, the bundle of configuration files that it is served, the
// paths they travel on, and the rendezvous hash that decides which replica
// owns a target. Both sides import it, so they cannot disagree on a field or
// on an owner.
//
// A bundle is a small tree of files, keyed by their path relative to the
// bundle's root: the main configuration at ConfigFile, its collector files
// beside it at the relative paths that its collector_files entries name,
// and this replica's share of the static targets at StaticTargetsFile. The
// replica writes the tree into its cache directory and loads it there just
// as it loads mounted files, so a bundle is checked by the same loader, and
// a restart resumes from the cache directory.
package allocapi

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"time"
)

// The paths of the allocator's API.
const (
	HeartbeatPath = "/v1/heartbeat"
	BundlePath    = "/v1/bundle"
	// SDPathPrefix is followed by the probe job's name: /v1/sd/<job>.
	SDPathPrefix = "/v1/sd/"
)

// The timings that both sides follow. The allocator has one heartbeat
// interval, its --heartbeat.interval (DefaultHeartbeatInterval unless set),
// and states it in every HeartbeatReply. A replica heartbeats at the
// interval it was last told, DefaultHeartbeatInterval until then. The
// allocator drops a member that it has not heard from for MemberTTL of its
// interval (TTLIntervals intervals), so a member survives two lost
// heartbeats in a row. The interval is a single setting, the allocator's, so
// the replicas cannot heartbeat more slowly than the allocator expects.
const (
	DefaultHeartbeatInterval = 10 * time.Second
	// MinHeartbeatInterval and MaxHeartbeatInterval bound the interval. The
	// allocator refuses one outside them, and a replica takes an announced
	// one outside them as the nearest bound, so a bogus answer can neither
	// make it heartbeat without pause nor fall silent for longer than any
	// allocator waits.
	MinHeartbeatInterval = time.Second
	MaxHeartbeatInterval = 2 * time.Minute
	TTLIntervals         = 3
)

// MemberTTL is how long an allocator heartbeating at interval waits for a
// member's heartbeat before it drops the member.
func MemberTTL(interval time.Duration) time.Duration { return TTLIntervals * interval }

// ClampHeartbeatInterval is seconds, an announced heartbeat interval, as a
// duration between MinHeartbeatInterval and MaxHeartbeatInterval: one
// below them, NaN included, is the least, and one above them the most.
func ClampHeartbeatInterval(seconds float64) time.Duration {
	if !(seconds >= MinHeartbeatInterval.Seconds()) {
		return MinHeartbeatInterval
	}
	if seconds > MaxHeartbeatInterval.Seconds() {
		return MaxHeartbeatInterval
	}
	return time.Duration(seconds * float64(time.Second))
}

// The files of a bundle that have a fixed place. AllocationFile holds the
// placement settings that differ from the allocator's defaults
// (--allocation.balance-factor), and is left out with the defaults. It is in
// the bundle so that two allocator instances that place with different
// settings serve bundles of different generations, instead of shares that
// differ while their generations claim to be the same. The exporter does
// not read it.
const (
	ConfigFile        = "config.yaml"
	StaticTargetsFile = "static-targets.yaml"
	AllocationFile    = "allocation.yaml"
)

// A replica's capacity (--allocator.capacity) is its share of the targets'
// weight relative to the other members: a member of capacity 2 gets about
// twice the weight of one of capacity 1. The exporter refuses a capacity
// outside MinCapacity and MaxCapacity on its command line, and the
// allocator clamps a heartbeat's capacity to the nearest bound, so no
// heartbeat can claim nearly every target. A heartbeat that states none
// gets DefaultCapacity.
const (
	DefaultCapacity = 1.0
	MinCapacity     = 0.1
	MaxCapacity     = 100.0
)

// CapacityMilli is a heartbeat's capacity in thousandths, as the allocator
// places by it: 0 (no capacity stated) is DefaultCapacity, anything below
// MinCapacity, NaN included, is MinCapacity, anything above MaxCapacity is
// MaxCapacity, and any other value is rounded to the thousandth.
func CapacityMilli(capacity float64) int64 {
	switch {
	case capacity == 0:
		capacity = DefaultCapacity
	case !(capacity >= MinCapacity):
		capacity = MinCapacity
	case capacity > MaxCapacity:
		capacity = MaxCapacity
	}
	return int64(math.Round(capacity * 1000))
}

// Heartbeat is what a replica sends to HeartbeatPath every heartbeat
// interval.
type Heartbeat struct {
	// Member is the replica's id: its pod name, or --allocator.member.
	Member string `json:"member"`
	// Advertise is host:port Prometheus should send this replica's probes to.
	Advertise string `json:"advertise"`
	// Instance names the replica's process. It is random and made at
	// startup, so a restarted replica of the same Member can be told apart
	// from the previous one.
	//
	// Sequence counts the process's heartbeats from 1, in the order they
	// are built. A heartbeat can reach the allocator after a later one, when
	// the replica gave up waiting for it and sent the next. The allocator
	// applies a heartbeat only when its Sequence is greater than the last
	// one applied for the same Instance, so a late heartbeat cannot undo a
	// later one, and a member shutting down is not made live again by an
	// earlier heartbeat. A heartbeat of another Instance is applied whatever
	// its Sequence, since a restarted replica counts from 1 again, except
	// one of the Instance that it replaced, which has ended.
	Instance string `json:"instance"`
	Sequence uint64 `json:"sequence"`
	// ShuttingDown is set from the first signal on. The replica stops being
	// live at once, not a member TTL after its heartbeats stop, so no probe
	// is routed to it any more. It leaves the member list, and its static
	// targets move, once the live members have held for the allocator's
	// settle time.
	ShuttingDown bool `json:"shutting_down,omitempty"`
	// Generation is the generation of the bundle the replica has in force,
	// empty before its first.
	Generation string `json:"generation,omitempty"`
	// Refused names the bundle generation the replica could not load, and
	// why; its last good bundle stays in force.
	Refused *Refusal `json:"refused,omitempty"`
	// IntervalSeconds is the longest heartbeat interval, in seconds, that
	// the allocator instances found by the replica stated in their last
	// answers. It is left out until one has stated an interval, because the
	// replica's starting default belongs to no instance. The replica
	// heartbeats at the shortest of them, so this may be longer. A newly
	// started allocator instance with a shorter interval than the earlier
	// instances learns from it how long the replicas still on the older
	// interval may take to reach it, and forms its first member list no
	// sooner.
	IntervalSeconds float64 `json:"interval_seconds,omitempty"`
	// Capacity is the replica's --allocator.capacity, its share of the
	// targets' weight against the other members'; left out, the allocator
	// takes DefaultCapacity (CapacityMilli).
	Capacity float64 `json:"capacity,omitempty"`
	// StaticTargetsPath is the path the replica serves its static targets
	// at, --web.static-targets-path, its status page at it followed by
	// /status: the allocator's page links them at Advertise.
	StaticTargetsPath string `json:"static_targets_path,omitempty"`
}

// Refusal is a bundle a replica would not load.
type Refusal struct {
	Generation string `json:"generation"`
	Error      string `json:"error"`
}

// HeartbeatReply is the allocator's answer to a heartbeat.
type HeartbeatReply struct {
	// Generation is the generation of the bundle the allocator now holds
	// for the member; a replica whose bundle differs fetches it.
	Generation string `json:"generation"`
	// HeartbeatIntervalSeconds is the allocator's heartbeat interval, in
	// seconds, which the replica uses from its next heartbeat on. A replica
	// that hears it from several allocator instances takes the shortest, so
	// each instance hears it often enough. One that hears none, from an
	// allocator that does not state it, keeps its current interval.
	HeartbeatIntervalSeconds float64 `json:"heartbeat_interval_seconds,omitempty"`
	// Instance names the allocator process that answered. A replica keys
	// what each instance said by this name, not by the address it reached
	// the instance at. Behind one address (a Service's ClusterIP or a load
	// balancer), several instances may answer in turn, with different
	// intervals during a rollout; keyed by address, the stored value would
	// change at every answer. A reply without a name is keyed by its
	// address.
	Instance string `json:"instance,omitempty"`
	// SameName says the allocator hears another process heartbeat under
	// the replica's member name: two exporters were given one name, each
	// has the member's bundle, and each scrapes its share.
	SameName bool `json:"same_name,omitempty"`
}

// Bundle is what BundlePath answers for one member.
type Bundle struct {
	Generation string            `json:"generation"`
	Files      map[string]string `json:"files"`
}

// Seal sets the bundle's generation from its files: the same files give
// the same generation, on every allocator.
func (b *Bundle) Seal() { b.Generation = Generation(b.Files) }

// Generation is the digest of a bundle's files: their paths and contents,
// in path order, each length-prefixed.
func Generation(files map[string]string) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	var n [8]byte
	for _, name := range names {
		for _, part := range []string{name, files[name]} {
			binary.BigEndian.PutUint64(n[:], uint64(len(part)))
			h.Write(n[:])
			h.Write([]byte(part))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// CheckFiles refuses a bundle that has no main configuration, whose file
// paths could reach outside the directory it is written into, or that names
// a file also as a directory of another file (such as a and a/b), which no
// directory can hold.
func CheckFiles(files map[string]string) error {
	if _, ok := files[ConfigFile]; !ok {
		return fmt.Errorf("the bundle has no %s", ConfigFile)
	}
	for name := range files {
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
			return fmt.Errorf("the bundle names a file %q that is not a relative path", name)
		}
		if clean := path.Clean(name); clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("the bundle names a file %q outside its directory", name)
		}
	}
	// Checked after every name is known to be clean, so a prefix is one by
	// whole path components.
	for name := range files {
		for i := range len(name) {
			if name[i] != '/' {
				continue
			}
			if _, ok := files[name[:i]]; ok {
				return fmt.Errorf("the bundle names %q both as a file and as a directory of %q", name[:i], name)
			}
		}
	}
	return nil
}

// ErrNoMembers is Owner's answer when there is no member to own a key.
var ErrNoMembers = errors.New("no member to own the target")

// Score is the rendezvous weight of member for key: the first 8 bytes of
// SHA-256 of member, a NUL, and key.
func Score(member, key string) uint64 {
	sum := sha256.Sum256([]byte(member + "\x00" + key))
	return binary.BigEndian.Uint64(sum[:8])
}

// Owner is the member with the highest Score for key, the lower name winning
// a tie: plain rendezvous hashing. Adding a member moves only the keys that
// it now wins; removing one moves only its own keys. The allocator does not
// call it. It places a target by its weight and the members' capacities,
// using capacity-weighted rendezvous hashing with bounded loads, which ranks
// members of one capacity by Score and, with every weight and capacity
// equal and enough room, picks Owner's member. Owner is what its tests check
// against.
func Owner(members []string, key string) (string, error) {
	if len(members) == 0 {
		return "", ErrNoMembers
	}
	best, bestScore := "", uint64(0)
	for i, m := range members {
		s := Score(m, key)
		if i == 0 || s > bestScore || (s == bestScore && m < best) {
			best, bestScore = m, s
		}
	}
	return best, nil
}
