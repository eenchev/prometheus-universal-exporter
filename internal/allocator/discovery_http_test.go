//go:build !select_request_types || request_type_http

package allocator

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const discoveredProbes = `probe_jobs:
  - name: websites
    collector: demo
    labels:
      team: web
      zone: ""
    target_template: https://{{address}}/health
    targets:
      - https://dup.example:8443/health
    discovery:
      - name: cmdb
        file_sd:
          files: [sd/*.json]
        labels:
          site: __meta_site
`

const discoveredGroups = `[{"targets": ["db1.example:8443", "dup.example:8443", "bad host:1"],
  "labels": {"team": "ops", "zone": "eu", "job": "x", "instance": "y", "__param_timeout": "1s",
             "__meta_site": "ams", "owner": "me"}}]`

// A probe job's discovered targets are served beside those it writes, made
// by its target_template, each once, with its source's labels under the
// job's: the job's labels win, one written "" takes a discovered one away,
// __meta_ labels pass for relabeling, and job, instance and any other label
// starting with __ do not. A target the collector refuses is left out.
func TestDiscoveredProbeTargetsAreServed(t *testing.T) {
	p := writeInputs(t, testConfig, "", discoveredProbes)
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "sd/a.json", discoveredGroups)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	before, _ := a.SD("websites")
	if len(before) != 1 {
		t.Fatalf("before discovery the job has %d targets, want the one it writes", len(before))
	}
	a.refreshDiscovery(context.Background())
	groups, err := a.SD("websites")
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, g := range groups {
		targets = append(targets, g.Labels["__param_target"])
	}
	if want := []string{"https://dup.example:8443/health", "https://db1.example:8443/health"}; !slices.Equal(targets, want) {
		t.Fatalf("served %v, want %v", targets, want)
	}
	if got := groups[0].Labels; got["owner"] != "" || got["team"] != "web" {
		t.Errorf("the written target took a discovered target's labels: %v", got)
	}
	want := map[string]string{
		"__metrics_path__": "/probe", "__param_collector": "demo", "__param_target": "https://db1.example:8443/health",
		"instance": "https://db1.example:8443/health", MemberMetaLabel: "r1",
		"team": "web", "owner": "me", "__meta_site": "ams", "site": "ams",
	}
	if got := groups[1].Labels; len(got) != len(want) {
		t.Errorf("got the labels %v, want %v", got, want)
	} else {
		for name, value := range want {
			if got[name] != value {
				t.Errorf("label %s is %q, want %q", name, got[name], value)
			}
		}
	}
}

const discoveredStatic = `interval: 30s
targets:
  - name: legacy
    collector: demo
    target: http://legacy.example
discovered_targets:
  - name: node
    collector: demo
    target_template: http://{{address}}/metrics
    interval: 1m
    labels:
      team: infra
    discovery:
      - file_sd:
          files: [nodes.yaml]
        labels:
          rack: __meta_rack
`

const discoveredNodes = `- targets: [a-b.example:9100, c.example:9100, a.b.example:9100]
  labels: {team: other, __meta_rack: r7, job: x, region: eu}
`

// allTargets are the static targets of every member's share, by name.
func allTargets(t *testing.T, a *Allocator, members ...string) map[string]model.StaticTarget {
	t.Helper()
	out := map[string]model.StaticTarget{}
	for _, m := range members {
		b := a.Bundle(m)
		if b == nil {
			t.Fatalf("no bundle for %s", m)
		}
		if _, targets := loadBundle(t, b); targets != nil {
			for _, target := range targets.Targets {
				out[target.Name] = target
			}
		}
	}
	return out
}

