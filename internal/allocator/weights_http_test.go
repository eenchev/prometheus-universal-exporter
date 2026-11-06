//go:build !select_request_types || request_type_http

package allocator

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// beatWith sends a heartbeat of member with capacity.
func beatWith(t *testing.T, a *Allocator, member string, capacity float64) {
	t.Helper()
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: member, Advertise: member + ":9115", Capacity: capacity})); err != nil {
		t.Fatal(err)
	}
}

// spreadRowOf is member's row of the status's spread.
func spreadRowOf(t *testing.T, a *Allocator, member string) SpreadMember {
	t.Helper()
	for _, r := range statusOf(t, a.Handler(""), "").Spread.Members {
		if r.Member == member {
			return r
		}
	}
	t.Fatalf("the spread has no row of %s", member)
	return SpreadMember{}
}

// What the files say of weights is checked at load: a weight is a number
// above 0 and at most 1000 of at most three decimals, wherever it is
// written, and a weight_label a label name; a refusal names the file and
// the target, entry or job.
func TestWeightsAreCheckedAtLoad(t *testing.T) {
	entry := func(rest string) string {
		return "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\ndiscovered_targets:\n  - name: node\n    collector: demo\n    target_template: http://{{address}}/\n    discovery:\n      - file_sd: {files: [sd.json]}\n" + rest
	}
	job := func(rest string) string {
		return "probe_jobs:\n  - name: websites\n    collector: demo\n    targets: [https://a.example]\n" + rest
	}
	for name, c := range map[string]struct{ static, probes string }{
		"weights everywhere": {
			static: "interval: 1m\ndefault_weight: 2\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\n    weight: 0.25\n  - name: b\n    collector: demo\n    target: http://b.example\ndiscovered_targets:\n  - name: node\n    collector: demo\n    target_template: http://{{address}}/\n    weight: 1000\n    weight_label: __meta_pue_weight\n    discovery:\n      - file_sd: {files: [sd.json]}\n",
			probes: "default_weight: 0.001\nprobe_jobs:\n  - name: websites\n    collector: demo\n    targets: [https://a.example]\n    weight: 3.5\n  - name: found\n    collector: demo\n    weight_label: weight\n    discovery:\n      - file_sd: {files: [sd.json]}\n",
		},
	} {
		in, err := LoadInputs(writeInputs(t, testConfig, c.static, c.probes))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(in.weights, []int64{250, 2000}) || in.entries[0].weight != 1000*milli || in.entries[0].weightLabel != "__meta_pue_weight" {
			t.Errorf("%s: static weights %v, entry %+v", name, in.weights, in.entries[0])
		}
		if in.Probes.Jobs[0].weight != 3500 || in.Probes.Jobs[1].weight != 1 || in.Probes.Jobs[1].WeightLabel != "weight" {
			t.Errorf("%s: job weights %d and %d", name, in.Probes.Jobs[0].weight, in.Probes.Jobs[1].weight)
		}
	}
	for name, c := range map[string]struct{ static, probes, want string }{
		"a default_weight of 0":      {static: "interval: 1m\ndefault_weight: 0\ntargets:\n  - {name: a, collector: demo, target: http://a.example}\n", want: "static target file %s: default_weight 0 is not a weight; " + weightRule},
		"a target's weight of text":  {static: "interval: 1m\ntargets:\n  - {name: a, collector: demo, target: http://a.example, weight: heavy}\n", want: `static target file %s: target 0 (a) weight "heavy" is not a number`},
		"a target's weight too high": {static: "interval: 1m\ntargets:\n  - {name: a, collector: demo, target: http://a.example, weight: 1000.5}\n", want: "target 0 (a) weight 1000.5 is not a weight"},
		"four decimals":              {static: "interval: 1m\ntargets:\n  - {name: a, collector: demo, target: http://a.example, weight: 0.0001}\n", want: "weight 0.0001 is not a weight"},
		"a weight_label on a target": {static: "interval: 1m\ntargets:\n  - {name: a, collector: demo, target: http://a.example, weight_label: w}\n", want: "target 0 (a) sets weight_label"},
		"an entry's weight infinite": {static: entry("    weight: .inf\n"), want: `entry "node" weight .inf is not a weight`},
		"an entry's bad label":       {static: entry("    weight_label: 'a-b'\n"), want: `entry "node" weight_label "a-b" is not a label name`},
		"a probe default_weight":     {probes: "default_weight: -1\n" + job(""), want: "probe targets file %s: default_weight -1 is not a weight"},
		"a job's weight":             {probes: job("    weight: 1001\n"), want: `probe job "websites": weight 1001 is not a weight`},
		"a job's weight of text":     {probes: job("    weight: '2'\n"), want: "cannot unmarshal"},
		"a job's bad label":          {probes: job("    discovery:\n      - file_sd: {files: [sd.json]}\n    weight_label: 9w\n"), want: `probe job "websites": weight_label "9w" is not a label name`},
		"a label without discovery":  {probes: job("    weight_label: w\n"), want: `probe job "websites": weight_label is set without discovery`},
	} {
		p := writeInputs(t, testConfig, c.static, c.probes)
		file := p.StaticTargets
		if c.probes != "" {
			file = p.ProbeTargets
		}
		want := c.want
		if strings.Contains(want, "%s") {
			want = fmt.Sprintf(want, file)
		}
		if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", name, err, want)
		}
	}
}

