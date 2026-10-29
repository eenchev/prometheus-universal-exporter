package fetch

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// An entry of request.accept_status that is text with a sign, leading zeros
// or blanks around a status, "+503", "0503" or " 503 ", is refused, of a
// collector, of its retry.statuses and of a static target, naming the form
// an entry takes: the schemas' pattern refuses it, and the exporter took it
// as the status 503. A class in either case is taken, in lower case.
func TestAStatusWithASignLeadingZerosOrBlanksIsRefused(t *testing.T) {
	entries := []string{"503", "2XX", "4xX"}
	if err := normalizeAcceptStatus(entries); err != nil {
		t.Fatal(err)
	}
	if want := []string{"503", "2xx", "4xx"}; !slices.Equal(entries, want) {
		t.Errorf("the entries are %q, want %q", entries, want)
	}
	for _, entry := range []string{"0503", "+503", " 503 ", "503 ", " 503", "+0200", "0404", " 2xx", "5XX ", "\t503", "503\n"} {
		want := fmt.Sprintf("entry %q is not an HTTP status from 100 to 599 written with its three digits, such as 503, or a class such as 2xx", entry)
		if err := normalizeAcceptStatus([]string{entry}); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %s", entry, err, want)
		}
		c := model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptStatus: []string{entry}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `collector "web" request.accept_status `+want) {
			t.Errorf("collector, %q: %v", entry, err)
		}
		c = model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Statuses: []string{entry}}}}
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `collector "web" request.retry.statuses `+want) {
			t.Errorf("retry.statuses, %q: %v", entry, err)
		}
		// A target's entries were written in lower case when its file was
		// loaded, and are named so.
		target := model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{AcceptStatus: []string{entry}}}
		NormalizeTargetRequest(&target)
		want = fmt.Sprintf("entry %q is not an HTTP status", strings.ToLower(entry))
		if err := CheckTargetRequest(&target, &model.Collector{Name: "web", Request: model.RequestConfig{Type: RequestTypeHTTP}}); err == nil || !strings.Contains(err.Error(), `request.accept_status `+want) {
			t.Errorf("target, %q: %v", entry, err)
		}
	}
}

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
