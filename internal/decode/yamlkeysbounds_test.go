package decode

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A key written many times is refused in memory linear in the body: the
// YAML library made a text for each two of them, 137 MB for the 1200 lines
// of a 6 kB body and more than there is for the 100,000 of a 500 kB one,
// which now cost what parsing them costs, some 120 bytes for each byte of
// the body. The error is the one the library made: the first ten problems
// in its words, and the others counted, which for 100,000 lines are
// 4,999,949,990 and for the 209,715 of a 1 MiB body 21,990,085,745;
// recognised by the one problem it is.
func TestAYAMLKeyWrittenManyTimesIsRefusedInMemoryLinearInTheBody(t *testing.T) {
	for _, count := range []int{1200, 100000, (1 << 20) / 5} {
		if raceDetector && count > 100000 {
			continue
		}
		body := []byte(strings.Repeat("a: 1\n", count))
		var err error
		allocated := alloctest.BytesAtMost(1, 250*uint64(len(body)), func() { _, err = decodeYAML(body) })
		var want strings.Builder
		want.WriteString("yaml: unmarshal errors:")
		for line := 2; line <= 11; line++ {
			want.WriteString(writtenTwice("a", line, 1))
		}
		fmt.Fprintf(&want, "\n  ... and %d more problems", count*(count-1)/2-10)
		if err == nil || err.Error() != want.String() {
			t.Fatalf("a key written %d times is refused with\n%.1000v\nwant\n%s", count, err, want.String())
		}
		if same := "yaml: unmarshal errors:" + sameWrittenTwiceA; model.SameFailureText(err) != same {
			t.Errorf("a key written %d times is recognised by %.300q, want %q", count, model.SameFailureText(err), same)
		}
		if !raceDetector && allocated > 250*uint64(len(body)) {
			// The next would cost more than there is.
			t.Fatalf("refusing a key written %d times, %d bytes, allocated %d bytes, %d for each of the body", count, len(body), allocated, allocated/uint64(len(body)))
		}
	}
}
