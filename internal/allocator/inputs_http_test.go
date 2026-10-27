//go:build !select_request_types || request_type_http

package allocator

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A member's bundle is the configuration and its collector files, each as
// written, at the configuration's place and their paths relative to it, and
// the member's share of the static targets as a static target file of the
// same interval and concurrency, which the replica's loader takes. A target
// left to its default name keeps the name it has in the whole file, an
// alias and a merge key are written out as what they stand for, and the
// file's x- keys stay behind.
func TestABundleCarriesTheConfigurationAndTheMembersShare(t *testing.T) {
	static := `x-base: &base
  collector: demo
interval: 30s
concurrency: 4
targets:
  - name: first
    <<: *base
    target: http://first.example
  - name: second
    collector: other
    target: http://second.example
    labels: &labels
      team: a
  - <<: *base
    target: http://third.example
    labels: *labels
`
	p := writeInputs(t, testConfig, static, "")
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	b := a.Bundle("r1")
	if b.Files[allocapi.ConfigFile] != testConfig {
		t.Errorf("the bundle's configuration is not the file as written:\n%s", b.Files[allocapi.ConfigFile])
	}
	if b.Files["teams/a.yaml"] != testCollectorFile {
		t.Errorf("the bundle's collector file is not the file as written:\n%s", b.Files["teams/a.yaml"])
	}
	names := make([]string, 0, len(b.Files))
	for name := range b.Files {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"config.yaml", "static-targets.yaml", "teams/a.yaml"}; !slices.Equal(names, want) {
		t.Errorf("the bundle holds %v, want %v", names, want)
	}
	conf, targets := loadBundle(t, b)
	if got := testutil.CollectorNames(conf); !slices.Equal(got, []string{"demo", "other"}) {
		t.Errorf("the replica would load the collectors %v", got)
	}
	if targets.Interval != model.Duration(30*time.Second) || targets.Concurrency != 4 {
		t.Errorf("the share has the interval %v and the concurrency %d, want the file's 30s and 4", targets.Interval, targets.Concurrency)
	}
	if got := share(t, a, "r1"); !slices.Equal(got, []string{"first", "second", "demo_2"}) {
		t.Errorf("the share holds %v", got)
	}
	if third := targets.Targets[2]; third.Collector != "demo" || third.Labels["team"] != "a" {
		t.Errorf("the merged and aliased target reads as %+v", third)
	}
	text := b.Files[allocapi.StaticTargetsFile]
	if !strings.Contains(text, "  - name: demo_2\n") {
		t.Errorf("the target left to its default name does not have it written out:\n%s", text)
	}
	for _, gone := range []string{"x-base", "&", "*", "<<", "!!merge"} {
		if strings.Contains(text, gone) {
			t.Errorf("the share still holds %q:\n%s", gone, text)
		}
	}
}

// A member whose share is empty gets no static target file, which would
// have to hold a target; a member the member list does not have yet gets
// the configuration alone; and without a static target file no bundle has
// one.
func TestABundleWithoutStaticTargetsHasNoStaticTargetFile(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(1), "")
	a, _ := settledAllocator(t, mustLoad(t, p), "r1", "r2")
	empty := "r2"
	if len(share(t, a, "r1")) == 0 {
		empty = "r1"
	}
	for _, member := range []string{empty, "newcomer"} {
		if _, ok := a.Bundle(member).Files[allocapi.StaticTargetsFile]; ok {
			t.Errorf("%s's bundle has a static target file", member)
		}
		loadBundle(t, a.Bundle(member))
	}
	if a.Bundle("newcomer").Generation != a.Bundle(empty).Generation {
		t.Error("a newcomer's bundle is not the configuration alone")
	}
	p = writeInputs(t, testConfig, "", "")
	a, _ = settledAllocator(t, mustLoad(t, p), "r1")
	if _, ok := a.Bundle("r1").Files[allocapi.StaticTargetsFile]; ok {
		t.Error("a bundle has a static target file though the allocator has none")
	}
}

// The same input files and members give the same bundles: on a second
// allocator, and on this one when a reload finds the files unchanged.
func TestTheSameInputsGiveTheSameGenerations(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(40), testProbes)
	members := []string{"r1", "r2", "r3"}
	a, _ := settledAllocator(t, mustLoad(t, p), members...)
	b, _ := settledAllocator(t, mustLoad(t, p), members...)
	for _, m := range members {
		if a.Bundle(m).Generation != b.Bundle(m).Generation {
			t.Errorf("%s's bundle differs between two allocators of the same inputs", m)
		}
	}
	before := a.Bundle("r1").Generation
	if a.Reload(p) {
		t.Error("a reload of unchanged files changed the inputs")
	}
	if a.Bundle("r1").Generation != before {
		t.Error("a reload of unchanged files changed the bundle")
	}
}

