package fetch

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Request templates carry path parameters (pathparams.go) beyond the path: a
// {{param_name}} placeholder may also stand in the request body, in a header
// value and in a query value of an http collector, filled by the same probe
// parameter, with the same defaults and the same errors:
//
//	request:
//	  method: POST
//	  path: /graphql
//	  headers:
//	    X-Tenant: "{{param_tenant}}"
//	  query:
//	    region: "{{param_region:eu}}"
//	  body: '{"query": "{ status(service: {{param_service|json}}) { up } }"}'
//
// Where a value lands decides how it must be written, or a probe parameter
// could change the request's structure rather than fill a value in it:
//
//   - In the body, a filter says how: {{param_x|json}} is a JSON string,
//     quoted and escaped; |number is a JSON number, checked to be one;
//     |form is URL form encoding; |xml is escaped XML text; |raw, or no
//     filter, is the value as it is — for a template that owns the whole
//     structure, and only for values the monitor controls.
//   - In a header value, the value is refused if it holds a control
//     character, so it can never end the header and start another.
//   - In a query value, the value is encoded as a query value.
//   - In a graphite collector's request.targets, the value lands inside a
//     Graphite expression, which has no escaping, so it may hold only what a
//     path node or a tag value is made of: letters, digits and _ - . : @ % +
//     ~. Anything else — a quote, a comma, a parenthesis, a glob — could
//     change the expression rather than fill a value in it, and is refused.
//
// Filters exist only in the body: the path, header, query and targets each
// have one safe encoding, applied always. In the body, a header value, a
// query value and a target, `{{` opens a placeholder only when `param_` follows it, since a
// body may well contain braces of its own; `{{ param_x }}` with spaces is
// refused rather than sent as text. In a path, `{{` always opens one.

// The filters a body placeholder may name.
var bodyFilters = []string{"json", "number", "form", "xml", "raw"}

var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// graphiteValue is what a placeholder may be filled with in a Graphite
// expression: the characters of a path node or a tag value, and none that
// Graphite's grammar gives a meaning.
var graphiteValue = regexp.MustCompile(`^[A-Za-z0-9_.:@%+~-]*$`)

// parsePlaceholders finds the placeholders of text, reported as where. strict
// makes every `{{` a placeholder; otherwise only `{{param_`. filters allows a
// |filter.
func parsePlaceholders(where, text string, strict, filters bool) ([]pathPlaceholder, error) {
	var out []pathPlaceholder
	for offset := 0; ; {
		open := strings.Index(text[offset:], "{{")
		if open < 0 {
			return out, nil
		}
		open += offset
		if !strict {
			after := text[open+2:]
			trimmed := strings.TrimLeft(after, " \t")
			if !strings.HasPrefix(trimmed, PathParamPrefix) {
				offset = open + 2
				continue
			}
			if len(trimmed) != len(after) {
				return nil, fmt.Errorf("%s has a placeholder with a space after {{; write {{%s<name>}} without spaces", where, PathParamPrefix)
			}
		}
		closing := strings.Index(text[open+2:], "}}")
		if closing < 0 {
			return nil, fmt.Errorf("%s has an unclosed placeholder at %q; write {{param_name}} or {{param_name:default}}", where, text[open:])
		}
		closing += open + 2
		inner := text[open+2 : closing]
		filter := ""
		if i := strings.LastIndex(inner, "|"); i >= 0 {
			if !filters {
				return nil, fmt.Errorf("%s placeholder {{%s}} has a filter; filters apply only in request.body, and a path, header or query value is always encoded one way", where, inner)
			}
			filter = inner[i+1:]
			if !slices.Contains(bodyFilters, filter) {
				return nil, fmt.Errorf("%s placeholder {{%s}} has the unknown filter %q; use one of %s. A default may not contain | unless a filter follows", where, inner, filter, strings.Join(bodyFilters, ", "))
			}
			inner = inner[:i]
		}
		name, def, hasDefault := strings.Cut(inner, ":")
		if !PathParamName.MatchString(name) {
			return nil, fmt.Errorf("%s placeholder {{%s}} is not a path parameter; placeholders are named %s<name>, with letters, digits and underscores, e.g. {{param_tenant}}", where, inner, PathParamPrefix)
		}
		// A default ends at the first }}, so braces inside it can only come
		// from something the author did not mean as a default. The common case
		// is an environment reference left unexpanded because
		// --config.expand-env is off: {{param_x:${X}}} would otherwise bind the
		// default "${X" and leave a stray brace behind.
		if strings.ContainsAny(def, "{}") {
			return nil, fmt.Errorf("%s placeholder {{%s}} has a default containing a brace; if it is an environment reference, run with --config.expand-env so it is expanded first", where, inner)
		}
		out = append(out, pathPlaceholder{Name: name, Default: def, HasDefault: hasDefault, Filter: filter, start: open, end: closing + 2})
		offset = closing + 2
	}
}

