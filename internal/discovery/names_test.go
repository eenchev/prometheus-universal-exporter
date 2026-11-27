package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// targetName is a static target's name, as the configuration takes one.
var targetName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// unescapeAddress reads back what EscapeAddress wrote, left to right: _
// followed by a digit and an upper-case letter is an escaped byte, any
// other _ a dot. That it exists is what makes EscapeAddress injective.
func unescapeAddress(t *testing.T, escaped string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		switch {
		case c != '_':
			b.WriteByte(c)
		case i+2 < len(escaped) && escaped[i+1] >= '0' && escaped[i+1] <= '9' && escaped[i+2] >= 'A' && escaped[i+2] <= 'F':
			var v byte
			if _, err := fmt.Sscanf(escaped[i+1:i+3], "%02X", &v); err != nil {
				t.Fatalf("%q: %v", escaped, err)
			}
			b.WriteByte(v)
			i += 2
		default:
			b.WriteByte('.')
		}
	}
	return b.String()
}

// A discovered target's name is <entry>_ and its address escaped, readably
// for IPv4 and IPv6 addresses and lower-case host names, or, for any other
// address or a long one, the address cut and followed by _X and a digest.
func TestDiscoveredNames(t *testing.T) {
	for address, want := range map[string]string{
		"10.0.0.5:9100":          "node_10_0_0_5_3A9100",
		"[fd00::1]:9100":         "node__5Bfd00_3A_3A1_5D_3A9100",
		"node-1.example.com:443": "node_node_2D1_example_com_3A443",
		"a.b.example:9100":       "node_a_b_example_3A9100",
		"a-b.example:9100":       "node_a_2Db_example_3A9100",
		"Host.example:9100":      "node_host_example_9100_X" + hashHex("Host.example:9100"),
		"a_b.example:9100":       "node_a_b_example_9100_X" + hashHex("a_b.example:9100"),
		"https://a.example/x":    "node_https___a_example_x_X" + hashHex("https://a.example/x"),
		strings.Repeat("a", 129): "node_" + strings.Repeat("a", HashedAddressPrefix) + "_X" + hashHex(strings.Repeat("a", 129)),
	} {
		got := Name("node", address)
		if got != want {
			t.Errorf("%q is named %q, want %q", address, got, want)
		}
		if !targetName.MatchString(got) {
			t.Errorf("%q is named %q, which is no target's name", address, got)
		}
	}
}

func hashHex(address string) string {
	sum := sha256.Sum256([]byte(address))
	return hex.EncodeToString(sum[:8])
}

// EscapeAddress is read back whole for every address of its characters it
// takes, so no two of them share a name; checked over every address of up
// to 5 of those characters, and random longer ones.
func TestEscapedAddressesReadBack(t *testing.T) {
	const alphabet = "a9.:-[]"
	names := map[string]string{}
	check := func(address string) {
		escaped, ok := EscapeAddress(address)
		if !ok {
			return
		}
		if back := unescapeAddress(t, escaped); back != address {
			t.Fatalf("%q is escaped %q, read back as %q", address, escaped, back)
		}
		if other, seen := names[escaped]; seen && other != address {
			t.Fatalf("%q and %q are both escaped %q", other, address, escaped)
		}
		names[escaped] = address
	}
	var all func(prefix string, n int)
	all = func(prefix string, n int) {
		check(prefix)
		if n == 0 {
			return
		}
		for i := range len(alphabet) {
			all(prefix+alphabet[i:i+1], n-1)
		}
	}
	all("", 5)
	r := mrand.New(mrand.NewPCG(1, 2))
	for range 100000 {
		b := make([]byte, 6+r.IntN(40))
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		check(string(b))
	}
}

// No two addresses an entry finds share a name, whatever they are.
func FuzzDiscoveredNamesAreInjective(f *testing.F) {
	for _, pair := range [][2]string{
		{"a.b.example:9100", "a-b.example:9100"},
		{"a.b.example:9100", "a_b.example:9100"},
		{"a..b", "a._b"},
		{"[::1]:80", "::1:80"},
		{"10.0.0.5:9100", "10.0.0.5.9100"},
		{"x", "X"},
		{strings.Repeat("a", 200), strings.Repeat("a", 201)},
		{"a.3a", "a:"},
	} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		x, y := Name("node", a), Name("node", b)
		if !targetName.MatchString(x) {
			t.Fatalf("%q is named %q, which is no target's name", a, x)
		}
		if a != b && x == y {
			t.Fatalf("%q and %q are both named %q", a, b, x)
		}
		if a == b && x != y {
			t.Fatalf("%q is named both %q and %q", a, x, y)
		}
	})
}
