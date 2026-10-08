package exporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The bounds of a debug report (probedebug.go): no line of it but the
// body's is longer than debugLineLimit, a request's URL, a metric's name and
// a header's value are shown by their start and their length, and the
// headers are listed up to debugHeaderLimit. The tests here run in every
// build: they probe a request type of their own (registerShapedType), whose
// answer is whatever a test gives it, and compare the report with the one
// the writer made before any of this (oldReport).

// debugTrip is a debug probe's trip as serveDebugProbe makes it, kept so
// that its report can be written twice: as it is written now, and as it was
// written before a line of it had a bound.
type debugTrip struct {
	trace   *probeTrace
	probe   debugProbe
	verdict string
	answer  *model.MetricSet
}

// debugTripTook is how long every trip of these tests is said to have
// taken, so that two reports of one trip differ in nothing else.
const debugTripTook = 1500 * time.Millisecond

func (d debugTrip) report() string {
	return string(d.trace.report(d.probe, d.verdict, d.answer, debugTripTook))
}

func (d debugTrip) old() string {
	return string(oldReport(d.trace, d.probe, d.verdict, d.answer, debugTripTook))
}

// debugTripOf makes the trip of a debug probe of collector at target, with
// the probe's other parameters in query, as probeHandler and serveDebugProbe
// make it.
func debugTripOf(t *testing.T, server *Server, collector, target string, query url.Values) debugTrip {
	t.Helper()
	cfg, _ := server.manager.InForce()
	c := model.CollectorByName(cfg, collector)
	if c == nil {
		t.Fatalf("no collector %q", collector)
	}
	overrides, err := fetch.ParseRequestOverrides(query)
	if err != nil {
		t.Fatal(err)
	}
	p := debugProbe{
		upstreamProbe: upstreamProbe{collector: c, target: target, logTarget: fetch.DisplayTarget(c, target), overrides: overrides},
		method:        fetch.RequestMethodFor(c, overrides),
	}
	if label, err := fetch.RequestLabelFor(target, c, overrides); err == nil {
		p.requestURL = label
	}
	ctx, trace := newProbeTrace(context.Background())
	if limited := server.trips.tryAcquire(collector, maxConcurrentProbes(c)); limited != nil {
		t.Fatal(limited.message)
	}
	verdict, answer := server.debugTrip(ctx, trace, p, collectLog{
		failed: "probe failed", continuing: "probe stage failed; continuing", recovery: "probe recovered",
		attrs: []any{"collector", collector, "target", p.logTarget},
	})
	return debugTrip{trace: trace, probe: p, verdict: verdict, answer: answer}
}

