package fetch

import (
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// A debug report masks every query value of the requests it lists. A probe
// parameter, a placeholder or the target can put anything in a value,
// including a token under any name. The values a collector's configuration
// sets itself are the operator's, and the debug page already shows them in
// the collector's YAML. A request line shows these (RedactRequestURL), so
// graphite's target, from and until, http's request.query, and kubernetes'
// limit and selectors can be read off the report. A value is the
// configuration's only when it is sent as the collector's file writes it. A
// value expanded from a ${NAME} reference is the environment's, which the
// page never shows. A value with a {{param_...}} placeholder is the probe's.

// ConfiguredQuery says whether c's configuration set a query pair of a
// request c made as it was sent. The pair is given by its name and value as
// a server reads them. Set pairs are values of request.query without
// placeholders, and what the type sets itself (RequestType.ShownQuery). It
// says nothing about whether the name reads as a credential's;
// RedactRequestURL masks those whatever this says. A static target's own
// request keys are not the collector's, and are not shown.
func ConfiguredQuery(c *model.Collector) func(name, value string) bool {
	if c == nil {
		return nil
	}
	rt := RequestTypes[c.Request.Type]
	return func(name, value string) bool {
		if rt != nil && rt.Takes("query") {
			if v, ok := c.Request.Query[name]; ok && v == value && !HasPathParams(v) && writtenAsSent(c, value, "query", name) {
				return true
			}
		}
		return rt != nil && rt.ShownQuery != nil && rt.ShownQuery(c, name, value)
	}
}

// writtenAsSent says whether value, sent from the request key at path under
// request, is what c's file writes there, and not what a ${NAME} reference
// expanded to. A key the file does not write (a default, or a key of a
// collector held without a file) is the exporter's, and counts as written.
func writtenAsSent(c *model.Collector, value string, path ...string) bool {
	if c.Written == nil {
		return true
	}
	n := c.Written
	for _, key := range append([]string{"request"}, path...) {
		switch n.Kind {
		case yaml.MappingNode:
			entries := model.DecodedEntries(n)
			i := slices.IndexFunc(entries, func(e model.MappingEntry) bool { return model.KeyName(e.Key) == key })
			if i < 0 {
				return true
			}
			n = entries[i].Value
		case yaml.SequenceNode:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(n.Content) {
				return false
			}
			n = n.Content[i]
		default:
			return false
		}
	}
	return n.Kind == yaml.ScalarNode && strings.TrimSpace(n.Value) == strings.TrimSpace(value)
}
