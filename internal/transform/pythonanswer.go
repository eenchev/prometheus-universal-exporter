package transform

import (
	"bytes"
	"encoding/json"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A worker's answer is one line of JSON, {"ok": true, "log": "...",
// "metrics": [...]} or the same with "data", and reading it was a third of
// what a script of five thousand series cost the exporter: encoding/json
// made a map of every metric and of its labels, a string of every key and a
// json.Number of every number, through reflection, and each map was then
// read into the series it stands for. readPythonAnswer reads the line
// itself: a metric as metric(...) appends it becomes its series as it is
// read, with no map between, and what a pre-script left in data is read by
// the json decoder (decode.JSONValue), which makes of it what encoding/json
// and model.Normalize made (decode/jsondiff_test.go).
//
// It reads only what it reads exactly as before. An entry of metrics that is
// not what metric(...) appends — another type for an argument, a key of the
// script's own, a key written twice, a value that is not a number — is read
// by encoding/json as it was and checked by the code that always checked
// it, so its error is the error it was. And a line that is not an answer as
// the worker writes it, or is no JSON at all, is not read here: the reader
// says so and the line is read as it was (countedPythonResult), with the
// error it then had. pythonanswer_test.go compares the two over every kind
// of answer.

// pythonAnswerDepth is how deep encoding/json reads arrays and objects,
// which is how deep a metric of an answer that encoding/json reads may
// nest, the answer around it counted (skip). What a pre-script left in data
// is not read by it, and nests as deep as a decoded value does
// (decode.MaxDepth, decode.JSONValue).
const pythonAnswerDepth = 10000

// answerReader reads a worker's answer.
type answerReader struct {
	data []byte
	pos  int
	// strings holds the strings read last, by where each stood: the names,
	// types and label names of a script's metrics are mostly the same from
	// one metric to the next, and are made strings once.
	strings [answerStrings]string
	// values holds label values read before, each in the one place its
	// hash gives it: a label that says one of a few things, a region or a
	// state, is made a string once for each, and one that differs in every
	// series, an ID, costs only the looking.
	values [answerValues]string
}

// answerValues is how many label values are remembered, a power of two, and
// answerValueLength the longest that is.
const (
	answerValues      = 256
	answerValueLength = 32
)

// The places of answerReader.strings: a metric's name, type and help, and
// its first label names. answerValue and answerOnce are no place: a label
// value, which is remembered by what it says, and a string read once.
const (
	answerName = iota
	answerType
	answerHelp
	answerLabels
	answerStrings = answerLabels + 16
	answerValue   = -1
	answerOnce    = -2
)

// readPythonAnswer reads a worker's answer line, or reports false for a line
// it does not read, which is then read as it was.
func readPythonAnswer(line []byte) (*pythonOutput, bool) {
	a := &answerReader{data: line}
	out := &pythonOutput{read: true}
	a.space()
	if !a.take('{') {
		return nil, false
	}
	const (
		sawOK = 1 << iota
		sawLog
		sawError
		sawMetrics
		sawData
	)
	seen := 0
	for {
		a.space()
		key, plain, ok := a.text()
		if !ok || !plain {
			return nil, false
		}
		a.space()
		if !a.take(':') {
			return nil, false
		}
		a.space()
		saw := 0
		switch string(key) {
		case "ok":
			saw = sawOK
			switch {
			case a.word("true"):
				out.OK = true
			case a.word("false"):
			default:
				return nil, false
			}
		case "log":
			saw = sawLog
			if out.Log, ok = a.string(answerOnce); !ok {
				return nil, false
			}
		case "error":
			saw = sawError
			if out.Error, ok = a.string(answerOnce); !ok {
				return nil, false
			}
		case "metrics":
			saw = sawMetrics
			if !a.metrics(out) {
				return nil, false
			}
		case "data":
			saw = sawData
			// As deep as a decoder makes a value, whatever lies around it
			// here: a script may leave in data what it was given.
			value, rest, err := decode.JSONValue(a.data[a.pos:])
			if err != nil {
				return nil, false
			}
			out.Data, out.normalized = value, true
			a.pos = len(a.data) - len(rest)
		default:
			return nil, false
		}
		// A key written twice is read as it was.
		if seen&saw != 0 {
			return nil, false
		}
		seen |= saw
		a.space()
		if a.take(',') {
			continue
		}
		if !a.take('}') {
			return nil, false
		}
		break
	}
	a.space()
	if a.pos != len(a.data) {
		return nil, false
	}
	return out, true
}

// metrics reads the array of a script's metrics into out: each as its
// series, the first error of one that is none, and how many there are. It
// reports false for what is no array of values.
func (a *answerReader) metrics(out *pythonOutput) bool {
	if !a.take('[') {
		return false
	}
	// A metric as metric(...) appends it has two opening braces, its own
	// and its labels', so half of those there are is about how many series
	// to make room for.
	out.series = make([]model.Metric, 0, bytes.Count(a.data[a.pos:], []byte("{"))/2)
	a.space()
	if a.take(']') {
		return true
	}
	for {
		start := a.pos
		metric, ok := a.metric()
		var err error
		if !ok {
			// Not what metric(...) appends: read as any value is read, and
			// checked as it always was.
			a.pos = start
			if !a.skip(2) {
				return false
			}
			raw, valid := decodePythonValue(a.data[start:a.pos])
			if !valid {
				return false
			}
			var emitted pythonMetric
			if emitted, err = pythonMetricFrom(out.count, raw); err == nil {
				metric, err = emitted.metric()
			}
		}
		switch {
		case out.seriesErr != nil:
		case err != nil:
			out.seriesErr = err
		default:
			out.series = append(out.series, metric)
		}
		out.count++
		a.space()
		if a.take(',') {
			a.space()
			continue
		}
		return a.take(']')
	}
}

// metric reads a metric as metric(...) appends it: an object of name, type,
// value, labels, help and timestamp, each once at most, the name a string,
// the type and the help a string or null, the value a number, the labels an
// object of strings and nulls or null, the timestamp a number or null. It
// reports false for anything else, a metric that has an error among it,
// which the caller reads as it was read.
func (a *answerReader) metric() (model.Metric, bool) {
	metric := model.Metric{Type: model.GaugeMetricType}
	if !a.take('{') {
		return metric, false
	}
	const (
		sawName = 1 << iota
		sawType
		sawValue
		sawLabels
		sawHelp
		sawTimestamp
	)
	seen := 0
	for {
		a.space()
		key, plain, ok := a.text()
		if !ok || !plain {
			return metric, false
		}
		a.space()
		if !a.take(':') {
			return metric, false
		}
		a.space()
		saw := 0
		switch string(key) {
		case "name":
			saw = sawName
			if metric.Name, ok = a.string(answerName); !ok {
				return metric, false
			}
		case "type":
			saw = sawType
			if !a.word("null") {
				kind, ok := a.string(answerType)
				if !ok {
					return metric, false
				}
				if kind != "" {
					metric.Type = model.MetricType(kind)
				}
			}
		case "help":
			saw = sawHelp
			if !a.word("null") {
				if metric.Help, ok = a.string(answerHelp); !ok {
					return metric, false
				}
			}
		case "value":
			saw = sawValue
			if metric.Value, ok = a.number(); !ok {
				return metric, false
			}
		case "labels":
			saw = sawLabels
			if !a.word("null") {
				if metric.Labels, ok = a.labels(); !ok {
					return metric, false
				}
			}
		case "timestamp":
			saw = sawTimestamp
			if !a.word("null") {
				at, ok := a.number()
				if !ok {
					return metric, false
				}
				ms, err := timestampMillis(at)
				if err != nil {
					return metric, false
				}
				metric.Timestamp = &ms
			}
		default:
			return metric, false
		}
		if seen&saw != 0 {
			return metric, false
		}
		seen |= saw
		a.space()
		if a.take(',') {
			continue
		}
		if !a.take('}') {
			return metric, false
		}
		// A metric without a name or a value has an error to report.
		return metric, seen&sawName != 0 && seen&sawValue != 0
	}
}

// labels reads a metric's labels, an object of strings: null leaves a label
// out, as None does, also one written before it under the same name, since
// of a name written twice the later counted, and so does an empty string
// (pythonMetric.metric). A value that is no string, or is the marker of a
// float that is not finite, is not read here.
func (a *answerReader) labels() (map[string]string, bool) {
	if !a.take('{') {
		return nil, false
	}
	a.space()
	if a.take('}') {
		return nil, true
	}
	// Most metrics have a few labels; a map that turns out too small grows.
	labels := make(map[string]string, 4)
	for place := answerLabels; ; place++ {
		name, ok := a.string(min(place, answerStrings-1))
		if !ok {
			return nil, false
		}
		a.space()
		if !a.take(':') {
			return nil, false
		}
		a.space()
		if a.word("null") {
			delete(labels, name)
		} else {
			value, ok := a.string(answerValue)
			if !ok || value != "" && value[0] == 0 {
				return nil, false
			}
			if value == "" {
				delete(labels, name)
			} else {
				labels[name] = value
			}
		}
		a.space()
		if a.take(',') {
			a.space()
			continue
		}
		return labels, a.take('}')
	}
}

// space passes over whitespace.
func (a *answerReader) space() {
	for a.pos < len(a.data) {
		switch a.data[a.pos] {
		case ' ', '\t', '\r', '\n':
			a.pos++
		default:
			return
		}
	}
}

// take passes over c when it is the next byte, and reports whether it was.
func (a *answerReader) take(c byte) bool {
	if a.pos < len(a.data) && a.data[a.pos] == c {
		a.pos++
		return true
	}
	return false
}

// word passes over a literal, true, false or null, when it is what comes
// next and ends there, and reports whether it was.
func (a *answerReader) word(literal string) bool {
	end := a.pos + len(literal)
	if end > len(a.data) || string(a.data[a.pos:end]) != literal {
		return false
	}
	if end < len(a.data) {
		switch a.data[end] {
		case ',', '}', ']', ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	a.pos = end
	return true
}

// text reads the string that starts at pos and returns what is between its
// quotes, and whether that is plain: printable ASCII without an escape,
// which is the string as it is. It reports false when no string starts at
// pos or it does not end.
func (a *answerReader) text() (between []byte, plain, ok bool) {
	if a.pos >= len(a.data) || a.data[a.pos] != '"' {
		return nil, false, false
	}
	plain = true
	for i := a.pos + 1; i < len(a.data); i++ {
		switch c := a.data[i]; {
		case c == '"':
			between = a.data[a.pos+1 : i]
			a.pos = i + 1
			return between, plain, true
		case c == '\\':
			plain = false
			i++
		case c < ' ' || c > '~':
			plain = false
		}
	}
	return nil, false, false
}

// string reads the string at pos as encoding/json reads one. place is where
// in a.strings a string like it was read last, the same text again being
// the string made then, or answerValue or answerOnce. A worker writes
// ASCII, so a string without an escape is its own bytes; one with an
// escape, or with anything else, is read by encoding/json.
func (a *answerReader) string(place int) (string, bool) {
	start := a.pos
	between, plain, ok := a.text()
	if !ok {
		return "", false
	}
	if !plain {
		var text string
		if json.Unmarshal(a.data[start:a.pos], &text) != nil {
			return "", false
		}
		return text, true
	}
	var remembered *string
	switch {
	case place >= 0:
		remembered = &a.strings[place]
	case place == answerOnce || len(between) > answerValueLength:
		return string(between), true
	default:
		// FNV-1a, as the json decoder remembers its strings by
		// (decode/jsonvalue.go).
		h := uint32(2166136261)
		for _, c := range between {
			h = (h ^ uint32(c)) * 16777619
		}
		remembered = &a.values[(h^h>>15)&(answerValues-1)]
	}
	// The comparison converts nothing: the bytes are compared in place.
	if *remembered != string(between) {
		*remembered = string(between)
	}
	return *remembered, true
}

// number reads the JSON number at pos as the number of a value or a
// timestamp is read (pythonNumber of a json.Number). It reports false for
// what is no number, or is one beyond a float64, which has an error to
// report.
func (a *answerReader) number() (float64, bool) {
	data, i := a.data, a.pos
	if i < len(data) && data[i] == '-' {
		i++
	}
	switch {
	case i < len(data) && data[i] == '0':
		i++
	case i < len(data) && data[i] >= '1' && data[i] <= '9':
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	default:
		return 0, false
	}
	if i < len(data) && data[i] == '.' {
		i++
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return 0, false
		}
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return 0, false
		}
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	f, err := model.ParseFloat(string(data[a.pos:i]))
	if err != nil {
		return 0, false
	}
	a.pos = i
	return f, true
}

// skip passes over the value at pos without reading it: to the bracket
// that closes the one it opens with, or to the end of a string, a number or
// a literal. depth is how many arrays and objects the value lies inside. It
// reports false for a value that does not end or nests deeper than
// encoding/json reads; whether what it passed over is a value is for
// whoever reads it to say (decodePythonValue).
func (a *answerReader) skip(depth int) bool {
	data, i := a.data, a.pos
	if i >= len(data) {
		return false
	}
	switch data[i] {
	case '"':
		_, _, ok := a.text()
		return ok
	case '{', '[':
		for opened := 0; i < len(data); i++ {
			switch data[i] {
			case '"':
				for i++; i < len(data) && data[i] != '"'; i++ {
					if data[i] == '\\' {
						i++
					}
				}
			case '{', '[':
				if opened++; depth+opened > pythonAnswerDepth {
					return false
				}
			case '}', ']':
				if opened--; opened == 0 {
					a.pos = i + 1
					return true
				}
			}
		}
		return false
	}
	// A number or a literal ends where what it lies in goes on.
scalar:
	for ; i < len(data); i++ {
		switch data[i] {
		case ',', '}', ']', ' ', '\t', '\r', '\n':
			break scalar
		}
	}
	if i == a.pos {
		return false
	}
	a.pos = i
	return true
}

// decodePythonValue reads raw, a value of a worker's answer, as the whole
// answer was read: by encoding/json, its numbers as json.Number. It reports
// false for what is no JSON value, or is more than one.
func decodePythonValue(raw []byte) (any, bool) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil || decoder.InputOffset() != int64(len(raw)) {
		return nil, false
	}
	return value, true
}
