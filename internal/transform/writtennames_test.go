package transform

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkMetricNameBeforeEscapedNames is checkMetricName as it was while a
// name written in the configuration was held to the classic names whatever
// the collector's name_escaping, kept as an oracle.
func checkMetricNameBeforeEscapedNames(name string) error {
	if !model.ValidMetricName(name) {
		return fmt.Errorf("%q is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit", name)
	}
	if strings.HasPrefix(name, "__") {
		return fmt.Errorf("%q starts with \"__\", which Prometheus reserves", name)
	}
	return nil
}

// writtenNames are names as a configuration may write them: classic ones,
// reserved ones, and ones that are not classic for each of the reasons there
// are, a name of blanks, of bytes that are no UTF-8 and of nothing among
// them.
var writtenNames = []string{
	"up", "_", "a_b", "UP", "node_load1", "job:up:sum", "a1", "__up", "__", "___x", "__name__", ":a",
	"http.server.duration", "service.name", "a.b", "a-b", "node load", "1up", "9", "é", "naïve", "指标", "a\x00b", "a\nb", ".x", "..x", "._x", "_.x", "1_x", "11x", "-", ".", "a b ", " up", "up ",
	"", " ", "  ", "\t", " ", "a\xffb", "\xff", "\xff\xfe",
}

// A name written in a collector's configuration is held to what a scrape
// holds a name to under the collector's name_escaping. Under fail, written
// or left out, a name that is not classic is refused, as it was, and a name
// that is valid UTF-8 and not blanks is told what the scrape's refusal
// tells: that underscores or values would export it escaped. Under those
// two it is taken, whatever characters it has, with two exceptions: a name
// of nothing but blanks is no name under any of the three, and one that
// underscores exports beginning with "__", which Prometheus reserves, is
// refused naming what it is exported as. A classic name is taken or
// refused, for the two underscores it begins with, exactly as it was, under
// each of the three: the check as it was is the oracle.
func TestAWrittenNameIsHeldToWhatAScrapeHoldsItTo(t *testing.T) {
	const advice = "set the collector's name_escaping to underscores or values to export it escaped"
	classic, escaped, refused := 0, 0, 0
	for _, escaping := range []string{"", NameEscapingFail, NameEscapingUnderscores, NameEscapingValues} {
		x := &model.Collector{Name: "node", NameEscaping: escaping, Transform: model.TransformConfig{Type: "jq"}}
		escapes := escaping == NameEscapingUnderscores || escaping == NameEscapingValues
		for _, name := range writtenNames {
			text := func(err error) string {
				if err == nil {
					return ""
				}
				return err.Error()
			}
			blank, told := strings.TrimSpace(name) == "", strings.TrimSpace(name) != "" && utf8.ValidString(name)

			// The metric name.
			got, was := text(checkMetricName(x, name)), text(checkMetricNameBeforeEscapedNames(name))
			exported := escapeName(name, escaping, true)
			var want string
			switch {
			case model.ValidMetricName(name):
				classic++
				want = was
			case !escapes && told:
				want = was + ", or " + advice
			case !escapes || blank:
				want = was
			case strings.HasPrefix(exported, "__"):
				refused++
				want = fmt.Sprintf("%q is exported as %q under name_escaping %s, which starts with \"__\", which Prometheus reserves", name, exported, escaping)
			default:
				escaped++
			}
			if got != want {
				t.Errorf("name_escaping %q, metric name %q: %s\nwant %s", escaping, name, got, want)
			}
			if name != "" && want == "" && !model.ValidMetricName(exported) {
				t.Errorf("name_escaping %q: metric name %q is taken and exported as %q, which is no classic name", escaping, name, exported)
			}

			// The label name: whether it is one, what its refusal is told,
			// and whether it is reserved, each as the load asks it.
			exported = escapeName(name, escaping, false)
			takes := model.ValidLabelName(name) || escapes && !blank
			if got := TakesLabelName(x, name); got != takes {
				t.Errorf("name_escaping %q, label name %q: taken %v, want %v", escaping, name, got, takes)
			}
			if got, want := EscapingAdvice(name), map[bool]string{true: "; " + advice}[told]; got != want {
				t.Errorf("label name %q is told %q, want %q", name, got, want)
			}
			if !takes {
				continue
			}
			got = text(CheckExportedLabelName(x, name))
			switch {
			case model.ValidLabelName(name):
				want = text(model.CheckLabelName(name))
			case strings.HasPrefix(exported, "__"):
				want = fmt.Sprintf("label name %q is exported as %q under name_escaping %s, which starts with __, which Prometheus reserves for its own labels", name, exported, escaping)
			default:
				want = ""
			}
			if got != want {
				t.Errorf("name_escaping %q, label name %q: %s\nwant %s", escaping, name, got, want)
			}
			if want == "" && (!model.ValidLabelName(exported) || model.ReservedLabelName(exported)) {
				t.Errorf("name_escaping %q: label name %q is taken and exported as %q, which no scrape exports", escaping, name, exported)
			}
		}
	}
	if classic < 40 || escaped < 40 || refused < 5 {
		t.Fatalf("%d classic names, %d names taken escaped and %d refused as reserved were tried", classic, escaped, refused)
	}
	// What the refusals read as, in full.
	strict := &model.Collector{Name: "node", Transform: model.TransformConfig{Type: "jq"}}
	underscores := &model.Collector{Name: "node", NameEscaping: NameEscapingUnderscores, Transform: model.TransformConfig{Type: "jq"}}
	for got, want := range map[string]string{
		checkMetricName(strict, "http.server.duration").Error(): `"http.server.duration" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped`,
		checkMetricName(strict, " ").Error():                    `" " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`,
		checkMetricName(underscores, " ").Error():               `" " is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit`,
		checkMetricName(underscores, "1_x").Error():             `"1_x" is exported as "__x" under name_escaping underscores, which starts with "__", which Prometheus reserves`,
		checkMetricName(underscores, "__up").Error():            `"__up" starts with "__", which Prometheus reserves`,
		CheckExportedLabelName(underscores, "..x").Error():      `label name "..x" is exported as "__x" under name_escaping underscores, which starts with __, which Prometheus reserves for its own labels`,
		CheckExportedLabelName(underscores, "__x").Error():      `label name "__x" starts with __, which Prometheus reserves for its own labels`,
	} {
		if got != want {
			t.Errorf("%s\nwant %s", got, want)
		}
	}
}