// The input files the bundle cannot carry, or the exporter's loader
// refuses, are refused, each with a sentence naming what to change.
func TestInputsTheBundleCannotCarryAreRefused(t *testing.T) {
	for _, c := range []struct {
		name, config, static, probes, want string
		// extra is a file written beside the configuration.
		extra string
	}{
		{"an absolute collector file", "collector_files: [/etc/x.yaml]\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `collector_files entry "/etc/x.yaml" is an absolute path`, ""},
		{"a collector file outside the directory", "collector_files: [../x.yaml]\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `collector_files entry "../x.yaml" leaves the configuration's directory`, ""},
		{"a pattern matching the static target file", "collector_files: ['*.yaml']\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `would also match static-targets.yaml`, ""},
		{"a collector file named as the configuration", "collector_files: [teams/a.yaml, config.yaml]\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `would be config.yaml in the bundle`, "config.yaml"},
		{"a collector file named as the cache directory's own", "collector_files: [teams/a.yaml, ..b.yaml]\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `would be ..b.yaml in the bundle, but on the replicas the names beginning with ".." are the cache directory's own`, "..b.yaml"},
		{"a directory named as the cache directory's own", "collector_files: [teams/a.yaml, ..x/b.yaml]\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `collector_files entry "..x/b.yaml" begins with "..x"`, "..x/b.yaml"},
		{"a pattern of any directory", "collector_files: ['*/*.yaml']\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `collector_files entry "*/*.yaml" begins with "*", which on the replicas could also match the cache directory's own names`, ""},
		{"a pattern of hidden directories", "collector_files: ['.?*/a.yaml']\n" + strings.SplitN(testConfig, "\n", 2)[1], "", "", `begins with ".?*"`, ""},
		{"a configuration the loader refuses", "collectors: []\n", "", "", "collectors", ""},
		{"a static target of an unknown collector", testConfig, "interval: 30s\ntargets:\n  - collector: nope\n    target: http://x\n", "", `unknown collector "nope"`, ""},
		{"an empty probe targets file", testConfig, "", "# nothing\n", "is empty", ""},
		{"a probe targets file with another key", testConfig, "", "probe_jobs: []\nother: 1\n", `"other" is not allowed`, ""},
		{"no probe jobs", testConfig, "", "probe_jobs: []\n", "probe_jobs must not be empty", ""},
		{"a probe job of an unknown collector", testConfig, "", "probe_jobs:\n  - name: j\n    collector: nope\n    targets: [http://x]\n", `collector "nope" is not a collector`, ""},
		{"a probe job without a name", testConfig, "", "probe_jobs:\n  - collector: demo\n    targets: [http://x]\n", `has the name ""`, ""},
		{"a probe job named for no path", testConfig, "", "probe_jobs:\n  - name: a/b\n    collector: demo\n    targets: [http://x]\n", `has the name "a/b"`, ""},
		{"two probe jobs of one name", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    targets: [http://x]\n  - name: j\n    collector: demo\n    targets: [http://y]\n", "defined twice", ""},
		{"a probe job without targets", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n", "targets must not be empty", ""},
		{"a probe target listed twice", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    targets: [http://x, http://x]\n", "listed twice", ""},
		{"a probe target the collector cannot read", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    targets: [not a url]\n", `target "not a url"`, ""},
		{"an unknown shard key", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    shard_key: host\n    targets: [http://x]\n", `shard_key "host"`, ""},
		{"a parameter the allocator sets", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    params: {target: x}\n    targets: [http://x]\n", "sets itself", ""},
		{"a parameter that is no label name", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    params: {a-b: x}\n    targets: [http://x]\n", "__param_a-b", ""},
		{"a parameter the probe refuses", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    params: {timeout: soon}\n    targets: [http://x]\n", "timeout", ""},
		{"the instance label", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    labels: {instance: x}\n    targets: [http://x]\n", "instance", ""},
		{"a reserved label", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    labels: {__x: x}\n    targets: [http://x]\n", "__x", ""},
		{"an unknown key of a probe job", testConfig, "", "probe_jobs:\n  - name: j\n    collector: demo\n    target: http://x\n", "target", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := writeInputs(t, c.config, c.static, c.probes)
			if c.extra != "" {
				testutil.WriteIn(t, filepath.Dir(p.Config), c.extra, strings.ReplaceAll(testCollectorFile, "other", "extra"))
			}
			_, err := LoadInputs(p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

// A probe targets file may keep YAML anchors under x- keys, as the
// exporter's files may.
func TestAProbeTargetsFileMayKeepAnchorsUnderXKeys(t *testing.T) {
	probes := "x-common: &common\n  collector: demo\n  labels: {team: web}\nprobe_jobs:\n  - name: j\n    <<: *common\n    targets: [http://x]\n"
	in := mustLoad(t, writeInputs(t, testConfig, "", probes))
	if job := in.Probes.Jobs[0]; job.Collector != "demo" || job.Labels["team"] != "web" {
		t.Errorf("the job reads as %+v", job)
	}
}

// Files that are refused leave the last good ones in force, and the same
// refusal is logged and counted once however often it is read again;
// fixed files are put in force at the next reload.
func TestRefusedFilesLeaveTheLastGoodOnesInForce(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(5), "")
	logs := testutil.CaptureLogs(t)
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Clock: clock})
	beat(t, a, "r1")
	clock.Advance(allocapi.HeartbeatInterval)
	before := a.Bundle("r1").Generation
	testutil.WriteIn(t, filepath.Dir(p.Config), filepath.Base(p.StaticTargets), "interval: 30s\ntargets:\n  - collector: nope\n    target: http://x\n")
	for range 3 {
		if a.Reload(p) {
			t.Fatal("a refused file was put in force")
		}
	}
	if got := a.Bundle("r1").Generation; got != before {
		t.Error("the bundle changed with a refused file")
	}
	if got := strings.Count(logs.String(), "were refused"); got != 1 {
		t.Errorf("the refusal was logged %d times, want once:\n%s", got, logs)
	}
	if !strings.Contains(metrics(t, a), `pue_allocator_reloads_total{result="failure"} 1`) {
		t.Errorf("the refusal was not counted once:\n%s", metrics(t, a))
	}
	testutil.WriteIn(t, filepath.Dir(p.Config), filepath.Base(p.StaticTargets), staticTargets(6))
	if !a.Reload(p) {
		t.Fatal("the fixed file was not put in force")
	}
	if got := share(t, a, "r1"); len(got) != 6 {
		t.Errorf("the share after the fix holds %v", got)
	}
	if !strings.Contains(metrics(t, a), `pue_allocator_reloads_total{result="success"} 1`) {
		t.Errorf("the reload was not counted:\n%s", metrics(t, a))
	}
}

// The watch reads the files again at every tick and puts a change in force.
func TestTheWatchPutsAChangedFileInForce(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(2), "")
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	ticks := make(chan time.Time)
	done := make(chan struct{})
	defer close(done)
	go a.WatchLoop(done, ticks, p)
	testutil.WriteIn(t, filepath.Dir(p.Config), "teams/b.yaml", strings.ReplaceAll(testCollectorFile, "other", "third"))
	ticks <- time.Time{}
	testutil.WaitFor(t, "the new collector file in the bundle", func() bool {
		return a.Bundle("r1").Files["teams/b.yaml"] != ""
	})
	if err := os.Remove(filepath.Join(filepath.Dir(p.Config), "teams/b.yaml")); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Time{}
	testutil.WaitFor(t, "the removed collector file gone from the bundle", func() bool {
		_, ok := a.Bundle("r1").Files["teams/b.yaml"]
		return !ok
	})
}

// The defaults block of the configuration and of a collector file travel
// with the files, which the bundle carries as written, so a replica's
// collectors take them as the allocator's do.
func TestABundleCarriesTheDefaultsOfEachFile(t *testing.T) {
	config := "defaults:\n  limits: {max_metrics: 70}\n" + testConfig
	p := writeInputs(t, config, "", "")
	team := "defaults:\n  cache: {ttl: 45s}\n" + testCollectorFile
	testutil.WriteIn(t, filepath.Dir(p.Config), "teams/a.yaml", team)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	b := a.Bundle("r1")
	if b.Files[allocapi.ConfigFile] != config || b.Files["teams/a.yaml"] != team {
		t.Fatalf("the bundle does not carry the files as written:\n%s\n%s", b.Files[allocapi.ConfigFile], b.Files["teams/a.yaml"])
	}
	conf, _ := loadBundle(t, b)
	demo, other := conf.Collectors[0], conf.Collectors[1]
	if demo.Limits.MaxMetrics != 70 || demo.Cache.TTL != 0 || other.Limits.MaxMetrics != 10000 || other.Cache.TTL != model.Duration(45*time.Second) {
		t.Errorf("the replica's collectors take demo %+v %+v, other %+v %+v", demo.Limits, demo.Cache, other.Limits, other.Cache)
	}
}
