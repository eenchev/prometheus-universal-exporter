package transform

import (
	"reflect"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// ReadsTimings says whether a rule or a script of c may read what only the
// trace of its request sees: the timings, $timings or Python's
// response.timings, and the address the connection was made to,
// $remote_ip or response.remote_ip. A request is traced only when
// something reads them (model.Collector.TimingsUnread).
//
// A Python script may reach response.timings by a name it makes, so a
// collector with one always reads them. A jq or yq program can name a
// variable only as it is written, so one that reads $timings or $remote_ip
// holds that text: every text of the collector is looked through for them,
// whichever key holds it, so a key that comes to hold a program is never
// missed. A text that holds one and is no program, or that names another
// variable of that name of its own, only costs the trace it did not need.
func ReadsTimings(c *model.Collector) bool {
	if c.Transform.Type == "python" || strings.TrimSpace(c.Transform.PreScript) != "" {
		return true
	}
	for _, text := range tracedVariables {
		if (textSearch{text: text, seen: map[uintptr]bool{}}).in(reflect.ValueOf(c).Elem()) {
			return true
		}
	}
	return false
}

// tracedVariables are the jq variables whose values only the trace of a
// request sees.
var tracedVariables = []string{"$timings", "$remote_ip"}

// textSearch looks for text in every string of a value, however deep,
// each pointer followed once.
type textSearch struct {
	text string
	seen map[uintptr]bool
}

func (s textSearch) in(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), s.text)
	case reflect.Pointer:
		if v.IsNil() || s.seen[v.Pointer()] {
			return false
		}
		s.seen[v.Pointer()] = true
		return s.in(v.Elem())
	case reflect.Interface:
		return !v.IsNil() && s.in(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if s.in(v.Field(i)) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if s.in(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if s.in(iter.Key()) || s.in(iter.Value()) {
				return true
			}
		}
	}
	return false
}
