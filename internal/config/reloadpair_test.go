//go:build !select_request_types || request_type_http

package config

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The configuration and the static target file in force are one pair: a
// reload puts both in force in one step, and checks them against each other
// without writing into either, since scrapes are reading them (Manager's
// inForce and install in config.go).

// onReloadLog is a log handler that calls back at each line a reload logs
// once a file is in force, which is as close to the moment between two
// installs as a reader can be put.
type onReloadLog struct {
	slog.Handler
	installed func()
}

func (h onReloadLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "configuration reloaded" || r.Message == "static targets reloaded" {
		h.installed()
	}
	return nil
}

func (onReloadLog) Enabled(context.Context, slog.Level) bool { return true }

const acceptingTargets = "interval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://a.invalid\n    request:\n      accept_status: [200, ' 5XX ']\n"

// A reload that renames a collector in the configuration and in the target
// using it is put in force as one pair: at no moment does a reader find the
// new configuration with the old targets, whose collector it no longer has,
// whether it reads the two apart or together.
func TestAReloadOfBothFilesIsInForceAsOnePair(t *testing.T) {
	renamed := strings.Replace(oneCollector, "name: a\n", "name: renamed\n", 1)
	p := newPair(t, oneCollector, oneTarget)
	var seen, orphans []string
	p.manager.logger = slog.New(onReloadLog{installed: func() {
		apart := p.manager.Get()
		for _, target := range p.manager.StaticTargets() {
			if model.CollectorByName(apart, target.Collector) == nil {
				orphans = append(orphans, target.Name+" names "+target.Collector)
			}
		}
		cfg, file := p.manager.InForce()
		seen = append(seen, cfg.Collectors[0].Name+" with "+file.Targets[0].Collector)
	}})
	p.write(t, p.configPath, renamed)
	p.write(t, p.targetsAt, strings.Replace(oneTarget, "collector: a", "collector: renamed", 1))
	if err := p.manager.Reload(ReloadTriggerSignal); err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("mid-reload, targets in force name a collector the configuration in force lacks: %v", orphans)
	}
	if want := []string{"renamed with renamed", "renamed with renamed"}; !slices.Equal(seen, want) {
		t.Fatalf("the pairs in force as the reload logged each file: %v, want %v", seen, want)
	}
	// The pair is what each getter returns on its own.
	cfg, file := p.manager.InForce()
	if cfg != p.manager.Get() || file != p.manager.StaticTargetFile() || len(p.manager.StaticTargets()) != 1 {
		t.Fatal("the pair in force is not what the manager returns of each")
	}
	// A file reloaded alone keeps the other of the pair.
	p.write(t, p.targetsAt, strings.Replace(oneTarget, "collector: a", "collector: renamed", 1)+"concurrency: 3\n")
	p.manager.reloadChanged()
	if again, changed := p.manager.InForce(); again != cfg || changed == file || p.manager.StaticTargetConcurrency() != 3 {
		t.Fatal("reloading the target file alone did not keep the configuration and replace the targets")
	}
}

// Without a static target file the pair is the configuration alone, and the
// targets are none.
func TestThePairInForceWithoutATargetFile(t *testing.T) {
	cfg, err := Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", oneCollector))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, "", testutil.QuietLogger(t))
	if got, file := m.InForce(); got != cfg || file != nil || m.StaticTargets() != nil || m.StaticTargetFile() != nil || m.StaticTargetConcurrency() != 8 {
		t.Fatalf("config=%p file=%v", got, file)
	}
}

// A target's accept_status is written as a scrape compares it once, when the
// file is loaded, before it is checked against the configuration; that
// check, which every reload repeats on the file in force, changes nothing in
// it, even in a file that was not written so.
func TestTheCheckAgainstTheConfigurationWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(testutil.WriteIn(t, dir, "config.yaml", oneCollector))
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", acceptingTargets))
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Targets[0].Request.AcceptStatus; !slices.Equal(got, []string{"200", " 5XX "}) {
		t.Fatalf("as read: %q", got)
	}
	if err := ValidateStaticTargets(file); err != nil {
		t.Fatal(err)
	}
	if got := file.Targets[0].Request.AcceptStatus; !slices.Equal(got, []string{"200", "5xx"}) {
		t.Fatalf("after the file's own validation: %q, want the entries trimmed and in lower case", got)
	}
	if err := ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatal(err)
	}
	// A file that skipped its own validation is still only read.
	unwritten := &model.StaticTargetFile{Targets: []model.StaticTarget{{Name: "t", Collector: "a", Target: "http://a.invalid", Interval: model.Duration(60e9),
		Request: model.TargetRequestConfig{AcceptStatus: []string{" 5XX "}}}}}
	if err := ValidateStaticTargetsAgainst(unwritten, cfg); err != nil {
		t.Fatal(err)
	}
	if got := unwritten.Targets[0].Request.AcceptStatus; !slices.Equal(got, []string{" 5XX "}) {
		t.Fatalf("the check against the configuration rewrote accept_status to %q", got)
	}
	// It still refuses an entry that is no status, naming the target.
	unwritten.Targets[0].Request.AcceptStatus = []string{"6xx"}
	if err := ValidateStaticTargetsAgainst(unwritten, cfg); err == nil || !strings.Contains(err.Error(), `target "t" request.accept_status`) {
		t.Fatalf("err=%v", err)
	}
}

// Reloads racing scrapes: while the watch reloads the configuration alone,
// checking the target file in force against it each time, and reloads on
// demand replace both, a reader does what a scrape does with the pair in
// force. Under the race detector, a reload writing into either fails this
// test; and every pair read holds a target whose collector it has.
func TestReloadsNeverWriteIntoWhatIsInForce(t *testing.T) {
	p := newPair(t, oneCollector, acceptingTargets)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var orphan string
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			cfg, file := p.manager.InForce()
			for i := range file.Targets {
				target := file.Targets[i]
				collector := model.CollectorByName(cfg, target.Collector)
				if collector == nil {
					orphan = target.Name + " names " + target.Collector
					return
				}
				overrides := fetch.TargetOverrides(&target)
				_ = fetch.AcceptedStatus(collector, overrides, 503)
				_, _ = fetch.CheckRequestParams(collector, overrides)
			}
		}
	})
	for i := range 200 {
		switch i % 3 {
		case 0:
			// Only the configuration changed, as the watch finds it.
			p.write(t, p.configPath, oneCollector+strings.Repeat("\n", i))
			p.manager.reloadChanged()
		case 1:
			// Only the target file changed.
			p.write(t, p.targetsAt, acceptingTargets+strings.Repeat("\n", i))
			p.manager.reloadChanged()
		default:
			if err := p.manager.Reload(ReloadTriggerSignal); err != nil {
				t.Error(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	if orphan != "" {
		t.Fatalf("a pair in force held a target without its collector: %s", orphan)
	}
	if successes, failures := p.reloads(reloadFileConfig); failures != 0 || successes == 0 {
		t.Fatalf("configuration reloads: %d accepted, %d refused", successes, failures)
	}
}
