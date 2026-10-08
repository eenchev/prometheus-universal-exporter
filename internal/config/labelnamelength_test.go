//go:build !select_request_types || request_type_http

package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// limits.max_label_name_length is 200 when it is left out or 0, as
// limits.max_metric_name_length is, and a negative one is refused naming the
// key, as every other limit is.
func TestTheLabelNameLimitHasADefaultAndIsNotNegative(t *testing.T) {
	for _, limits := range []string{"", "    limits: {max_label_name_length: 0}\n"} {
		cfg, err := loadChecked(t, "", limits, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Collectors[0].Limits.MaxLabelNameLength; got != 200 {
			t.Errorf("with %q the limit is %d, want 200", limits, got)
		}
	}
	cfg, err := loadChecked(t, "", "    limits: {max_label_name_length: 7}\n", "")
	if err != nil || cfg.Collectors[0].Limits.MaxLabelNameLength != 7 {
		t.Fatalf("a limit that is set is not kept: %v", err)
	}
	_, err = loadChecked(t, "", "    limits: {max_label_name_length: -3}\n", "")
	if want := `collector "a" limits.max_label_name_length is -3, and a limit must not be negative; leave it out, or 0, for the default`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a negative limit: error %v, want %q", err, want)
	}
}

// A label name the configuration writes that is longer than the collector's
// limits.max_label_name_length is refused when the configuration loads,
// since every series with it would fail the scrape: a rule's label, a key of
// transform.labels and what transform.rename_labels renames to, the name as
// name_escaping exports it. A name at the limit loads, and one over the
// default loads where the limit is raised to it.
func TestALabelNameTheConfigurationWritesOverTheLimitIsRefused(t *testing.T) {
	over, at := strings.Repeat("l", 201), strings.Repeat("l", 200)
	for name, test := range map[string]struct{ collector, metric, want string }{
		"a rule's label": {"", "        labels: [{name: " + over + ", value: x}]\n",
			fmt.Sprintf(`collector "a" metric "m": label name %q is 201 bytes, longer than limits.max_label_name_length 200; shorten it or raise limits.max_label_name_length`, over)},
		"a key of transform.labels": {"    transform: {type: jq, labels: {" + over + ": x}}\n", "",
			fmt.Sprintf(`collector "a" transform.labels: label name %q is 201 bytes, longer than limits.max_label_name_length 200`, over)},
		"a rename_labels target": {"    transform: {type: jq, rename_labels: {a: " + over + "}}\n", "",
			fmt.Sprintf(`collector "a" transform.rename_labels "a": label name %q is 201 bytes, longer than limits.max_label_name_length 200`, over)},
		"a name escaping makes longer": {"    name_escaping: values\n    limits: {max_label_name_length: 8}\n", "        labels: [{name: a.b.c, value: x}]\n",
			`collector "a" metric "m": label name "a.b.c" is exported as "U__a_2e_b_2e_c" under name_escaping values, 14 bytes, longer than limits.max_label_name_length 8`},
	} {
		collector := test.collector
		if strings.HasPrefix(collector, "    transform:") {
			// The collector's own transform line replaces the one
			// checkedCollector writes.
			_, err := loadTransformed(t, collector)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("%s: error %v, want %q", name, err, test.want)
			}
			continue
		}
		if _, err := loadChecked(t, "", collector, test.metric); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	for name, test := range map[string]struct{ collector, metric string }{
		"a rule's label at the limit":      {"", "        labels: [{name: " + at + ", value: x}]\n"},
		"a rule's label at a raised limit": {"    limits: {max_label_name_length: 201}\n", "        labels: [{name: " + over + ", value: x}]\n"},
		"an escaped name at the limit":     {"    name_escaping: values\n    limits: {max_label_name_length: 14}\n", "        labels: [{name: a.b.c, value: x}]\n"},
		"a rule's label at a low limit":    {"    limits: {max_label_name_length: 3}\n", "        labels: [{name: abc, value: x}]\n"},
	} {
		if _, err := loadChecked(t, "", test.collector, test.metric); err != nil {
			t.Errorf("%s: refused with %v", name, err)
		}
	}
	if _, err := loadTransformed(t, "    transform: {type: jq, labels: {"+at+": x}, rename_labels: {a: "+at+"x}}\n    limits: {max_label_name_length: 201}\n"); err != nil {
		t.Errorf("transform.labels and rename_labels at a raised limit: refused with %v", err)
	}
}

// loadTransformed loads the collector of checkedCollector with its transform
// line replaced by the one transform holds, with whatever follows it.
func loadTransformed(t *testing.T, transform string) (*model.Config, error) {
	t.Helper()
	return Load(testutil.WriteFile(t, "config.yaml", strings.Replace(checkedCollector("", "", ""), "    transform: {type: jq}\n", transform, 1)))
}

// A static target's labels go on every series of its collector past the
// scrape's validation, so a label name longer than the collector's
// limits.max_label_name_length is refused when the target file is checked
// against the configuration, naming the target, the label, the limit and the
// collector; one at the limit passes.
func TestAStaticTargetsLabelNameOverItsCollectorsLimitIsRefused(t *testing.T) {
	cfg, err := loadChecked(t, "", "    limits: {max_label_name_length: 14}\n", "")
	if err != nil {
		t.Fatal(err)
	}
	want := `target "t" label name "notes_of_racks1" is 15 bytes, longer than limits.max_label_name_length 14 of collector "a"; shorten it or raise the collector's limit`
	if err := ValidateStaticTargetsAgainst(targetFileLabelled(map[string]string{"zones_of_racks_x": "x", "notes_of_racks1": "y", "a": "z"}), cfg); err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
	if err := ValidateStaticTargetsAgainst(targetFileLabelled(map[string]string{"zone_of_racks1": "x"}), cfg); err != nil {
		t.Errorf("a label at the limit: refused with %v", err)
	}
}