// placeholderValue is a placeholder's value: the probe's, else the default,
// else an error. An empty probe value counts as not given.
func placeholderValue(where string, p pathPlaceholder, params map[string]string) (string, error) {
	value, given := params[p.Name]
	if given && value != "" {
		return value, nil
	}
	if !p.HasDefault {
		return "", &MissingParamError{Where: where, Name: p.Name}
	}
	return p.Default, nil
}

// MissingParamError is a placeholder with neither a value nor a default.
type MissingParamError struct{ Where, Name string }

func (e *MissingParamError) Error() string {
	return fmt.Sprintf("%s needs %s, which the probe did not supply and which has no default; add &%s=<value> to the probe, or give it a default as {{%s:<default>}}", e.Where, e.Name, e.Name, e.Name)
}

// templateField is one templated part of a request other than its path.
type templateField struct {
	where string
	text  string
	// kind is "body", "header", "query" or "graphite", which decides the
	// encoding.
	kind string
}

// requestTemplates lists a collector's templated body, header and query
// values. The body is left out when the probe replaced it.
func requestTemplates(c *model.Collector, overrides RequestOverrides) []templateField {
	var out []templateField
	if overrides.Body == nil && HasPathParams(c.Request.Body) {
		out = append(out, templateField{"request.body", c.Request.Body, "body"})
	}
	for _, name := range model.SortedKeys(c.Request.Headers) {
		if value := c.Request.Headers[name]; HasPathParams(value) {
			out = append(out, templateField{"request.headers." + name, value, "header"})
		}
	}
	for _, name := range model.SortedKeys(c.Request.Query) {
		if value := c.Request.Query[name]; HasPathParams(value) {
			out = append(out, templateField{"request.query." + name, value, "query"})
		}
	}
	// A static target's own targets replace the collector's, placeholders
	// and all.
	if overrides.Targets == nil {
		for i, target := range c.Request.Targets {
			if HasPathParams(target) {
				out = append(out, templateField{fmt.Sprintf("request.targets[%d]", i), target, "graphite"})
			}
		}
	}
	return out
}

// parseField finds the placeholders of a field.
func (f templateField) parse() ([]pathPlaceholder, error) {
	return parsePlaceholders(f.where, f.text, false, f.kind == "body")
}

// render fills a field's placeholders in, each value written as its place
// requires.
func (f templateField) render(params map[string]string) (string, error) {
	placeholders, err := f.parse()
	if err != nil || len(placeholders) == 0 {
		return f.text, err
	}
	var b strings.Builder
	previous := 0
	for _, p := range placeholders {
		value, err := placeholderValue(f.where, p, params)
		if err != nil {
			return "", err
		}
		written, err := f.write(p, value)
		if err != nil {
			return "", err
		}
		b.WriteString(f.text[previous:p.start])
		b.WriteString(written)
		previous = p.end
	}
	b.WriteString(f.text[previous:])
	return b.String(), nil
}