// A member's share carries no weight: the allocator takes the weights out,
// and the exporter's loader, which knows no weight, takes the share as it
// is.
func TestASharesStaticTargetsCarryNoWeight(t *testing.T) {
	static := "interval: 30s\ndefault_weight: 2\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\n    weight: 5\n  - name: b\n    collector: demo\n    target: http://b.example\n"
	a, _ := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, static, "")), "r1")
	t.Cleanup(a.BeginShutdown)
	b := a.Bundle("r1")
	if text := b.Files[allocapi.StaticTargetsFile]; strings.Contains(text, "weight") {
		t.Errorf("the share carries weights:\n%s", text)
	}
	if got := share(t, a, "r1"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("the share holds %v", got)
	}
	if r := spreadRowOf(t, a, "r1"); r.Weight.Static != 7 || r.Static != 2 {
		t.Errorf("r1 has %+v, want 2 targets weighing 7", r)
	}
}

// A discovered target takes its weight from its owner's weight_label, and
// its owner's weight when the label gives none: missing, not a number, out
// of range or of too many decimals. Each fallback is counted as it begins,
// by owner and reason, and logged once while its source and reason stay so;
// the target is never left out.
func TestADiscoveredWeightFallsBackOnItsOwnersWeight(t *testing.T) {
	static := "interval: 30s\ntargets:\n  - name: a\n    collector: demo\n    target: http://a.example\ndiscovered_targets:\n  - name: node\n    collector: demo\n    target_template: http://{{address}}/\n    weight: 3\n    weight_label: __meta_pue_weight\n    discovery:\n      - name: nodes\n        file_sd: {files: [sd.json]}\n"
	probes := "probe_jobs:\n  - name: found\n    collector: demo\n    target_template: https://{{address}}/\n    weight: 0.5\n    weight_label: __meta_pue_weight\n    discovery:\n      - name: sites\n        file_sd: {files: [sd.json]}\n"
	p := writeInputs(t, testConfig, static, probes)
	sd := filepath.Join(filepath.Dir(p.StaticTargets), "sd.json")
	write := func(text string) { testutil.WriteIn(t, filepath.Dir(sd), "sd.json", text) }
	write(`[{"targets": ["n1:9100"], "labels": {"__meta_pue_weight": "7"}},
	 {"targets": ["n2:9100"]},
	 {"targets": ["n3:9100"], "labels": {"__meta_pue_weight": "lots"}},
	 {"targets": ["n4:9100"], "labels": {"__meta_pue_weight": "0"}},
	 {"targets": ["n5:9100"], "labels": {"__meta_pue_weight": "1.0001"}}]`)
	clock := newFakeClock()
	logs := &syncBuffer{}
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1")
	keepBeating(t, a, clock, 20*time.Second, "r1")
	a.refreshDiscovery(context.Background())

	in := a.currentInputs()
	weights := map[string]int64{}
	for i := range in.Targets.Targets {
		weights[in.Targets.Targets[i].Name] = in.staticWeight(i)
	}
	if len(weights) != 6 || weights["a"] != milli || weights["node_n1_3A9100"] != 7*milli {
		t.Errorf("static weights %v", weights)
	}
	for _, name := range []string{"node_n2_3A9100", "node_n3_3A9100", "node_n4_3A9100", "node_n5_3A9100"} {
		if weights[name] != 3*milli {
			t.Errorf("%s weighs %d, want the entry's 3000", name, weights[name])
		}
	}
	job := &in.Probes.Jobs[0]
	if len(job.Targets) != 5 || job.weightOfTarget("https://n1:9100/") != 7*milli || job.weightOfTarget("https://n2:9100/") != 500 {
		t.Errorf("probe targets %v weigh %d and %d", job.Targets, job.weightOfTarget("https://n1:9100/"), job.weightOfTarget("https://n2:9100/"))
	}
	metrics := func() string {
		var b strings.Builder
		if err := a.WriteMetrics(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for _, want := range []string{
		`pue_allocator_weight_label_fallbacks_total{kind="probe",owner="found",reason="missing"} 1`,
		`pue_allocator_weight_label_fallbacks_total{kind="probe",owner="found",reason="not_a_number"} 1`,
		`pue_allocator_weight_label_fallbacks_total{kind="static",owner="node",reason="out_of_range"} 1`,
		`pue_allocator_weight_label_fallbacks_total{kind="static",owner="node",reason="too_many_decimals"} 1`,
	} {
		if !strings.Contains(metrics(), want+"\n") {
			t.Errorf("/metrics lacks %s:\n%s", want, metrics())
		}
	}
	if n := strings.Count(logs.String(), "give no weight in their weight_label"); n != 8 {
		t.Errorf("logged %d fallbacks, want one for each owner, source and reason, 8:\n%s", n, logs)
	}
	for _, s := range statusOf(t, a.Handler(""), "").Sources {
		if s.WeightFallbacks != 4 {
			t.Errorf("the source %s/%s has %d fallbacks, want 4", s.Owner, s.Name, s.WeightFallbacks)
		}
	}

	// The same again is neither counted nor logged again; a target that
	// stops falling back, and falls back again, is counted again.
	write(`[{"targets": ["n1:9100"], "labels": {"__meta_pue_weight": "7"}},
	 {"targets": ["n2:9100"], "labels": {"__meta_pue_weight": "2"}},
	 {"targets": ["n3:9100"], "labels": {"__meta_pue_weight": "lots"}},
	 {"targets": ["n4:9100"], "labels": {"__meta_pue_weight": "0"}},
	 {"targets": ["n5:9100"], "labels": {"__meta_pue_weight": "1.0001"}}]`)
	a.refreshDiscovery(context.Background())
	write(`[{"targets": ["n1:9100"], "labels": {"__meta_pue_weight": "7"}},
	 {"targets": ["n2:9100"]},
	 {"targets": ["n3:9100"], "labels": {"__meta_pue_weight": "lots"}},
	 {"targets": ["n4:9100"], "labels": {"__meta_pue_weight": "0"}},
	 {"targets": ["n5:9100"], "labels": {"__meta_pue_weight": "1.0001"}}]`)
	a.refreshDiscovery(context.Background())
	if want := `pue_allocator_weight_label_fallbacks_total{kind="static",owner="node",reason="missing"} 2`; !strings.Contains(metrics(), want+"\n") {
		t.Errorf("/metrics lacks %s:\n%s", want, metrics())
	}
	if want := `pue_allocator_weight_label_fallbacks_total{kind="static",owner="node",reason="not_a_number"} 1`; !strings.Contains(metrics(), want+"\n") {
		t.Errorf("/metrics lacks %s:\n%s", want, metrics())
	}
	if n := strings.Count(logs.String(), "give no weight in their weight_label"); n != 10 {
		t.Errorf("logged %d fallbacks, want 10: the missing ones of each owner again once they came back:\n%s", n, logs)
	}
}

// A discovered weight that moves less than 20% from the weight in force
// moves no target and changes no bundle; one that moves more is taken.
func TestAWaveringWeightLabelMovesNothing(t *testing.T) {
	static := "interval: 30s\ndiscovered_targets:\n  - name: node\n    collector: demo\n    target_template: http://{{address}}/\n    weight_label: __meta_pue_weight\n    discovery:\n      - file_sd: {files: [sd.json]}\n"
	p := writeInputs(t, testConfig, static, "")
	dir := filepath.Dir(p.StaticTargets)
	write := func(w string) {
		testutil.WriteIn(t, dir, "sd.json", `[{"targets": ["n1:9100"], "labels": {"__meta_pue_weight": "`+w+`"}}, {"targets": ["n2:9100"]}]`)
	}
	write("10")
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1", "r2")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2")
	a.refreshDiscovery(context.Background())
	weight := func() int64 {
		in := a.currentInputs()
		return in.staticWeight(in.targetIndex["node_n1_3A9100"])
	}
	generations := func() string { return a.Bundle("r1").Generation + a.Bundle("r2").Generation }
	if weight() != 10*milli {
		t.Fatalf("n1 weighs %d", weight())
	}
	before := generations()
	for _, w := range []string{"11", "8.5", "11.9", "9"} {
		write(w)
		a.refreshDiscovery(context.Background())
		if weight() != 10*milli || generations() != before {
			t.Errorf("a label of %s moved the weight in force to %d, or changed a bundle", w, weight())
		}
	}
	write("12")
	a.refreshDiscovery(context.Background())
	if weight() != 12*milli {
		t.Errorf("a label of 12 left the weight %d", weight())
	}
}

// Static and probe targets are placed in one pass over the same members: a
// member's load is its static and its probe weight, so the probes go to the
// members the static targets leave room on.
func TestStaticAndProbeTargetsShareTheMembersLoad(t *testing.T) {
	var static strings.Builder
	static.WriteString("interval: 30s\ntargets:\n")
	for i := range 40 {
		fmt.Fprintf(&static, "  - {name: s%02d, collector: demo, target: http://s%02d.example, weight: %d}\n", i, i, 1+i%5)
	}
	var probes strings.Builder
	probes.WriteString("probe_jobs:\n  - name: sites\n    collector: demo\n    weight: 2\n    targets:\n")
	for i := range 40 {
		fmt.Fprintf(&probes, "      - https://p%02d.example\n", i)
	}
	members := []string{"r1", "r2", "r3"}
	a, _ := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, static.String(), probes.String())), members...)
	t.Cleanup(a.BeginShutdown)
	st := statusOf(t, a.Handler(""), "").Spread
	if st.TotalWeight.Static != 120 || st.TotalWeight.Probe != 80 || st.Capacity != 3 || st.BalanceFactor != DefaultBalanceFactor {
		t.Fatalf("the spread weighs %+v over a capacity of %v", st.TotalWeight, st.Capacity)
	}
	for _, r := range st.Members {
		load := r.Weight.Static + r.Weight.Probe
		if load > DefaultBalanceFactor*200/3 || r.FairShare < 66.6 || r.FairShare > 66.7 || r.Capacity != 1 || r.Overloaded {
			t.Errorf("%s weighs %v of a fair share of %v: %+v", r.Member, load, r.FairShare, r)
		}
		if diff := r.LoadRatio - load/r.FairShare; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s has a load ratio of %v for %v of %v", r.Member, r.LoadRatio, load, r.FairShare)
		}
	}
	// The probes alone, placed after the static targets of each member,
	// would not have spread so: their weight is spread by what the members
	// already hold.
	metrics := func() string {
		var b strings.Builder
		if err := a.WriteMetrics(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}()
	for _, r := range st.Members {
		for _, want := range []string{
			fmt.Sprintf(`pue_allocator_assigned_weight{kind="probe",member="%s"} %v`, r.Member, r.Weight.Probe),
			fmt.Sprintf(`pue_allocator_assigned_weight{kind="static",member="%s"} %v`, r.Member, r.Weight.Static),
			fmt.Sprintf(`pue_allocator_member_capacity{member="%s"} 1`, r.Member),
			fmt.Sprintf(`pue_allocator_member_load_ratio{member="%s"} `, r.Member),
		} {
			if !strings.Contains(metrics, want) {
				t.Errorf("/metrics lacks %s:\n%s", want, metrics)
			}
		}
	}
	if !strings.Contains(metrics, "pue_allocator_overloaded_members 0\n") {
		t.Errorf("/metrics counts overloaded members:\n%s", metrics)
	}
}

