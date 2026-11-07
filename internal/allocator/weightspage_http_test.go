//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// weightedProbes are testProbes of weight 2.
var weightedProbes = strings.Replace(testProbes, "    collector: demo\n", "    collector: demo\n    weight: 2\n", 1)

// beatingWith moves the clock on by d in steps of a heartbeat interval,
// each member heartbeating at each with its capacity.
func beatingWith(t *testing.T, a *Allocator, clock *fakeClock, d time.Duration, caps map[string]float64) {
	t.Helper()
	for step := time.Duration(0); step <= d; step += allocapi.DefaultHeartbeatInterval {
		for m, c := range caps {
			beatWith(t, a, m, c)
		}
		clock.Advance(min(allocapi.DefaultHeartbeatInterval, d-step))
	}
}

// weightedAllocator is an allocator of a target of weight 50, 10 of weight
// 1 and the two probes of weight 2 over r1, of capacity 2, r2 and r3, the
// member list formed.
func weightedAllocator(t *testing.T) (*Allocator, *fakeClock) {
	t.Helper()
	static := "interval: 30s\ntargets:\n  - {name: big, collector: demo, target: http://big.example, weight: 50}\n" + strings.TrimPrefix(staticTargets(10), "interval: 30s\nconcurrency: 4\ntargets:\n")
	clock := newFakeClock()
	a := New(mustLoad(t, writeInputs(t, testConfig, static, weightedProbes)), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	beatingWith(t, a, clock, 20*time.Second, map[string]float64{"r1": 2, "r2": 1, "r3": 1})
	if got := effective(a); len(got) != 3 {
		t.Fatalf("the member list is %v", got)
	}
	return a, clock
}

// The Targets table has each member's weight, the static and probe weight
// it is placed by, beside its total, then the weight it keeps draining, its
// capacity and its fair share; its bar runs on weight, with its fair share
// marked; the weight's imbalance is the most a member holds of its fair
// share. An overloaded member's weight is marked and the page says why, a
// heavy unit is named with its weight and member, and the Members table
// has each member's capacity, a change of it not yet in force shown as one.
func TestTheTargetsTableShowsWeightsAndCapacities(t *testing.T) {
	a, clock := weightedAllocator(t)
	h := a.Handler("")
	sp := statusOf(t, h, "").Spread
	if sp.Overloaded != 1 || len(sp.Heavy) != 1 {
		t.Fatalf("the spread has %d overloaded members and the heavy units %+v", sp.Overloaded, sp.Heavy)
	}
	page := ask(h, "/", "").Body.String()
	requireIn(t, page,
		`<thead>
<tr><th rowspan="2">Member</th><th class="group" colspan="2"><b class="swatch seg-static"></b>Static targets</th><th class="group"><b class="swatch seg-probe"></b>Probe targets</th><th class="num" rowspan="2">Total</th><th class="num" rowspan="2">Weight</th><th class="num" rowspan="2">Draining weight</th><th class="num" rowspan="2">Capacity</th><th class="num" rowspan="2">Fair share</th><th rowspan="2">Share</th></tr>
<tr><th class="num">Owned</th><th class="num">Draining</th><th class="num">Routed</th></tr>
</thead>`,
		`<th>State</th><th class="num">Capacity</th><th class="num">Draining</th></tr></thead>`,
		"They weigh 64 over a capacity of 4: a member's fair share is the weight times its capacity over the capacity of all, and the most loaded holds "+imbalance(sp.MaxLoadRatio)+" its fair share.",
		"within 1.25 times its fair share (<code class=\"flag\">--allocation.balance-factor</code>)",
		`<span class="bad">1 member is overloaded</span>, its weight marked: above 1.25 times its fair share`,
		`<div class="notice warning" id="heavy-units">`,
		"<strong>1 unit weighs more than a member's fair share</strong>, 32.0, that of the member of the largest capacity",
		fmt.Sprintf(`<li><code class="shard-key">big</code> weighs <strong>50</strong>, on <a href="?member=%s" title="the targets of %s">%s</a></li>`, sp.Heavy[0].Member, sp.Heavy[0].Member, sp.Heavy[0].Member),
		`<span><b class="avg"></b>the member's fair share</span>`,
		`<td class="num" title="the most a member holds of its fair share">`+imbalance(sp.MaxLoadRatio)+`</td>`,
		"<tr><td>Total</td><td class=\"num\">11</td><td class=\"num\">0</td><td class=\"num\">2</td><td class=\"num\">13</td><td class=\"num\">64</td><td class=\"num\">0</td><td class=\"num\">4</td><td class=\"num\">64</td><td></td></tr>")
	most := 0.0
	for _, m := range sp.Members {
		most = max(most, m.Weight.Total, m.FairShare)
	}
	pct := func(n float64) float64 { return math.Round(n/most*1000) / 10 }
	for _, m := range sp.Members {
		_, row, _ := strings.Cut(page, `<td class="id"><a href="?member=`+m.Member+`" title="the targets of `+m.Member+`"><strong>`+m.Member+`</strong></a></td>`+"\n<td class=\"num\">")
		row, _, _ = strings.Cut(row, "</tr>")
		w := weight(m.Weight.Static + m.Weight.Probe)
		mark := `<strong>` + w + `</strong>`
		if m.Overloaded {
			mark = `<strong class="bad" title="overloaded: above 1.25 times its fair share">` + w + `</strong>`
		}
		for _, want := range []string{
			`<td class="num">` + mark + `</td>`,
			fmt.Sprintf(`<td class="num">%s</td>`+"\n"+`<td class="num">%s</td>`+"\n"+`<td class="num">%s</td>`, weight(m.Weight.Draining), weight(m.Capacity), average(m.FairShare)),
			fmt.Sprintf(`<i class="avg" style="left: %v%%" title="its fair share, %s">`, pct(m.FairShare), average(m.FairShare)),
		} {
			if !strings.Contains(row, want) {
				t.Errorf("%s's row lacks %q:\n%s", m.Member, want, row)
			}
		}
		if m.Weight.Static > 0 && !strings.Contains(row, fmt.Sprintf(`<span class="seg-static" style="width: %v%%"`, pct(m.Weight.Static))) {
			t.Errorf("%s's bar does not run on its static weight %v:\n%s", m.Member, m.Weight.Static, row)
		}
	}
	if !strings.Contains(page, `<td class="num">2</td>`+"\n"+`<td class="num">0</td>`) {
		t.Error("the Members table has no capacity 2 beside r1's draining count")
	}

	// A member's capacity changed in its heartbeats is shown beside the
	// one the member list places it by until it has held for the settle
	// time.
	beatingWith(t, a, clock, 5*time.Second, map[string]float64{"r1": 2, "r2": 3, "r3": 1})
	requireIn(t, ask(h, "/", "").Body.String(),
		`<span title="the member list places it by 1 until its heartbeats' 3 has held for the settle time">1 <span class="bad">→</span> 3</span>`)
}

// A member's view gives each target's weight in a column of its own, and
// the member's capacity; the probe jobs table of the status page, and a
// member's, the weight of a job's targets, and a job's view each target's
// weight, each member's and the job's.
func TestTheViewsShowEachTargetsWeight(t *testing.T) {
	a, _ := weightedAllocator(t)
	h := a.Handler("")
	sp := statusOf(t, h, "").Spread
	heavy := sp.Heavy[0].Member
	member := ask(h, "/?member="+heavy, "").Body.String()
	requireIn(t, member,
		`<tr><th>Capacity</th><td>`,
		`<thead><tr><th>Name</th><th>Target</th><th>Collector</th><th>Labels</th><th class="num">Interval</th><th class="num">Weight</th></tr></thead>`,
		`<td class="num" data-label="Interval"><span class="v">30s</span></td><td class="num" data-label="Weight"><span class="v">50</span></td></tr>`)
	var prober string
	for _, m := range sp.Members {
		if m.Probe > 0 {
			prober = m.Member
		}
	}
	view := ask(h, "/?member="+prober, "").Body.String()
	requireIn(t, view,
		`<thead><tr><th>Probe job</th><th>Collector</th><th class="num">Targets</th><th class="num">Weight</th></tr></thead>`,
		`<thead><tr><th>Target</th><th>Probe job</th><th>Collector</th><th class="num">Weight</th></tr></thead>`,
		`<td data-label="Collector"><span class="v">demo</span></td><td class="num" data-label="Weight"><span class="v">2</span></td></tr>`)
	mv, _ := a.MemberView(prober, "")
	if j := mv.Probes.Jobs[0]; j.Weight != 2*float64(j.Targets) {
		t.Errorf("the member's job is %+v, want a weight of 2 a target", j)
	}

	requireIn(t, ask(h, "/", "").Body.String(),
		`<th class="num" data-sort="num">Targets</th><th class="num" data-sort="num">Weight</th><th class="num" data-sort="num">Members</th>`,
		`<td class="num" data-value="2">2</td>`+"\n"+`<td class="num" data-value="4">4</td>`)
	job := ask(h, "/?job=websites", "").Body.String()
	requireIn(t, job,
		`<tr><th>Weight</th><td>4 <span class="muted">(2 a target)</span></td></tr>`,
		`<thead><tr><th>Member</th><th class="num">Targets</th><th class="num">Weight</th><th>Share</th></tr></thead>`,
		`<thead><tr><th>Target</th><th>Member</th><th>Labels</th><th class="num">Weight</th></tr></thead>`,
		`<td class="num" data-label="Weight"><span class="v">2</span></td></tr>`)
	jv, _ := a.JobView("websites", "", false)
	if jv.JobWeight != 2 || jv.WeightLabel != "" {
		t.Errorf("the job view says a weight of %v and the label %q", jv.JobWeight, jv.WeightLabel)
	}
}

// The discovery sources table says how many of a source's targets fell
// back on their owner's weight, and a job's view names its weight_label.
func TestTheSourcesTableShowsTheWeightsThatFellBack(t *testing.T) {
	static := "interval: 30s\ndiscovered_targets:\n  - name: node\n    collector: demo\n    target_template: http://{{address}}/\n    weight: 3\n    weight_label: __meta_pue_weight\n    discovery:\n      - name: nodes\n        file_sd: {files: [sd.json]}\n"
	probes := "probe_jobs:\n  - name: found\n    collector: demo\n    target_template: https://{{address}}/\n    weight: 0.5\n    weight_label: __meta_pue_weight\n    discovery:\n      - name: sites\n        file_sd: {files: [sd.json]}\n"
	p := writeInputs(t, testConfig, static, probes)
	testutil.WriteIn(t, filepath.Dir(p.StaticTargets), "sd.json", `[{"targets": ["n1:9100"], "labels": {"__meta_pue_weight": "7"}}, {"targets": ["n2:9100", "n3:9100"]}]`)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	t.Cleanup(a.BeginShutdown)
	a.refreshDiscovery(context.Background())
	h := a.Handler("")
	page := ask(h, "/", "").Body.String()
	if n := strings.Count(page, `<span class="bad">2 weights fell back</span> <span class="muted">on the owner's: their weight label gave no weight</span>`); n != 2 {
		t.Errorf("the sources table says 2 weights fell back %d times, want once for each source", n)
	}
	requireIn(t, ask(h, "/?job=found", "").Body.String(),
		`<tr><th>Weight</th><td>8 <span class="muted">(each target's from its label <code>__meta_pue_weight</code>, else the job's 0.5)</span></td></tr>`)
}
