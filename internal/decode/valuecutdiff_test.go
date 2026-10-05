package decode

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The errors of the Graphite decoder, of the sample lines the Prometheus
// decoder leaves out and of a CSV header as they were before a value in them
// was cut, kept as the oracles of the tests below: each formats the value
// whole, with %q, %s and %v. They share with the decoders what reads a line
// and nothing of what words an error.

// oracleCarbonLine is the error parseCarbonLine refused a line with.
func oracleCarbonLine(line string) error {
	fields := strings.Fields(line)
	if len(fields) != 2 && len(fields) != 3 {
		return fmt.Errorf("%q has %d fields; want <path> <value> <timestamp>", line, len(fields))
	}
	if err := oracleGraphitePath(fields[0]); err != nil {
		return err
	}
	if _, err := model.ParseFloat(fields[1]); err != nil {
		return fmt.Errorf("the value %q is not a number", fields[1])
	}
	if len(fields) == 3 {
		stamp, err := model.ParseFloat(fields[2])
		switch {
		case err != nil:
			return fmt.Errorf("the timestamp %q is not a number of Unix seconds", fields[2])
		case stamp >= carbonMillisecondsAbove:
			return fmt.Errorf("the timestamp %q is in milliseconds, it seems; carbon lines take Unix seconds", fields[2])
		}
	}
	return nil
}

// oracleGraphitePath is the error parseGraphitePath refused a series with.
func oracleGraphitePath(raw string) error {
	path, rest, tagged := cutTopLevel(raw)
	if path == "" {
		return fmt.Errorf("the series %q has no path", raw)
	}
	for tagged {
		var tag string
		tag, rest, tagged = strings.Cut(rest, ";")
		if name, _, ok := strings.Cut(tag, "="); !ok || name == "" {
			return fmt.Errorf("the series %q has a tag %q that is not name=value", raw, tag)
		}
	}
	return nil
}