// The static targets of one shard_key are one unit: they go to one member,
// and weigh there together.
func TestAShardKeyGroupStaysTogether(t *testing.T) {
	var static strings.Builder
	static.WriteString("interval: 30s\ntargets:\n")
	for i := range 30 {
		fmt.Fprintf(&static, "  - {name: s%02d, collector: demo, target: http://s%02d.example, shard_key: db%d, weight: %d}\n", i, i, i%6, 1+i%4)
	}
	members := []string{"r1", "r2", "r3", "r4"}
	in := mustLoad(t, writeInputs(t, testConfig, static.String(), ""))
	a, _ := settledAllocator(t, in, members...)
	t.Cleanup(a.BeginShutdown)
	owner := map[string]string{}
	for _, m := range members {
		for _, name := range share(t, a, m) {
			i := in.targetIndex[name]
			if o, seen := owner[in.shardKeys[i]]; seen && o != m {
				t.Errorf("%s of %s is on %s, and others of it on %s", name, in.shardKeys[i], m, o)
			}
			owner[in.shardKeys[i]] = m
		}
	}
	if len(owner) != 6 {
		t.Errorf("the groups are on %v", owner)
	}
}

// A unit heavier than a fair share is logged once while it stays so and
// shown in the status; it is never left out.
func TestAHeavyUnitIsLoggedOnceAndShown(t *testing.T) {
	static := "interval: 30s\ntargets:\n  - {name: big, collector: demo, target: http://big.example, weight: 50}\n" + strings.TrimPrefix(staticTargets(10), "interval: 30s\nconcurrency: 4\ntargets:\n")
	clock := newFakeClock()
	logs := &syncBuffer{}
	a := New(mustLoad(t, writeInputs(t, testConfig, static, testProbes)), Options{Settle: 15 * time.Second, Clock: clock, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r1", "r2", "r3")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2", "r3")
	// Members that come and go rebuild the bundles; the unit stays heavy.
	beat(t, a, "r4")
	keepBeating(t, a, clock, 20*time.Second, "r1", "r2", "r3", "r4")
	if n := strings.Count(logs.String(), "weigh more than a member's fair share"); n != 1 {
		t.Errorf("logged the heavy unit %d times:\n%s", n, logs)
	}
	st := statusOf(t, a.Handler(""), "").Spread
	if len(st.Heavy) != 1 || st.Heavy[0].ShardKey != "big" || st.Heavy[0].Weight != 50 || st.Heavy[0].Member == "" {
		t.Fatalf("the status shows the heavy units %+v", st.Heavy)
	}
	if got := share(t, a, st.Heavy[0].Member); !slices.Contains(got, "big") {
		t.Errorf("big is not in %s's share %v", st.Heavy[0].Member, got)
	}
	if st.Overloaded != 1 || st.MaxLoadRatio <= DefaultBalanceFactor {
		t.Errorf("overloaded %d, the most loaded at %v", st.Overloaded, st.MaxLoadRatio)
	}
}

// A member a whole unit puts a little over its bound is not overloaded:
// 3 targets of weight 1 over 2 members at the default balance factor, a
// bound of 1.875 each, and 11 over 2 at a balance factor of 1, a bound of
// 5.5, leave one member a unit over it, which is the placement working. A
// target of weight 5 among four of 1 at a balance factor of 1, too heavy
// for the bound of 4 of either member, overloads the member it is on.
func TestAMemberAWholeUnitPutsOverItsBoundIsNotOverloaded(t *testing.T) {
	for _, c := range []struct {
		static     string
		factor     float64
		overloaded int
	}{
		{staticTargets(3), 0, 0},
		{staticTargets(11), 1, 0},
		{"interval: 30s\ntargets:\n  - {name: five, collector: demo, target: http://five.example, weight: 5}\n" + strings.TrimPrefix(staticTargets(4), "interval: 30s\nconcurrency: 4\ntargets:\n"), 1, 1},
	} {
		a, _ := settledAllocatorWith(t, mustLoad(t, writeInputs(t, testConfig, c.static, "")), Options{BalanceFactor: c.factor}, "r1", "r2")
		st := statusOf(t, a.Handler(""), "").Spread
		marked := 0
		for _, m := range st.Members {
			if m.Overloaded {
				marked++
			}
		}
		if st.Overloaded != c.overloaded || marked != c.overloaded || !strings.Contains(metricsOf(t, a), fmt.Sprintf("pue_allocator_overloaded_members %d\n", c.overloaded)) {
			t.Errorf("%d targets at the balance factor %v: %d overloaded, %d marked, want %d (spread %+v)", len(a.inputs.Targets.Targets), c.factor, st.Overloaded, marked, c.overloaded, st.Members)
		}
	}
}

// A static target that moves to a member that joins weighs on its new
// owner, and on the member that keeps it draining only as draining.
func TestADrainingTargetWeighsOnItsNewOwner(t *testing.T) {
	var static strings.Builder
	static.WriteString("interval: 30s\ntargets:\n")
	for i := range 30 {
		fmt.Fprintf(&static, "  - {name: s%02d, collector: demo, target: http://s%02d.example, weight: %d}\n", i, i, 1+i%3)
	}
	in := mustLoad(t, writeInputs(t, testConfig, static.String(), ""))
	a, clock := settledAllocator(t, in, "r1", "r2")
	t.Cleanup(a.BeginShutdown)
	beat(t, a, "r3")
	keepBeating(t, a, clock, 15*time.Second, "r1", "r2", "r3")
	if got := effective(a); len(got) != 3 {
		t.Fatalf("the member list is %v", got)
	}
	st := statusOf(t, a.Handler(""), "").Spread
	r3 := spreadRowOf(t, a, "r3")
	if r3.Draining != 0 || r3.Weight.Static == 0 {
		t.Fatalf("r3 has %+v", r3)
	}
	draining := 0.0
	for _, r := range st.Members {
		draining += r.Weight.Draining
	}
	if draining == 0 || st.TotalWeight.Static != 60 || st.TotalWeight.Draining != draining {
		t.Errorf("draining weighs %v, the static targets %v", draining, st.TotalWeight)
	}
	for _, m := range []string{"r1", "r2"} {
		r := spreadRowOf(t, a, m)
		owned := 0.0
		for _, name := range owned2(t, a, m) {
			owned += float64(in.staticWeight(in.targetIndex[name])) / milli
		}
		if r.Weight.Static != owned {
			t.Errorf("%s weighs %v of the static targets, but owns %v", m, r.Weight.Static, owned)
		}
	}
}

// owned2 are the static targets member owns, without those it keeps
// draining, by name.
func owned2(t *testing.T, a *Allocator, member string) []string {
	t.Helper()
	view, ok := a.MemberView(member, "")
	if !ok {
		t.Fatalf("no member %s", member)
	}
	var names []string
	for _, st := range view.Static.List {
		names = append(names, st.Name)
		if st.Weight == 0 {
			t.Errorf("%s has no weight in the view", st.Name)
		}
	}
	return names
}

// A heartbeat's capacity is held to 0.1 to 100, and one that says none is
// 1; a changed capacity takes effect, as a member's coming does, once it has
// held for the settle time, and the member of capacity 3 then has about
// three times the weight.
func TestAHeartbeatsCapacity(t *testing.T) {
	in := mustLoad(t, writeInputs(t, testConfig, staticTargets(600), ""))
	clamped, _ := settledAllocator(t, in, "r0")
	t.Cleanup(clamped.BeginShutdown)
	for capacity, want := range map[float64]float64{1000: 100, 0.001: 0.1, -5: 0.1, 0: 1, 2.5: 2.5, 1.0004: 1} {
		beatWith(t, clamped, "r3", capacity)
		for _, m := range statusOf(t, clamped.Handler(""), "").Members.List {
			if m.ID == "r3" && m.Capacity != want {
				t.Errorf("a heartbeat of capacity %v is taken as %v, want %v", capacity, m.Capacity, want)
			}
		}
	}
	a, clock := settledAllocator(t, in, "r1", "r2")
	t.Cleanup(a.BeginShutdown)
	beatWith(t, a, "r1", 3)
	before := a.Bundle("r1").Generation
	clock.Advance(10 * time.Second)
	beatWith(t, a, "r1", 3)
	beat(t, a, "r2")
	if a.Bundle("r1").Generation != before {
		t.Error("a changed capacity moved targets before the settle time")
	}
	clock.Advance(6 * time.Second)
	beatWith(t, a, "r1", 3)
	beat(t, a, "r2")
	r1, r2 := spreadRowOf(t, a, "r1"), spreadRowOf(t, a, "r2")
	if r1.Capacity != 3 || r2.Capacity != 1 {
		t.Fatalf("the capacities are %v and %v", r1.Capacity, r2.Capacity)
	}
	if ratio := r1.Weight.Static / r2.Weight.Static; ratio < 2.6 || ratio > 3.4 {
		t.Errorf("r1 of capacity 3 has %v times r2's weight", ratio)
	}
	for _, m := range statusOf(t, a.Handler(""), "").Members.List {
		if m.ID == "r1" && m.CapacityInList != 3 {
			t.Errorf("r1's capacity in the list is %v", m.CapacityInList)
		}
	}
}

// Two instances of the allocator of the same input files and the same
// heartbeats, heard in any order, serve the same shares of the same
// generations; one of another balance factor serves other generations,
// whether or not its shares differ.
func TestTheBalanceFactorIsInTheGeneration(t *testing.T) {
	var static strings.Builder
	static.WriteString("interval: 30s\ntargets:\n")
	for i := range 60 {
		fmt.Fprintf(&static, "  - {name: s%02d, collector: demo, target: http://s%02d.example, weight: %d}\n", i, i, 1+i%7)
	}
	p := writeInputs(t, testConfig, static.String(), testProbes)
	members := []string{"r1", "r2", "r3", "r4"}
	generations := func(opts Options, order []string) map[string]string {
		clock := newFakeClock()
		opts.Settle, opts.Clock, opts.Logger = 15*time.Second, clock, testutil.QuietLogger(t)
		a := New(mustLoad(t, p), opts)
		t.Cleanup(a.BeginShutdown)
		for step := 0; step < 4; step++ {
			for i, m := range order {
				beatWith(t, a, m, float64(1+i%2))
			}
			clock.Advance(6 * time.Second)
		}
		out := map[string]string{}
		for _, m := range members {
			out[m] = a.Bundle(m).Generation
		}
		return out
	}
	// The capacities go by the order of the heartbeats; the same capacities
	// in another order are the reverse of the members taken two by two.
	one := generations(Options{}, members)
	two := generations(Options{BalanceFactor: DefaultBalanceFactor}, []string{"r3", "r2", "r1", "r4"})
	for _, m := range members {
		if one[m] != two[m] {
			t.Errorf("%s: two instances alike serve %s and %s", m, one[m], two[m])
		}
	}
	other := generations(Options{BalanceFactor: 10}, members)
	for _, m := range members {
		if other[m] == one[m] {
			t.Errorf("%s: a balance factor of 10 serves the generation of 1.25, %s", m, one[m])
		}
	}
	a, _ := settledAllocatorWith(t, mustLoad(t, p), Options{BalanceFactor: 2}, "r1")
	if text := a.Bundle("r1").Files[allocapi.AllocationFile]; !strings.Contains(text, "balance_factor: 2\n") {
		t.Errorf("the bundle's %s is %q", allocapi.AllocationFile, text)
	}
	if b, _ := settledAllocator(t, mustLoad(t, p), "r1"); b.Bundle("r1").Files[allocapi.AllocationFile] != "" {
		t.Errorf("the default balance factor puts %s in the bundle", allocapi.AllocationFile)
	}
	loadBundle(t, a.Bundle("r1"))
}

// A collector file the bundle would carry as its allocation.yaml, or an
// entry that would match it, is refused at load, as one of its
// static-targets.yaml is.
func TestACollectorFileCannotBeTheAllocationFile(t *testing.T) {
	for _, entry := range []string{"allocation.yaml", "a*.yaml"} {
		p := writeInputs(t, strings.Replace(testConfig, "collector_files: [teams/*.yaml]", "collector_files: [teams/*.yaml, '"+entry+"']", 1), "", "")
		testutil.WriteIn(t, filepath.Dir(p.Config), "allocation.yaml", strings.ReplaceAll(testCollectorFile, "other", "third"))
		if _, err := LoadInputs(p); err == nil || !strings.Contains(err.Error(), "the settings the allocator placed the targets by") && !strings.Contains(err.Error(), "would be allocation.yaml in the bundle") {
			t.Errorf("%s: got %v", entry, err)
		}
	}
}
