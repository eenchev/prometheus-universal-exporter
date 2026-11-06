//go:build !select_request_types || request_type_http

package allocator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// A job is uneven where its most is further above the average than
// hashing puts a member's targets by chance, 3 targets or more and 3
// standard deviations of a member's count or more: a few targets on one
// member, as hashing spreads them as often as not, are not, and all of 4
// or more on one member of 4 are.
func TestAJobIsUnevenOnlyBeyondWhatHashingGivesByChance(t *testing.T) {
	for _, c := range []struct {
		most, routed, members int
		want                  bool
	}{
		{2, 2, 2, false}, {3, 3, 3, false}, {3, 4, 4, false}, {4, 4, 4, true},
		{5, 6, 3, false}, {6, 6, 3, true}, {5, 8, 4, false}, {8, 8, 4, true},
		{8, 20, 4, false}, {12, 20, 4, true}, {35, 100, 4, false}, {40, 100, 4, true},
		{120, 1000, 10, false}, {130, 1000, 10, true}, {5, 5, 1, false}, {0, 0, 3, false},
	} {
		if got := uneven(c.most, c.routed, c.members); got != c.want {
			t.Errorf("%d of %d targets over %d members is uneven %v, want %v", c.most, c.routed, c.members, got, c.want)
		}
	}
}

// onOneMember are n targets of prefix that hash to one member of members,
// the first's.
func onOneMember(t *testing.T, members []string, prefix string, n int) []string {
	t.Helper()
	var targets []string
	var owner string
	for i := 0; len(targets) < n; i++ {
		target := fmt.Sprintf("https://%s%d.example/", prefix, i)
		o, err := allocapi.Owner(members, target)
		if err != nil {
			t.Fatal(err)
		}
		if owner == "" {
			owner = o
		}
		if o == owner {
			targets = append(targets, target)
		}
	}
	return targets
}

// The status page and the job's view mark a job of 4 targets all on one
// member of 4 and leave unmarked one of 3 all on one member, which hashing
// gives one job in 16, and which the rule of half again the evenest split
// marked.
func TestThePagesMarkOnlyAJobHashingWouldSeldomSpreadSo(t *testing.T) {
	members := []string{"r1", "r2", "r3", "r4"}
	probes := fmt.Sprintf("probe_jobs:\n  - name: skewed\n    collector: demo\n    targets: [%s]\n  - name: few\n    collector: demo\n    targets: [%s]\n",
		strings.Join(onOneMember(t, members, "s", 4), ", "), strings.Join(onOneMember(t, members, "f", 3), ", "))
	a, _ := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, staticTargets(3), probes)), members...)
	t.Cleanup(a.BeginShutdown)
	h := a.Handler("")
	jobs := map[string]JobSpread{}
	for _, j := range statusOf(t, h, "").Spread.Jobs {
		jobs[j.Job] = j
	}
	if j := jobs["skewed"]; j.Max != 4 || !j.Uneven {
		t.Errorf("skewed is %+v, want 4 on one member, uneven", j)
	}
	if j := jobs["few"]; j.Max != 3 || j.Uneven {
		t.Errorf("few is %+v, want 3 on one member, not uneven", j)
	}
	page := ask(h, "/", "").Body.String()
	requireIn(t, page, "marked</span> where Max is further above Avg than hashing puts a member's targets by chance: by 3 targets or more, and by 3 standard deviations of a member's count or more, √(Targets × (1 − 1/Members) ÷ Members).")
	for job, marked := range map[string]bool{"skewed": true, "few": false} {
		_, row, _ := strings.Cut(page, `<a href="?job=`+job+`"`)
		row, _, _ = strings.Cut(row, "</tr>")
		if strings.Contains(row, `<span class="bad" title="Max is further above Avg`) != marked {
			t.Errorf("%s, uneven %v, has the row %s", job, marked, row)
		}
		view := ask(h, "/?job="+job, "").Body.String()
		if strings.Contains(view, `<span class="bad" title="the most is further above the average`) != marked {
			t.Errorf("the view of %s, uneven %v, says %s", job, marked, visibleText(view))
		}
	}
}