// oldReport is probeTrace.report as it was before a report's lines were
// bounded, with the functions it called (oldWriteResponse, oldWriteBody,
// oldWriteTransform, oldWriteHeaders): the oracle the reports of these tests
// are compared with.
func oldReport(t *probeTrace, p debugProbe, verdict string, answer *model.MetricSet, took time.Duration) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b bytes.Buffer
	c := p.collector
	b.WriteString(p.whose())
	b.WriteString("\n")
	if p.static != nil {
		fmt.Fprintf(&b, "Took %s. The scrape would have published %s.\n", took.Round(time.Millisecond), verdict)
		b.WriteString("A debug scrape skips the response cache, publishes nothing on the endpoint, records no self-metric and exports nothing over OTLP.\n")
	} else {
		fmt.Fprintf(&b, "Took %s. A probe would have answered %s.\n", took.Round(time.Millisecond), verdict)
		b.WriteString("A debug probe skips the response cache, shares no trip, records no self-metric and exports nothing over OTLP.\n")
	}

	b.WriteString("\nRequests\n")
	requests := t.requests.Requests()
	if len(requests) == 0 {
		if p.requestURL != "" {
			fmt.Fprintf(&b, "  %s %s\n", p.method, p.requestURL)
		} else {
			b.WriteString("  none sent\n")
		}
	}
	for i, req := range requests {
		via := ""
		if req.Redirect {
			via = " (redirect)"
		}
		outcome := req.Outcome
		if outcome == "" {
			outcome = "no answer"
		} else {
			outcome += " in " + req.Duration.Round(time.Millisecond).String()
		}
		fmt.Fprintf(&b, "  %d. %s %s%s -> %s\n", i+1, req.Method, fetch.RedactURLString(req.URL, fetch.MaskQueryValues), via, outcome)
		oldWriteHeaders(&b, req.Header, "     ")
		if len(req.Withheld) > 0 {
			fmt.Fprintf(&b, "     not sent: %s — %s\n", strings.Join(req.Withheld, ", "), req.WithheldWhy)
		}
	}

	b.WriteString("\nResponse\n")
	if t.response == nil {
		b.WriteString("  none\n")
	} else {
		oldWriteResponse(&b, t.response, t.convertedFrom)
	}

	b.WriteString("\nStages\n")
	for _, s := range t.steps {
		line := fmt.Sprintf("  %-12s %-10s %8s", s.stage, s.outcome, s.took.Round(time.Millisecond))
		if s.note != "" {
			line += "  " + s.note
		}
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteString("\n")
	}

	if t.transform != nil || len(t.failures) > 0 {
		b.WriteString("\nTransform\n")
		oldWriteTransform(&b, c, t.transform, t.failures, t.decoded)
	}

	b.WriteString("\nLogs\n")
	if t.logs.Len() == 0 {
		b.WriteString("  none\n")
	}
	for _, line := range strings.Split(strings.TrimRight(t.logs.String(), "\n"), "\n") {
		if line != "" {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	if p.static != nil {
		b.WriteString("\nMetrics the scrape would have published, without its health series\n")
	} else {
		b.WriteString("\nMetrics a probe would have served\n")
	}
	if answer == nil || len(answer.Metrics) == 0 {
		b.WriteString("  none\n")
	} else {
		b.Write(appendMetricSet(nil, answer))
	}
	return b.Bytes()
}

func oldWriteResponse(b *bytes.Buffer, r *fetch.HTTPResponse, convertedFrom string) {
	switch {
	case r.GRPCCode != nil:
		fmt.Fprintf(b, "  gRPC status %d\n", *r.GRPCCode)
	case r.NoStatus:
	case r.StatusCode != 0:
		fmt.Fprintf(b, "  Status %d %s\n", r.StatusCode, http.StatusText(r.StatusCode))
	}
	if len(r.Headers) > 0 {
		b.WriteString("  Headers\n")
		oldWriteHeaders(b, r.Headers, "    ")
	}
	if d := r.Directory; d != nil {
		fmt.Fprintf(b, "  Directory %s, %d files read\n", d.Path, len(d.Files))
		for _, f := range d.Files {
			if f.Err != nil {
				fmt.Fprintf(b, "    %s: %v\n", f.Name, f.Err)
				continue
			}
			size := 0
			if f.Response != nil {
				size = len(f.Response.Body)
			}
			fmt.Fprintf(b, "    %s: %d bytes\n", f.Name, size)
		}
		for _, name := range d.Skipped {
			fmt.Fprintf(b, "    %s: skipped, over request.max_files\n", name)
		}
		return
	}
	oldWriteBody(b, r.Body, convertedFrom)
}

func oldWriteBody(b *bytes.Buffer, body []byte, convertedFrom string) {
	shown, note := body, ""
	if convertedFrom != "" {
		if text, err := decode.TextFrom(body, convertedFrom); err == nil {
			shown, note = text, " in "+convertedFrom+", converted to UTF-8 before decoding and shown here as UTF-8"
		}
	}
	cut := len(shown) > debugBodyLimit
	if cut {
		shown = shown[:debugBodyLimit]
		for i := 0; i < utf8.UTFMax && len(shown) > 0 && !utf8.Valid(shown); i++ {
			shown = shown[:len(shown)-1]
		}
	}
	if !utf8.Valid(shown) {
		fmt.Fprintf(b, "  Body: %d bytes, not text\n", len(body))
		return
	}
	fmt.Fprintf(b, "  Body: %d bytes%s\n", len(body), note)
	for _, line := range strings.Split(strings.TrimRight(string(shown), "\n"), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if cut {
		fmt.Fprintf(b, "    ... cut at %d bytes\n", debugBodyLimit)
	}
}

func oldWriteTransform(b *bytes.Buffer, c *model.Collector, set *model.MetricSet, failures []transform.RuleFailure, decoded string) {
	if decoded != "" {
		fmt.Fprintf(b, "  %s transform of a %s response\n", c.Transform.Type, decoded)
	}
	counts := map[string]int{}
	var order []string
	if set != nil {
		for _, m := range set.Metrics {
			if _, seen := counts[m.Name]; !seen {
				order = append(order, m.Name)
			}
			counts[m.Name]++
		}
	}
	if len(order) == 0 {
		b.WriteString("  no series\n")
	} else {
		b.WriteString("  Series by metric\n")
		for _, name := range order {
			fmt.Fprintf(b, "    %s: %d\n", name, counts[name])
		}
	}
	var empty []string
	seen := map[string]bool{}
	for _, rule := range c.Metrics {
		name := transform.ExportedMetricName(c, rule.Name)
		if rule.Name == "" || seen[name] || counts[name] > 0 {
			continue
		}
		seen[name] = true
		empty = append(empty, name)
	}
	if len(empty) > 0 && c.Transform.Type != "prometheus" {
		b.WriteString("  Rules that gave no series: ")
		b.WriteString(strings.Join(empty, ", "))
		b.WriteString("\n")
	}
	if len(failures) > 0 {
		b.WriteString("  Rules that carried on without some series\n")
		var shared sharedRuleNames
		for _, f := range failures {
			rule := f.Metric
			if rule == "" {
				rule = "rule without a name"
			}
			if shared.has(c, f.Metric) {
				rule += fmt.Sprintf(" (expression %q", f.Expression)
				if f.Items != "" {
					rule += fmt.Sprintf(", items %q", f.Items)
				}
				rule += ")"
			}
			line := fmt.Sprintf("    %s: %d failed", rule, f.Failures)
			if f.Missing > 0 {
				line += fmt.Sprintf(", %d of them missing values", f.Missing)
			}
			if f.First != nil {
				line += "; first: " + f.First.Error()
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
}

func oldWriteHeaders(b *bytes.Buffer, h http.Header, indent string) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range h[name] {
			fmt.Fprintf(b, "%s%s: %s\n", indent, name, fetch.RedactHeaderValue(name, value))
		}
	}
}

// lineLimit is the bound of a report's line as the documentation gives it,
// written out here so that the tests do not follow the constant wherever it
// goes.
const lineLimit = 8192

// cutAsDocumented is a report with the line rule applied to it as the
// documentation states the rule, by the test's own reading of it: every
// line outside the body that is longer than 8,192 bytes is its first 8,192
// bytes, or fewer where that would end inside a character, and `... (N
// bytes)` with its whole length. The body's lines are those from the line
// that gives its size to the heading of the stages. It is what the old
// writer's report has to become where no other bound applies.
func cutAsDocumented(report string) string {
	var b strings.Builder
	body := false
	for _, line := range strings.SplitAfter(report, "\n") {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "  Body: "):
			body = true
		case text == "Stages":
			body = false
		}
		if body || len(text) <= lineLimit {
			b.WriteString(line)
			continue
		}
		head := lineLimit
		for head > lineLimit-utf8.UTFMax && !utf8.RuneStart(text[head]) {
			head--
		}
		fmt.Fprintf(&b, "%s... (%d bytes)%s", text[:head], len(text), line[len(text):])
	}
	return b.String()
}

// longestLine is the longest line of a report outside its body, and the
// line.
func longestLine(report string) (longest int, line string) {
	body := false
	for _, text := range strings.Split(report, "\n") {
		switch {
		case strings.HasPrefix(text, "  Body: "):
			body = true
		case text == "Stages":
			body = false
		}
		if !body && len(text) > longest {
			longest, line = len(text), text
		}
	}
	return longest, line
}

// mostCutLine is the longest a line of a report can be: the bound, the rest
// of a <redacted> the bound fell inside, and the length of a line of any
// size.
const mostCutLine = lineLimit + len(fetch.Redacted) + len("... (18446744073709551615 bytes)")

// requireSame fails with the place two reports first differ at.
func requireSame(t *testing.T, what, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	from := max(0, at-80)
	t.Fatalf("%s: the reports, of %d and %d bytes, differ at byte %d:\n got %q\nwant %q", what, len(got), len(want), at, got[from:min(len(got), at+120)], want[from:min(len(want), at+120)])
}

// registerShapedType registers a request type, shaped, whose fetch answers
// what answer gives for the target: a response of any shape a request type
// can give — any headers, a directory's files, a gRPC status — or an error,
// without a target that has to be made to send it.
func registerShapedType(t *testing.T, answer func(target string) (*fetch.HTTPResponse, error)) {
	t.Helper()
	fetch.RequestTypes["shaped"] = &fetch.RequestType{
		Name:         "shaped",
		Fields:       []string{"path"},
		TargetFields: []string{"path"},
		Validate:     func(*model.Collector) error { return nil },
		Fetch: func(_ context.Context, target string, c *model.Collector, _ fetch.RequestOverrides, _ http.Header) (*fetch.HTTPResponse, error) {
			response, err := answer(target)
			if err != nil {
				return nil, err
			}
			response.Target, response.Collector = target, c.Name
			return response, nil
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "shaped") })
}

