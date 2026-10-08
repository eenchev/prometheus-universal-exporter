package model

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A name is shown in a log line's attribute by the rule an error quotes it
// by, without the error's quotes: a name of 200 bytes or fewer is the very
// string, and a longer one its first 200 bytes, or the one to three fewer
// that end between two characters, and its length. So for generated names of
// every length around the bound, of characters of one to four bytes and of
// bytes that are no UTF-8, what the attribute shows between quotes is what
// the error of a set refused for the name shows, to the letter; a name that
// was valid UTF-8 is shown as valid UTF-8; and the name shown holds nothing
// of the name it was cut from.
func TestANameIsShownInAnAttributeAsAnErrorShowsItWithoutTheQuotes(t *testing.T) {
	for _, c := range []struct{ what, name, want string }{
		{"an empty name", "", ""},
		{"an ordinary name", "depot_pallets", "depot_pallets"},
		{"a name with a space and a quote", `queue "depth" total`, `queue "depth" total`},
		{"a name of 200 bytes", strings.Repeat("a", 200), strings.Repeat("a", 200)},
		{"a name of 200 bytes that ends in a character of two", strings.Repeat("a", 198) + "é", strings.Repeat("a", 198) + "é"},
		{"a name of 201 bytes", strings.Repeat("a", 201), strings.Repeat("a", 200) + "... (201 bytes)"},
		{"a character of two bytes across byte 200", strings.Repeat("a", 199) + "é" + "z", strings.Repeat("a", 199) + "... (202 bytes)"},
		{"a character of three bytes across byte 200", strings.Repeat("a", 198) + "€" + "z", strings.Repeat("a", 198) + "... (202 bytes)"},
		{"a character of four bytes across byte 200", strings.Repeat("a", 197) + "𝄞" + "z", strings.Repeat("a", 197) + "... (202 bytes)"},
		{"a character that ends at byte 200", strings.Repeat("a", 198) + "é" + "z", strings.Repeat("a", 198) + "é... (201 bytes)"},
		{"bytes that are no UTF-8 at byte 200", strings.Repeat("a", 196) + "\xff\xff\xff\xff\xff\xff", strings.Repeat("a", 196) + "\xff\xff\xff\xff... (202 bytes)"},
		{"a name of a megabyte", strings.Repeat("n", 1<<20), strings.Repeat("n", 200) + "... (1048576 bytes)"},
	} {
		if got := ShownName(c.name); got != c.want {
			t.Errorf("%s is shown as %d bytes, %q, want %q", c.what, len(got), got, c.want)
		}
	}
	random := rand.New(rand.NewPCG(41, 7))
	alphabet := []string{"a", "_", ":", " ", `"`, "é", "€", "𝄞", "\xff", "\xc3"}
	as, cut := 0, 0
	for range alloctest.UnlessRaced(4000, 800) {
		var b strings.Builder
		for length := 150 + random.IntN(120); b.Len() < length; {
			b.WriteString(alphabet[random.IntN(len(alphabet))])
		}
		name := b.String()
		got := ShownName(name)
		if len(name) <= maxShownName {
			as++
			if got != name || unsafe.StringData(got) != unsafe.StringData(name) {
				t.Fatalf("a name of %d bytes is shown as %q, want the name itself", len(name), got)
			}
			continue
		}
		cut++
		head, mark, found := strings.Cut(got, "... (")
		if !found || mark != strconv.Itoa(len(name))+" bytes)" || !strings.HasPrefix(name, head) || len(head) > maxShownName || len(head) <= maxShownName-utf8.UTFMax {
			t.Fatalf("a name of %d bytes is shown as %q, want its first 200 bytes or up to three fewer and its length", len(name), got)
		}
		if utf8.ValidString(name) && !utf8.ValidString(got) {
			t.Fatalf("a name of %d bytes that is UTF-8 is shown cut inside a character: %q", len(name), got)
		}
		// What the error of the name shows of it: the same start, quoted,
		// and the same length.
		set := MetricSet{Metrics: []Metric{{Name: name, Type: GaugeMetricType}}}
		err := set.Validate(Limits{MaxMetricNameLength: 100})
		if want := fmt.Sprintf("invalid metric name %s... (%s", strconv.Quote(head), mark); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("a name shown as %q in an attribute is refused with %.400q, want an error that starts %q", got, fmt.Sprint(err), want)
		}
	}
	if floor := alloctest.UnlessRaced(800, 150); as < floor || cut < floor {
		t.Errorf("%d names were shown whole and %d by their start, want %d of each", as, cut, floor)
	}
	// The name shown is made anew: of a megabyte, some two hundred bytes
	// are allocated for it.
	huge := strings.Repeat("n", 1<<20)
	var kept string
	if size := alloctest.BytesAtMost(20, 512, func() { kept = ShownName(huge) }); size > 512 || len(kept) > 256 {
		t.Errorf("a name of a megabyte is shown in %d bytes, for which %d are allocated, want no more than 256 and 512", len(kept), size)
	}
}
