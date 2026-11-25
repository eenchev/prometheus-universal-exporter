//go:build !select_request_types || request_type_http

package fetch

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A step of a chain (chain.go) sends values read from earlier answers with
// {{chain.<name>}}, and a fan-out step its entry with {{each}},
// {{each.<field>}} and {{each_index}}. Each is rewritten, when the step's
// request is built, into a placeholder of a reserved parameter, such as
// {{param__chain_token:}}, which the request's own code fills in and writes
// as its place requires: path-escaped in the path, query-escaped in the
// query, as JSON with |json in a body. A probe's parameter of the reserved
// prefix is never passed to a step (chainParams), so a probe cannot set a
// value the chain reads.

// chainRef is a chain placeholder found in a step's text: what it names,
// and where it is in the text.
type chainRef struct {
	// Kind is "chain", "each", "each_field" or "each_index".
	Kind string
	// Name is the value's name for chain, and the field's for each_field.
	Name string
}

// chainRefPattern matches every placeholder a chain reads, with its
// optional filter.
var chainRefPattern = regexp.MustCompile(`\{\{\s*(chain\.[^}|]*|each(?:\.[^}|]*)?|each_index)(\|[^}]*)?\}\}`)

// chainNamePattern is what a value's name, and a field of {{each.<field>}},
// must match.
var chainNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// rewriteChainRefs returns text with its chain placeholders rewritten into
// placeholders of reserved parameters, and what they name. where names the
// text in an error.
func rewriteChainRefs(where, text string) (string, []chainRef, error) {
	if !strings.Contains(text, "{{") {
		return text, nil, nil
	}
	var refs []chainRef
	var failed error
	rewritten := chainRefPattern.ReplaceAllStringFunc(text, func(match string) string {
		if failed != nil {
			return match
		}
		groups := chainRefPattern.FindStringSubmatch(match)
		inner, filter := groups[1], groups[2]
		if strings.TrimLeft(match[2:], " \t") != match[2:] {
			failed = fmt.Errorf("%s has a placeholder with a space after {{; write %s without spaces", where, strings.Join(strings.Fields(match), ""))
			return match
		}
		var ref chainRef
		var param string
		switch {
		case inner == "each":
			ref, param = chainRef{Kind: "each"}, chainParamPrefix+"each"
		case inner == "each_index":
			ref, param = chainRef{Kind: "each_index"}, chainParamPrefix+"each_index"
		case strings.HasPrefix(inner, "each."):
			field := strings.TrimPrefix(inner, "each.")
			if !chainNamePattern.MatchString(field) {
				failed = fmt.Errorf("%s placeholder {{%s}} names the field %q; a field is letters, digits and _, such as {{each.name}}", where, inner, field)
				return match
			}
			ref, param = chainRef{Kind: "each_field", Name: field}, chainParamPrefix+"each__"+field
		default:
			name := strings.TrimPrefix(inner, "chain.")
			if !chainNamePattern.MatchString(name) {
				failed = fmt.Errorf("%s placeholder {{%s}} names the value %q; a value's name is letters, digits and _, such as {{chain.job_id}}, and takes no default", where, inner, name)
				return match
			}
			ref, param = chainRef{Kind: "chain", Name: name}, chainParamPrefix+"chain_"+name
		}
		refs = append(refs, ref)
		// The empty default: a value is always given, as the empty text
		// when it is optional and was not found (chainParams).
		return "{{" + param + ":" + filter + "}}"
	})
	if failed != nil {
		return text, nil, failed
	}
	return rewritten, refs, nil
}

// chainValueParam is the reserved parameter a value of the chain is given
// to a step in.
func chainValueParam(name string) string { return chainParamPrefix + "chain_" + name }

// chainEachParams are the reserved parameters of a fan-out item: its entry,
// its place, and each field of an entry that is an object.
func chainEachParams(params map[string]string, entry any, index int) {
	params[chainParamPrefix+"each"] = chainText(entry)
	params[chainParamPrefix+"each_index"] = strconv.Itoa(index)
	if object, ok := entry.(map[string]any); ok {
		for field, value := range object {
			if chainNamePattern.MatchString(field) {
				params[chainParamPrefix+"each__"+field] = chainText(value)
			}
		}
	}
}