// targetFileLabelled is a static target file of one target, t, of collector
// a, with the labels given.
func targetFileLabelled(labels map[string]string) *model.StaticTargetFile {
	return &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "a", Target: "http://t.invalid", Labels: labels}}}
}

// The static targets endpoint puts static_target, 13 bytes, on every series
// of a target, past the scrape's validation, and the target's health series
// carry collector and target beside it: a target whose collector's
// limits.max_label_name_length is under 13 is refused when the target file
// is checked against the configuration, naming the target, the label, its
// length, the limit and the collector, where it loaded and the endpoint
// served a label name the validation refuses. A limit of 13 passes, and
// so does any limit with no target of the collector.
func TestAStaticTargetWhoseCollectorsLimitIsUnderTheLabelsTheExporterAddsIsRefused(t *testing.T) {
	for _, limit := range []int{12, 9, 1} {
		cfg, err := loadChecked(t, "", fmt.Sprintf("    limits: {max_label_name_length: %d}\n", limit), "")
		if err != nil {
			t.Fatalf("limit %d without a target: %v", limit, err)
		}
		want := fmt.Sprintf(`target "t" gets the label static_target, 13 bytes, on every series of it, longer than limits.max_label_name_length %d of collector "a"; raise the collector's limit to 13 or more`, limit)
		if err := ValidateStaticTargetsAgainst(targetFileLabelled(nil), cfg); err == nil || err.Error() != want {
			t.Errorf("limit %d: error %v, want %q", limit, err, want)
		}
	}
	cfg, err := loadChecked(t, "", "    limits: {max_label_name_length: 13}\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(targetFileLabelled(map[string]string{"zone": "x"}), cfg); err != nil {
		t.Errorf("a limit of 13: refused with %v", err)
	}
}

// A label name the configuration writes that transform.rename_labels renames
// or transform.remove_labels removes is never exported under that name: the
// scrape holds the name after them to limits.max_label_name_length. So a
// rule's label and a key of transform.labels over the limit load when either
// takes them off, as they did before the limit, where they were refused by
// the name as written; the name a rename gives is held to the limit still,
// and so is a name a rename renames that another rename gives, which the
// series keeps.
func TestALabelNameARenameOrARemovalTakesOffIsNotHeldToTheLimit(t *testing.T) {
	long := strings.Repeat("l", 201)
	for name, test := range map[string]struct{ transform, metric string }{
		"a rule's label renamed":         {"    transform: {type: jq, rename_labels: {" + long + ": short}}\n", "        labels: [{name: " + long + ", expression: .y}]\n"},
		"a rule's label removed":         {"    transform: {type: jq, remove_labels: [" + long + "]}\n", "        labels: [{name: " + long + ", expression: .y}]\n"},
		"a transform.labels key renamed": {"    transform: {type: jq, labels: {" + long + ": x}, rename_labels: {" + long + ": short}}\n", ""},
		"a transform.labels key removed": {"    transform: {type: jq, labels: {" + long + ": x}, remove_labels: [" + long + "]}\n", ""},
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(checkedCollector("", "", test.metric), "    transform: {type: jq}\n", test.transform, 1))); err != nil {
			t.Errorf("%s: refused with %v", name, err)
		}
	}
	for name, test := range map[string]struct{ transform, want string }{
		"the rename's target": {"    transform: {type: jq, labels: {" + long + ": x}, rename_labels: {" + long + ": " + long + "x}}\n",
			`collector "a" transform.rename_labels "` + long + `": label name "` + long + `x" is 202 bytes`},
		"a renamed name another rename gives": {"    transform: {type: jq, rename_labels: {a: " + long + ", " + long + ": short}}\n",
			`collector "a" transform.rename_labels "a": label name "` + long + `" is 201 bytes`},
		"a name renamed into another key": {"    transform: {type: jq, labels: {" + long + ": x, " + long + "y: x}, rename_labels: {" + long + ": short}}\n",
			`collector "a" transform.labels: label name "` + long + `y" is 202 bytes`},
	} {
		if _, err := loadTransformed(t, test.transform); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
}

// A reload of the configuration that lowers a collector's
// limits.max_label_name_length under a label name of the static target file
// in force is refused, naming the target, the label and the new limit, and
// the configuration in force stays, its limit and all.
func TestAReloadThatLowersTheLimitUnderATargetsLabelIsRefused(t *testing.T) {
	dir := t.TempDir()
	withLimit := func(limit int) string {
		return strings.Replace(checkedCollector("", "", ""), "    transform: {type: jq}\n", fmt.Sprintf("    transform: {type: jq}\n    limits: {max_label_name_length: %d}\n", limit), 1)
	}
	cfgPath := testutil.WriteIn(t, dir, "config.yaml", withLimit(20))
	targetsPath := testutil.WriteIn(t, dir, "targets.yaml", "interval: 1m\ntargets:\n  - {name: t, collector: a, target: 'http://x.invalid', labels: {datacenter_name: x}}\n")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(targetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(file, cfg); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg, cfgPath, slog.New(slog.DiscardHandler))
	m.SetTargets(targetsPath, file)
	testutil.WriteIn(t, dir, "config.yaml", withLimit(14))
	want := `target "t" label name "datacenter_name" is 15 bytes, longer than limits.max_label_name_length 14 of collector "a"`
	if err := m.Reload("test"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("reload: error %v, want %q", err, want)
	}
	if got := m.Get().Collectors[0].Limits.MaxLabelNameLength; got != 20 {
		t.Errorf("the limit in force is %d, want the 20 of the configuration in force", got)
	}
}