// shapedCollector passes the Prometheus text of a shaped answer through.
func shapedCollector() model.Collector {
	return model.Collector{
		Name:          "shaped",
		Request:       model.RequestConfig{Type: "shaped", Path: "/data"},
		Transform:     model.TransformConfig{Type: "prometheus"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
}

// textAnswer is a shaped answer of status 200 with headers and a body of
// Prometheus text.
func textAnswer(headers http.Header, body string) *fetch.HTTPResponse {
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "text/plain; version=0.0.4")
	}
	return &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: headers, Body: []byte(body)}
}

// shapedServer is an exporter of the shaped collector and of the collectors
// given, with debug probes enabled, answering what answer gives.
func shapedServer(t *testing.T, answer func(target string) (*fetch.HTTPResponse, error), more ...model.Collector) *Server {
	t.Helper()
	testutil.CaptureLogs(t)
	registerShapedType(t, answer)
	server := pathServer(t, append([]model.Collector{shapedCollector()}, more...)...)
	server.SetProbeDebug(true)
	return server
}

// What a report shows of a text longer than its bound ends where a
// failure's text does (model.HeadOf): at the bound, or up to three bytes
// before it where the bound falls inside a character, and at the bound in a
// text that is no UTF-8 there. Where it would end inside a <redacted> it
// ends after it, so that no part of the word is shown as if it were the
// start of a value, and a cut just before or just after one is left where
// it is. A report's lines are cut by the same rule as the documentation
// states it, whatever they are: lines of exactly the bound stay, a line
// without its new-line and empty lines are kept as they are.
func TestWhatAReportShowsOfALongTextEndsBetweenCharactersAndOutsideARedaction(t *testing.T) {
	rng := rand.New(rand.NewPCG(39, 42))
	pieces := []string{"a", "b", "é", "я", "€", "😀", "\xff", "\x80", "\xe2\x82"}
	for range alloctest.UnlessRaced(4000, 800) {
		var text strings.Builder
		for range 1 + rng.IntN(40) {
			text.WriteString(pieces[rng.IntN(len(pieces))])
		}
		limit := 1 + rng.IntN(text.Len()+2)
		if text.Len() <= limit || strings.Contains(text.String(), "<") {
			continue
		}
		want := model.HeadOf(text.String(), limit)
		if got := shownHead(text.String(), limit); got != want {
			t.Fatalf("%q at %d: %d bytes are shown, and a failure's text shows %d", text.String(), limit, got, want)
		}
		if got := shownHead([]byte(text.String()), limit); got != want {
			t.Fatalf("%q at %d, as bytes: %d bytes are shown, and a failure's text shows %d", text.String(), limit, got, want)
		}
		if got, cut := shownStart(text.String(), limit), text.String()[:want]+fmt.Sprintf("... (%d bytes)", text.Len()); got != cut {
			t.Fatalf("%q at %d is shown as %q, want %q", text.String(), limit, got, cut)
		}
	}
	if got := shownStart("whole", 5); got != "whole" {
		t.Errorf("a text of its bound is shown as %q", got)
	}
	// The bound falls after each byte of the word that stands for a
	// credential, before it and after it.
	const before, after = "http://db.internal/status?token=", "&view=<redacted>&page=<redacted>"
	text := before + fetch.Redacted + after
	for inside := 0; inside <= len(fetch.Redacted); inside++ {
		want := before
		if inside > 0 {
			want += fetch.Redacted
		}
		want += fmt.Sprintf("... (%d bytes)", len(text))
		if got := shownStart(text, len(before)+inside); got != want {
			t.Errorf("cut %d bytes into the redaction: shown as %q, want %q", inside, got, want)
		}
	}
	// A text that ends inside what only starts like one is cut at its bound.
	if got := shownStart("token=<redirect>&more", 10); got != "token=<red... (21 bytes)" {
		t.Errorf("a text that is no redaction is shown as %q", got)
	}

	line := func(size int, piece string) string { return strings.Repeat(piece, size/len(piece)+1)[:size] }
	texts := []string{
		"", "\n", "\n\n", "short\n", "no new-line",
		line(lineLimit, "x") + "\n",
		line(lineLimit+1, "x") + "\n",
		line(lineLimit+1, "x"),
		"first\n" + line(3*lineLimit, "y") + "\n\nlast\n",
	}
	// A character of two, three and four bytes across the bound, with one,
	// two and three of its bytes before it, is left out whole.
	for _, tc := range []struct {
		before, piece string
		head          int
	}{
		{"a", "я", lineLimit - 1}, {"", "я", lineLimit},
		{"a", "€", lineLimit - 1}, {"", "€", lineLimit - 2}, {"ab", "€", lineLimit},
		{"abc", "😀", lineLimit - 1}, {"ab", "😀", lineLimit - 2}, {"a", "😀", lineLimit - 3}, {"", "😀", lineLimit},
	} {
		straddled := tc.before + strings.Repeat(tc.piece, lineLimit/len(tc.piece)+3)
		var b bytes.Buffer
		appendCutLines(&b, []byte(straddled+"\n"))
		if got, want := b.String(), straddled[:tc.head]+fmt.Sprintf("... (%d bytes)\n", len(straddled)); got != want || !utf8.ValidString(got) {
			t.Errorf("a line of %q after %q is cut to %d bytes, valid UTF-8 %t, want its first %d and its length", tc.piece, tc.before, len(got), utf8.ValidString(got), tc.head)
		}
		texts = append(texts, straddled+"\n", "before\n"+straddled)
	}
	for range alloctest.UnlessRaced(200, 40) {
		var text strings.Builder
		for range 1 + rng.IntN(6) {
			text.WriteString(line(rng.IntN(3)*(lineLimit-2)+rng.IntN(5), pieces[rng.IntN(6)]))
			if rng.IntN(5) > 0 {
				text.WriteString("\n")
			}
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		var b bytes.Buffer
		b.WriteString("kept\n")
		appendCutLines(&b, []byte(text))
		if got, want := b.String(), "kept\n"+cutAsDocumented(text); got != want {
			requireSame(t, fmt.Sprintf("a text of %d bytes", len(text)), got, want)
		}
		// The lines written to a report are cut where they stand, and those
		// before them are not looked at again.
		b.Reset()
		before := line(lineLimit+5, "k") + "\n"
		b.WriteString(before)
		b.WriteString(text)
		cutLongLines(&b, len(before))
		requireSame(t, fmt.Sprintf("a text of %d bytes, written after a line that was not to be cut", len(text)), b.String(), before+cutAsDocumented(text))
		if long, cut := longLines([]byte(text)), strings.Count(cutAsDocumented(text), "... (")-strings.Count(text, "... ("); long != cut {
			t.Errorf("a text of %d bytes has %d long lines, and %d are cut", len(text), long, cut)
		}
	}
}

// ordinaryAnswer is a response as a target of ordinary manners sends one,
// made of rng: any number of headers up to the hundred a report lists, with
// values up to the 1,024 bytes it shows, some under names that read as a
// credential's and some given twice; and Prometheus text whose metric names
// are up to the 200 bytes a report shows, with labels, help and characters
// of several bytes, in a body that may be longer than the 64 KiB shown and
// may have lines longer than any other line of a report may be. Now and
// then it is a status the collector fails on, a gRPC status, an answer
// without one, or a directory with files read, failed and skipped.
func ordinaryAnswer(rng *rand.Rand) *fetch.HTTPResponse {
	pick := func(sizes ...int) int { return sizes[rng.IntN(len(sizes))] }
	// word is size bytes of pieces: a few of them in an order of rng's,
	// over and over, and the first of them where no other fits at the end.
	word := func(size int, pieces ...string) string {
		var few strings.Builder
		for range 1 + rng.IntN(7) {
			few.WriteString(pieces[rng.IntN(len(pieces))])
		}
		text := strings.Repeat(few.String(), size/few.Len())
		for len(text) < size {
			piece := pieces[rng.IntN(len(pieces))]
			if len(text)+len(piece) > size {
				piece = pieces[0]
			}
			text += piece
		}
		return text
	}
	headers := http.Header{"Content-Type": {"text/plain; version=0.0.4"}}
	for count := pick(0, 0, 3, 12, debugHeaderLimit-2, debugHeaderLimit-1); count > 0; count-- {
		name := fmt.Sprintf("X-Shaped-%03d", count)
		if rng.IntN(8) == 0 {
			name = fmt.Sprintf("X-Api-Key-%03d", count)
		}
		headers.Add(name, word(pick(0, 1, 24, 300, debugHeaderValueLimit-1, debugHeaderValueLimit), "a", "b", " ", "/", ";"))
		if count > 1 && rng.IntN(6) == 0 {
			headers.Add(name, "again")
			count--
		}
	}
	var body strings.Builder
	for series := pick(0, 1, 4, 30); series > 0; series-- {
		name := "m" + word(pick(4, 60, debugNameLimit-2, debugNameLimit-1), "a", "b", "_", "0")
		if rng.IntN(3) == 0 {
			fmt.Fprintf(&body, "# HELP %s %s\n# TYPE %s gauge\n", name, word(pick(10, 400), "h", "elp ", "é"), name)
		}
		for range pick(1, 1, 3) {
			fmt.Fprintf(&body, "%s{id=\"%d\",place=%q} %d\n", name, rng.IntN(1_000_000_000), word(pick(0, 9, 400), "s", "я", "€", " "), rng.IntN(100))
		}
	}
	switch rng.IntN(6) {
	case 0:
		// A comment longer than a line of the report may be, which a body
		// shows as it is, and a body past what is shown of one.
		fmt.Fprintf(&body, "# %s\n", word(lineLimit+pick(1, 500), "c"))
	case 1:
		for body.Len() <= debugBodyLimit+pick(-200, 0, 1, 9000) {
			fmt.Fprintf(&body, "# %s\n", word(pick(1, 70, 900), "p", "é"))
		}
	}
	response := &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: headers, Body: []byte(body.String())}
	switch rng.IntN(12) {
	case 0:
		response.StatusCode = http.StatusServiceUnavailable
	case 1:
		code := 0
		response.GRPCCode = &code
	case 2:
		response.NoStatus = true
	case 3:
		file := func(text string) *fetch.HTTPResponse {
			return &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/plain"}}, Body: []byte(text)}
		}
		read := &fetch.DirectoryRead{Path: "/var/lib/" + word(pick(4, 200), "d", "/"), Files: []fetch.FileRead{
			{Name: "a.prom", ModTime: time.Unix(1_000_000, 0), Response: file("up 1\n")},
			{Name: word(pick(8, 255), "f") + ".prom", Response: file(body.String())},
			{Name: "gone.prom", Err: errors.New("open gone.prom: no such file or directory")},
		}}
		for skipped := pick(0, 2, 150); skipped > 0; skipped-- {
			read.Skipped = append(read.Skipped, fmt.Sprintf("skipped-%03d.prom", skipped))
		}
		read.Matched = len(read.Files) + len(read.Skipped)
		response = &fetch.HTTPResponse{StatusCode: http.StatusOK, Directory: read}
	}
	return response
}