// The collector-wide settings that write a name — the keys of
// transform.labels and what transform.rename and transform.rename_labels
// rename to — are held to the same: refused under fail with what would
// export them, taken under underscores and values, and refused there too
// when underscores would export one as a reserved name.
func TestTheNamesOfTheTransformSettingsFollowNameEscaping(t *testing.T) {
	settings := model.TransformConfig{Type: "prometheus", Labels: map[string]string{"deployment.env": "prod"}, Rename: map[string]string{"http.server.duration": "rpc.duration"}, RenameLabels: map[string]string{"service.name": "svc.name"}}
	for escaping, want := range map[string][]string{
		"": {
			`collector "node" transform.rename "http.server.duration" to "rpc.duration": "rpc.duration" is not a valid Prometheus metric name; use letters, digits, underscores and colons, not starting with a digit, or set the collector's name_escaping to underscores or values to export it escaped`,
			`collector "node" transform.labels has invalid label name "deployment.env"; set the collector's name_escaping to underscores or values to export it escaped`,
			`collector "node" transform.rename_labels "service.name" to invalid label name "svc.name"; set the collector's name_escaping to underscores or values to export it escaped`,
		},
		NameEscapingUnderscores: nil,
		NameEscapingValues:      nil,
	} {
		x := &model.Collector{Name: "node", NameEscaping: escaping, Transform: settings}
		if got := problemTexts(CheckTransformSettings(x)); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("name_escaping %q: the check says\n%s\nwant\n%s", escaping, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	reserved := model.TransformConfig{Type: "prometheus", Labels: map[string]string{"..env": "prod", " ": "x"}, Rename: map[string]string{"up": "1_up"}, RenameLabels: map[string]string{"job": "._job"}}
	want := []string{
		`collector "node" transform.rename "up" to "1_up": "1_up" is exported as "__up" under name_escaping underscores, which starts with "__", which Prometheus reserves`,
		`collector "node" transform.labels has invalid label name " "`,
		`collector "node" transform.labels: label name "..env" is exported as "__env" under name_escaping underscores, which starts with __, which Prometheus reserves for its own labels`,
		`collector "node" transform.rename_labels "job": label name "._job" is exported as "__job" under name_escaping underscores, which starts with __, which Prometheus reserves for its own labels`,
	}
	if got := problemTexts(CheckTransformSettings(&model.Collector{Name: "node", NameEscaping: NameEscapingUnderscores, Transform: reserved})); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("under underscores the check says\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := problemTexts(CheckTransformSettings(&model.Collector{Name: "node", NameEscaping: NameEscapingValues, Transform: reserved})); len(got) != 1 || got[0] != want[1] {
		t.Errorf("under values the check says %q, want the name of blanks alone refused", got)
	}
}

// A rule's name is exported as a scrape exports a series of that name: with
// the collector's metrics_prefix joined, and the whole escaped as its
// name_escaping escapes a name that is not classic (Transform). The load
// holds that name to limits.max_metric_name_length, as it held the prefixed
// one, and says what it is exported as; a classic name is exported, and
// held, as it was.
func TestARulesNameIsMeasuredAsItIsExported(t *testing.T) {
	for _, tc := range []struct{ prefix, escaping, name, want string }{
		{"", "", "up", "up"}, {"otel", "", "up", "otel_up"}, {"otel", NameEscapingValues, "up", "otel_up"}, {"", NameEscapingUnderscores, "up", "up"},
		{"", NameEscapingUnderscores, "http.server.duration", "http_server_duration"}, {"otel", NameEscapingUnderscores, "http.server.duration", "otel_http_server_duration"},
		{"", NameEscapingValues, "http.server.duration", "U__http_2e_server_2e_duration"}, {"otel", NameEscapingValues, "http.server.duration", "U__otel__http_2e_server_2e_duration"},
		{"", NameEscapingFail, "http.server.duration", "http.server.duration"},
	} {
		x := &model.Collector{MetricsPrefix: tc.prefix, NameEscaping: tc.escaping}
		if got := ExportedMetricName(x, tc.name); got != tc.want {
			t.Errorf("prefix %q, name_escaping %q: %q is exported as %q, want %q", tc.prefix, tc.escaping, tc.name, got, tc.want)
		}
		set := &model.MetricSet{Metrics: []model.Metric{{Name: tc.name}}}
		applyMetricsPrefix(set, tc.prefix)
		if err := escapeNames(set, tc.escaping); err != nil || set.Metrics[0].Name != tc.want {
			t.Errorf("prefix %q, name_escaping %q: a series named %q leaves the transform as %q: %v", tc.prefix, tc.escaping, tc.name, set.Metrics[0].Name, err)
		}
	}
	x := &model.Collector{Name: "node", MetricsPrefix: "otel", NameEscaping: NameEscapingValues, Limits: model.Limits{MaxMetricNameLength: 30}, Metrics: []model.MetricRule{{Name: "up"}, {Name: "http.server.duration"}}}
	want := `collector "node" metric "http.server.duration" is exported as "U__otel__http_2e_server_2e_duration", which is longer than limits.max_metric_name_length 30`
	if err := ValidateMetricsPrefix(x); err == nil || err.Error() != want {
		t.Errorf("a name that is too long once escaped: %v\nwant %s", err, want)
	}
	x.NameEscaping = NameEscapingUnderscores
	if err := ValidateMetricsPrefix(x); err != nil {
		t.Errorf("the same name under underscores, which fits: %v", err)
	}
	x.Metrics, x.NameEscaping = []model.MetricRule{{Name: "a_name_of_some_thirty_characters"}}, ""
	want = `collector "node" metric "a_name_of_some_thirty_characters" is exported as "otel_a_name_of_some_thirty_characters", which is longer than limits.max_metric_name_length 30`
	for _, escaping := range []string{"", NameEscapingFail, NameEscapingUnderscores, NameEscapingValues} {
		x.NameEscaping = escaping
		if err := ValidateMetricsPrefix(x); err == nil || err.Error() != want {
			t.Errorf("a classic name that is too long, name_escaping %q: %v\nwant %s", escaping, err, want)
		}
	}
}