// A discovered_targets entry is written out as a static target for every
// target its discovery finds, named <entry>_<address> with the address
// escaped, so a-b.example and a.b.example keep apart, and handed to the
// replicas in their shares as a written target is, with the entry's labels
// over the discovered ones and no label starting with __.
func TestDiscoveredStaticTargetsAreShared(t *testing.T) {
	p := writeInputs(t, testConfig, discoveredStatic, "")
	testutil.WriteIn(t, filepath.Dir(p.StaticTargets), "nodes.yaml", discoveredNodes)
	members := []string{"r1", "r2"}
	a, _ := settledAllocator(t, mustLoad(t, p), members...)
	if got := allTargets(t, a, members...); len(got) != 1 {
		t.Fatalf("before discovery the shares hold %d targets, want legacy alone", len(got))
	}
	a.refreshDiscovery(context.Background())
	got := allTargets(t, a, members...)
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{"legacy", "node_a_2Db_example_3A9100", "node_a_b_example_3A9100", "node_c_example_3A9100"}
	if !slices.Equal(names, want) {
		t.Fatalf("the shares hold %v, want %v", names, want)
	}
	c := got["node_c_example_3A9100"]
	if c.Target != "http://c.example:9100/metrics" || time.Duration(c.Interval) != time.Minute {
		t.Errorf("got the target %q every %s", c.Target, time.Duration(c.Interval))
	}
	if wantLabels := map[string]string{"team": "infra", "rack": "r7", "region": "eu"}; len(c.Labels) != len(wantLabels) || c.Labels["team"] != "infra" || c.Labels["rack"] != "r7" || c.Labels["region"] != "eu" {
		t.Errorf("got the labels %v, want %v", c.Labels, wantLabels)
	}
	var metrics bytes.Buffer
	if err := a.WriteMetrics(&metrics); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`pue_allocator_targets{kind="static"} 4`,
		`pue_allocator_discovered_targets{kind="static",owner="node",source="file_sd_0"} 3`,
		`pue_allocator_discovery_refreshes_total{kind="static",owner="node",source="file_sd_0",result="success"} 1`,
		`pue_allocator_discovery_refreshes_total{kind="static",owner="node",source="file_sd_0",result="failure"} 0`,
		`pue_allocator_discovery_last_success_timestamp_seconds{kind="static",owner="node",source="file_sd_0"} 1792152020`,
	} {
		if !strings.Contains(metrics.String(), line) {
			t.Errorf("the metrics have no %s:\n%s", line, metrics.String())
		}
	}
}

// A change of what discovery finds is put in force as a reload is: a
// member's bundle changes only when its share does, a target no longer
// found leaves at once, and a reload of the files that moves discovered
// targets, by a changed shard_key, hands them over for one interval while
// the sources keep what they found.
func TestDiscoveryChangesAreAppliedAsAReload(t *testing.T) {
	p := writeInputs(t, testConfig, discoveredStatic, "")
	dir := filepath.Dir(p.StaticTargets)
	var targets []string
	for i := range 40 {
		targets = append(targets, "n"+string(rune('a'+i%26))+string(rune('a'+i/26))+".example:9100")
	}
	write := func(list []string) {
		testutil.WriteIn(t, dir, "nodes.yaml", "- targets: ["+strings.Join(list, ", ")+"]\n")
	}
	write(targets)
	members := []string{"r1", "r2", "r3"}
	a, clock := settledAllocator(t, mustLoad(t, p), members...)
	ctx := context.Background()
	a.refreshDiscovery(ctx)
	generations := func() map[string]string {
		out := map[string]string{}
		for _, m := range members {
			out[m] = a.Bundle(m).Generation
		}
		return out
	}
	before := generations()

	// One more target changes its owner's bundle alone.
	write(append(slices.Clone(targets), "new.example:9100"))
	a.refreshDiscovery(ctx)
	owner := ""
	for _, m := range members {
		if slices.Contains(share(t, a, m), "node_new_example_3A9100") {
			owner = m
		}
	}
	after := generations()
	for _, m := range members {
		if changed := after[m] != before[m]; changed != (m == owner) {
			t.Errorf("%s's bundle changed %v, its share took the new target %v", m, changed, m == owner)
		}
	}

	// A target no longer found leaves at once.
	write(targets[1:])
	a.refreshDiscovery(ctx)
	gone := discoveredName("node", targets[0])
	for _, m := range members {
		if slices.Contains(share(t, a, m), gone) || slices.Contains(share(t, a, m), "node_new_example_3A9100") {
			t.Errorf("%s keeps a target discovery no longer finds", m)
		}
	}

	// A reload allocating by address moves targets, which are handed over,
	// and the source keeps what it found: no target is missing meanwhile.
	ownerOf := func() map[string]string {
		out := map[string]string{}
		for _, m := range members {
			for _, name := range share(t, a, m) {
				out[name] += m
			}
		}
		return out
	}
	was := ownerOf()
	testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(discoveredStatic, "    interval: 1m\n", "    interval: 1m\n    shard_key: address\n", 1))
	if !a.Reload(p) {
		t.Fatal("the reload changed nothing")
	}
	moved := 0
	for name, owners := range ownerOf() {
		if len(owners) > 2 {
			moved++
			if !strings.Contains(owners, was[name]) {
				t.Errorf("%s moved to %s without its old owner %s keeping it", name, owners, was[name])
			}
		}
	}
	if moved == 0 || len(ownerOf()) != len(was) {
		t.Fatalf("%d of %d targets moved, now %d targets; want some moved and none missing", moved, len(was), len(ownerOf()))
	}
	keepBeating(t, a, clock, time.Minute+time.Second, members...)
	for name, owners := range ownerOf() {
		if len(owners) != 2 {
			t.Errorf("%s is still in the shares of %s after its interval", name, owners)
		}
	}
}