// oracleRender is the error the render API's answer of one series without
// tags was refused with, and what it was recognised by; both empty for an
// answer that was read.
func oracleRender(t *testing.T, body []byte) (text, same string) {
	t.Helper()
	var raw []struct {
		Target     string  `json:"target"`
		Datapoints [][]any `json:"datapoints"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&raw); err != nil || len(raw) != 1 {
		t.Fatalf("%.200q is not the answer of one series: %v", body, err)
	}
	if err := oracleGraphitePath(raw[0].Target); err != nil {
		return "graphite render JSON: series 0: " + err.Error(), "graphite render JSON: series #: " + err.Error()
	}
	for j, point := range raw[0].Datapoints {
		const elements, numbers = "graphite render JSON: series %q point %s has %d elements, not [value, timestamp]", "graphite render JSON: series %q point %s is %v, not [value, timestamp] as numbers"
		if len(point) != 2 {
			return fmt.Sprintf(elements, raw[0].Target, strconv.Itoa(j), len(point)), fmt.Sprintf(elements, raw[0].Target, "#", len(point))
		}
		if point[0] == nil {
			continue
		}
		_, okValue := jsonFloat(point[0])
		_, okTime := jsonFloat(point[1])
		if !okValue || !okTime {
			return fmt.Sprintf(numbers, raw[0].Target, strconv.Itoa(j), point), fmt.Sprintf(numbers, raw[0].Target, "#", point)
		}
	}
	return "", ""
}

// oracleStray is what the report of a sample line left out said of the
// sample and of its histogram or summary family.
func oracleStray(summary bool, family, sample string) string {
	if summary {
		return fmt.Sprintf("expected %[1]s with a quantile label, %[1]s_sum or %[1]s_count as a sample of the summary %[1]s, got %[2]s without a quantile label", family, sample)
	}
	got := sample
	if got == family+"_bucket" {
		got += " without an le label"
	}
	return fmt.Sprintf("expected %[1]s_bucket with an le label, %[1]s_sum or %[1]s_count as a sample of the histogram %[1]s, got %[2]s", family, got)
}

// recognisedAsCutOf reports whether err is recognised by what the error as
// it was is recognised by, was, with the values err cut cut in it too and
// the mark for each of their lengths: by was itself for an error that cuts
// nothing.
func recognisedAsCutOf(err error, was string) bool {
	lengths := cutLength.FindAllString(err.Error(), -1)
	next := 0
	numbered := model.SameFailureText(err)
	const marked = "... (" + model.MovingMark + " bytes)"
	for next < len(lengths) && strings.Contains(numbered, marked) {
		numbered = strings.Replace(numbered, marked, lengths[next], 1)
		next++
	}
	return next == len(lengths) && isCutOf(numbered, was)
}

// textOf writes a text of up to most bytes drawn from an alphabet, a byte
// at a time, so that it may be no UTF-8.
func textOf(random *rand.Rand, alphabet string, most int) string {
	var b strings.Builder
	for n := random.IntN(most + 1); b.Len() < n; {
		b.WriteByte(alphabet[random.IntN(len(alphabet))])
	}
	return b.String()
}

// A carbon line is read, or refused, as it was: over mistaken lines written
// out and 30,000 drawn at random, of fields of up to 90 bytes with quotes,
// brackets, tags, characters of several bytes and bytes that are no UTF-8,
// the decoder reads what it read and refuses with the error it refused
// with, to the letter where no value in it is over 64 bytes, and otherwise
// with the value cut to its first 64 bytes and its length; the failure is
// recognised by the error's text as it was, with the mark for the length of
// a value that was cut.
func TestCarbonLinesAreReadAndRefusedAsTheyWere(t *testing.T) {
	now := time.Unix(1727000000, 0)
	lines := []string{
		"a", "a b c d", "a b c d e", "a 1", "a 1 1", "a x", "a 1 x", "a 1 1727000000000", "a 1 -1", "a 1_0 1", "a 0x1p-2", ";a 1 1", "a; 1 1", "a;b 1 1", "a;=c 1 1", "a;b=c;d 1 1", "a;b=c 1 1",
		"movingAverage(cpu;env=prod,'5min') 1 1", "a(;b 1 1", "é.ü 1 1", "a\xff 1 1", "a nan 1", "a 1 nan", "a +Inf 1e12",
		strings.Repeat("a", 64) + " x", strings.Repeat("a", 65) + " x", "a " + strings.Repeat("9", 70) + "x 1", "a 1 " + strings.Repeat("9", 70), strings.Repeat("é", 40) + ";b 1 1", ";" + strings.Repeat("\xff", 80) + " 1 1",
	}
	random := rand.New(rand.NewPCG(18, 1))
	for range 30000 {
		fields := make([]string, random.IntN(5))
		for i := range fields {
			switch {
			case i == 0:
				fields[i] = textOf(random, "ab.;;==('\"é\xff", 90)
			case random.IntN(3) == 0:
				fields[i] = textOf(random, "0123456789.e-+_xn", 90)
			default:
				fields[i] = textOf(random, "0123456789", 14)
			}
		}
		lines = append(lines, strings.Join(fields, " "))
	}
	refused, cut := 0, 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		was := oracleCarbonLine(line)
		path, tags, point, err := parseCarbonLine(line, now)
		if (err == nil) != (was == nil) {
			t.Fatalf("%q: err=%v, was %v", line, err, was)
		}
		if err == nil {
			if wantPath, _, _ := cutTopLevel(strings.Fields(line)[0]); path != wantPath || tags["name"] != path || fmt.Sprint(point.value) == "NaN" && !strings.Contains(strings.ToLower(line), "nan") {
				t.Errorf("%q is read as the series %q, %v, with the point %v", line, path, tags, point)
			}
			continue
		}
		refused++
		if !isCutOf(err.Error(), was.Error()) || !recognisedAsCutOf(err, was.Error()) || model.SameFailureText(err) != lengthsMarked(err.Error()) {
			t.Errorf("%q: err=%v, recognised by %q; was %v", line, err, model.SameFailureText(err), was)
		}
		if strings.Contains(err.Error(), " bytes)") {
			cut++
		} else if err.Error() != was.Error() || model.SameFailureText(err) != was.Error() {
			t.Errorf("%q: err=%v, recognised by %q; was %v", line, err, model.SameFailureText(err), was)
		}
	}
	if refused < 10000 || cut < 1000 || refused-cut < 5000 {
		t.Errorf("%d lines are refused, %d of them with a value that is cut: the lines should make many of both", refused, cut)
	}
	t.Logf("%d lines, %d refused, %d of them with a value that is cut", len(lines), refused, cut)
}

// renderValue writes a JSON value drawn at random: null, a boolean, a
// number, a text, an array or an object, nested no deeper than depth.
func renderValue(random *rand.Rand, depth int) any {
	switch kind := random.IntN(9); {
	case kind == 0:
		return nil
	case kind == 1:
		return random.IntN(2) == 0
	case kind <= 3:
		return json.Number([]string{"1", "0", "-2.5", "1e3", "1727000000", "123456789012345678901234567890"}[random.IntN(6)])
	case kind <= 5 || depth == 0:
		return textOf(random, "ab \"\\:[]mapé<nil>", []int{3, 3, 30, 80}[random.IntN(4)])
	case kind == 6:
		items := make([]any, random.IntN(4))
		for i := range items {
			items[i] = renderValue(random, depth-1)
		}
		return items
	default:
		keyed := map[string]any{}
		for range random.IntN(4) {
			keyed[textOf(random, "ab é:", 12)] = renderValue(random, depth-1)
		}
		return keyed
	}
}

// An answer of the render API is read, or refused, as it was: over 20,000
// answers of one series drawn at random, whose name is a path or none and
// whose points are of none to three values of any kind, nested and of up to
// some hundred bytes, the decoder refuses what it refused, with the error it
// refused with to the letter where no value in it is over 64 bytes, a point
// written as %v writes it, and otherwise with the name and the point cut to
// their first 64 bytes and their lengths; the failure is recognised by what
// it was, with the mark for those lengths.
func TestRenderAnswersAreReadAndRefusedAsTheyWere(t *testing.T) {
	random := rand.New(rand.NewPCG(18, 2))
	refused, cut, read := 0, 0, 0
	for range 20000 {
		target := "t" + textOf(random, "ab.é", []int{5, 5, 60, 90}[random.IntN(4)])
		if random.IntN(4) == 0 {
			target = textOf(random, "ab.;=é", []int{5, 60, 90}[random.IntN(3)])
		}
		points := make([]any, 1+random.IntN(2))
		for i := range points {
			values := make([]any, []int{2, 2, 2, 2, 0, 1, 3}[random.IntN(7)])
			for j := range values {
				if values[j] = renderValue(random, 2); random.IntN(3) == 0 {
					values[j] = json.Number("7")
				}
			}
			points[i] = values
		}
		body, err := json.Marshal([]any{map[string]any{"target": target, "datapoints": points}})
		if err != nil {
			t.Fatal(err)
		}
		was, recognised := oracleRender(t, body)
		c := model.Collector{Decoder: model.DecoderConfig{Type: "graphite"}}
		_, err = decodeBody(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{}}, &c)
		if (err == nil) != (was == "") {
			t.Fatalf("%s: err=%v, was %q", body, err, was)
		}
		if err == nil {
			read++
			continue
		}
		refused++
		if !isCutOf(err.Error(), was) || !recognisedAsCutOf(err, recognised) {
			t.Errorf("%s: err=%v, recognised by %q; was %q, recognised by %q", body, err, model.SameFailureText(err), was, recognised)
		}
		if strings.Contains(err.Error(), " bytes)") {
			cut++
		} else if err.Error() != was || model.SameFailureText(err) != recognised {
			t.Errorf("%s: err=%v, recognised by %q; was %q, recognised by %q", body, err, model.SameFailureText(err), was, recognised)
		}
	}
	if read < 500 || cut < 2000 || refused-cut < 2000 {
		t.Errorf("%d answers are read and %d refused, %d of them with a value that is cut: the answers should make many of each", read, refused, cut)
	}
	t.Logf("%d answers read, %d refused, %d of them with a value that is cut", read, refused, cut)
}

// The report of the first sample line left out reads as it did: for a
// sample named as its histogram, a bucket without an le label and a sample
// named as its summary, of families named in 1 to 5,000 bytes, bare and
// quoted, the report is the one it was to the letter where neither name is
// over 64 bytes, and otherwise shows each name by its first 64 bytes and its
// length; it is recognised by what it was, without the line, with the mark
// for those lengths.
func TestTheReportOfASampleLeftOutReadsAsItDid(t *testing.T) {
	random := rand.New(rand.NewPCG(18, 3))
	names := []string{"a", "h", "a_bucket", "x:y", strings.Repeat("a", 57), strings.Repeat("a", 58), strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("n", 5000)}
	for range 300 {
		names = append(names, "m"+textOf(random, "ab_:09", []int{10, 60, 70, 200}[random.IntN(4)]))
	}
	quoted := len(names)
	for range 100 {
		name := "é"
		for n := random.IntN([]int{10, 60, 70, 200}[random.IntN(4)]); len(name) < n; {
			name += []string{"a", ".", "é", " ", "ü", "-", "日"}[random.IntN(7)]
		}
		names = append(names, name)
	}
	cut := 0
	for i, name := range names {
		written, sample := name, func(of string) string { return of + `{a="b"} 1` + "\n" }
		if i >= quoted {
			written, sample = `"`+name+`"`, func(of string) string { return `{"` + of + `",a="b"} 1` + "\n" }
		}
		for _, tc := range []struct {
			summary bool
			sample  string
		}{{false, name}, {false, name + "_bucket"}, {true, name}} {
			kind := "histogram"
			if tc.summary {
				kind = "summary"
			}
			body := "# TYPE " + written + " " + kind + "\n" + sample(tc.sample)
			_, report, err := parseExpositionReporting([]byte(body), promOptions{})
			if err != nil || report == nil || report.LeftOutLines != 1 {
				t.Fatalf("%.200q: %v, reported %+v", body, err, report)
			}
			text, same, was := report.FirstLeftOut.Error(), model.SameFailureText(report.FirstLeftOut), oracleStray(tc.summary, name, tc.sample)
			if !isCutOf(text, "line 2: "+was) || !recognisedAsCutOf(report.FirstLeftOut, "line #: "+was) || len(text) > 900 {
				t.Errorf("%.200q: the report is %q, recognised by %q; it was %q", body, text, same, "line 2: "+was)
			}
			if strings.Contains(text, " bytes)") {
				cut++
			} else if text != "line 2: "+was || same != "line #: "+was {
				t.Errorf("%.200q: the report is %q, recognised by %q; it was %q", body, text, same, "line 2: "+was)
			}
		}
	}
	if cut < 150 || 3*len(names)-cut < 300 {
		t.Errorf("%d of %d reports cut a name: the names should make many of both", cut, 3*len(names))
	}
	t.Logf("%d reports, %d of them with a name that is cut", 3*len(names), cut)
}

// A CSV header that names a column twice is refused as it was: for names
// with quotes, line breaks, control characters, characters of several bytes
// and bytes that are no UTF-8, of the lengths around 64 bytes, the error is
// the one %q made to the letter up to 64 bytes, recognised by itself, and
// past that shows the first 64 bytes of the name, to a character boundary,
// and its length, and still says which columns they are and what to do.
func TestACSVColumnNamedTwiceIsRefusedAsItWas(t *testing.T) {
	const format = "CSV header names column %s twice, as columns 2 and 3; rename one, or set response.csv.header: false and read the columns by number"
	names := []string{"a", "used", `a"b`, "a\nb", "a\tb\x00", "é", "日本", "a\xffb", " a ", "a,b", `\`, "'"}
	for _, unit := range []string{"a", "é", "日", "\U0001F600", `"`, "\x01", "\xff"} {
		for _, bytes := range []int{60, 63, 64, 65, 66, 67, 68, 300} {
			names = append(names, strings.Repeat(unit, bytes/len(unit)), "a"+strings.Repeat(unit, (bytes-1)/len(unit)))
		}
	}
	for _, name := range names {
		var body bytes.Buffer
		w := csv.NewWriter(&body)
		if err := w.WriteAll([][]string{{"x", name, name}, {"1", "2", "3"}}); err != nil {
			t.Fatal(err)
		}
		err, made := decodeError(t, "csv", body.String(), nil)
		was := fmt.Sprintf(strings.Replace(format, "%s", "%q", 1), name)
		switch {
		case err.Error() != made.Error() || !isCutOf(err.Error(), was) || model.SameFailureText(err) != lengthsMarked(err.Error()):
			t.Errorf("the column %q: err=%v, recognised by %q; was %q", name, err, model.SameFailureText(err), was)
		case len(name) <= 64 && err.Error() != was:
			t.Errorf("the column %q: err=%v, was %q", name, err, was)
		case len(name) > 64 && err.Error() != fmt.Sprintf(format, model.QuoteValue(name)):
			t.Errorf("the column %q: err=%v, want %q", name, err, fmt.Sprintf(format, model.QuoteValue(name)))
		}
	}
}

// The expositions of the repository are read, and refused, as they were:
// each file whole, cut off after every few bytes, and with a token of each
// kind spoilt — a value, a timestamp, a label's value left open, a type —
// read as the text format and as OpenMetrics, gives the series or the error
// the parser as it was gives (promoracle_test.go): to the letter where no
// value in the error is over 64 bytes, and with the value cut to its start
// where one is. Each error is recognised by its text without its line.
func TestTheRepositorysExpositionsAreReadAndRefusedAsTheyWere(t *testing.T) {
	files, err := filepath.Glob("../../testdata/prometheus/*.prom")
	if err != nil || len(files) < 2 {
		t.Fatalf("the expositions are %v: %v", files, err)
	}
	accepted, refused := 0, 0
	for _, file := range files {
		whole, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		bodies := [][]byte{whole}
		for cut := 1; cut < len(whole); cut += 5 {
			bodies = append(bodies, whole[:cut], append(bytes.Clone(whole[:cut]), '\\'), append(bytes.Clone(whole[:cut]), "\"\n"...))
		}
		lines := strings.Split(string(whole), "\n")
		for i, line := range lines {
			for _, spoilt := range []string{line + "x", line + " 1 2", strings.Replace(line, `"}`, `}`, 1), strings.Replace(line, "gauge", "nope", 1), strings.Replace(line, "{", "{{", 1), line + "\n" + line} {
				if spoilt != line {
					bodies = append(bodies, []byte(strings.Join(lines[:i], "\n")+"\n"+spoilt+"\n"+strings.Join(lines[i+1:], "\n")))
				}
			}
		}
		for _, body := range bodies {
			for _, reading := range []promReading{{}, {openMetrics: true}} {
				if compareExposition(t, body, reading) {
					accepted++
				} else {
					refused++
				}
			}
		}
	}
	if accepted < 1000 || refused < 1000 {
		t.Errorf("%d readings are accepted and %d refused: the bodies should make many of both", accepted, refused)
	}
	t.Logf("%d readings accepted, %d refused", accepted, refused)
}
