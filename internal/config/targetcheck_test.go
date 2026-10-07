package config

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The check of a static target file against a configuration found each
// target's collector by going through the configuration's collectors
// (model.CollectorByName), four times for a target, and targetsChecked once
// more for each configuration. Both now find it where collectorsByName noted
// it. The two as they were are kept here, and the tests below hold the
// present ones to them.

// oldValidateStaticTargetsAgainst is ValidateStaticTargetsAgainst as it was
// while it went through the collectors for every target.
func oldValidateStaticTargetsAgainst(f *model.StaticTargetFile, c *model.Config) error {
	for i := range f.Targets {
		t := &f.Targets[i]
		if !t.ExportViaOTLP {
			continue
		}
		if !c.OTLP.Enabled {
			return fmt.Errorf("target %q sets export_via_otlp, which needs OTLP export; set otlp.enabled: true and otlp.endpoint, or leave the target to the static targets endpoint", t.Name)
		}
		if strings.TrimSpace(c.OTLP.Endpoint) == "" {
			return fmt.Errorf("target %q sets export_via_otlp, which needs otlp.endpoint", t.Name)
		}
	}
	known := map[string]bool{}
	for i := range c.Collectors {
		known[c.Collectors[i].Name] = true
	}
	for i := range f.Targets {
		t := &f.Targets[i]
		if !known[t.Collector] {
			return fmt.Errorf("target %q references unknown collector %q", t.Name, t.Collector)
		}
		if err := fetch.CheckTarget(model.CollectorByName(c, t.Collector), t.Target, true); err != nil {
			if errors.Is(err, fetch.ErrMissingTarget) {
				return fmt.Errorf("target %q has no target address", t.Name)
			}
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		if err := fetch.CheckTargetRequest(t, model.CollectorByName(c, t.Collector)); err != nil {
			return err
		}
		if err := checkRetriesFitTheInterval(t, model.CollectorByName(c, t.Collector)); err != nil {
			return err
		}
		if collector := model.CollectorByName(c, t.Collector); collector != nil {
			unused, err := fetch.CheckRequestParams(collector, fetch.TargetOverrides(t))
			var missing *fetch.MissingParamError
			if errors.As(err, &missing) {
				alternatives := "set it under the target's params, or give the placeholder a default"
				if missing.Where == "request.path" {
					alternatives = "set it under the target's params, give the placeholder a default, or set request.path on the target"
				}
				return fmt.Errorf("target %q uses collector %q, whose %s needs %s, a parameter without a default; a static target has no probe to supply it, so %s", t.Name, t.Collector, missing.Where, missing.Name, alternatives)
			}
			if err != nil {
				return fmt.Errorf("target %q uses collector %q: %w", t.Name, t.Collector, err)
			}
			if len(unused) > 0 {
				return fmt.Errorf("target %q params %s are not used by collector %q: no placeholder in its request or its label values names them", t.Name, strings.Join(unused, ", "), t.Collector)
			}
		}
	}
	return nil
}

// oldTargetsChecked is targetsChecked as it was while it went through the
// collectors for every target, whether or not the target set a message.
func oldTargetsChecked(f *model.StaticTargetFile, configs ...*model.Config) *model.Config {
	var checked model.Config
	for _, c := range configs {
		if c == nil {
			continue
		}
		for i := range f.Targets {
			if collector := model.CollectorByName(c, f.Targets[i].Collector); collector != nil && f.Targets[i].Request.Message != "" {
				checked.Collectors = append(checked.Collectors, *collector)
			}
		}
	}
	return &checked
}

// countCollectorIndexes counts, until the test or the benchmark ends, how
// many times the collectors of a configuration are gone through to note
// where each is (collectorsIndexedHook).
func countCollectorIndexes(tb testing.TB) *atomic.Int64 {
	tb.Helper()
	var count atomic.Int64
	hook := func() { count.Add(1) }
	collectorsIndexedHook.Store(&hook)
	tb.Cleanup(func() { collectorsIndexedHook.Store(nil) })
	return &count
}

// checkCorpusNames are the names the generated collectors have and the
// generated targets name: few, so that collectors share one, and one more
// that no collector has.
var checkCorpusNames = []string{"a", "b", "c", "d", ""}

// oneOf is one of values, each as likely as the others.
func oneOf[T any](r *rand.Rand, values ...T) T { return values[r.IntN(len(values))] }

// checkCorpusConfig is a generated configuration as the check may be given
// one, not a loaded one: up to six collectors, two of which may share a
// name, each of a request type the build carries or of one it does not, with
// or without placeholders in its path and a header, with and without
// defaults, and retries; and OTLP export on or off, with an endpoint, a
// blank one or none. mark tells its collectors from another configuration's
// and from each other, in a file each names.
func checkCorpusConfig(r *rand.Rand, types []string, mark int) *model.Config {
	cfg := &model.Config{}
	cfg.OTLP.Enabled = r.IntN(4) > 0
	cfg.OTLP.Endpoint = oneOf(r, "http://collector.invalid/v1/metrics", "http://collector.invalid/v1/metrics", " ", "")
	cfg.Collectors = make([]model.Collector, r.IntN(7))
	for i := range cfg.Collectors {
		c := &cfg.Collectors[i]
		c.Name = oneOf(r, checkCorpusNames...)
		c.Request.Type = oneOf(r, types...)
		if r.IntN(12) == 0 {
			c.Request.Type = "unregistered"
		}
		c.Request.ProtosetFile = fmt.Sprintf("/proto/%d-%d.protoset", mark, i)
		c.Request.Path = oneOf(r, "", "", "/status", "/tenants/{{param_tenant}}", "/tenants/{{param_tenant:acme}}", "/tenants/{{param_tenant}}/{{param_zone:eu}}")
		if r.IntN(5) == 0 {
			c.Request.Headers = map[string]string{"X-Token": "{{param_token}}"}
		}
		if r.IntN(3) == 0 {
			c.Request.Retry = model.RetryConfig{Attempts: r.IntN(4), Backoff: model.Duration(oneOf(r, 0, time.Second, 20*time.Second, 40*time.Second))}
		}
	}
	return cfg
}

// checkCorpusFile is a generated static target file as the check may be
// given one: up to six targets, each of a collector of cfg, mostly, of one
// by any of the names or of one that no configuration has, and each with
// none, one or several of
// what the check refuses, whichever comes first for it: no address or one
// its collector's type refuses, request keys of one type or another, retries
// that do not fit its interval, params that fill a placeholder, miss one or
// name none, and export over OTLP.
func checkCorpusFile(r *rand.Rand, cfg *model.Config) *model.StaticTargetFile {
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute)}
	for i := range r.IntN(7) {
		t := model.StaticTarget{Name: fmt.Sprint("t", i), Collector: oneOf(r, checkCorpusNames...), Interval: model.Duration(oneOf(r, 30*time.Second, time.Minute))}
		switch pick := r.IntN(12); {
		case pick == 0:
			t.Collector = "missing"
		case pick < 10 && len(cfg.Collectors) > 0:
			t.Collector = cfg.Collectors[r.IntN(len(cfg.Collectors))].Name
		}
		t.Target = oneOf(r, "http://target.invalid:8080", "http://target.invalid:8080", "target.invalid:9090", "target.invalid:9090", "/srv/metrics/a.txt", "", "::")
		if r.IntN(8) == 0 {
			t.Request.Method = "POST"
		}
		if r.IntN(4) == 0 {
			t.Request.Message = "{}"
		}
		if r.IntN(8) == 0 {
			t.Request.Path, t.Request.PathSet = "/own", true
		}
		if r.IntN(10) == 0 {
			t.Request.Body, t.Request.BodySet = "ping", true
		}
		if r.IntN(10) == 0 {
			t.Request.Targets = []string{"servers.a.load"}
		}
		if r.IntN(5) == 0 {
			t.Request.Retry = &model.TargetRetryConfig{Attempts: ptrTo(r.IntN(5)), Backoff: ptrTo(model.Duration(oneOf(r, 0, time.Second, 40*time.Second)))}
			if r.IntN(4) == 0 {
				t.Request.Retry.Codes = []string{"UNAVAILABLE"}
			}
			if r.IntN(4) == 0 {
				t.Request.Retry.NonIdempotent = ptrTo(true)
			}
		}
		if r.IntN(10) == 0 {
			t.Request.AcceptStatus = []string{oneOf(r, "2xx", "banana")}
		}
		if r.IntN(10) == 0 {
			t.Request.Headers = oneOf(r, map[string]string{"X-A": "v"}, map[string]string{"bad name": "v"}, map[string]string{"X-A": "v\n"})
		}
		t.Params = oneOf(r, nil, nil, map[string]string{"param_tenant": "acme"}, map[string]string{"param_tenant": "acme"}, map[string]string{"param_tenant": "acme", "param_zone": "us"}, map[string]string{"param_other": "x"}, map[string]string{"param_token": "s"}, map[string]string{"param_token": "s", "param_tenant": "acme"})
		t.ExportViaOTLP = r.IntN(10) == 0
		file.Targets = append(file.Targets, t)
	}
	return file
}

