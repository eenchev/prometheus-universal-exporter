//go:build !select_request_types || request_type_http

package allocator

import (
	"strings"
	"testing"
)

// Each sample of /metrics stands under its own family's # TYPE line, the
// peers' families included: a sample under another family's is one a parser
// that holds to the text format takes for a series without a type.
func TestEachSampleOfTheMetricsIsUnderItsOwnFamily(t *testing.T) {
	leader, follower, _ := leaderAndFollower(t)
	for name, c := range map[string]*peerInstance{"leader": leader, "follower": follower} {
		family, samples := "", 0
		for line := range strings.SplitSeq(c.metrics(t), "\n") {
			switch {
			case strings.HasPrefix(line, "# TYPE "):
				family = strings.Fields(line)[2]
			case line == "" || strings.HasPrefix(line, "#"):
			default:
				samples++
				if series := line[:strings.IndexAny(line, "{ ")]; series != family {
					t.Errorf("the %s's %s is under the # TYPE of %s", name, line, family)
				}
			}
		}
		if samples == 0 {
			t.Errorf("the %s's metrics have no sample", name)
		}
	}
}
