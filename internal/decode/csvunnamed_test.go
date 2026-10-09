package decode

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// columnIsEmpty is how the csv decoder once found whether an unnamed header
// column holds nothing: by looking through every row for each such column.
// What filledColumns finds in one pass is held against it, and the decoders
// as they were, kept by the tests, still read the columns by it.
func columnIsEmpty(rows [][]string, i int, trim bool) bool {
	for _, row := range rows {
		if i >= len(row) {
			continue
		}
		v := row[i]
		if trim {
			v = strings.TrimSpace(v)
		}
		if v != "" {
			return false
		}
	}
	return true
}

// countFilledColumnsLooked has the test count the cells filledColumns looks
// at, and how many times it is called, until the test ends.
func countFilledColumnsLooked(t *testing.T) (cells, calls *int) {
	t.Helper()
	cells, calls = new(int), new(int)
	filledColumnsLooked = func(n int) { *cells += n; *calls++ }
	t.Cleanup(func() { filledColumnsLooked = nil })
	return cells, calls
}

// A header of one named column and k unnamed ones over k rows of one field
// each, the unnamed columns left out as empty, is decoded by looking at
// each cell of the rows once, k of them, where every row was looked through
// for each unnamed column, k times k: a body of 450 kB held a processor for
// 34 seconds, past the time a probe may take. With rows as wide as the
// header, the cells looked at are the cells the rows hold. A header without
// an unnamed column has no cell looked at for it.
func TestUnnamedCSVColumnsAreFoundEmptyByLookingAtEachCellOnce(t *testing.T) {
	k := alloctest.UnlessRaced(50000, 10000)
	cells, calls := countFilledColumnsLooked(t)
	body := "a" + strings.Repeat(",", k) + "\n" + strings.Repeat("1\n", k)
	d, err := decodeCSVBody([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	rows := d.Data.(*CSVRows)
	if columns := rows.Columns(); rows.Len() != k || len(columns) != 1 || columns[0].Name != "a" {
		t.Fatalf("%d rows of the columns %v, want %d of a", rows.Len(), columns, k)
	}
	if *calls != 1 || *cells > k {
		t.Errorf("the unnamed columns were looked for %d times, at %d cells; want once, at %d cells at most", *calls, *cells, k)
	}

	*cells, *calls = 0, 0
	const wide, lines = 200, 50
	line := "1" + strings.Repeat(",", wide)
	body = "a" + strings.Repeat(",", wide) + "\n" + strings.Repeat(line+"\n", lines)
	if _, err := decodeCSVBody([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || *cells > (wide+1)*lines {
		t.Errorf("rows as wide as the header had their unnamed columns looked for %d times, at %d cells; want once, at %d cells at most", *calls, *cells, (wide+1)*lines)
	}

	*cells, *calls = 0, 0
	if _, err := decodeCSVBody([]byte("a,b\n1,2\n3,4\n")); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Errorf("a header naming every column had its columns looked through %d times", *calls)
	}
}

// What filledColumns finds of each column is what columnIsEmpty, which the
// decoder looked through the rows with for each column, found of it: of
// generated tables of ragged rows, empty, blank and filled cells, shorter
// and longer than the columns asked about, with trim and without.
func TestFilledColumnsIsWhatLookingThroughEachColumnFound(t *testing.T) {
	random := rand.New(rand.NewSource(53))
	filledSome, emptySome := 0, 0
	for range alloctest.UnlessRaced(20000, 4000) {
		rows := make([][]string, random.Intn(6))
		for r := range rows {
			rows[r] = make([]string, random.Intn(8))
			for i := range rows[r] {
				rows[r][i] = []string{"", "", "", " ", "\t", "1", " x "}[random.Intn(7)]
			}
		}
		n, trim := random.Intn(9), random.Intn(2) == 0
		filled := filledColumns(rows, n, trim)
		if len(filled) != n {
			t.Fatalf("%d columns found of %d", len(filled), n)
		}
		for i := range n {
			if filled[i] == columnIsEmpty(rows, i, trim) {
				t.Fatalf("column %d of %q (trim %v) is found filled %v, and columnIsEmpty says empty %v", i, rows, trim, filled[i], columnIsEmpty(rows, i, trim))
			}
			if filled[i] {
				filledSome++
			} else {
				emptySome++
			}
		}
	}
	if filledSome == 0 || emptySome == 0 {
		t.Errorf("the tables had %d filled columns and %d empty ones: they do not cover both", filledSome, emptySome)
	}
}

// A CSV with unnamed header columns decodes to what the decoder gave when it
// looked through every row for each unnamed column (decodeCSVBeforeRows,
// which still does): the same rows, and the same refusal of an unnamed
// column that holds a value, or of a name given twice, whichever comes
// first. The generated headers have unnamed columns written empty, quoted
// empty and blank, duplicate names and a named column among them, over
// ragged rows of empty, quoted empty, blank and filled cells, with
// trim_space and without.
func TestUnnamedCSVColumnsDecodeAsWhenEachWasLookedForInEveryRow(t *testing.T) {
	random := rand.New(rand.NewSource(530))
	pick := func(of ...string) string { return of[random.Intn(len(of))] }
	counted := map[string]int{}
	bodies := alloctest.UnlessRaced(10000, 2000)
	for range bodies {
		width := 1 + random.Intn(10)
		heads := make([]string, width)
		for i := range heads {
			heads[i] = pick("", "", `""`, " ", "c"+strconv.Itoa(i), "c"+strconv.Itoa(i), "c"+strconv.Itoa(i), "a")
		}
		lines := []string{strings.Join(heads, ",")}
		for n := random.Intn(6); n > 0; n-- {
			cells := make([]string, random.Intn(width+3))
			for i := range cells {
				cells[i] = pick("", "", "", "", "", "", `""`, " ", "1", " 2 ")
			}
			lines = append(lines, strings.Join(cells, ","))
		}
		cfg := model.CSVConfig{TrimSpace: random.Intn(2) == 0}
		diff, outcome := decodeCSVBothWays([]byte(strings.Join(lines, "\n")+"\n"), cfg)
		if diff != "" {
			t.Fatalf("%q under %+v: %s", lines, cfg, diff)
		}
		counted[outcome]++
	}
	t.Logf("%v", counted)
	for _, outcome := range []string{"refused", "read by name", "read by name, with a short row"} {
		if counted[outcome] < bodies/20 {
			t.Errorf("%d generated bodies were %s, fewer than %d: the bodies do not cover it", counted[outcome], outcome, bodies/20)
		}
	}
}
