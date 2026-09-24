package decode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The graphite decoder reads Graphite series in either of the two forms they
// travel in, and hands every transform the same document:
//
//   - The render API's JSON, which a graphite collector asks for:
//     [{"target": "app.web01.requests", "tags": {...}, "datapoints": [[42, 1727000000], [null, 1727000060]]}]
//   - Carbon's plaintext lines, as a localfile collector reads them from a
//     file: `app.web01.requests 42 1727000000`, a tagged path written
//     `cpu.load;env=prod;host=a`. A path given several times is one series
//     with several points.
//
// The document is
//
//	{"series": [{"path": "app.web01.requests", "segments": ["app", "web01", "requests"],
//	             "tags": {"name": "app.web01.requests"}, "value": 42, "time": 1727000000,
//	             "points": [[40, 1726999940], [42, 1727000000]]}]}
//
// where value is the series' points reduced to one by response.graphite.value
// — the newest point by default, what a dashboard shows — time is the newest
// point's, in Unix seconds, and points are every point with a value, oldest
// first. A metric rule reads it like any JSON, with items: .series[].
//
// A point without a value — null in the render API, NaN or an infinity on a
// carbon line — is left out, since Graphite means by it that nothing was
// written, and a series left with no point is left out altogether: it would
// otherwise reach the rules as a series without a value, failing them on
// every scrape. So is one whose newest point is older than
// response.graphite.max_age, whose writer has stopped, and a series the
// render API answered twice, path, tags and points alike, because two
// expressions matched it. What was left out is reported with the document
// (GraphiteReport), for the self-metrics and the log.
//
// A graphite collector's answer must be render JSON: anything else — the
// HTML of a login page in front of Graphite, a JSON error — is refused as
// what it is, rather than read as carbon lines that fail on the first line.

// graphiteNow is the clock max_age and a carbon line without a timestamp
// are measured against; a test sets it.
var graphiteNow = time.Now

// GraphiteReport is what the graphite decoder left out of its document.
type GraphiteReport struct {
	// NoPoints, Stale and Duplicates count the series left out: with no
	// point that has a value, with the newest older than max_age, and
	// answered twice.
	NoPoints, Stale, Duplicates int
	// SkippedLines counts the carbon lines skipped under invalid_lines:
	// skip, and FirstSkipped says which was first and why.
	SkippedLines int
	FirstSkipped string
}

// LeftOut is how many series were left out.
func (r *GraphiteReport) LeftOut() int { return r.NoPoints + r.Stale + r.Duplicates }

// carbonMillisecondsAbove is the smallest carbon timestamp taken for
// milliseconds: 1e11 seconds is in the year 5138, and 1e11 milliseconds in
// 1973, so no timestamp written by anything running today is ambiguous.
const carbonMillisecondsAbove = 1e11

// graphitePoint is one point of a series, its time in Unix seconds.
type graphitePoint struct{ value, time float64 }

// graphiteSeries is one series as read, before its points are reduced.
type graphiteSeries struct {
	path   string
	tags   map[string]string
	points []graphitePoint
}

// decodeGraphite reads render JSON, a body starting with [, or carbon lines.
func decodeGraphite(r *fetch.HTTPResponse, c *model.Collector) (*Decoded, error) {
	settings := c.Response.Graphite
	if how := settings.Value; how != "" && !slices.Contains(model.GraphiteValues, how) {
		return nil, fmt.Errorf("unknown response.graphite.value %q; want one of %s", how, strings.Join(model.GraphiteValues, ", "))
	}
	now := graphiteNow()
	report := &GraphiteReport{}
	var series []*graphiteSeries
	var err error
	body := bytes.TrimSpace(r.Body)
	switch {
	case len(body) > 0 && body[0] == '[':
		series, err = parseGraphiteRender(body, report)
	case c.Request.Type == fetch.RequestTypeGraphite:
		err = fmt.Errorf("the Graphite server did not answer with render JSON, a list of series; %s", answerStart(body))
	default:
		series, err = parseCarbonLines(r.Body, now, settings.InvalidLines == "skip", report)
	}
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(series))
	for _, s := range series {
		document, leftOut := s.document(settings, now)
		switch leftOut {
		case "":
			out = append(out, document)
		case "no points":
			report.NoPoints++
		case "stale":
			report.Stale++
		}
	}
	return &Decoded{Kind: "graphite", Data: map[string]any{"series": out}, Raw: r.Body, Graphite: report}, nil
}

