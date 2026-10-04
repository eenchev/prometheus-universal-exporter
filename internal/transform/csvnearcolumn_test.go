package transform

import (
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// In a table of more columns than an error lists, the column that is the
// one asked for in another case, or with blanks around its name, was among
// those cut off: a rule naming Used was shown twelve names from the start of
// the alphabet and "and 10 more", with used among the ten. Such names come
// first now, in their order, and the others after them as before, the list
// cut where it was.
func TestCSVErrorListsTheColumnNearTheOneAskedForFirst(t *testing.T) {
	wide := map[string]any{"used": "1", "zone": "1"}
	// Names with a blank before and after, as a header read without
	// trim_space leaves them.
	const blankBefore, blankAfter = " " + "used", "used" + " "
	for i := range 20 {
		wide[fmt.Sprintf("attr%02d", i)] = "1"
	}
	const attrs = `"attr00", "attr01", "attr02", "attr03", "attr04", "attr05", "attr06", "attr07", "attr08", "attr09", "attr10"`
	for name, tc := range map[string]struct {
		rows   []any
		column string
		want   string
	}{
		"another case in a wide table": {rows: []any{wide}, column: "Used",
			want: `whose columns are "used", ` + attrs + ` and 10 more; column names are matched exactly`},
		"no such name in a wide table": {rows: []any{wide}, column: "usage",
			want: `whose columns are ` + attrs + `, "attr11" and 10 more; column names are matched exactly`},
		"blanks around the name": {rows: []any{map[string]any{"host": "a", blankBefore: "1", "zone": "b"}}, column: "used",
			want: `whose columns are " used", "host", "zone"; column names are matched exactly`},
		"blanks around the name asked for": {rows: []any{map[string]any{"host": "a", "used": "1", "zone": "b"}}, column: " Used ",
			want: `whose columns are "used", "host", "zone"; column names are matched exactly`},
		"several such names": {rows: []any{map[string]any{"host": "a", "USED": "1", blankAfter: "2", "zone": "b", "Used": "3"}}, column: "used",
			want: `whose columns are "USED", "Used", "used ", "host", "zone"; column names are matched exactly`},
		"the only column": {rows: []any{map[string]any{"Used": "1"}}, column: "used",
			want: `whose only column is "Used"; column names are matched exactly`},
		"beside rows read by number": {rows: []any{map[string]any{"a": "1", "Used": "1"}, []any{"1", "2", "3"}}, column: "used",
			want: `whose columns are "Used", "a", and whose longest row has 3 columns, read by number from 1 to 3; column names are matched exactly`},
	} {
		run := transformCSVRows(t, tc.rows, "", model.MetricRule{Name: "m", Expression: tc.column})
		if want := fmt.Sprintf(`m: %d failed, %d missing, logged: CSV column %q is not in the response, %s`, len(tc.rows), len(tc.rows), tc.column, tc.want); run.err != nil || !slices.Equal(run.failures, []string{want}) {
			t.Errorf("%s: %q, %v; want %s", name, run.failures, run.err, want)
		}
	}
	// One response's list is made for each column asked after, and that of
	// a column with no such name once for them all.
	columns := csvColumns{rows: []any{wide}}
	for column, want := range map[string]string{
		"Used":  `whose columns are "used", ` + attrs + ` and 10 more; column names are matched exactly`,
		"usage": `whose columns are ` + attrs + `, "attr11" and 10 more; column names are matched exactly`,
		"ZONE":  `whose columns are "zone", ` + attrs + ` and 10 more; column names are matched exactly`,
		"other": `whose columns are ` + attrs + `, "attr11" and 10 more; column names are matched exactly`,
	} {
		if got := columns.notInResponse(column).err.Error(); got != fmt.Sprintf("CSV column %q is not in the response, %s", column, want) {
			t.Errorf("asked for %s after others: %s, want %s", column, got, want)
		}
	}
}

// csvColumnsBeforeNearNamesCameFirst is csvColumns.describe as it was
// before the names near the one asked for were put first.
func csvColumnsBeforeNearNamesCameFirst(rows []any) string {
	named, longest := map[string]struct{}{}, 0
	var names []string
	for _, raw := range rows {
		switch row := raw.(type) {
		case map[string]any:
			for name := range row {
				if _, seen := named[name]; !seen {
					named[name] = struct{}{}
					names = append(names, name)
				}
			}
		case []any:
			longest = max(longest, len(row))
		}
	}
	slices.Sort(names)
	var numbered string
	switch {
	case longest == 1:
		numbered = "longest row has 1 column, read by number as 1"
	case longest > 1:
		numbered = fmt.Sprintf("longest row has %d columns, read by number from 1 to %d", longest, longest)
	}
	if len(names) == 0 {
		if numbered == "" {
			return "which has no columns"
		}
		return "whose " + numbered
	}
	var text strings.Builder
	text.WriteString("whose columns are ")
	if len(names) == 1 {
		text.Reset()
		text.WriteString("whose only column is ")
	}
	for i, name := range names[:min(len(names), csvColumnsShown)] {
		if i > 0 {
			text.WriteString(", ")
		}
		text.WriteString(model.QuoteValue(name))
	}
	if more := len(names) - csvColumnsShown; more > 0 {
		fmt.Fprintf(&text, " and %d more", more)
	}
	if numbered != "" {
		text.WriteString(", and whose ")
		text.WriteString(numbered)
	}
	text.WriteString("; column names are matched exactly")
	return text.String()
}

// Only the order of a list that holds a name near the one asked for
// changed: over 3,000 generated responses — rows by name and by number, few
// columns and many, names in two cases and with blanks — asked for a column
// they lack, the list of one with no such name is word for word what it
// was, and that of one with such a name holds the same names when nothing is
// cut, those names first.
func TestCSVColumnListIsWhatItWasButForTheNamesNearTheOneAskedFor(t *testing.T) {
	random := rand.New(rand.NewSource(12))
	spellings := func(name string) []string {
		return []string{name, strings.ToUpper(name), " " + name, name + " ", strings.ToUpper(name[:1]) + name[1:]}
	}
	near, plain := 0, 0
	for range 3000 {
		var rows []any
		for range 1 + random.Intn(3) {
			if random.Intn(4) == 0 {
				rows = append(rows, make([]any, random.Intn(4)))
				continue
			}
			row := map[string]any{}
			for range random.Intn(18) {
				name := "c" + strconv.Itoa(random.Intn(20))
				row[spellings(name)[random.Intn(5)]] = "1"
			}
			rows = append(rows, row)
		}
		column := spellings("c" + strconv.Itoa(random.Intn(24)))[random.Intn(5)]
		columns := csvColumns{rows: rows}
		if columns.inResponse(column) {
			continue
		}
		got, was := columns.describe(column), csvColumnsBeforeNearNamesCameFirst(rows)
		var first []string
		for _, raw := range rows {
			if row, named := raw.(map[string]any); named {
				for name := range row {
					if csvNearColumn(name, column) && !slices.Contains(first, name) {
						first = append(first, name)
					}
				}
			}
		}
		if len(first) == 0 {
			plain++
			if got != was {
				t.Fatalf("%v asked for %q: the list is\n%s\nand was\n%s", rows, column, got, was)
			}
			continue
		}
		near++
		slices.Sort(first)
		at := 0
		for _, name := range first[:min(len(first), csvColumnsShown)] {
			next := strings.Index(got[at:], model.QuoteValue(name))
			if next < 0 || (at == 0 && !strings.HasPrefix(got, "whose columns are "+model.QuoteValue(name)) && !strings.HasPrefix(got, "whose only column is "+model.QuoteValue(name))) {
				t.Fatalf("%v asked for %q: the list\n%s\ndoes not start with %q in order", rows, column, got, first)
			}
			at += next + 1
		}
		// The same names when none is cut, and the same end.
		names := func(text string) []string {
			_, list, _ := strings.Cut(strings.SplitN(strings.SplitN(text, ", and whose ", 2)[0], "; column", 2)[0], ` "`)
			listed := strings.Split(`"`+list, ", ")
			slices.Sort(listed)
			return listed
		}
		if !strings.Contains(was, " more") && !slices.Equal(names(got), names(was)) {
			t.Fatalf("%v asked for %q: the list\n%s\nholds other names than it did,\n%s", rows, column, got, was)
		}
		if tail := func(text string) string { return text[strings.LastIndex(text, `"`):] }; tail(got) != tail(was) {
			t.Fatalf("%v asked for %q: the list\n%s\nends otherwise than it did,\n%s", rows, column, got, was)
		}
	}
	if near < 300 || plain < 300 {
		t.Fatalf("%d lists with a name near the one asked for and %d without: the generator shows too little", near, plain)
	}
}
