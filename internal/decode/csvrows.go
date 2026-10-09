package decode

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// CSVRows is what the csv decoder makes of a response: its rows, each the
// cells its line holds, and, with a header row, the header's names, once for
// all the rows. A row read by name has every column the header names, and
// those a short row lacks are empty in it, but they are not made: the
// decoder once gave each row a map with an entry for every column the
// header names, so a header of 5,000 columns over 5,000 lines of one field,
// a body of 39 kB, was 25 million entries and 2 GB, and a plain body of one
// short column held 175 times its size. The rows now cost the cells the
// body holds.
//
// What a rule, an error and a script read of the rows is what they read of
// those maps: a row read by name as a mapping of every column the header
// names, the cells it lacks empty (Cell, Document), and without a header row
// a list of the row's fields, read by number from 1.
type CSVRows struct {
	// named says the rows are read by the header's names.
	named bool
	// column is the place of each name the header gives a column, which it
	// gives once each; an unnamed column, which the decoder leaves out when
	// it is empty, has none.
	column map[string]int
	// rows is the cells of each row: with a header, as far as the header's
	// last column at most, since the decoder refuses a value past it.
	rows [][]string
}

// CSVColumn is a column the header names, and its place.
type CSVColumn struct {
	Name  string
	Place int
}

// Len is how many rows there are, the header's line not counted.
func (t *CSVRows) Len() int { return len(t.rows) }

// Named reports whether the rows are read by the header's names.
func (t *CSVRows) Named() bool { return t.named }

// Row is the cells of row i, from 0: those its line holds, which with a
// header are as many as the header's columns at most.
func (t *CSVRows) Row(i int) []string { return t.rows[i] }

// Columns is the columns the header names, unnamed ones left out, in the
// order of their names' bytes, as a script reads a row's keys; nil without
// a header row.
func (t *CSVRows) Columns() []CSVColumn {
	if !t.named {
		return nil
	}
	columns := make([]CSVColumn, 0, len(t.column))
	for name, place := range t.column {
		columns = append(columns, CSVColumn{Name: name, Place: place})
	}
	slices.SortFunc(columns, func(a, b CSVColumn) int { return strings.Compare(a.Name, b.Name) })
	return columns
}

// Cell is the cell of row i in column, and whether the row has the column:
// with a header, a column the header names, the cell "" where the row is
// short of it; without one, a column number from 1, written as a rule names
// it, up to the row's own length.
func (t *CSVRows) Cell(i int, column string) (string, bool) {
	row := t.rows[i]
	if t.named {
		place, ok := t.column[column]
		if !ok {
			return "", false
		}
		if place < len(row) {
			return row[place], true
		}
		return "", true
	}
	n := csvColumnNumber(column)
	if n == 0 || n > len(row) {
		return "", false
	}
	return row[n-1], true
}

// Has reports whether any row has column: with a header, one the header
// names, when there is a row; without one, a number up to the longest row's
// length.
func (t *CSVRows) Has(column string) bool {
	if t.named {
		_, ok := t.column[column]
		return ok && len(t.rows) > 0
	}
	n := csvColumnNumber(column)
	return n > 0 && n <= t.Longest()
}

// Longest is the length of the longest row.
func (t *CSVRows) Longest() int {
	longest := 0
	for _, row := range t.rows {
		longest = max(longest, len(row))
	}
	return longest
}

// csvColumnNumber is the column number a rule names a column of a row read
// without a header by, from 1, and 0 for text that is not one as the number
// would be written: "1" is column 1, "01" and "+1" are none.
func csvColumnNumber(column string) int {
	if n, err := strconv.Atoi(column); err == nil && n > 0 && strconv.Itoa(n) == column {
		return n
	}
	return 0
}

// Document is the rows as the decoder once gave them: a list of maps of
// every column the header names to its cell, "" where the row is short of
// it, or without a header a list of lists of fields. It costs what that
// cost, the header's columns times the rows, and is for json.Marshal, which
// writes the rows as a script reads them, and for tests.
func (t *CSVRows) Document() []any {
	out := make([]any, 0, len(t.rows))
	for i, row := range t.rows {
		if !t.named {
			fields := make([]any, len(row))
			for j, v := range row {
				fields[j] = v
			}
			out = append(out, fields)
			continue
		}
		m := make(map[string]any, len(t.column))
		for name := range t.column {
			m[name], _ = t.Cell(i, name)
		}
		out = append(out, m)
	}
	return out
}

// MarshalJSON writes the rows as json.Marshal writes Document.
func (t *CSVRows) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Document())
}
