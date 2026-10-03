//go:build !select_request_types || request_type_http

package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A refused reload's line said what the watch reads the file again for only
// when the file waited for the other one, and then named the other file
// alone: "retried_when":"the configuration changes" read as if nothing else
// helped, although the file itself changing does, and so does a file stamped
// for it while it is refused. The line now names each thing that holds, and
// nothing without the watch, which is what does the reading again.

// rejectedLine is the one line logged since the last look, a refused reload.
func rejectedLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	line := testutil.AssertJSONLines(t, logs, 1)[0]
	logs.Reset()
	return line
}

// watchedPair is newPair with the watch on or off, and what the manager
// logs.
func watchedPair(t *testing.T, config, targets string, watch bool) (*pair, *bytes.Buffer) {
	t.Helper()
	p := newPair(t, config, targets)
	logs := &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	if watch {
		p.manager.SetWatchInterval(time.Minute)
	}
	return p, logs
}

// With the watch on, a refused file's line names under retried_when what
// the watch reads it again for: the file itself; with it a file stamped for
// it, when the configuration names one that the load opens; and the other
// file, when this one was refused only for disagreeing with it. Without the
// watch nothing is read again, and the line has no retried_when.
func TestARefusedReloadSaysWhatTheWatchReadsItAgainFor(t *testing.T) {
	password := testutil.WriteIn(t, t.TempDir(), "password", "s3cret\n")
	named := "web:\n  basic_auth: {enabled: true, username: admin, password_file: " + password + "}\n"
	const notYAML = "collectors: [\n"
	const targetsNotYAML = "interval: 1m\ntargets: [\n"
	unknownCollector := strings.Replace(twoTargets, "collector: b", "collector: c", 1)
	for name, tc := range map[string]struct {
		config    string
		targets   bool
		content   string
		msg, want string
	}{
		"the configuration, for itself":                             {config: twoCollectors, content: notYAML, msg: "configuration reload rejected", want: "the configuration changes"},
		"the configuration, for itself, naming a file":              {config: named + twoCollectors, content: notYAML, msg: "configuration reload rejected", want: "the configuration or a file it names changes"},
		"the configuration, for the target file":                    {config: twoCollectors, content: oneCollector, msg: "configuration reload rejected", want: "the configuration or the static target file changes"},
		"the configuration, for the target file, naming a file":     {config: named + twoCollectors, content: named + oneCollector, msg: "configuration reload rejected", want: "the configuration, a file it names or the static target file changes"},
		"the target file, for itself":                               {config: twoCollectors, targets: true, content: targetsNotYAML, msg: "static target reload rejected", want: "the static target file changes"},
		"the target file, for the configuration":                    {config: twoCollectors, targets: true, content: unknownCollector, msg: "static target reload rejected", want: "the static target file or the configuration changes"},
		"the target file, for the configuration, which names files": {config: named + twoCollectors, targets: true, content: unknownCollector, msg: "static target reload rejected", want: "the static target file or the configuration changes"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, watch := range []bool{true, false} {
				p, logs := watchedPair(t, tc.config, twoTargets, watch)
				path := p.configPath
				if tc.targets {
					path = p.targetsAt
				}
				p.write(t, path, tc.content)
				p.manager.reloadChanged()
				line := rejectedLine(t, logs)
				when, said := line["retried_when"]
				if line["msg"] != tc.msg || line["trigger"] != reloadTriggerWatch {
					t.Fatalf("watch %v: logged %v, want %s", watch, line, tc.msg)
				}
				if watch && when != tc.want {
					t.Errorf("with the watch: retried_when %q, want %q", when, tc.want)
				}
				if !watch && said {
					t.Errorf("without the watch: retried_when %q, want none", when)
				}
			}
		})
	}
}

// What triggered the refused reload makes no difference to what the watch
// reads the file again for: a configuration refused at a SIGHUP for the
// target file's sake says the same with the watch on, and nothing without
// it.
func TestAReloadRefusedOnDemandSaysWhatTheWatchReadsItAgainFor(t *testing.T) {
	for _, watch := range []bool{true, false} {
		p, logs := watchedPair(t, twoCollectors, twoTargets, watch)
		p.write(t, p.configPath, oneCollector)
		if err := p.manager.Reload(ReloadTriggerSignal); err == nil {
			t.Fatal("a configuration without a collector its targets use was put in force")
		}
		var rejected map[string]any
		for _, line := range testutil.AssertJSONLines(t, logs, 2) {
			if line["msg"] == "configuration reload rejected" {
				rejected = line
			}
		}
		when, said := rejected["retried_when"]
		if rejected["trigger"] != ReloadTriggerSignal || said != watch || watch && when != "the configuration or the static target file changes" {
			t.Fatalf("watch %v: logged %v", watch, rejected)
		}
	}
}