// checkRefusals are what the check refuses a target file for, each by a
// part of its message that no other has.
var checkRefusals = []string{
	"sets export_via_otlp, which needs OTLP export",
	"sets export_via_otlp, which needs otlp.endpoint",
	"references unknown collector",
	"has no registered request type",
	"has no target address",
	"which does not apply to collector",
	"the last retries could never be made",
	"a parameter without a default",
	"are not used by collector",
}

// Over generated configurations and static target files the check of a file
// against a configuration says what it said while it went through the
// collectors for every target (oldValidateStaticTargetsAgainst): nothing
// where that said nothing, and otherwise the same message, which is that of
// the first target the old check refused and of the first thing it refused
// that target for. The configurations are not loaded ones: collectors share
// a name, where the first of the name is the target's collector as it was,
// and are of a request type the build does not carry; and the targets are
// refused for everything the check refuses, in every order. The collectors
// whose descriptor files the check opens (targetsChecked) are the ones it
// found before, copies of the same collectors in the same order, for the
// configurations given in any order, one of them twice, and none; and the
// collector found by a name is the very one going through them finds.
func TestTheCheckOfATargetFileFindsEachCollectorAsGoingThroughThemDid(t *testing.T) {
	types := slices.Sorted(maps.Keys(fetch.RequestTypes))
	if len(types) == 0 {
		t.Fatal("the build carries no request type")
	}
	needsAddress := slices.ContainsFunc(types, func(name string) bool { return !fetch.RequestTypes[name].OptionalTarget })
	cases := alloctest.UnlessRaced(20_000, 2_000)
	refused := map[string]int{}
	accepted, found, shared := 0, 0, 0
	for seed := range cases {
		r := rand.New(rand.NewPCG(uint64(seed), 41))
		cfg, other := checkCorpusConfig(r, types, 0), checkCorpusConfig(r, types, 1)
		file := checkCorpusFile(r, cfg)

		was, got := oldValidateStaticTargetsAgainst(file, cfg), ValidateStaticTargetsAgainst(file, cfg)
		if (was == nil) != (got == nil) || was != nil && was.Error() != got.Error() {
			t.Fatalf("case %d: the check says %v, and said %v", seed, got, was)
		}
		if was == nil {
			accepted++
		} else {
			kind := slices.IndexFunc(checkRefusals, func(part string) bool { return strings.Contains(was.Error(), part) })
			if kind >= 0 {
				refused[checkRefusals[kind]]++
			}
		}

		configs := oneOf(r, []*model.Config{cfg}, []*model.Config{cfg, other}, []*model.Config{other, nil, cfg}, []*model.Config{cfg, cfg}, []*model.Config{nil}, nil)
		wasChecked, gotChecked := oldTargetsChecked(file, configs...), targetsChecked(file, configs...)
		if !reflect.DeepEqual(gotChecked, wasChecked) {
			t.Fatalf("case %d: the collectors whose descriptor files the check opens are those of %v, and were those of %v", seed, namedFiles(gotChecked), namedFiles(wasChecked))
		}
		found += len(wasChecked.Collectors)

		byName := collectorsByName(cfg)
		names := 0
		for _, name := range append(slices.Clone(checkCorpusNames), "missing") {
			first := model.CollectorByName(cfg, name)
			if got := byName[name]; got != first {
				t.Fatalf("case %d: the collector named %q is %p, want %p, the first so named", seed, name, got, first)
			}
			if first != nil {
				names++
			}
		}
		if names != len(byName) {
			t.Fatalf("case %d: %d names have a collector, and going through them finds %d", seed, len(byName), names)
		}
		if names < len(cfg.Collectors) {
			shared++
		}
	}
	// The corpus reaches what it is about: files the check accepts, each
	// refusal, collectors found for their message and names shared.
	least := cases / 400
	for what, count := range map[string]int{"files accepted": accepted, "collectors found for a target's message": found, "configurations with two collectors of a name": shared} {
		if count < least {
			t.Errorf("%d %s among %d cases, want at least %d", count, what, cases, least)
		}
	}
	for _, refusal := range checkRefusals {
		if refusal == "has no target address" && !needsAddress {
			continue
		}
		if refused[refusal] < least {
			t.Errorf("%d files refused with %q among %d cases, want at least %d", refused[refusal], refusal, cases, least)
		}
	}
}