// A source that has never answered adds nothing, and the allocator serves
// the rest; a source's answer over --discovery.max-targets is refused.
func TestAFailingSourceLeavesTheRest(t *testing.T) {
	probes := `probe_jobs:
  - name: websites
    collector: demo
    targets: [https://shop.example]
    discovery:
      - http_sd:
          url: http://127.0.0.1:1/sd
      - file_sd:
          files: [sd.json]
`
	p := writeInputs(t, testConfig, "", probes)
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "sd.json", `[{"targets":["https://a.example","https://b.example"]}]`)
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t), MaxDiscoveredTargets: 1})
	beat(t, a, "r1")
	keepBeating(t, a, clock, 20*time.Second, "r1")
	a.refreshDiscovery(context.Background())
	groups, err := a.SD("websites")
	if err != nil || len(groups) != 1 || groups[0].Labels["__param_target"] != "https://shop.example" {
		t.Errorf("got %v, %v; want the written target alone", groups, err)
	}
}

// What a probe job's or a discovered_targets entry's discovery is checked
// for at load, beside what checkDiscovery checks of each source.
func TestDiscoveredInputsAreCheckedAtLoad(t *testing.T) {
	job := func(rest string) string {
		return "probe_jobs:\n  - name: websites\n    collector: demo\n" + rest
	}
	source := "    discovery:\n      - file_sd: {files: [sd.json]}\n"
	for name, c := range map[string]struct{ probes, want string }{
		"no targets nor discovery":   {job(""), "unless discovery finds them"},
		"a template without address": {job(source + "    target_template: https://x/\n"), "has no {{address}}"},
		"a template alone":           {job("    targets: [https://a.example]\n    target_template: https://{{address}}/\n"), "without discovery"},
		"an unknown source key":      {job("    discovery:\n      - file_sd: {files: [sd.json]}\n        bogus: 1\n"), "bogus"},
		"a bad source":               {job("    discovery:\n      - http_sd: {url: ftp://x}\n"), "absolute http or https URL"},
	} {
		_, err := LoadInputs(writeInputs(t, testConfig, "", c.probes))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, c.want)
		}
	}
	entry := func(rest string) string {
		return "interval: 30s\ndiscovered_targets:\n  - name: node\n    collector: demo\n" + rest
	}
	good := "    target_template: http://{{address}}/\n" + source
	for name, c := range map[string]struct{ static, want string }{
		"no name":              {"interval: 30s\ndiscovered_targets:\n  - collector: demo\n" + good, "name \"\" is required"},
		"a bad name":           {"interval: 30s\ndiscovered_targets:\n  - name: a-b\n    collector: demo\n" + good, "letters, digits and underscores"},
		"no discovery":         {entry("    target_template: http://{{address}}/\n"), "has no discovery"},
		"a target":             {entry(good + "    target: http://x/\n"), "target is set"},
		"a bad shard_key":      {entry(good + "    shard_key: db\n"), "shard_key \"db\""},
		"no URL for http":      {entry(source), "checked with the address 192.0.2.1:9100"},
		"an unknown key":       {entry(good + "    bogus: 1\n"), "bogus"},
		"an unknown collector": {"interval: 30s\ndiscovered_targets:\n  - name: node\n    collector: nope\n" + good, "nope"},
		"twice":                {entry(good) + "  - name: node\n    collector: demo\n" + good, "defined twice"},
		"a name's prefix":      {entry(good) + "  - name: node_eu\n    collector: demo\n" + good, `entry "node_eu" begins with entry "node"'s name and _`},
		"no list":              {"interval: 30s\ndiscovered_targets: {}\n", "must be a list"},
		"neither":              {"interval: 30s\n", "targets must not be empty"},
	} {
		_, err := LoadInputs(writeInputs(t, testConfig, c.static, ""))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, c.want)
		}
	}
	// A file of discovered_targets alone is taken, and serves no static
	// target until one is found.
	in := mustLoad(t, writeInputs(t, testConfig, entry(good), ""))
	if len(in.Targets.Targets) != 0 || len(in.entries) != 1 || len(in.sources) != 1 {
		t.Errorf("got %d targets, %d entries and %d sources", len(in.Targets.Targets), len(in.entries), len(in.sources))
	}
	a, _ := settledAllocator(t, in, "r1")
	if b := a.Bundle("r1"); b == nil || b.Files["static-targets.yaml"] != "" {
		t.Errorf("a share of no target was served: %v", b)
	}
}

