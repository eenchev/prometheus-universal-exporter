package transform

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A sum() that fails names how many nodes it added up and how many of them
// are no numbers, which is measured of the response: a table that grew by a
// row, or has one more cell that is no number, fails the same way to the
// log, whether the exporter adds the sum up, the engine does, or the sum is
// a label's. The text of the first such node is part of what the failure
// is: another text is another failure.
func TestASumFailureIsRecognisedWhateverItsNodeCounts(t *testing.T) {
	for name, tc := range map[string]struct {
		rule         model.MetricRule
		a, b         string
		wantA, wantB string
	}{
		"the whole expression": {model.MetricRule{Name: "m", Expression: "sum(//v)"},
			"<r><v>1</v><v>x</v></r>", "<r><v>1</v><v>x</v><v>x</v><v>2</v></r>",
			`first "x" (1 of 2 nodes)`, `first "x" (2 of 4 nodes)`},
		"a part of an expression": {model.MetricRule{Name: "m", Expression: "sum(//v) div 2"},
			"<r><v>1</v><v>x</v></r>", "<r><v>1</v><v>x</v><v>x</v><v>2</v></r>",
			`first "x" (1 of 2 nodes)`, `first "x" (2 of 4 nodes)`},
		"a label": {model.MetricRule{Name: "m", Expression: "//n", Labels: []model.LabelRule{{Name: "total", Expression: "sum(../v) div 2"}}},
			"<r><n>1</n><v>1</v><v>x</v></r>", "<r><n>1</n><v>1</v><v>x</v><v>x</v><v>2</v></r>",
			`first "x" (1 of 2 nodes)`, `first "x" (2 of 4 nodes)`},
	} {
		t.Run(name, func(t *testing.T) {
			a := failureOf(t, "xml", "xpath", tc.rule, tc.a)
			b := failureOf(t, "xml", "xpath", tc.rule, tc.b)
			if !strings.Contains(a.Error(), tc.wantA) || !strings.Contains(b.Error(), tc.wantB) {
				t.Fatalf("the failures read\n%s\n%s\nwant them to hold %s and %s", a, b, tc.wantA, tc.wantB)
			}
			if sameA, sameB := model.SameFailureText(a), model.SameFailureText(b); sameA != sameB {
				t.Fatalf("the failures are recognised by\n%s\n%s\nwant one text", sameA, sameB)
			}
			other := failureOf(t, "xml", "xpath", tc.rule, strings.ReplaceAll(tc.a, "x", "y"))
			if model.SameFailureText(other) == model.SameFailureText(a) {
				t.Fatalf("a sum over %q and one over %q are recognised by one text: %s", "x", "y", model.SameFailureText(a))
			}
		})
	}
}
