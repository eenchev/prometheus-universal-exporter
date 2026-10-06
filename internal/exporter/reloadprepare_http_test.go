//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"weak"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A reload works out the fingerprints of its configuration before it puts
// the configuration in force (prepareReload, config.Manager's OnPrepare): the
// tests here hold, by counting where a collector's definition is encoded and
// never by time, that none is encoded once the configuration is in force, by
// the reload's own following or by a probe that comes before it, that each
// is still encoded once for a reload, and that what is kept of them does not
// grow with the reloads.

// reloadWindow is a server over a configuration file the test rewrites, whose
// reloads the test can hold in the window in which a probe follows the
// configuration itself: where the reload has put its configuration in force
// and has not yet followed it. It holds a reload there while inForce is set,
// by calling it.
type reloadWindow struct {
	*reloadable
	inForce atomic.Pointer[func()]
}

func newReloadWindow(t *testing.T, document string) *reloadWindow {
	t.Helper()
	w := &reloadWindow{reloadable: &reloadable{t: t, path: filepath.Join(t.TempDir(), "config.yaml")}}
	if err := os.WriteFile(w.path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(w.path)
	if err != nil {
		t.Fatal(err)
	}
	w.manager = config.NewManager(cfg, w.path, testutil.QuietLogger(t))
	w.manager.SetPythonPath("python3")
	// Told before the server is, which follows the reload when it is told.
	w.manager.OnInstall(func() {
		if held := w.inForce.Load(); held != nil {
			(*held)()
		}
	})
	w.server = NewServer(w.manager, "python3", testutil.QuietLogger(t))
	return w
}

// A configuration goes in force with the fingerprint of every one of its
// collectors made, so following it encodes no definition: a reload of 100
// collectors (10 under the race detector) that leaves them as they were,
// changes every one, or removes half and adds as many has encoded, where its
// configuration is in force and not yet followed, each collector of the
// configuration once, and encodes none from there to its end, where it
// encoded all of them. The first reload after the start has also encoded by
// then the collectors it started with and kept, which nothing had asked for.
// None is encoded with the statistics lock held.
func TestAReloadedConfigurationGoesInForceWithItsFingerprintsMade(t *testing.T) {
	n := alloctest.UnlessRaced(100, 10)
	for _, shape := range followBenchShapes {
		documents := [2]string{shape.first(n), shape.second(n)}
		w := newReloadWindow(t, documents[0])
		encoded, locked := countFingerprints(t, w.server)
		var before *model.Config
		var whenInForce int64
		var wasFollowed, wasInForce bool
		noted := func() {
			whenInForce = encoded.Load()
			wasInForce = w.manager.Get() != before
			wasFollowed = w.server.followed.Load().config != before
		}
		w.inForce.Store(&noted)
		for reload := 1; reload <= 3; reload++ {
			encoded.Store(0)
			before = w.manager.Get()
			w.reloadTo(documents[reload%2])
			if !wasInForce || wasFollowed {
				t.Fatalf("%s: where reload %d was looked at, its configuration was in force %v and followed %v, want in force and not yet followed", shape.name, reload, wasInForce, wasFollowed)
			}
			if followed := w.server.followed.Load(); followed.config != w.manager.Get() {
				t.Fatalf("%s: reload %d returned with its configuration not followed", shape.name, reload)
			}
			if after := encoded.Load() - whenInForce; after != 0 {
				t.Errorf("%s: reload %d encoded %d collectors' definitions once its configuration was in force, want none: they are made before", shape.name, reload, after)
			}
			if reload > 1 && whenInForce != int64(n) {
				t.Errorf("%s: reload %d had encoded %d collectors' definitions when its configuration went in force, want %d, each collector of it once", shape.name, reload, whenInForce, n)
			} else if reload == 1 && (whenInForce < int64(n) || whenInForce > int64(2*n)) {
				t.Errorf("%s: the first reload had encoded %d collectors' definitions when its configuration went in force, want %d to %d, each collector of it once and those it kept of the configuration it started with", shape.name, whenInForce, n, 2*n)
			}
			if locked.Load() != 0 {
				t.Errorf("%s: reload %d encoded %d collectors' definitions while the statistics lock was held, want none", shape.name, reload, locked.Load())
			}
		}
	}
}

// A probe that comes where a reload has put its configuration in force and
// has not yet followed it follows the configuration itself, and encodes no
// collector's definition to do so, where it encoded every one: it is
// answered by the new configuration, one generation on, and the reload then
// finds its following made and encodes none either. So it is for the first
// reload after the start, when nothing had asked for the fingerprints of the
// configuration the exporter started with, and for a later one, where the
// probe of the collector the reload left as it was is answered with the
// result cached for it and the one of the collector it changed by its
// target.
func TestAProbeBeforeAReloadIsFollowedEncodesNoCollector(t *testing.T) {
	target, requests, _ := failableTarget(t)
	documents := []string{
		cachedDocument("kept", "changed"),
		strings.Replace(cachedDocument("kept", "changed"), "changed_value", "renamed_value", 1),
		strings.Replace(cachedDocument("kept", "changed"), "changed_value", "again_value", 1),
	}
	w := newReloadWindow(t, documents[0])
	encoded, _ := countFingerprints(t, w.server)
	for reload := 1; reload <= 2; reload++ {
		reached, resume := make(chan struct{}), make(chan struct{})
		held := func() {
			close(reached)
			<-resume
		}
		w.inForce.Store(&held)
		before := w.server.followed.Load()
		w.write(w.path, documents[reload])
		encoded.Store(0)
		reloaded := make(chan error, 1)
		go func() { reloaded <- w.manager.Reload(config.ReloadTriggerSignal) }()
		<-reached
		if w.manager.Get() == before.config || w.server.followed.Load() != before {
			t.Fatalf("reload %d: where the reload is held, its configuration is not in force, or is followed already", reload)
		}
		whenInForce, asked := encoded.Load(), requests.Load()
		// The two collectors of its configuration, and for the first reload
		// the two the exporter started with as well, which it compares them
		// with and nothing had asked for.
		if want := map[int]int64{1: 4, 2: 2}[reload]; whenInForce != want {
			t.Errorf("reload %d had encoded %d collectors' definitions when its configuration went in force, want %d", reload, whenInForce, want)
		}
		for _, name := range []string{"kept", "changed"} {
			outcome := probeOnce(t, w.server, probePath(name, target.URL, ""), nil)
			if outcome.Code != http.StatusOK {
				t.Fatalf("reload %d: the probe of %s that came before the reload was followed was answered %d: %s", reload, name, outcome.Code, outcome.Body)
			}
			if name == "changed" && !strings.Contains(outcome.Body.String(), map[int]string{1: "renamed_value 42", 2: "again_value 42"}[reload]) {
				t.Errorf("reload %d: the probe of the changed collector was not answered by its new definition: %s", reload, outcome.Body)
			}
		}
		// The first reload's probes go to the target, nothing cached yet; at
		// the second the collector kept is answered with its cached result.
		if got, want := requests.Load()-asked, map[int]int64{1: 2, 2: 1}[reload]; got != want {
			t.Errorf("reload %d: the two probes made %d requests of the target, want %d", reload, got, want)
		}
		if got := encoded.Load() - whenInForce; got != 0 {
			t.Errorf("reload %d: the probes that came before the reload was followed encoded %d collectors' definitions, want none", reload, got)
		}
		followed := w.server.followed.Load()
		if followed.config != w.manager.Get() || followed.generation != before.generation+1 || followed.defined["kept"] != before.defined["kept"] || followed.defined["changed"] != followed.generation {
			t.Fatalf("reload %d: the probes left generation %d followed, the configuration in force %v, collectors defined %v, want generation %d of it, the collector kept as it was and the changed one from then", reload, followed.generation, followed.config == w.manager.Get(), followed.defined, before.generation+1)
		}
		select {
		case err := <-reloaded:
			t.Fatalf("reload %d returned (%v) before it was let go on", reload, err)
		default:
		}
		close(resume)
		if err := <-reloaded; err != nil {
			t.Fatal(err)
		}
		if after := w.server.followed.Load(); after != followed {
			t.Errorf("reload %d followed again what the probe had followed: generation %d, want %d", reload, after.generation, followed.generation)
		}
		if got := encoded.Load() - whenInForce; got != 0 {
			t.Errorf("reload %d: %d collectors' definitions were encoded once its configuration was in force, want none", reload, got)
		}
	}
}

// While a reload prepares its configuration the one before it is still in
// force, and probes are answered by it without waiting for the reload: a
// probe made where the reload encodes the first definition of its
// configuration, on the very goroutine that reloads, so that it could not
// be answered if it waited for what the reload prepares, finds the former
// configuration in force and followed and is answered by the former
// definition of the collector the reload changes, with the result cached for
// it and no definition encoded; after the reload the collector is answered
// by its new definition, from its target.
func TestAProbeIsAnsweredByTheFormerConfigurationWhileAReloadPrepares(t *testing.T) {
	target, requests, _ := failableTarget(t)
	r := newReloadable(t, cachedDocument("kept", "changed"), "")
	for _, name := range []string{"kept", "changed"} {
		if outcome := probeOnce(t, r.server, probePath(name, target.URL, ""), nil); outcome.Code != http.StatusOK {
			t.Fatalf("the probe of %s before the reload was answered %d: %s", name, outcome.Code, outcome.Body)
		}
	}
	before := r.server.followed.Load()
	var encoded, whilePreparing atomic.Int64
	probed := false
	hook := func() {
		if encoded.Add(1) > 1 {
			return
		}
		probed = true
		if r.manager.Get() != before.config || r.server.followed.Load() != before {
			// A probe would now follow the configuration itself, and wait
			// for the fingerprint being made here: for ever.
			t.Error("where the reload encodes the first definition of its configuration, that configuration is in force already, or followed")
			return
		}
		outcome := probeOnce(t, r.server, probePath("changed", target.URL, ""), nil)
		if outcome.Code != http.StatusOK || !strings.Contains(outcome.Body.String(), "changed_value 42") || requests.Load() != 2 {
			t.Errorf("the probe that came while the reload prepared was answered %d after %d requests of the target, want by the former definition, with the result cached after 2: %s", outcome.Code, requests.Load(), outcome.Body)
		}
		whilePreparing.Store(encoded.Load() - 1)
	}
	fingerprintedHook.Store(&hook)
	t.Cleanup(func() { fingerprintedHook.Store(nil) })
	r.reloadTo(strings.Replace(cachedDocument("kept", "changed"), "changed_value", "renamed_value", 1))
	if !probed {
		t.Fatal("the reload encoded no collector's definition")
	}
	if whilePreparing.Load() != 0 {
		t.Errorf("the probe that came while the reload prepared encoded %d collectors' definitions, want none", whilePreparing.Load())
	}
	if encoded.Load() != 2 {
		t.Errorf("the reload encoded %d collectors' definitions, want 2, each collector of its configuration once", encoded.Load())
	}
	if outcome := probeOnce(t, r.server, probePath("changed", target.URL, ""), nil); outcome.Code != http.StatusOK || !strings.Contains(outcome.Body.String(), "renamed_value 42") || requests.Load() != 3 {
		t.Errorf("the probe after the reload was answered %d after %d requests of the target, want by the new definition, from its target, the third request: %s", outcome.Code, requests.Load(), outcome.Body)
	}
}

// What is kept of the fingerprints does not grow with the reloads: after
// each of 1,000 reloads (200 under the race detector), between two
// configurations, the fingerprints remembered and those prepared are the
// ones of the configuration in force, and when all are made nothing holds
// any configuration that was in force before but the last few, which the
// collector of garbage may not have looked at: the others are gone.
func TestTheFingerprintsKeptDoNotGrowWithTheReloads(t *testing.T) {
	reloads := alloctest.UnlessRaced(1000, 200)
	documents := [2]string{cachedDocument("kept", "changed"), strings.Replace(cachedDocument("kept", "changed"), "changed_value", "renamed_value", 1)}
	r := newReloadable(t, documents[0], "")
	inForce := make([]weak.Pointer[model.Config], 0, reloads)
	for reload := 1; reload <= reloads; reload++ {
		r.reloadTo(documents[reload%2])
		cfg := r.manager.Get()
		inForce = append(inForce, weak.Make(cfg))
		memo := r.server.fingerprints
		if remembered, prepared := memo.current.Load(), memo.prepared.Load(); remembered == nil || prepared != remembered || remembered.config != cfg || r.server.followed.Load().fingerprints != remembered {
			t.Fatalf("after reload %d the fingerprints remembered, prepared and followed are not all those of the configuration in force", reload)
		}
	}
	runtime.GC()
	runtime.GC()
	held := 0
	for i := range inForce {
		if inForce[i].Value() != nil {
			held++
		}
	}
	if held > 3 {
		t.Errorf("%d of the %d configurations the reloads put in force are still held, want the one in force and no more than two before it", held, reloads)
	}
	if last := inForce[len(inForce)-1].Value(); last != r.manager.Get() {
		t.Error("the configuration in force is not held")
	}
}

// The fingerprints prepared for a configuration are kept beside those
// remembered, and are the ones its users find: preparing a configuration
// encodes each of its collectors once and leaves the probes of the
// configuration still in force the fingerprints they have; the first to ask
// for those of the prepared one is given them, made; a probe that still
// holds the former configuration afterwards gets its own afresh, as it did,
// and the ones of the prepared configuration are not made again for the
// next that asks, nor by preparing it again. A configuration whose
// fingerprints are remembered already, some of them made, has only the
// others made when it is prepared.
func TestFingerprintsPreparedAreFoundMade(t *testing.T) {
	var encoded atomic.Int64
	hook := func() { encoded.Add(1) }
	fingerprintedHook.Store(&hook)
	t.Cleanup(func() { fingerprintedHook.Store(nil) })
	encodes := func(what string, want int64, do func()) {
		t.Helper()
		encoded.Store(0)
		do()
		if got := encoded.Load(); got != want {
			t.Errorf("%s encoded %d collectors' definitions, want %d", what, got, want)
		}
	}
	memo := &fingerprintMemo{}
	former, next := fingerprintTestConfig(), fingerprintTestConfig()
	next.Collectors[0].Request.Path = "/changed"
	want := []string{collectorFingerprint(&next.Collectors[0]), collectorFingerprint(&next.Collectors[1])}
	var formerFingerprints *fingerprintGeneration
	encodes("the first probe of the configuration in force", 1, func() {
		formerFingerprints = memo.of(former)
		formerFingerprints.at(0)
	})
	encodes("preparing a configuration of two collectors", 2, func() { memo.prepare(next) })
	encodes("a probe of the configuration still in force", 0, func() {
		if memo.of(former) != formerFingerprints {
			t.Error("preparing a configuration replaced the fingerprints remembered for the one in force")
		}
		memo.of(former).at(0)
	})
	encodes("the first to ask for the fingerprints of the prepared configuration", 0, func() {
		fingerprints := memo.of(next)
		for i := range next.Collectors {
			if got := fingerprints.at(i); got != want[i] {
				t.Errorf("the fingerprint prepared for collector %d is %s, want %s", i, got, want[i])
			}
		}
	})
	encodes("a probe that still holds the former configuration", 1, func() { memo.of(former).at(1) })
	encodes("the next to ask for the fingerprints of the prepared configuration, and preparing it again", 0, func() {
		memo.of(next).at(0)
		memo.of(next).at(1)
		memo.prepare(next)
	})
	if memo.prepared.Load() != memo.current.Load() || memo.current.Load().config != next {
		t.Error("the fingerprints prepared and those remembered are not the same ones, of the prepared configuration")
	}
	third := fingerprintTestConfig()
	encodes("the first probe of a configuration that was not prepared", 1, func() { memo.of(third).at(1) })
	encodes("preparing a configuration one of whose two fingerprints is remembered", 1, func() { memo.prepare(third) })
	if memo.prepared.Load() != memo.current.Load() || memo.current.Load().config != third {
		t.Error("the fingerprints prepared for a configuration remembered already are not the ones remembered")
	}
}

// A static target file reloaded alone prepares nothing: the configuration it
// is put in force with is the one followed, whose fingerprints are kept, so
// no collector's definition is encoded before the file is in force or after,
// and nothing is kept as prepared. A configuration that is not the one
// followed is prepared, with a static target file or without, as far as it
// is not yet, and once.
func TestAStaticTargetFileReloadedAlonePreparesNothing(t *testing.T) {
	names := followBenchNames(0, 4)
	var targets strings.Builder
	targets.WriteString("interval: 1m\ntargets:\n")
	for _, name := range names {
		fmt.Fprintf(&targets, "  - name: t_%s\n    collector: %s\n    target: http://127.0.0.1:9/%s\n", name, name, name)
	}
	r := newReloadable(t, testutil.CollectorsDocument(names...), targets.String())
	encoded, _ := countFingerprints(t, r.server)
	cfg, file := r.manager.InForce()
	r.server.prepareReload(cfg, &model.StaticTargetFile{Targets: file.Targets[:2]})
	if encoded.Load() != 0 || r.server.fingerprints.prepared.Load() != nil {
		t.Errorf("preparing a static target file with the configuration followed encoded %d collectors' definitions and kept fingerprints prepared %v, want neither", encoded.Load(), r.server.fingerprints.prepared.Load() != nil)
	}
	r.server.prepareReload(nil, file)
	if encoded.Load() != 0 {
		t.Errorf("preparing no configuration encoded %d collectors' definitions, want none", encoded.Load())
	}
	// Another configuration, which is not followed: one of its fingerprints
	// is asked for, and preparing it makes the three left, with the four of
	// the configuration followed, which nothing had asked for; prepared
	// again, with the file or without, it has none left to make.
	other, err := config.Load(r.path)
	if err != nil {
		t.Fatal(err)
	}
	r.server.fingerprints.fingerprint(other, &other.Collectors[1])
	encoded.Store(0)
	r.server.prepareReload(other, file)
	if got := encoded.Load(); got != 7 {
		t.Errorf("preparing a configuration not followed, one of its four fingerprints made, encoded %d collectors' definitions, want 7: its three others, and the four of the configuration followed", got)
	}
	r.server.prepareReload(other, file)
	r.server.prepareReload(other, nil)
	if got := encoded.Load(); got != 7 {
		t.Errorf("preparing the configuration again encoded %d collectors' definitions more, want none", got-7)
	}
}