// The report of a trip whose every part is within the report's bounds is
// what it was, byte for byte: three hundred responses of every shape a
// request type gives, with up to a hundred headers of up to 1,024 bytes,
// metric names of up to 200, bodies around and past the 64 KiB shown, and
// body lines longer than any other line may be, are reported as the writer
// reported them before a line had a bound. The line of a report that is
// exactly 8,192 bytes is among them, and a hundred headers, each as they
// were. Under the race detector the responses are thirty.
func TestAReportWithinItsBoundsIsWrittenAsItWas(t *testing.T) {
	var answer *fetch.HTTPResponse
	server := shapedServer(t, func(string) (*fetch.HTTPResponse, error) { return answer, nil })
	rng := rand.New(rand.NewPCG(2026, 39))
	shapes := map[string]int{}
	for i := range alloctest.UnlessRaced(300, 30) {
		answer = ordinaryAnswer(rng)
		trip := debugTripOf(t, server, "shaped", fmt.Sprintf("shaped://target-%d", i), url.Values{})
		report := trip.report()
		requireSame(t, fmt.Sprintf("response %d", i), report, trip.old())
		switch {
		case answer.Directory != nil:
			shapes["directory"]++
		case answer.GRPCCode != nil:
			shapes["grpc"]++
		case strings.Contains(report, "... cut at 65536 bytes"):
			shapes["body cut"]++
		case strings.Contains(report, "A probe would have answered 502"):
			shapes["failed"]++
		case strings.Contains(report, "A probe would have answered 200 with"):
			shapes["answered"]++
		}
	}
	for _, shape := range []string{"directory", "grpc", "body cut", "failed", "answered"} {
		if shapes[shape] == 0 {
			t.Errorf("none of the responses was one with %s: %v", shape, shapes)
		}
	}

	// At each bound itself: a first line of 8,192 bytes, a hundred headers,
	// a value of 1,024 bytes and a name of 200.
	headers := http.Header{}
	for i := range debugHeaderLimit - 1 {
		headers.Set(fmt.Sprintf("X-Shaped-%03d", i), strings.Repeat("v", debugHeaderValueLimit))
	}
	name := "m" + strings.Repeat("n", debugNameLimit-1)
	answer = textAnswer(headers, name+" 1\n")
	target := "shaped://" + strings.Repeat("t", lineLimit)
	target = target[:lineLimit-len(`Debug probe of collector "shaped", target `)]
	trip := debugTripOf(t, server, "shaped", target, url.Values{})
	report := trip.report()
	requireSame(t, "a report at its bounds", report, trip.old())
	first, _, _ := strings.Cut(report, "\n")
	if len(first) != lineLimit || strings.Count(report, "\n    X-Shaped-") != debugHeaderLimit-1 || !strings.Contains(report, "\n    "+name+": 1\n") || !strings.Contains(report, ": "+strings.Repeat("v", debugHeaderValueLimit)+"\n") {
		t.Errorf("the report at its bounds has a first line of %d bytes and %d headers:\n%.300s", len(first), strings.Count(report, "\n    X-Shaped-"), report)
	}
}

