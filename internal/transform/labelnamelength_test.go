package transform

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkExportedLabelNameBefore is CheckExportedLabelName as it was before
// limits.max_label_name_length.
func checkExportedLabelNameBefore(x *model.Collector, name string) error {
	exported := escapeName(name, x.NameEscaping, false)
	if exported == name {
		return model.CheckLabelName(name)
	}
	if model.ReservedLabelName(exported) {
		return fmt.Errorf("label name %q is exported as %q under name_escaping %s, which starts with __, which Prometheus reserves for its own labels", name, exported, x.NameEscaping)
	}
	return nil
}

// A label name the configuration writes is refused at load, or accepted, in
// the words it was before limits.max_label_name_length while its exported
// name is within the limit: over classic, reserved, non-classic, escaped and
// long names under every name_escaping, with the limit 0, at the exported
// name's length and above it. Past the limit the name is refused naming its
// length and the limit, and the exported name where escaping changed it.
func TestALabelNameWithinTheLimitIsCheckedAtLoadAsBefore(t *testing.T) {
	names := []string{"a", "zone", "le", "__name__", "__x", "_x", "a.b", "a-b", "é", "1a", "a b", ".x", "__", strings.Repeat("l", 200), strings.Repeat("é", 70), "a." + strings.Repeat("b", 190)}
	for _, escaping := range []string{"", NameEscapingFail, NameEscapingUnderscores, NameEscapingValues} {
		for _, name := range names {
			x := &model.Collector{Name: "c", NameEscaping: escaping}
			exported := escapeName(name, escaping, false)
			want := checkExportedLabelNameBefore(x, name)
			for _, limit := range []int{0, len(exported), len(exported) + 1, 200} {
				if limit != 0 && limit < len(exported) {
					continue
				}
				x.Limits.MaxLabelNameLength = limit
				if got := CheckExportedLabelName(x, name); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%q under %q at %d: %v, was %v", name, escaping, limit, got, want)
				}
			}
			if want != nil || len(exported) < 2 {
				continue
			}
			x.Limits.MaxLabelNameLength = len(exported) - 1
			got := CheckExportedLabelName(x, name)
			wantPart := fmt.Sprintf("label name %q is %d bytes, longer than limits.max_label_name_length %d", name, len(name), len(exported)-1)
			if exported != name {
				wantPart = fmt.Sprintf("label name %q is exported as %q under name_escaping %s, %d bytes, longer than limits.max_label_name_length %d", name, exported, escaping, len(exported), len(exported)-1)
			}
			if got == nil || !strings.HasPrefix(got.Error(), wantPart) {
				t.Errorf("%q under %q one byte under its length: %v, want %s", name, escaping, got, wantPart)
			}
		}
	}
}

// labelNameVerdictBefore is what the load said of a label name a series has
// before transform.remove_labels and rename_labels — a rule's label, a key of
// transform.labels — before a name either takes off was let through: refused
// for its characters (TakesLabelName), then by CheckLabelNameBeforeRenames as
// it was, which held a name taken off to all but the length.
func labelNameVerdictBefore(x *model.Collector, name string) (taken bool, err error) {
	if !TakesLabelName(x, name) {
		return false, nil
	}
	_, renamed := x.Transform.RenameLabels[name]
	if renamed || slices.Contains(x.Transform.RemoveLabels, name) {
		return true, checkExportedLabelNameBefore(x, name)
	}
	return true, CheckExportedLabelName(x, name)
}

// A label name a series has before transform.remove_labels and rename_labels
// — a rule's label, a key of transform.labels — that one of them takes off is
// taken whatever its characters, and refused for nothing: the scrape escapes
// and validates the series after both, so the name is never exported. One
// neither takes off is checked as it was, in the same words. Compared with
// the checks as they were over the names of the tests above and generated
// ones, under every name_escaping, at limits from 0 to over the exported
// name's length, renamed, removed, renamed to, or none of these: the
// verdicts differ only for a name a rename or a removal takes off, and only
// from refused to taken. A name of blanks alone is refused, taken off or not.
func TestALabelNameARenameOrRemovalTakesOffIsTakenWhateverItIs(t *testing.T) {
	names := []string{"a", "zone", "le", "__name__", "__x", "_x", "a.b", "a-b", "é", "1a", "a b", ".x", "..x", "__", " ", "\t", strings.Repeat("l", 200), strings.Repeat("é", 70), "a." + strings.Repeat("b", 190), strings.Repeat("l", 300)}
	alphabet := []string{"a", "_", "1", ".", "-", "é", " ", "Z"}
	for n := range 300 {
		var b strings.Builder
		for k := n; ; k /= len(alphabet) {
			b.WriteString(alphabet[k%len(alphabet)])
			if k < len(alphabet) {
				break
			}
		}
		names = append(names, b.String())
	}
	changed := 0
	for _, escaping := range []string{"", NameEscapingFail, NameEscapingUnderscores, NameEscapingValues} {
		for _, name := range names {
			exported := escapeName(name, escaping, false)
			for _, limit := range []int{0, 1, len(exported) - 1, len(exported), 200} {
				for _, settings := range []model.TransformConfig{
					{},
					{RenameLabels: map[string]string{name: "short"}},
					{RemoveLabels: []string{"other", name}},
					{RenameLabels: map[string]string{"other": name}},
					{RemoveLabels: []string{"other"}, RenameLabels: map[string]string{"x": "y"}},
					{Labels: map[string]string{name: "v"}, RemoveLabels: []string{name}},
				} {
					x := &model.Collector{Name: "c", NameEscaping: escaping, Transform: settings, Limits: model.Limits{MaxLabelNameLength: limit}}
					_, renamed := settings.RenameLabels[name]
					takenOff := renamed || slices.Contains(settings.RemoveLabels, name)
					wasTaken, wasErr := labelNameVerdictBefore(x, name)
					taken := TakesLabelNameBeforeRenames(x, name)
					var err error
					if taken {
						err = CheckLabelNameBeforeRenames(x, name)
					}
					switch {
					case !takenOff || strings.TrimSpace(name) == "":
						if taken != wasTaken || fmt.Sprint(err) != fmt.Sprint(wasErr) {
							t.Errorf("%q under %q at %d with %+v: taken %v, %v; was taken %v, %v", name, escaping, limit, settings, taken, err, wasTaken, wasErr)
						}
					case !taken || err != nil:
						t.Errorf("%q under %q at %d with %+v, which takes it off: taken %v, %v", name, escaping, limit, settings, taken, err)
					case !wasTaken || wasErr != nil:
						changed++
					}
				}
			}
		}
	}
	if changed < 100 {
		t.Errorf("%d verdicts went from refused to taken", changed)
	}
}