// Finding the collectors whose descriptor files the check opens goes
// through the collectors of a configuration once, to note where each is, and
// not once for each target: for one target with a message and for hundreds,
// once for each configuration given, also the same one given twice, and not
// for a nil one. A target file none of whose targets sets a message, which
// is every file without a grpc collector, goes through no collectors at
// all.
func TestTheCollectorsOfATargetFilesCheckAreGoneThroughOnceForAConfiguration(t *testing.T) {
	config := func(collectors int) *model.Config {
		cfg := &model.Config{Collectors: make([]model.Collector, collectors)}
		for i := range cfg.Collectors {
			cfg.Collectors[i].Name = fmt.Sprint("collector_", i)
		}
		return cfg
	}
	file := func(targets, messages int) *model.StaticTargetFile {
		f := &model.StaticTargetFile{Targets: make([]model.StaticTarget, targets)}
		for i := range f.Targets {
			f.Targets[i] = model.StaticTarget{Name: fmt.Sprint("target_", i), Collector: fmt.Sprint("collector_", i)}
			if i >= targets-messages {
				f.Targets[i].Request.Message = "{}"
			}
		}
		return f
	}
	one, another := config(300), config(300)
	for _, tc := range []struct {
		what              string
		targets, messages int
		configs           []*model.Config
		want, found       int64
	}{
		{"no target sets a message", 300, 0, []*model.Config{one, another}, 0, 0},
		{"one target of 300 sets a message", 300, 1, []*model.Config{one}, 1, 1},
		{"every target of 300 sets a message", 300, 300, []*model.Config{one}, 1, 300},
		{"every target sets a message, for two configurations", 300, 300, []*model.Config{one, another}, 2, 600},
		{"every target sets a message, for one configuration given twice and none", 300, 300, []*model.Config{one, nil, one}, 2, 600},
		{"every target sets a message, for no configuration", 300, 300, []*model.Config{nil}, 0, 0},
		{"a file without targets", 0, 0, []*model.Config{one}, 0, 0},
	} {
		f := file(tc.targets, tc.messages)
		indexed := countCollectorIndexes(t)
		checked := targetsChecked(f, tc.configs...)
		if got := indexed.Load(); got != tc.want {
			t.Errorf("%s: the collectors of a configuration were gone through %d times, want %d", tc.what, got, tc.want)
		}
		if got := int64(len(checked.Collectors)); got != tc.found {
			t.Errorf("%s: %d collectors found, want %d", tc.what, got, tc.found)
		}
	}
}