// A line of a report that is longer than 8,192 bytes is shown by its first
// 8,192 bytes and its length, whatever section wrote it and whatever it is
// made of: the first line with a target of 8 kB, a header whose name is
// that long, a directory's line with its path, a file's with its name or
// its error and a skipped file's, the stage that names the directory, the
// log lines, which name the target, and the line of the rules that gave no
// series when there are a hundred and more. The report is the one the
// writer made before, with those lines cut as the documentation says and
// nothing else changed; a character of two bytes across the bound is left
// out whole; and the lines were ten and twenty kilobytes each.
func TestALineOfAReportLongerThanItsBoundIsCutWhateverWroteIt(t *testing.T) {
	long := func(piece string) string { return strings.Repeat(piece, 12_000/len(piece)) }
	file := &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("up 1\n")}
	directory := &fetch.DirectoryRead{Path: "/var/lib/" + long("d"), Matched: 4, Files: []fetch.FileRead{
		{Name: "a.prom", ModTime: time.Unix(1_000_000, 0), Response: file},
		// Its line is four spaces and the name: the bound falls inside a я.
		{Name: "a" + long("я") + ".prom", Response: file},
		{Name: "gone.prom", Err: errors.New("open " + long("e") + ": no such file or directory")},
	}, Skipped: []string{long("s") + ".prom"}}
	answers := map[string]*fetch.HTTPResponse{
		"a header's name": textAnswer(http.Header{"X-" + long("N"): {"short"}}, "up 1\n"),
		"a directory":     {StatusCode: http.StatusOK, Directory: directory},
	}
	var answer *fetch.HTTPResponse
	rules := testutil.Collector("rules", "text")
	rules.Request = model.RequestConfig{Type: "shaped", Path: "/data"}
	rules.Metrics = nil
	for i := range 120 {
		rules.Metrics = append(rules.Metrics, model.MetricRule{Name: fmt.Sprintf("rule_%03d_of_the_collector_%s", i, strings.Repeat("r", 80)), Type: model.GaugeMetricType, Expression: fmt.Sprintf(`value%d=(\d+)`, i), ErrorMode: model.ErrorModeIgnore})
	}
	server := shapedServer(t, func(string) (*fetch.HTTPResponse, error) { return answer, nil }, rules)
	check := func(name string, trip debugTrip, cuts int) string {
		t.Helper()
		report, old := trip.report(), trip.old()
		requireSame(t, name, report, cutAsDocumented(old))
		if longest, line := longestLine(old); longest < 10_000 {
			t.Errorf("%s: the longest line of the report was %d bytes, %.80q", name, longest, line)
		}
		if longest, line := longestLine(report); longest > mostCutLine {
			t.Errorf("%s: a line of the report is %d bytes, %.80q", name, longest, line)
		}
		if cut := strings.Count(report, " bytes)\n") - strings.Count(old, " bytes)\n"); cut != cuts {
			t.Errorf("%s: %d lines are cut, want %d:\n%.300s", name, cut, cuts, report)
		}
		return report
	}

	answer = answers["a header's name"]
	check("a header's name", debugTripOf(t, server, "shaped", "shaped://target", url.Values{}), 1)

	// The first line, with the target; the directory's, the three files',
	// the stage's, and the three log lines, each with the target.
	answer = answers["a directory"]
	report := check("a directory", debugTripOf(t, server, "shaped", "shaped://"+strings.Repeat("t", MaxProbeParameterBytes-len("shaped://")), url.Values{}), 9)
	name := directory.Files[1].Name
	if want := "\n    " + name[:lineLimit-5] + fmt.Sprintf("... (%d bytes)\n", len("    "+name+": 5 bytes")); !strings.Contains(report, want) || !utf8.ValidString(report) {
		t.Errorf("the file whose name has a character across the bound is not shown up to the character before it, in valid UTF-8 %t", utf8.ValidString(report))
	}

	answer = textAnswer(nil, "value0=1\n")
	report = check("rules that gave no series", debugTripOf(t, server, "rules", "shaped://target", url.Values{}), 1)
	if want := "\n  Rules that gave no series: rule_001_of_the_collector_" + strings.Repeat("r", 80) + ", rule_002_of_the_collector_"; !strings.Contains(report, want) {
		t.Errorf("the line of the rules that gave no series does not start as it did:\n%.600s", report[max(0, strings.Index(report, "\nTransform")):])
	}
}

