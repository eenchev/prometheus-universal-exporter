package decode

import (
	"bytes"
	"fmt"
	"testing"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// csvPaddedBodies are bodies of about size bytes that trim_space takes
// blanks out of: a report with every text in quotes and blanks after the
// closing quote, three runs of blanks a row, and the body with the most runs
// of blanks its size can hold, an empty quoted field and a blank over and
// over.
func csvPaddedBodies(size int) map[string][]byte {
	const row = "\"web01\" ,\"eu west\"  ,\"72\" \n"
	return map[string][]byte{
		"a padded report":        bytes.Repeat([]byte(row), size/len(row)),
		"a blank every 4 bytes":  append(bytes.Repeat([]byte(`"" ,`), size/4), "\"\"\n"...),
		"a line of padded texts": append(bytes.Repeat([]byte("\"a\" \t,"), size/7), "\"z\"\n"...),
	}
}

// csvReadCost is how many bytes and how many allocations one call of read
// takes, over several calls after a first one, as alloctest measures them.
func csvReadCost(t *testing.T, read func() error) (bytesPerRun uint64, allocations float64) {
	t.Helper()
	const runs = 3
	var err error
	allocations, bytesPerRun = alloctest.Allocations(runs, func() {
		if failed := read(); failed != nil {
			err = failed
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytesPerRun, allocations
}

// Taking the blanks around quotes out of a body costs one copy of the body
// and nothing else, however many runs of blanks it has: reading a padded
// body under trim_space allocates, beyond what reading the same body written
// without those blanks does, one buffer of the body's size, for a body of 64
// KiB as for one eight times as long. It kept the place and length of every
// run it took out, which only an error's column is counted from, in a list
// that grew with the body: some twenty times the body's size in all for a
// blank every four bytes, in thirty allocations where there is one.
func TestCSVBlanksAroundQuotesCostOneCopyOfTheBody(t *testing.T) {
	for _, size := range []int{64 << 10, 512 << 10} {
		for name, padded := range csvPaddedBodies(size) {
			clean, removed := withoutBlanksAroundQuotes(padded, ',', false)
			if len(removed) < size/16 || len(clean) >= len(padded) {
				t.Fatalf("%s of %d bytes: %d runs of blanks were taken out, which is too few to tell", name, len(padded), len(removed))
			}
			paddedBytes, paddedAllocations := csvReadCost(t, func() error {
				_, err := readCSV(padded, ',', true)
				return err
			})
			cleanBytes, cleanAllocations := csvReadCost(t, func() error {
				_, err := readCSVBody(clean, ',', true)
				return err
			})
			if extra := paddedAllocations - cleanAllocations; extra > 2 {
				t.Errorf("%s of %d bytes: %.0f allocations beyond those of reading it without the blanks, want the one of the copy", name, len(padded), extra)
			}
			if extra, most := int64(paddedBytes)-int64(cleanBytes), int64(len(padded)+len(padded)/4); extra > most {
				t.Errorf("%s of %d bytes: %d bytes allocated beyond those of reading it without the blanks, want at most %d, a copy of the body", name, len(padded), extra, most)
			}
		}
	}
	// A body without such blanks is read where it lies.
	plain := bytes.Repeat([]byte("\"web01\",\"eu west\",72\n"), 1000)
	plainBytes, plainAllocations := csvReadCost(t, func() error {
		_, err := readCSV(plain, ',', true)
		return err
	})
	bodyBytes, bodyAllocations := csvReadCost(t, func() error {
		_, err := readCSVBody(plain, ',', true)
		return err
	})
	if plainAllocations > bodyAllocations+1 || plainBytes > bodyBytes+uint64(len(plain)/4) {
		t.Errorf("a body without blanks around its quotes: %.0f allocations and %d bytes, the reader alone %.0f and %d", plainAllocations, plainBytes, bodyAllocations, bodyBytes)
	}
}

// An error of a body that blanks were taken out of still names the column
// the body has it in, now that what was taken out is found only once the
// reader has failed: after sixteen thousand runs of blanks on the error's
// line, of one byte and of three, on the first line and on a later one, with
// the blanks before opening quotes taken out as well, and in a body read
// leniently for a bare quote. The column is the one counted from what the
// pre-pass says it took out, as it was.
func TestCSVErrorColumnAfterManyBlanksAroundQuotes(t *testing.T) {
	const runs = 16 << 10
	for _, tc := range []struct {
		name      string
		delimiter rune
		body      string
		line      int
		column    int
	}{
		{"one line", ',', string(bytes.Repeat([]byte(`"" ,`), runs)) + "\"x\"y\n", 1, 4*runs + 3},
		{"a later line", ',', "a,b\n\"q\" ,\"r\"\n" + string(bytes.Repeat([]byte("\"a\" \t,"), runs)) + "\"open\n", 3, 7*runs + 7},
		{"before and after quotes", '\t', string(bytes.Repeat([]byte(" \"a\" \t"), runs)) + " \"st\"ray\"\n", 1, 6*runs + 5},
		{"beside a bare quote", ',', "5\" disk,x\n" + string(bytes.Repeat([]byte(`"" ,`), runs)) + "\"x\"y\n", 2, 4*runs + 3},
	} {
		body := []byte(tc.body)
		_, err := readCSV(body, tc.delimiter, true)
		want := fmt.Sprintf(`parse error on line %d, column %d: extraneous or missing " in quoted-field`, tc.line, tc.column)
		if err == nil || err.Error() != want {
			t.Errorf("%s: err=%v, want %s", tc.name, err, want)
		}
		// What the error was before the blanks were taken out of the body
		// without a list of them.
		readerSkips := !unicode.IsSpace(tc.delimiter)
		clean, removed := withoutBlanksAroundQuotes(body, tc.delimiter, !readerSkips)
		_, was := readCSVBody(clean, tc.delimiter, readerSkips)
		if was = originalColumn(was, clean, removed); was == nil || err == nil || was.Error() != err.Error() {
			t.Errorf("%s: err=%v, from the list of what was taken out %v", tc.name, err, was)
		}
	}
}

// BenchmarkCSVPaddedBody measures reading a body of a MiB under trim_space
// when every text of it is in quotes with blanks after the closing quote, and
// the same body written without those blanks, which is what the reader is
// given of the first: the difference is what taking the blanks out costs.
func BenchmarkCSVPaddedBody(b *testing.B) {
	padded := csvPaddedBodies(1 << 20)["a padded report"]
	clean, _ := withoutBlanksAroundQuotes(padded, ',', false)
	for name, body := range map[string][]byte{"padded": padded, "without the blanks": clean} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := readCSV(body, ',', true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
