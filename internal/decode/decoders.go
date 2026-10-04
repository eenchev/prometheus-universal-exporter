package decode

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Decoded is a response decoded for a transform. Kind names the decoder, and
// Data holds what it produced: normalized JSON or YAML values, CSV rows, an
// *xmlquery.Node, an *HTMLDecoded, a model.MetricSet for Prometheus text, the
// Graphite series document (graphite.go), or the body as a string. Raw is the body the decoder read.
type Decoded struct {
	Kind string
	Data any
	Raw  []byte
	// Graphite reports what the graphite decoder left out; nil for the
	// others.
	Graphite *GraphiteReport
	// Prometheus reports the sample lines the prometheus decoder left out;
	// nil when it left out none, and for the others.
	Prometheus *PrometheusReport
}

// HTMLDecoded is a parsed HTML document and the body it was parsed from.
type HTMLDecoded struct {
	Document *goquery.Document
	Raw      []byte
}

// detectFormat picks the decoder for a collector whose decoder.type is auto,
// by the response's content type, and then by its content.
func detectFormat(r *fetch.HTTPResponse) string {
	kind, _ := detectFormatKeeping(r)
	return kind
}

// sniffedJSON is what a detection that read the body as JSON, to see
// whether it is JSON, made of it: the value, and the body it is the value
// of. Decode returns it rather than reading the same body a second time.
type sniffedJSON struct {
	body  []byte
	value any
}

// of returns the value when body is the body it was read from, the same
// bytes in the same place, and says whether it is.
func (s *sniffedJSON) of(body []byte) (any, bool) {
	if s == nil || len(body) == 0 || len(body) != len(s.body) || &body[0] != &s.body[0] {
		return nil, false
	}
	return s.value, true
}

// detectFormatKeeping is detectFormat, and what the detection read of a
// body it took for JSON by its content.
func detectFormatKeeping(r *fetch.HTTPResponse) (string, *sniffedJSON) {
	rawCT := strings.ToLower(r.Headers.Get("Content-Type"))
	ct := strings.TrimSpace(strings.Split(rawCT, ";")[0])
	switch {
	case ct == "application/json" || strings.HasSuffix(ct, "+json"):
		return "json", nil
	case ct == "application/yaml" || ct == "text/yaml" || ct == "application/x-yaml":
		return "yaml", nil
	case ct == "application/xml" || ct == "text/xml":
		return "xml", nil
	case ct == "text/csv":
		return "csv", nil
	case ct == "text/html":
		return "html", nil
	case ct == "application/openmetrics-text" || ct == "text/plain" && strings.Contains(rawCT, "version=0.0.4"):
		return "prometheus", nil
	case ct == fetch.GraphiteContentType:
		return "graphite", nil
	}
	b := bytes.TrimSpace(r.Body)
	// JSON first: a JSON document may hold "# HELP " in a string, while
	// Prometheus exposition, which starts with a comment or a name, never
	// parses as JSON.
	//
	// Whether it parses is found by decoding it, with the decoder the json
	// decoder is (sniffJSON), and what that made is kept: the body was
	// parsed here by encoding/json, the result thrown away, and then
	// decoded, so a JSON answer without a JSON Content-Type was read twice.
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') {
		if value, ok := sniffJSON(b); ok {
			// The value is the body's when the decoder reads the body as
			// it read b: when nothing but the whitespace of JSON was
			// trimmed. A form feed or a no-break space before or after the
			// document is none, and the decode that follows refuses the
			// body over it, saying where it is.
			if len(bytes.Trim(r.Body, " \t\r\n")) == len(b) {
				return "json", &sniffedJSON{body: r.Body, value: value}
			}
			return "json", nil
		}
	}
	if bytes.Contains(b, []byte("# TYPE ")) || bytes.Contains(b, []byte("# HELP ")) {
		return "prometheus", nil
	}
	// An HTML page is markup too, and rarely well-formed XML, so it is
	// recognised by its doctype or root element before anything starting
	// with < is taken for XML.
	if looksLikeHTML(b) {
		return "html", nil
	}
	if bytes.HasPrefix(b, []byte("<")) {
		return "xml", nil
	}
	if looksLikeCarbon(b) {
		return "graphite", nil
	}
	return "text", nil
}