// The headers of a response are listed up to a hundred, and then how many
// there are: of five thousand, the first hundred by name, each value of a
// name counted, and `... (5001 headers)`, where the report had a line for
// each. A hundred are listed as they were, and a hundred and one are a
// hundred and their number. A value longer than 1,024 bytes is shown by its
// first 1,024 and its length, cut before a character the bound falls
// inside, and the megabyte of it is not in the report; a value under a name
// that reads as a credential's is withheld whatever its length, and nothing
// of it is shown.
func TestTheHeadersOfAReportAreListedUpToAHundredAndTheirValuesShownByTheirStart(t *testing.T) {
	var answer *fetch.HTTPResponse
	server := shapedServer(t, func(string) (*fetch.HTTPResponse, error) { return answer, nil })
	many := func(count int) http.Header {
		headers := http.Header{"Content-Type": {"text/plain; version=0.0.4"}}
		for i := 1; i < count; i++ {
			headers.Set(fmt.Sprintf("X-Shaped-%04d", i), "v")
		}
		return headers
	}
	headerLines := func(report string) []string {
		_, after, _ := strings.Cut(report, "\n  Headers\n")
		listed, _, _ := strings.Cut(after, "\n  Body: ")
		return strings.Split(listed, "\n")
	}

	headers := many(5000)
	headers.Add("X-Shaped-0001", "twice")
	answer = textAnswer(headers, "up 1\n")
	trip := debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
	report, old := trip.report(), trip.old()
	lines := headerLines(report)
	if len(lines) != debugHeaderLimit+1 || lines[0] != "    Content-Type: text/plain; version=0.0.4" || lines[1] != "    X-Shaped-0001: v" || lines[2] != "    X-Shaped-0001: twice" || lines[99] != "    X-Shaped-0098: v" || lines[100] != "    ... (5001 headers)" {
		t.Fatalf("5,001 headers are listed in %d lines, the last %q:\n%.400s", len(lines), lines[len(lines)-1], report)
	}
	listed, _, _ := strings.Cut(report, "    ... (5001 headers)")
	if was := headerLines(old); len(was) != 5001 || !strings.HasPrefix(old, listed) {
		t.Errorf("the report listed %d headers before, and the hundred listed now are not the first of them", len(was))
	}
	if len(report) > 8<<10 || len(old) < 80<<10 {
		t.Errorf("the report is %d bytes, and was %d", len(report), len(old))
	}

	answer = textAnswer(many(debugHeaderLimit), "up 1\n")
	trip = debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
	requireSame(t, "a hundred headers", trip.report(), trip.old())
	answer = textAnswer(many(debugHeaderLimit+1), "up 1\n")
	if lines := headerLines(debugTripOf(t, server, "shaped", "shaped://target", url.Values{}).report()); len(lines) != debugHeaderLimit+1 || lines[100] != "    ... (101 headers)" {
		t.Errorf("a hundred and one headers are listed in %d lines, the last %q", len(lines), lines[len(lines)-1])
	}

	size := alloctest.UnlessRaced(1<<20, 1<<16)
	answer = textAnswer(http.Header{
		"X-Long":       {strings.Repeat("h", size)},
		"X-Characters": {"a" + strings.Repeat("é", size/2)},
		"X-Api-Key":    {"s3cret-" + strings.Repeat("k", size)},
		"X-Whole":      {strings.Repeat("w", debugHeaderValueLimit)},
	}, "up 1\n")
	trip = debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
	report, old = trip.report(), trip.old()
	assertContains(t, report,
		"\n    X-Long: "+strings.Repeat("h", debugHeaderValueLimit)+fmt.Sprintf("... (%d bytes)\n", size),
		"\n    X-Characters: a"+strings.Repeat("é", (debugHeaderValueLimit-1)/2)+fmt.Sprintf("... (%d bytes)\n", 1+2*(size/2)),
		"\n    X-Api-Key: <redacted>\n",
		"\n    X-Whole: "+strings.Repeat("w", debugHeaderValueLimit)+"\n",
	)
	if strings.Contains(report, "s3cret") || strings.Contains(report, "kkk") || !utf8.ValidString(report) {
		t.Errorf("the report shows a credential, or is not valid UTF-8 (%t)", utf8.ValidString(report))
	}
	if len(report) > 8<<10 || len(old) < 2*size {
		t.Errorf("the report is %d bytes, and was %d", len(report), len(old))
	}
}