// write encodes a value for its place.
func (f templateField) write(p pathPlaceholder, value string) (string, error) {
	switch f.kind {
	case "header":
		for _, r := range value {
			if r < 0x20 && r != '\t' || r == 0x7f {
				return "", fmt.Errorf("%s: the value of %s contains a control character, which a header value may not", f.where, p.Name)
			}
		}
		return value, nil
	case "query":
		// url.Values.Encode escapes the whole value.
		return value, nil
	case "graphite":
		if !graphiteValue.MatchString(value) {
			return "", fmt.Errorf("%s: the value of %s, %q, may hold only letters, digits and _ - . : @ %% + ~, since it lands inside a Graphite expression, where anything else could change the expression", f.where, p.Name, value)
		}
		return value, nil
	}
	switch p.Filter {
	case "json":
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("%s: %s: %w", f.where, p.Name, err)
		}
		return string(encoded), nil
	case "number":
		if !jsonNumber.MatchString(value) {
			return "", fmt.Errorf("%s: %s must be a number for its |number filter, not %q", f.where, p.Name, value)
		}
		return value, nil
	case "form":
		return url.QueryEscape(value), nil
	case "xml":
		var b strings.Builder
		if err := xml.EscapeText(&b, []byte(value)); err != nil {
			return "", fmt.Errorf("%s: %s: %w", f.where, p.Name, err)
		}
		return b.String(), nil
	}
	return value, nil
}

// requestParamNames lists the parameters a collector's request uses: in its
// path, unless the probe replaced it, and in its templated body, headers and
// query values.
func requestParamNames(c *model.Collector, overrides RequestOverrides) (map[string]bool, error) {
	used := map[string]bool{}
	if !overrides.PathSet && HasPathParams(c.Request.Path) {
		placeholders, err := parsePathParams(c.Request.Path)
		if err != nil {
			return nil, err
		}
		for _, p := range placeholders {
			used[p.Name] = true
		}
	}
	for _, f := range requestTemplates(c, overrides) {
		placeholders, err := f.parse()
		if err != nil {
			return nil, err
		}
		for _, p := range placeholders {
			used[p.Name] = true
		}
	}
	return used, nil
}

// RequestParam is a parameter a collector's request takes from the probe, as
// param_<name>: the value a {{param_<name>}} placeholder is filled with.
type RequestParam struct {
	Name string
	// Required is set when some placeholder naming it has no default, so a
	// probe must supply it. Default is the first default given otherwise.
	Required bool
	Default  string
}

// RequestParams lists the parameters a collector's request takes, in its
// path, templated body, headers and query values, sorted by name. A request
// whose placeholders cannot be parsed takes none; the configuration refuses
// it before it gets here.
func RequestParams(c *model.Collector) []RequestParam {
	var placeholders []pathPlaceholder
	if HasPathParams(c.Request.Path) {
		found, err := parsePathParams(c.Request.Path)
		if err != nil {
			return nil
		}
		placeholders = append(placeholders, found...)
	}
	for _, f := range requestTemplates(c, RequestOverrides{}) {
		found, err := f.parse()
		if err != nil {
			return nil
		}
		placeholders = append(placeholders, found...)
	}
	byName := map[string]*RequestParam{}
	var names []string
	for _, p := range placeholders {
		param := byName[p.Name]
		if param == nil {
			param = &RequestParam{Name: p.Name}
			byName[p.Name] = param
			names = append(names, p.Name)
		}
		if !p.HasDefault {
			param.Required = true
		} else if param.Default == "" {
			param.Default = p.Default
		}
	}
	sort.Strings(names)
	out := make([]RequestParam, 0, len(names))
	for _, name := range names {
		out = append(out, *byName[name])
	}
	return out
}

// CheckRequestParams binds every placeholder of a request against the
// parameters without sending anything, so a missing or unfit value is known
// before the target is contacted, and reports parameters nothing uses.
func CheckRequestParams(c *model.Collector, overrides RequestOverrides) (unused []string, err error) {
	used, err := requestParamNames(c, overrides)
	if err != nil {
		return nil, err
	}
	if !overrides.PathSet && HasPathParams(c.Request.Path) {
		if _, _, err := bindPathParams(c.Request.Path, overrides.Params); err != nil {
			return nil, err
		}
	}
	for _, f := range requestTemplates(c, overrides) {
		if _, err := f.render(overrides.Params); err != nil {
			return nil, err
		}
	}
	for name := range overrides.Params {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	return unused, nil
}

// renderedValues is a request's header or query values with their
// placeholders filled in.
func renderedValues(values map[string]string, kind, prefix string, params map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for name, value := range values {
		if HasPathParams(value) {
			rendered, err := templateField{prefix + name, value, kind}.render(params)
			if err != nil {
				return nil, err
			}
			value = rendered
		}
		out[name] = value
	}
	return out, nil
}