// A discovered target's name depends on its entry, its address and the
// file alone: it is not renamed when another target that looked like it
// comes or goes, and one named as a target the file writes takes the
// digest form, which stays while the file does.
func TestADiscoveredTargetKeepsItsName(t *testing.T) {
	static := strings.Replace(discoveredStatic, "  - name: legacy\n", "  - name: node_c_example_3A9100\n", 1)
	p := writeInputs(t, testConfig, static, "")
	dir := filepath.Dir(p.StaticTargets)
	testutil.WriteIn(t, dir, "nodes.yaml", discoveredNodes)
	members := []string{"r1", "r2"}
	a, _ := settledAllocator(t, mustLoad(t, p), members...)
	ctx := context.Background()
	names := func() []string {
		var out []string
		for name := range allTargets(t, a, members...) {
			out = append(out, name)
		}
		slices.Sort(out)
		return out
	}
	a.refreshDiscovery(ctx)
	hashed := "node_" + hashedAddress("c.example:9100")
	want := []string{"node_a_2Db_example_3A9100", "node_a_b_example_3A9100", "node_c_example_3A9100", hashed}
	slices.Sort(want)
	if got := names(); !slices.Equal(got, want) {
		t.Fatalf("the shares hold %v, want %v", got, want)
	}
	if got := allTargets(t, a, members...)[hashed].Target; got != "http://c.example:9100/metrics" {
		t.Errorf("%s is the target %q", hashed, got)
	}
	testutil.WriteIn(t, dir, "nodes.yaml", "- targets: [a.b.example:9100, c.example:9100]\n")
	a.refreshDiscovery(ctx)
	want = []string{"node_a_b_example_3A9100", "node_c_example_3A9100", hashed}
	slices.Sort(want)
	if got := names(); !slices.Equal(got, want) {
		t.Errorf("once a-b.example is gone the shares hold %v, want %v: no target renamed", got, want)
	}
}

// StartDiscovery asks every source at once and puts what it finds in
// force without a test's hand, and BeginShutdown stops the sources.
func TestStartDiscoveryRefreshesOnItsOwn(t *testing.T) {
	p := writeInputs(t, testConfig, "", discoveredProbes)
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "sd/a.json", discoveredGroups)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	a.StartDiscovery()
	testutil.WaitFor(t, "the discovered target to be served", func() bool {
		groups, _ := a.SD("websites")
		return len(groups) == 2
	})
	a.BeginShutdown()
	a.dmu.Lock()
	defer a.dmu.Unlock()
	for _, s := range a.sources {
		if s.cancel != nil {
			t.Error("a source still runs after the shutdown began")
		}
	}
}

// refreshDiscovery asks every source once, in turn, and puts what they
// found in force: what a test does in place of the refresh loops.
func (a *Allocator) refreshDiscovery(ctx context.Context) {
	a.dmu.Lock()
	sources := make([]*sourceState, 0, len(a.sources))
	for _, s := range a.sources {
		sources = append(sources, s)
	}
	a.dmu.Unlock()
	sort.Slice(sources, func(i, j int) bool { return sources[i].spec.key < sources[j].spec.key })
	changed := false
	for _, s := range sources {
		changed = a.refreshSource(ctx, s) || changed
	}
	if changed {
		a.applyDiscovery()
	}
}