// answerStart describes the start of an answer that is not what was asked
// for, as much as a log line needs to tell a login page from an error.
func answerStart(body []byte) string {
	if len(body) == 0 {
		return "the answer is empty"
	}
	const most = 80
	if len(body) > most {
		return fmt.Sprintf("the answer starts %q…", body[:most])
	}
	return fmt.Sprintf("the answer is %q", body)
}

// parseGraphiteRender reads the render API's format=json answer. A series
// answered twice — path, tags and points alike — is kept once.
func parseGraphiteRender(body []byte, report *GraphiteReport) ([]*graphiteSeries, error) {
	var raw []struct {
		Target     string         `json:"target"`
		Tags       map[string]any `json:"tags"`
		Datapoints [][]any        `json:"datapoints"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&raw); err != nil {
		return nil, fmt.Errorf("graphite render JSON: %w", err)
	}
	out := make([]*graphiteSeries, 0, len(raw))
	seen := map[string]bool{}
	for i, entry := range raw {
		s := &graphiteSeries{}
		var err error
		s.path, s.tags, err = parseGraphitePath(entry.Target)
		if err != nil {
			return nil, fmt.Errorf("graphite render JSON: series %d: %w", i, err)
		}
		// The render API's own tags, when it gives them, are the series'
		// tags as Graphite knows them, and a function such as alias() may
		// have given the series a target that is not its path.
		if entry.Tags != nil {
			s.tags = map[string]string{}
			for name, value := range entry.Tags {
				s.tags[name] = fmt.Sprint(value)
			}
			// carbonapi leaves name out of the tags of some function
			// results; the document always has it.
			if s.tags["name"] == "" {
				s.tags["name"] = s.path
			}
		}
		for j, point := range entry.Datapoints {
			if len(point) != 2 {
				return nil, fmt.Errorf("graphite render JSON: series %q point %d has %d elements, not [value, timestamp]", entry.Target, j, len(point))
			}
			if point[0] == nil {
				continue
			}
			value, okValue := jsonFloat(point[0])
			at, okTime := jsonFloat(point[1])
			if !okValue || !okTime {
				return nil, fmt.Errorf("graphite render JSON: series %q point %d is %v, not [value, timestamp] as numbers", entry.Target, j, point)
			}
			s.points = append(s.points, graphitePoint{value, at})
		}
		key := seriesKey(s.path, s.tags) + fmt.Sprint(s.points)
		if seen[key] {
			report.Duplicates++
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out, nil
}

func jsonFloat(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

// parseCarbonLines reads carbon plaintext: `<path> <value> [<timestamp>]` a
// line. A missing timestamp, or -1, is now, as carbon takes it. Blank lines
// and lines starting with # are skipped, and so, with skip, is a line that
// cannot be read, counted in report; without, it fails the decode.
func parseCarbonLines(body []byte, now time.Time, skip bool, report *GraphiteReport) ([]*graphiteSeries, error) {
	var out []*graphiteSeries
	byKey := map[string]*graphiteSeries{}
	for number, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, tags, point, err := parseCarbonLine(line, now)
		if err != nil {
			err = fmt.Errorf("carbon line %d: %w", number+1, err)
			if !skip {
				return nil, err
			}
			if report.SkippedLines == 0 {
				report.FirstSkipped = err.Error()
			}
			report.SkippedLines++
			continue
		}
		key := seriesKey(path, tags)
		s := byKey[key]
		if s == nil {
			s = &graphiteSeries{path: path, tags: tags}
			byKey[key] = s
			out = append(out, s)
		}
		s.points = append(s.points, point)
	}
	return out, nil
}

// parseCarbonLine reads one carbon line.
func parseCarbonLine(line string, now time.Time) (string, map[string]string, graphitePoint, error) {
	fields := strings.Fields(line)
	if len(fields) != 2 && len(fields) != 3 {
		return "", nil, graphitePoint{}, fmt.Errorf("%q has %d fields; want <path> <value> <timestamp>", line, len(fields))
	}
	path, tags, err := parseGraphitePath(fields[0])
	if err != nil {
		return "", nil, graphitePoint{}, err
	}
	value, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return "", nil, graphitePoint{}, fmt.Errorf("the value %q is not a number", fields[1])
	}
	at := float64(now.Unix())
	if len(fields) == 3 {
		stamp, err := strconv.ParseFloat(fields[2], 64)
		switch {
		case err != nil:
			return "", nil, graphitePoint{}, fmt.Errorf("the timestamp %q is not a number of Unix seconds", fields[2])
		case stamp >= carbonMillisecondsAbove:
			return "", nil, graphitePoint{}, fmt.Errorf("the timestamp %q is in milliseconds, it seems; carbon lines take Unix seconds", fields[2])
		case stamp != -1:
			at = stamp
		}
	}
	return path, tags, graphitePoint{value, at}, nil
}

// parseGraphitePath splits a path from its tags, as Graphite writes a tagged
// series: name;tag=value;tag=value. The tags always hold name.
func parseGraphitePath(raw string) (string, map[string]string, error) {
	path, rest, tagged := strings.Cut(raw, ";")
	if path == "" {
		return "", nil, fmt.Errorf("the series %q has no path", raw)
	}
	tags := map[string]string{"name": path}
	for tagged {
		var tag string
		tag, rest, tagged = strings.Cut(rest, ";")
		name, value, ok := strings.Cut(tag, "=")
		if !ok || name == "" {
			return "", nil, fmt.Errorf("the series %q has a tag %q that is not name=value", raw, tag)
		}
		tags[name] = value
	}
	return path, tags, nil
}

// seriesKey tells series apart by path and tags, whatever order a line
// wrote the tags in.
func seriesKey(path string, tags map[string]string) string {
	var b strings.Builder
	b.WriteString(path)
	for _, name := range model.SortedKeys(tags) {
		b.WriteString(";")
		b.WriteString(name)
		b.WriteString("=")
		b.WriteString(tags[name])
	}
	return b.String()
}

// document is the series as the transforms see it, or why it is left out:
// "no points" when it has no point with a value, "stale" when its newest is
// older than max_age.
func (s *graphiteSeries) document(cfg model.GraphiteConfig, now time.Time) (map[string]any, string) {
	points := make([]graphitePoint, 0, len(s.points))
	for _, p := range s.points {
		if isFinite(p.value) && isFinite(p.time) {
			points = append(points, p)
		}
	}
	if len(points) == 0 {
		return nil, "no points"
	}
	// Oldest first; points written for the same time keep their order, so
	// the last written is the newest.
	sort.SliceStable(points, func(i, j int) bool { return points[i].time < points[j].time })
	newest := points[len(points)-1]
	if maxAge := time.Duration(cfg.MaxAge); maxAge > 0 {
		if age := now.Sub(unixTime(newest.time)); age > maxAge {
			return nil, "stale"
		}
	}
	value := reduceGraphitePoints(cfg.Value, points)
	segments := strings.Split(s.path, ".")
	segmentValues := make([]any, len(segments))
	for i, segment := range segments {
		segmentValues[i] = segment
	}
	tags := make(map[string]any, len(s.tags))
	for name, tag := range s.tags {
		tags[name] = tag
	}
	pointValues := make([]any, len(points))
	for i, p := range points {
		pointValues[i] = []any{p.value, p.time}
	}
	return map[string]any{
		"path":     s.path,
		"segments": segmentValues,
		"tags":     tags,
		"value":    value,
		"time":     newest.time,
		"points":   pointValues,
	}, ""
}

// unixTime is a time in Unix seconds, fractions kept, without the overflow
// that multiplying it into nanoseconds has past the year 2262.
func unixTime(seconds float64) time.Time {
	whole, fraction := math.Modf(seconds)
	return time.Unix(int64(whole), int64(fraction*float64(time.Second)))
}

// reduceGraphitePoints is response.graphite.value applied to points, which
// hold at least one and are oldest first. Unset, or last, it is the newest.
func reduceGraphitePoints(how string, points []graphitePoint) float64 {
	switch how {
	case "max", "min":
		out := points[0].value
		for _, p := range points[1:] {
			if how == "max" && p.value > out || how == "min" && p.value < out {
				out = p.value
			}
		}
		return out
	case "sum", "avg":
		sum := 0.0
		for _, p := range points {
			sum += p.value
		}
		if how == "avg" {
			return sum / float64(len(points))
		}
		return sum
	}
	return points[len(points)-1].value
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// looksLikeCarbon reports whether a body without a content type that says
// what it is reads as carbon lines: every line `<path> <number> <number>`,
// and some path with a dot or a tag, which a Prometheus sample line, whose
// timestamp would also be a third number, cannot have in its name.
func looksLikeCarbon(body []byte) bool {
	dotted, lines := false, 0
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || strings.ContainsAny(fields[0], `{}"`) {
			return false
		}
		for _, number := range fields[1:] {
			if _, err := strconv.ParseFloat(number, 64); err != nil {
				return false
			}
		}
		if strings.ContainsAny(fields[0], ".;") {
			dotted = true
		}
		lines++
	}
	return lines > 0 && dotted
}
