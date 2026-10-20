// Package allocapi is what the target allocator (cmd/pue-allocator) and an
// exporter in allocator mode say to each other: the heartbeat a replica sends,
// the bundle of configuration files it is served, the paths they travel on,
// and the rendezvous hash that decides which replica owns a target. Both
// sides import it, so the two cannot disagree on a field or on an owner.
//
// A bundle is a small tree of files, keyed by their path relative to the
// bundle's root: the main configuration at ConfigFile, its collector files
// beside it at the relative paths its collector_files entries name, and this
// replica's share of the static targets at StaticTargetsFile. The replica
// writes the tree into its cache directory and loads it there exactly as it
// loads mounted files, so a bundle is checked by the same loader, and the
// cache directory is what a restart resumes from.
package allocapi

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
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

// The timings both sides keep to: a replica heartbeats every
// HeartbeatInterval, and the allocator drops a member it has not heard from
// for MemberTTL.
const (
	HeartbeatInterval = 10 * time.Second
	MemberTTL         = 30 * time.Second
)

// The files of a bundle that have a fixed place.
const (
	ConfigFile        = "config.yaml"
	StaticTargetsFile = "static-targets.yaml"
)

// Heartbeat is what a replica sends to HeartbeatPath every HeartbeatInterval.
type Heartbeat struct {
	// Member is the replica's id: its pod name, or --allocator.member.
	Member string `json:"member"`
	// Advertise is host:port Prometheus should send this replica's probes to.
	Advertise string `json:"advertise"`
	// Instance names the replica's process: random, made at its start, so
	// a restarted replica of the same Member is told from the one before.
	// Sequence counts the process's heartbeats from 1, one more for each,
	// in the order they are built. A heartbeat can reach the allocator
	// after a later one, when the replica gave up waiting for it and sent
	// the next: the allocator applies a heartbeat only when its Sequence is
	// greater than that of the last it applied of the same Instance, so a
	// late one cannot undo a later one, and a member shutting down is not
	// made live again by a heartbeat it sent before. A heartbeat of another
	// Instance is applied whatever its Sequence, as a restarted replica
	// counts from 1 again, but not one of the Instance it replaced, which
	// has ended.
	Instance string `json:"instance"`
	Sequence uint64 `json:"sequence"`
	// ShuttingDown is set from the first signal on: the replica stops being
	// live at once rather than a MemberTTL after its heartbeats stop, so no
	// probe is routed to it any more, and it leaves the member list, and
	// its static targets move, once the live members have held for the
	// allocator's settle time.
	ShuttingDown bool `json:"shutting_down,omitempty"`
	// Generation is the generation of the bundle the replica has in force,
	// empty before its first.
	Generation string `json:"generation,omitempty"`
	// Refused names the bundle generation the replica could not load, and
	// why; its last good bundle stays in force.
	Refused *Refusal `json:"refused,omitempty"`
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

// CheckFiles refuses a bundle whose file paths could reach outside the
// directory it is written into, that names a file also as a directory of
// another, such as a and a/b, which no directory can hold, or that has no
// main configuration.
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

// Owner is the member with the highest Score for key, the lower name on a
// tie. Adding a member moves only the keys it now wins; removing one moves
// only its own.
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