// A metric's name in the Transform section is shown as the failure of the
// validation shows it: by its first 200 bytes and its length, with the
// number of its series after it. A name of 200 bytes is listed whole, and
// the megabyte of a longer one is neither in the section nor in the line
// that says what a probe would have answered, where the report had it
// whole.
func TestAMetricsNameInAReportIsShownAsItsFailureShowsIt(t *testing.T) {
	size := alloctest.UnlessRaced(1<<20, 1<<16)
	name, whole := "h"+strings.Repeat("a", size), "w"+strings.Repeat("b", debugNameLimit-1)
	server := shapedServer(t, func(string) (*fetch.HTTPResponse, error) {
		return textAnswer(nil, whole+"{id=\"1\"} 1\n"+whole+"{id=\"2\"} 2\n"+name+" 3\n"), nil
	})
	trip := debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
	report, old := trip.report(), trip.old()
	head := name[:debugNameLimit]
	assertContains(t, report,
		"\n  Series by metric\n    "+whole+": 2\n    "+head+fmt.Sprintf("... (%d bytes): 1\n", len(name)),
		// The failure quotes the name, and shows as much of it.
		fmt.Sprintf("A probe would have answered 502: collector shaped validation failed: invalid metric name %q... (%d bytes): longer than limits.max_metric_name_length 200.\n", head, len(name)),
	)
	// The body, which is the name, is shown up to its own bound.
	if !strings.Contains(old, "\n    "+name+": 1\n") || len(old) < size || len(report) > debugBodyLimit+16<<10 {
		t.Errorf("the report is %d bytes, and was %d with the name whole (%t)", len(report), len(old), strings.Contains(old, "\n    "+name+": 1\n"))
	}
	if longest, line := longestLine(report); longest > 2*model.MaxFailureBytes {
		t.Errorf("a line of the report is %d bytes, %.80q", longest, line)
	}
}

// The body of a report keeps its own bound and no other: a body of 64 KiB
// and a byte in one line is shown by its first 64 KiB, in one line, and the
// line that says it was cut, and a body whose lines are longer than any
// other line of a report may be is shown with them whole, each as the
// report showed it before.
func TestTheBodyOfAReportKeepsItsOwnBound(t *testing.T) {
	var answer *fetch.HTTPResponse
	server := shapedServer(t, func(string) (*fetch.HTTPResponse, error) { return answer, nil })
	answer = textAnswer(nil, "# "+strings.Repeat("x", debugBodyLimit-1))
	trip := debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
	report := trip.report()
	requireSame(t, "a body of 64 KiB and a byte", report, trip.old())
	assertContains(t, report, "\n  Body: 65537 bytes\n    # "+strings.Repeat("x", debugBodyLimit-2)+"\n    ... cut at 65536 bytes\n\nStages\n")

	answer = textAnswer(nil, "# "+strings.Repeat("y", 3*lineLimit)+"\nup 1\n# "+strings.Repeat("z", lineLimit+1)+"\n")
	trip = debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
	report = trip.report()
	requireSame(t, "a body of long lines", report, trip.old())
	assertContains(t, report, "\n    # "+strings.Repeat("y", 3*lineLimit)+"\n    up 1\n    # "+strings.Repeat("z", lineLimit+1)+"\n\nStages\n", "A probe would have answered 200 with 1 series.")
}

