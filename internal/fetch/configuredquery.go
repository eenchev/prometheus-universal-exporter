package fetch

import (
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// A debug report masks every query value of the requests it lists, since
// a probe parameter, a placeholder or the target can put anything in one,
// a token under any name among it. The values a collector's configuration
// sets itself are the operator's, and the debug page shows them in the
// collector's YAML already: those a request line shows (RedactRequestURL),
// so that graphite's target, from and until, http's request.query and
// kubernetes' limit and selectors can be read off the report. A value is
// the configuration's only when it is sent as the collector's file writes
// it: one a ${NAME} reference expanded is the environment's, which the
// page never shows, and one with a {{param_...}} placeholder the probe's.

// ConfiguredQuery says, of a query pair of a request c made, its name and
// its value as a server reads them, whether c's configuration set it as it
// was sent: a value of request.query without placeholders, and what its
// type sets itself (RequestType.ShownQuery). It says nothing of the name's
// reading as a credential's, which RedactRequestURL masks whatever this
// says. A static target's own request keys are not the collector's, and
// are not shown.
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

// writtenAsSent says whether value, sent from the request key at path
// under request, is what c's file writes there: not what a ${NAME}
// reference expanded to. A key the file does not write, a default or one
// of a collector held without a file, is the exporter's, and is.
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