// A reload that changes a source's settings, its refresh_interval say,
// replaces it with a source of the new settings, which starts with what the
// old one found: the targets stay in the bundles until the new source
// answers, rather than leaving every share and coming back a refresh later.
// A first failure of the new source keeps them too, and is counted.
func TestAChangedSourceKeepsItsTargetsUntilItAnswers(t *testing.T) {
	p := writeInputs(t, testConfig, discoveredStatic, "")
	dir := filepath.Dir(p.StaticTargets)
	testutil.WriteIn(t, dir, "nodes.yaml", discoveredNodes)
	members := []string{"r1", "r2"}
	a, _ := settledAllocator(t, mustLoad(t, p), members...)
	ctx := context.Background()
	a.refreshDiscovery(ctx)
	names := func() []string {
		var out []string
		for name := range allTargets(t, a, members...) {
			out = append(out, name)
		}
		slices.Sort(out)
		return out
	}
	before := names()
	if len(before) != 4 {
		t.Fatalf("the shares hold %v, want the written target and three discovered ones", before)
	}

	testutil.WriteIn(t, dir, "targets.yaml", strings.Replace(discoveredStatic, "files: [nodes.yaml]", "files: [nodes.yaml]\n          refresh_interval: 2m", 1))
	if !a.Reload(p) {
		t.Fatal("the reload changed nothing")
	}
	if got := names(); !slices.Equal(got, before) {
		t.Fatalf("after the reload the shares hold %v, want %v until the source answers", got, before)
	}
	testutil.WriteIn(t, dir, "nodes.yaml", "not: [a list")
	a.refreshDiscovery(ctx)
	if got := names(); !slices.Equal(got, before) {
		t.Errorf("after the new source's first failure the shares hold %v, want %v", got, before)
	}
	if m := a.sourceMetrics(); len(m) != 1 || m[0].failures != 1 || m[0].successes != 1 || m[0].found != 3 {
		t.Errorf("the source's metrics are %+v, want the old source's success and the new one's failure, with its 3 targets", m)
	}
	testutil.WriteIn(t, dir, "nodes.yaml", "- targets: [d.example:9100]\n")
	a.refreshDiscovery(ctx)
	if got := names(); !slices.Equal(got, []string{"legacy", "node_d_example_3A9100"}) {
		t.Errorf("once the new source answers the shares hold %v", got)
	}

	// A probe job's kubernetes source whose label_selector changes keeps
	// its targets in the service discovery answer the same way.
	k := newKubeAPI(t)
	k.pages["/apis/discovery.k8s.io/v1/namespaces/db/endpointslices?"] = `{"metadata":{},"items":[` + slice("postgres", `{"addresses":["10.0.0.2"]}`) + `]}`
	probes := "probe_jobs:\n  - name: databases\n    collector: demo\n    target_template: https://{{address}}/\n    discovery:\n      - kubernetes: {port: metrics, label_selector: app=pg}\n"
	p = writeInputs(t, testConfig, "", probes)
	a, _ = settledAllocator(t, mustLoad(t, p), members...)
	a.refreshDiscovery(ctx)
	served := func() int {
		groups, _ := a.SD("databases")
		return len(groups)
	}
	if served() != 1 {
		t.Fatalf("the job serves %d targets, want the one discovered", served())
	}
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "probes.yaml", strings.Replace(probes, "app=pg", "app in (pg)", 1))
	if !a.Reload(p) {
		t.Fatal("the reload changed nothing")
	}
	if served() != 1 {
		t.Errorf("after a reload changing the label_selector the job serves %d targets, want 1 until the new source answers", served())
	}
	testutil.WriteIn(t, k.dir, "token", "a-revoked-token\n")
	a.refreshDiscovery(ctx)
	if served() != 1 {
		t.Errorf("after the new source's first failure the job serves %d targets, want 1", served())
	}

	// A source of another name is another source, and starts with nothing.
	testutil.WriteIn(t, filepath.Dir(p.ProbeTargets), "probes.yaml", strings.Replace(probes, "      - kubernetes:", "      - name: pg\n        kubernetes:", 1))
	if !a.Reload(p) {
		t.Fatal("the reload changed nothing")
	}
	if served() != 0 {
		t.Errorf("a source of another name serves %d targets before it answers, want none", served())
	}
}