// A failure at its own bound, 2,000 bytes ending with its length, is not
// cut again by the line that quotes it: the line of what a probe would have
// answered, the stage's and the log's each end with the failure's length,
// that of an error of a megabyte, and not with their own.
func TestALineOfAReportDoesNotCutAFailureAtItsBoundAgain(t *testing.T) {
	size := alloctest.UnlessRaced(1<<20, 1<<16)
	server := shapedServer(t, func(string) (*fetch.HTTPResponse, error) {
		return nil, errors.New("the target said " + strings.Repeat("no ", size/3))
	})
	trip := debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
	report := trip.report()
	requireSame(t, "a failure of a megabyte", report, trip.old())
	mark := fmt.Sprintf("... (%d bytes)", len("the target said ")+size/3*3)
	failure := regexp.MustCompile(`the target said (no )+n?o?` + regexp.QuoteMeta(mark))
	found := failure.FindAllString(report, -1)
	if len(found) != 3 {
		t.Fatalf("the failure is in %d lines of the report with its length, want the answer's, the stage's and the log's:\n%.400s", len(found), report)
	}
	for _, text := range found {
		if len(text) != model.MaxFailureBytes {
			t.Errorf("the failure is shown in %d bytes, want the %d of any failure", len(text), model.MaxFailureBytes)
		}
	}
	assertContains(t, report, mark+".\n", "  http         failed", `error="the target said no no`)
}

// What the report of a hostile response allocates is what it shows, not
// what the response holds: with a metric name of ten megabytes, and with a
// header value of one, the report is written in no more than eight times the
// 64 KiB it shows of a body, half a megabyte, where it allocated the name or
// the value twice over, for the line and for the report as it grew. Under
// the race detector the name and the value are a megabyte each.
func TestTheReportOfAHostileResponseAllocatesWhatItShows(t *testing.T) {
	var answer *fetch.HTTPResponse
	server := shapedServer(t, func(string) (*fetch.HTTPResponse, error) { return answer, nil })
	for name, tc := range map[string]struct {
		size   int
		answer func(size int) *fetch.HTTPResponse
	}{
		"a metric's name": {alloctest.UnlessRaced(10<<20, 1<<20), func(size int) *fetch.HTTPResponse {
			return textAnswer(nil, "h"+strings.Repeat("a", size)+" 1\n")
		}},
		"a header's value": {1 << 20, func(size int) *fetch.HTTPResponse {
			return textAnswer(http.Header{"X-Long": {strings.Repeat("h", size)}}, "up 1\n")
		}},
	} {
		answer = tc.answer(tc.size)
		trip := debugTripOf(t, server, "shaped", "shaped://target", url.Values{})
		const most = 8 * debugBodyLimit
		if got := alloctest.BytesAtMost(1, most, func() { _ = trip.report() }); got > most {
			t.Errorf("%s of %d bytes: the report allocates %d bytes, want at most %d", name, tc.size, got, most)
		}
		if _, was := alloctest.Once(1, func() { _ = trip.old() }); was < 2*uint64(tc.size) {
			t.Errorf("%s of %d bytes: the report allocated %d bytes before, less than twice the %s", name, tc.size, was, name)
		}
	}
}

// A static target's debug report is written by the same writer: a header
// value of a megabyte is shown by its first 1,024 bytes and its length, a
// line of twelve kilobytes by its first 8,192, and the series the scrape
// would have published are those it published before, to the byte.
func TestAStaticTargetsDebugReportIsBoundedAsAProbesIs(t *testing.T) {
	size := alloctest.UnlessRaced(1<<20, 1<<16)
	registerShapedType(t, func(string) (*fetch.HTTPResponse, error) {
		return textAnswer(http.Header{"X-Long": {strings.Repeat("h", size)}, "X-" + strings.Repeat("N", 12_000): {"short"}}, "up 1\n"), nil
	})
	testutil.CaptureLogs(t)
	cfg := &model.Config{Collectors: []model.Collector{shapedCollector()}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "shaped", Collector: "shaped", Target: "shaped://target"}}}
	server := newStaticServer(t, cfg, file)
	server.SetProbeDebug(true)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static-targets?debug=shaped", nil))
	report := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("answered %d: %.300s", recorder.Code, report)
	}
	assertContains(t, report,
		`Debug scrape of static target "shaped", collector "shaped"`,
		"The scrape would have published target up 1 with 1 series.",
		"\n    X-Long: "+strings.Repeat("h", debugHeaderValueLimit)+fmt.Sprintf("... (%d bytes)\n", size),
		"\n    X-"+strings.Repeat("N", lineLimit-len("    X-"))+fmt.Sprintf("... (%d bytes)\n", len("    X-: short")+12_000),
		"\nMetrics the scrape would have published, without its health series\n# TYPE up untyped\nup{static_target=\"shaped\"} 1\n",
	)
	if longest, line := longestLine(report); longest > mostCutLine || len(report) > 16<<10 {
		t.Errorf("the report is %d bytes and its longest line %d, %.80q", len(report), longest, line)
	}
}
