package allocapi

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// scoreOracle is the rendezvous weight as the contract words it, written
// out again: the first 8 bytes of SHA-256 of member, NUL, key.
func scoreOracle(member, key string) uint64 {
	h := sha256.New()
	h.Write([]byte(member))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

// Owner is the member of the highest score, whatever the order the members
// are given in, and there is no owner without members.
func TestOwnerIsTheMemberOfTheHighestScore(t *testing.T) {
	if _, err := Owner(nil, "k"); !errors.Is(err, ErrNoMembers) {
		t.Errorf("no members: got %v, want ErrNoMembers", err)
	}
	members := []string{"r1", "r2", "r3", "r4", "r5"}
	reversed := []string{"r5", "r4", "r3", "r2", "r1"}
	for i := range 500 {
		key := fmt.Sprintf("target-%d", i)
		want, best := "", uint64(0)
		for j, m := range members {
			if s := scoreOracle(m, key); j == 0 || s > best {
				want, best = m, s
			}
			if Score(m, key) != scoreOracle(m, key) {
				t.Fatalf("Score(%s, %s) is not the first 8 bytes of SHA-256 of member, NUL, key", m, key)
			}
		}
		for _, list := range [][]string{members, reversed} {
			if got, err := Owner(list, key); err != nil || got != want {
				t.Fatalf("Owner(%v, %s) = %s %v, want %s", list, key, got, err, want)
			}
		}
	}
}

// Adding a member moves only the keys it now owns, and about 1/(N+1) of
// them; removing it again, read the other way, moves only its own.
func TestOwnerMovesOnlyWhatAMemberChangeMust(t *testing.T) {
	before := []string{"r1", "r2", "r3"}
	after := []string{"r1", "r2", "r3", "r4"}
	moved := 0
	const keys = 4000
	for i := range keys {
		key := fmt.Sprintf("k%d", i)
		was, _ := Owner(before, key)
		is, _ := Owner(after, key)
		if was != is {
			moved++
			if is != "r4" {
				t.Fatalf("%s moved from %s to %s, not to the new member", key, was, is)
			}
		}
	}
	if moved < keys/4-250 || moved > keys/4+250 {
		t.Errorf("adding a fourth member moved %d of %d keys, far from a quarter", moved, keys)
	}
}

// The generation depends on the files alone: the same files give the same
// generation whatever order the map holds them in, any change to a name or
// a content changes it, and a name and a content cannot trade bytes.
func TestGenerationIsTheDigestOfTheFiles(t *testing.T) {
	a := map[string]string{"config.yaml": "collectors: []\n", "teams/a.yaml": "x"}
	b := map[string]string{"teams/a.yaml": "x", "config.yaml": "collectors: []\n"}
	if Generation(a) != Generation(b) {
		t.Error("the same files gave two generations")
	}
	if len(Generation(a)) != 32 {
		t.Errorf("the generation %q is not 32 hex digits", Generation(a))
	}
	for _, other := range []map[string]string{
		{"config.yaml": "collectors: []\n", "teams/a.yaml": "y"},
		{"config.yaml": "collectors: []\n", "teams/b.yaml": "x"},
		{"config.yaml": "collectors: []\n"},
	} {
		if Generation(other) == Generation(a) {
			t.Errorf("%v has the generation of %v", other, a)
		}
	}
	if Generation(map[string]string{"ab": "c"}) == Generation(map[string]string{"a": "bc"}) {
		t.Error("a name and a content traded a byte and kept the generation")
	}
	bundle := &Bundle{Files: a}
	bundle.Seal()
	if bundle.Generation != Generation(a) {
		t.Errorf("Seal set %s, want %s", bundle.Generation, Generation(a))
	}
}

// CheckFiles takes a bundle of relative paths inside its directory with a
// configuration, and refuses every other.
func TestCheckFilesRefusesPathsOutsideTheBundle(t *testing.T) {
	ok := map[string]string{ConfigFile: "", StaticTargetsFile: "", "teams/a.yaml": "", "a/b/c.yaml": ""}
	if err := CheckFiles(ok); err != nil {
		t.Errorf("a good bundle was refused: %v", err)
	}
	if err := CheckFiles(map[string]string{StaticTargetsFile: ""}); err == nil {
		t.Error("a bundle without its configuration was taken")
	}
	for _, name := range []string{"", "/etc/passwd", "../x", "a/../../x", "a/../b", "./a", "a//b", "a/", ".", "..", `a\b`, "a\x00b"} {
		if err := CheckFiles(map[string]string{ConfigFile: "", name: ""}); err == nil {
			t.Errorf("the file name %q was taken", name)
		}
	}
}