// looksLikeHTML reports whether markup starts with an HTML doctype or an
// <html> element, in any case, within its first KiB, once comments, an XML
// declaration (XHTML has one) and the whitespace between them are passed
// over. A byte order mark is gone by now (textencoding.go).
func looksLikeHTML(b []byte) bool {
	head := bytes.ToLower(b[:min(len(b), 1024)])
	for {
		head = bytes.TrimLeft(head, " \t\r\n\f")
		switch {
		case bytes.HasPrefix(head, []byte("<!--")):
			end := bytes.Index(head, []byte("-->"))
			if end < 0 {
				return false
			}
			head = head[end+3:]
		case bytes.HasPrefix(head, []byte("<?xml")):
			end := bytes.Index(head, []byte("?>"))
			if end < 0 {
				return false
			}
			head = head[end+2:]
		default:
			return bytes.HasPrefix(head, []byte("<!doctype html")) || bytes.HasPrefix(head, []byte("<html")) && (len(head) == 5 || !isNameByte(head[5]))
		}
	}
}

// isNameByte reports whether c continues an element name, so <htmlfoo> is
// not taken for <html>.
func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':'
}

// Decode converts r's body to UTF-8 and decodes it with the decoder c's
// configuration, the response's content type or its content selects.
func Decode(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
	// The body is converted to UTF-8 before anything reads it (textencoding.go).
	named, err := convertToUTF8(r, c)
	if err != nil {
		return nil, err
	}
	kind := c.Decoder.Type
	var sniffed *sniffedJSON
	if kind == "" || kind == "auto" {
		kind, sniffed = detectFormatKeeping(r)
	}
	if !named {
		if err := convertFromDocument(r, kind); err != nil {
			return nil, err
		}
	}
	switch kind {
	case "json":
		// Read in one pass into the values the transforms use (jsonvalue.go),
		// which a detection by content has done already.
		if v, read := sniffed.of(r.Body); read {
			return &Decoded{Kind: kind, Data: v, Raw: r.Body}, nil
		}
		v, err := decodeJSON(r.Body)
		if err != nil {
			return nil, fmt.Errorf("JSON decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: v, Raw: r.Body}, nil
	case "yaml":
		v, err := decodeYAML(r.Body)
		if err != nil {
			return nil, fmt.Errorf("YAML decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: model.Normalize(v), Raw: r.Body}, nil
	case "csv":
		return decodeCSV(r, c)
	case "xml":
		n, err := ParseXML(r.Body)
		if err != nil {
			return nil, fmt.Errorf("XML decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: n, Raw: r.Body}, nil
	case "html":
		d, err := ParseHTML(r.Body)
		if err != nil {
			return nil, fmt.Errorf("HTML decode: %w", err)
		}
		return &Decoded{Kind: kind, Data: &HTMLDecoded{Document: d, Raw: r.Body}, Raw: r.Body}, nil
	case "prometheus":
		return decodePrometheus(r, c)
	case "text":
		return &Decoded{Kind: kind, Data: string(r.Body), Raw: r.Body}, nil
	case "graphite":
		return decodeGraphite(r, c)
	default:
		return nil, fmt.Errorf("unsupported decoder %q", kind)
	}
}

func decodeCSV(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
	cfg := c.Response.CSV
	delim := ','
	if cfg.Delimiter != "" {
		rr := []rune(cfg.Delimiter)
		if len(rr) != 1 {
			return nil, errors.New("CSV delimiter must be one character")
		}
		delim = rr[0]
	}
	rows, err := readCSV(r.Body, delim, cfg.TrimSpace)
	if err != nil {
		// The line and the column the reader names are where in the body
		// it stopped, which is no part of what the failure is to the log.
		var parse *csv.ParseError
		if errors.As(err, &parse) {
			err = model.SameFailureAs(err, strings.Replace(err.Error(), parse.Error(), "parse error: "+parse.Err.Error(), 1))
		}
		return nil, fmt.Errorf("CSV decode: %w", err)
	}
	if len(rows) == 0 {
		return &Decoded{Kind: "csv", Data: []any{}, Raw: r.Body}, nil
	}
	header := true
	if cfg.Header != nil {
		header = *cfg.Header
	}
	out := []any{}
	if header {
		heads := rows[0]
		// A header naming one column twice, or leaving two unnamed, would
		// have the later column overwrite the earlier in every row, without
		// a word. An unnamed column empty in every row, as a delimiter
		// ending each line leaves, holds nothing to lose and is left out.
		column := map[string]int{}
		for i := range heads {
			if cfg.TrimSpace {
				heads[i] = strings.TrimSpace(heads[i])
			}
			if heads[i] == "" {
				if columnIsEmpty(rows[1:], i, cfg.TrimSpace) {
					continue
				}
				return nil, fmt.Errorf("CSV header leaves column %d unnamed, and it holds values; name it, or set response.csv.header: false and read the columns by number", i+1)
			}
			if first, seen := column[heads[i]]; seen {
				return nil, fmt.Errorf("CSV header names column %q twice, as columns %d and %d; rename one, or set response.csv.header: false and read the columns by number", heads[i], first+1, i+1)
			}
			column[heads[i]] = i
		}
		for n, row := range rows[1:] {
			// A field past the header's last column has no name to be read
			// by, and went unseen: a delimiter the header's line was split
			// by and the rows' were not, or a quote read as text, showed
			// only as values in the wrong columns. Empty fields there are
			// what a delimiter ending the line leaves.
			if len(row) > len(heads) {
				if i := len(heads) + firstValue(row[len(heads):], cfg.TrimSpace); i < len(row) {
					return nil, model.Errorf("CSV line %d has a value in column %d, which the header does not name; name the column in the header, or set response.csv.header: false and read the columns by number; if the line is split where it should not be, check response.csv.delimiter and response.csv.trim_space", model.Position(csvFieldLine(r.Body, delim, cfg.TrimSpace, n+1, i)), model.Position(i+1))
				}
			}
			m := map[string]any{}
			for i, k := range heads {
				if k == "" {
					continue
				}
				if i < len(row) {
					v := row[i]
					if cfg.TrimSpace {
						v = strings.TrimSpace(v)
					}
					m[k] = v
				} else {
					m[k] = ""
				}
			}
			out = append(out, m)
		}
	} else {
		for _, row := range rows {
			a := make([]any, len(row))
			for i, v := range row {
				// Both sides, as with a header: the reader itself trims
				// only what leads a field.
				if cfg.TrimSpace {
					v = strings.TrimSpace(v)
				}
				a[i] = v
			}
			out = append(out, a)
		}
	}
	return &Decoded{Kind: "csv", Data: out, Raw: r.Body}, nil
}

// firstValue is the place of the first of fields that holds a value, which
// blanks alone are not under trim, or how many fields there are when none
// does.
func firstValue(fields []string, trim bool) int {
	for i, v := range fields {
		if trim {
			v = strings.TrimSpace(v)
		}
		if v != "" {
			return i
		}
	}
	return len(fields)
}

// columnIsEmpty reports whether column i holds nothing in any of rows.
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

func decodePrometheus(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
	options := promOptions{openMetrics: isOpenMetrics(r)}
	// A prometheus transform passes on only the series its rules, or its
	// include and exclude, pick, each at least once, so the decoder keeps
	// only those, and stops at the first past limits.max_metrics, as the
	// transform would have: a body of a million series costs a scrape
	// that keeps ten what the ten cost. A pre-script, or a python
	// transform, is given every series, and the transform counts what it
	// makes of them.
	if c.Transform.Type == "prometheus" && strings.TrimSpace(c.Transform.PreScript) == "" {
		options.keep = prometheusKeeps(c)
		options.limit = c.Limits.MaxMetrics
	}
	metrics, report, err := parseExpositionReporting(r.Body, options)
	if errors.Is(err, model.ErrLimitExceeded) {
		return nil, err
	}
	if err != nil {
		format := "Prometheus exposition"
		if options.openMetrics {
			format = "OpenMetrics exposition"
		}
		return nil, fmt.Errorf("decoding %s: %w", format, err)
	}
	return &Decoded{Kind: "prometheus", Data: model.MetricSet{Metrics: metrics}, Raw: r.Body, Prometheus: report}, nil
}

// prometheusKeeps says which metric names a prometheus transform passes on,
// as applyPrometheusTransform decides: with rules, the names a rule's
// expression, or its name when it has none, matches; without, the names
// include matches, or every name when it is empty, less those exclude
// matches. An expression that does not compile keeps everything, and the
// transform reports it.
func prometheusKeeps(c *model.Collector) func(string) bool {
	compile := func(patterns []string) ([]*regexp.Regexp, bool) {
		out := make([]*regexp.Regexp, 0, len(patterns))
		for _, pattern := range patterns {
			re, err := expr.CompileRegex(pattern)
			if err != nil {
				return nil, false
			}
			out = append(out, re)
		}
		return out, true
	}
	matchesAny := func(res []*regexp.Regexp, name string) bool {
		for _, re := range res {
			if re.MatchString(name) {
				return true
			}
		}
		return false
	}
	if len(c.Metrics) > 0 {
		patterns := make([]string, 0, len(c.Metrics))
		for _, rule := range c.Metrics {
			pattern := rule.Expression
			if pattern == "" {
				pattern = "^" + regexp.QuoteMeta(rule.Name) + "$"
			}
			patterns = append(patterns, pattern)
		}
		rules, ok := compile(patterns)
		if !ok {
			return nil
		}
		return func(name string) bool { return matchesAny(rules, name) }
	}
	includes, okIncludes := compile(c.Transform.Include)
	excludes, okExcludes := compile(c.Transform.Exclude)
	if !okIncludes || !okExcludes {
		return nil
	}
	if len(includes) == 0 && len(excludes) == 0 {
		return nil
	}
	return func(name string) bool {
		return (len(includes) == 0 || matchesAny(includes, name)) && !matchesAny(excludes, name)
	}
}

// decodeYAML decodes a YAML document as the transforms read it. A scalar
// that YAML reads as a timestamp, such as updated: 2024-06-01, is kept as the
// text it was written as: decoded, it would be a time.Time, which neither jq
// nor a label can use as written.
//
// The body must be one document: a stream of several is refused, as JSON
// with trailing data is, rather than every document after the first being
// dropped unseen. A leading --- and an empty document after the first, as a
// trailing --- makes, are not a second document.
//
// The document is decoded by the YAML library, but for one with a mapping of
// very many keys or with many keys written twice, which the library takes
// time and memory that grow with the square of the document for: those are
// decoded, and refused, in time and memory linear in it (yamlkeys.go).
//
// The library panics on some documents it should refuse: a merge into a
// mapping that has a sequence or a mapping for a key, `<<: {[x]: 1}` beside a
// key that is no text, hashes what cannot be hashed. The panic took the probe
// with it, answered as an internal error with a stack in the log on every
// scrape; it is the decode's failure instead (yamlPanicked), and so is a
// panic of the exporter's own code here, which says that it is one and
// where.
func decodeYAML(body []byte) (any, error) {
	return yamlReading{large: yamlLargeMapping}.decode(body)
}

// decode is decodeYAML, as r decodes the document parsed.
func (r yamlReading) decode(body []byte) (v any, err error) {
	defer func() {
		if failed := recover(); failed != nil {
			v, err = nil, yamlPanicked(failed, yamlPanicPlace())
		}
	}()
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if err := decoder.Decode(&root); errors.Is(err, io.EOF) {
		// An empty document.
		return nil, nil
	} else if err != nil {
		return nil, yamlFailure(err)
	}
	for {
		var next yaml.Node
		err := decoder.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, yamlFailure(err)
		}
		if !emptyYAMLDocument(&next) {
			return nil, errors.New("the body holds more than one YAML document (separated by ---); multi-document YAML is not supported")
		}
	}
	timestampsAsText(&root, map[*yaml.Node]bool{})
	return r.value(&root)
}

// yamlPlace is where a panic was raised: the file, without its directory,
// the line and the function, with its package's name, and whether that is
// the YAML library's.
type yamlPlace struct {
	file     string
	line     int
	function string
	library  bool
}

// yamlLibrary is the YAML library's package, and yamlRaisesAgain the
// function of it that recovers every panic of a decoding and raises again
// one that is not the library's own error (yaml.go, handleErr).
const (
	yamlLibrary     = "gopkg.in/yaml.v3"
	yamlRaisesAgain = "yaml.v3.handleErr"
)

// yamlPanicPlace is where the panic that is being recovered was raised. It
// is for the deferred function that recovers it, during which what
// panicked is still on the stack: under that function is the runtime, which
// called it, and under the runtime what was running.
//
// The place is the innermost function there of the YAML library or of the
// exporter. The standard library's are passed over — the runtime's own, which
// raises a runtime error, reflect's, which the library sets a map through,
// and any other one of the two called — and so is the library's function
// that passes a panic on, under which is what raised it. So a panic is the
// library's when the library was running, and the exporter's own, a defect
// here, when the exporter was.
func yamlPanicPlace() yamlPlace {
	for size := 64; ; size *= 8 {
		callers := make([]uintptr, size)
		callers = callers[:runtime.Callers(1, callers)]
		frames := runtime.CallersFrames(callers)
		raised := false
		for more := len(callers) > 0; more; {
			var frame runtime.Frame
			frame, more = frames.Next()
			in, function := yamlFunction(frame.Function)
			switch first, _, _ := strings.Cut(in, "/"); {
			case !strings.Contains(first, "."):
				// The standard library's: first of them the runtime's panic.
				raised = true
			case raised && function != yamlRaisesAgain:
				return yamlPlace{file: filepath.Base(frame.File), line: frame.Line, function: function, library: in == yamlLibrary}
			}
		}
		if len(callers) < size {
			// Not on the stack, which this function's caller is.
			return yamlPlace{file: "an unknown file", function: "an unknown function"}
		}
	}
}

// yamlFunction is the path of the package of a function named as the
// runtime names it, `gopkg.in/yaml%2ev3.(*decoder).mapping`, and the
// function's name with its package's, `yaml.v3.(*decoder).mapping`. The
// package's path ends at the first dot after its last slash, and the name of
// a method or of a function with type arguments may hold both after a
// bracket; a dot in the last part of the path is written %2e.
func yamlFunction(name string) (in, function string) {
	path := name
	if bracket := strings.IndexAny(name, "(["); bracket >= 0 {
		path = name[:bracket]
	}
	last := strings.LastIndexByte(path, '/') + 1
	dot := strings.IndexByte(path[last:], '.')
	if dot < 0 {
		dot = len(path) - last
	}
	return strings.ReplaceAll(name[:last+dot], "%2e", "."), strings.ReplaceAll(name[last:], "%2e", ".")
}

// yamlPanicked is the error for a document the decoding panicked on with
// failed, at a place: in the words of the panic and without a stack.
//
// A panic of the YAML library is the library failing on the document. A
// panic of the exporter's own code is a defect of the exporter, and says so,
// with the place for whoever is to find it: the document may be a good one,
// and nothing the operator changes in it is known to help.
//
// A runtime error's words can hold numbers of the document, an index or a
// length, so such a failure is recognised (model.SameFailureAs) by its text
// up to there and by the place, and is one failure to the log whatever the
// numbers, and another than a runtime error raised elsewhere. No line is in
// what a failure is recognised by: the file and the function are the place
// whichever build it is.
func yamlPanicked(failed any, at yamlPlace) error {
	said := fmt.Sprint(failed)
	_, ours := failed.(runtime.Error)
	if at.library {
		const library = "the YAML library failed on the document: "
		err := errors.New(library + said)
		if ours {
			return model.SameFailureAs(err, fmt.Sprintf("%sruntime error (%s %s)", library, at.file, at.function))
		}
		return err
	}
	const exporter, report = "the exporter failed on the YAML document (%s %s): %s", "; this is a defect of the exporter and not of the document, please report it"
	err := fmt.Errorf(exporter+report, fmt.Sprintf("%s:%d", at.file, at.line), at.function, said)
	if ours {
		return model.SameFailureAs(err, fmt.Sprintf(exporter, at.file, at.function, "runtime error"))
	}
	return model.SameFailureAs(err, fmt.Sprintf(exporter+report, at.file, at.function, said))
}

// yamlFailure gives an error of the YAML library the text the failure is
// recognised by (model.SameFailureAs): its text without the lines it names,
// which are where in the body it happened and no part of what the failure is
// to the log. The message is unchanged, but for a list of problems longer
// than yamlProblemsShown (yamlProblems).
//
// The library keeps the line nowhere but in the text it writes, so here, and
// only here, the text is read for it, in the forms the library itself writes
// (gopkg.in/yaml.v3 v3.0.1, decode.go): a scanner's or a parser's error is
// `yaml: line N: problem`; a *yaml.TypeError lists its problems, each `line N:
// problem`, and the one such problem a document decoded into plain values
// has, a key written twice, ends with the line of the first, `already
// defined at line N`. Every other error of the library has no line: an
// unknown anchor, an anchor that holds itself, excessive aliasing, a merge
// of something that is no mapping, a tagged value that does not fit its
// tag. A text in none of these forms is recognised by the whole of it, as
// it was.
//
// The library counts lines from nought and writes no line at all for one
// that is nought (decode.go, fail): a scanner's or a parser's error on the
// document's first line is `yaml: problem`, where on any other it is `yaml:
// line N: problem`. Were the line only marked, as it is in a list of
// problems, a failure that moved to the first line, or from it, would read
// as another failure to the log. So such an error is recognised by its
// problem alone, `yaml: problem`, which is what the first line's reads as
// already: the words `line N: ` are left out, not replaced.
func yamlFailure(err error) error {
	if problems, ok := err.(*yaml.TypeError); ok { //nolint:errorlint // the library returns its list of problems as it is, wrapped in nothing
		return yamlProblems(problems)
	}
	const library = "yaml: "
	text := err.Error()
	problem, ok := strings.CutPrefix(text, library)
	if !ok {
		return err
	}
	if same := withoutYAMLLines(problem); same != problem {
		return model.SameFailureAs(err, library+strings.TrimPrefix(same, "line "+model.MovingMark+": "))
	}
	return err
}

// yamlProblemsShown is how many of a document's problems its error lists;
// the rest are counted.
const yamlProblemsShown = 10

// yamlProblems is the error for a document the YAML library refused with a
// list of problems, and what that failure is recognised by.
//
// The library lists a key written k times as k(k-1)/2 problems, one for
// each two of them, so the list grows with the square of the document: 1200
// lines of `a: 1`, 6 kB, are 719,400 problems and 40 MB of text, which would
// be logged as one line and answered to the scraper. So the error lists the
// first yamlProblemsShown problems, as the library words them, and says how
// many more there were; a list no longer than that is the library's own
// error, untouched. The text of the whole list is never made.
//
// The failure is recognised by its problems without their lines, each
// written once however many times it is listed: a list of items that each
// write a key twice has one problem an item, and is the same failure when it
// grows by an item. No more than yamlProblemsShown different problems are
// told apart: past that the recognised text ends, as the error does, with
// the mark for how many more there are.
func yamlProblems(problems *yaml.TypeError) error {
	var err error = problems
	// moved is whether the failure is recognised by another text than its
	// own: the count of the problems left out is no part of it.
	moved := len(problems.Errors) > yamlProblemsShown
	if moved {
		more := "problems"
		if len(problems.Errors) == yamlProblemsShown+1 {
			more = "problem"
		}
		// A copy, so that the library's list can be let go of.
		shown := append(slices.Clone(problems.Errors[:yamlProblemsShown]), fmt.Sprintf("... and %d more %s", len(problems.Errors)-yamlProblemsShown, more))
		err = &yaml.TypeError{Errors: shown}
	}
	same := make([]string, 0, min(len(problems.Errors), yamlProblemsShown+1))
	var masked []byte
listed:
	for _, problem := range problems.Errors {
		masked = appendWithoutYAMLLines(masked[:0], problem)
		for _, known := range same {
			if known == string(masked) {
				continue listed
			}
		}
		if len(same) == yamlProblemsShown {
			same = append(same, "... and "+model.MovingMark+" more problems")
			break
		}
		moved = moved || string(masked) != problem
		same = append(same, string(masked))
	}
	if !moved && len(same) == len(problems.Errors) {
		return err
	}
	return model.SameFailureAs(err, (&yaml.TypeError{Errors: same}).Error())
}

// withoutYAMLLines is a problem of the YAML library with the mark of a
// position (model.MovingMark) in place of the line it starts with, as in
// `line 12: did not find expected key`, and of the line a key written twice
// ends with. A problem that starts with no line is returned as it is.
func withoutYAMLLines(problem string) string {
	return string(appendWithoutYAMLLines(nil, problem))
}

// appendWithoutYAMLLines adds to text what withoutYAMLLines makes of
// problem. A list of problems is read through it into one buffer, so that a
// problem listed many times costs no text of its own.
func appendWithoutYAMLLines(text []byte, problem string) []byte {
	const starts, ends = "line ", " already defined at line "
	rest, ok := strings.CutPrefix(problem, starts)
	if !ok {
		return append(text, problem...)
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 || !strings.HasPrefix(rest[digits:], ": ") {
		return append(text, problem...)
	}
	rest = rest[digits:]
	text = append(append(text, starts...), model.MovingMark...)
	// The line a key written twice ends with: the digits the problem ends
	// with, after the words for it.
	first := len(rest)
	for first > 0 && rest[first-1] >= '0' && rest[first-1] <= '9' {
		first--
	}
	if first < len(rest) && strings.HasSuffix(rest[:first], ends) && strings.HasPrefix(rest, ": mapping key ") {
		return append(append(text, rest[:first]...), model.MovingMark...)
	}
	return append(text, rest...)
}

// emptyYAMLDocument reports whether a document holds nothing, not even an
// explicit null.
func emptyYAMLDocument(document *yaml.Node) bool {
	if len(document.Content) == 0 {
		return true
	}
	content := document.Content[0]
	return len(document.Content) == 1 && content.Kind == yaml.ScalarNode && content.ShortTag() == "!!null" && content.Value == "" && content.Style == 0
}

// timestampsAsText retags every timestamp scalar under n as a string.
func timestampsAsText(n *yaml.Node, seen map[*yaml.Node]bool) {
	if n == nil || seen[n] {
		return
	}
	seen[n] = true
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!timestamp" {
		n.Tag = "!!str"
	}
	for _, child := range n.Content {
		timestampsAsText(child, seen)
	}
	timestampsAsText(n.Alias, seen)
}
