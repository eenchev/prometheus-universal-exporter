//go:build !select_request_types || request_type_localfile

package exporter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A directory and its files may have any names, and a static target may
// read the directory "static target textfiles" with a file named schedule
// in it: that file's failure is the file's, and the turns the schedule gives
// up for the static target textfiles are the target's. While a key was its
// parts with a NUL between them the two had one key — the collector, the
// words "static target textfiles", which were both the address the file is
// at and how the target's own failures were told, and schedule, both the
// file's name and what told a skipped turn — so a skipped turn was taken for
// the file failing with another error and the file's failure for the turn
// skipped with another: each was logged in full every time, and a scrape
// that started, on schedule again, ended the file's failure. They are two
// now: each is logged in full once and then as a repeat, the scrape that
// starts ends the skipped turns' failure alone, and a reload that removes
// or changes the static target forgets the skipped turns and leaves the
// file's failure, which a probe of the directory shares.
func TestAFileNamedScheduleIsNotTheScheduleOfAStaticTarget(t *testing.T) {
	const address = "static target textfiles"
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, address), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, address, "schedule"), []byte("not an exposition {\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := model.StaticTarget{Name: "textfiles", Collector: "dir", Target: address}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{target}}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{dirCollector("dir", root, "*")}}, file)
	logs := debugLogs(server)
	const fileFailed, skipped = "file of a directory failed; its series are left out and the other files' are answered", "static target scrape skipped"
	scrape := func() { server.scrapeStaticTargets(t.Context(), 0) }
	// skip is the turn the schedule gives up for the target, whose last
	// scrape still runs, as the loop reports it.
	skip := func() {
		generation := server.followedInForce().generation
		server.turnSkipped(generation, dueTarget{target: staticTargetsOf(file)[0], state: &staticTargetState{interval: time.Minute}})
	}
	scrape()
	skip()
	scrape()
	skip()
	if files, turns := levelsOf(t, logs, fileFailed), levelsOf(t, logs, skipped); strings.Join(files, " ") != "WARN DEBUG" || strings.Join(turns, " ") != "WARN DEBUG" {
		t.Errorf("the failures of the file named schedule are logged at %v and the skipped turns at %v, want each in full and then as a repeat:\n%s", files, turns, logs)
	}
	if remembered := rememberedOf(server, "dir"); len(remembered) != 2 {
		t.Errorf("the failure log remembers %v of the collector, want the file's failure and the skipped turns", remembered)
	}
	// A reload removed or changed the static target: its skipped turns
	// are forgotten, and the file's failure is not.
	server.failures.mu.Lock()
	server.failures.forgetStaticTargetsLocked(map[string]bool{"textfiles": true})
	server.failures.mu.Unlock()
	if remembered := rememberedOf(server, "dir"); len(remembered) != 1 || !strings.HasPrefix(remembered[0], "decode ") {
		t.Errorf("after the static target is forgotten the failure log remembers %v of the collector, want the file's failure alone", remembered)
	}
}
