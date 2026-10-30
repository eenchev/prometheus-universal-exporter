package fetch

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// schemaStatusPattern is the pattern the schemas hold a status written as
// text to (internal/config/configschema.go).
var schemaStatusPattern = regexp.MustCompile(`^[1-5][xX][xX]$|^[1-5][0-9][0-9]$`)

// Over generated entries of digits, signs, blanks, points, letters and
// classes, the load of request.accept_status takes exactly the entries the
// schemas' pattern takes, writing a class in lower case and a status as it
// is, and refuses the first other entry in its words.
func TestAnAcceptStatusEntryIsTakenExactlyWhenTheSchemaTakesIt(t *testing.T) {
	random := rand.New(rand.NewPCG(117, 2))
	alphabet := []string{"0", "1", "2", "5", "9", "+", "-", " ", ".", "x", "X", "e", "_", "\t"}
	table := []string{"200", "0200", "+200", "-200", "00200", "2xx", "2XX", " 503 ", "600", "099", "0700", "", "+", "1e2", "200.0", "0x1F7", "5_03", "+0", "6xx", "5Xx"}
	var taken, refused int
	for round := range alloctest.UnlessRaced(20000, 4000) {
		var entries []string
		for range 1 + random.IntN(3) {
			if random.IntN(3) == 0 {
				entries = append(entries, table[random.IntN(len(table))])
				continue
			}
			var entry strings.Builder
			for range random.IntN(6) {
				entry.WriteString(alphabet[random.IntN(len(alphabet))])
			}
			entries = append(entries, entry.String())
		}
		now := slices.Clone(entries)
		err := normalizeAcceptStatus(now)
		first := slices.IndexFunc(entries, func(entry string) bool { return !schemaStatusPattern.MatchString(entry) })
		if first < 0 {
			taken++
			if err != nil {
				t.Fatalf("round %d, %q: %v, which the schema takes", round, entries, err)
			}
		} else {
			refused++
			want := fmt.Sprintf("entry %q is not an HTTP status from 100 to 599 written with its three digits, such as 503, or a class such as 2xx", entries[first])
			if err == nil || err.Error() != want {
				t.Fatalf("round %d, %q: %v, want %s", round, entries, err, want)
			}
		}
		for i := range now {
			if i <= first || first < 0 {
				if want := strings.ToLower(entries[i]); now[i] != want {
					t.Fatalf("round %d, %q: entry %d is %q, want %q", round, entries, i, now[i], want)
				}
			}
		}
	}
	if taken == 0 || refused == 0 {
		t.Fatalf("taken %d, refused %d: the entries did not try both", taken, refused)
	}
}
