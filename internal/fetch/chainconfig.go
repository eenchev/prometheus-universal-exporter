//go:build !select_request_types || request_type_http

package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The load checks of an http collector's chain: request.extract,
// request.then and the collector's answer. The schemas refuse the same
// mistakes where JSON Schema can say them (chainKeyRules); a value used
// before a step reads it, and the origin of a path, the exporter alone
// refuses.

// chainStepName is what a step's name must match.
var chainStepName = regexp.MustCompile(model.ChainStepNamePattern)

// chainURLPattern is what a step's url must be: one placeholder of a value,
// alone.
var chainURLPattern = regexp.MustCompile(`^\{\{chain\.([A-Za-z_][A-Za-z0-9_]*)\}\}$`)

// chainMethods are the methods a step may use.
var chainMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch}

// chainValue is what the load knows of a value of the chain: the step that
// reads it, and whether it is a json value, the only kind for_each takes.
type chainValue struct {
	step string
	json bool
	used bool
}

// validateChain checks an http collector's chain at load and fills in its
// defaults: each step's method, and a fan-out step's concurrency, max_items
// and on_item_error, and the collector's answer.
func validateChain(c *model.Collector) error {
	own := httpRequest(c)
	if own.Then == nil {
		switch {
		case own.Extract != nil:
			return fmt.Errorf("collector %q sets request.extract without request.then; the values it reads are sent by the steps of request.then", c.Name)
		case c.Answer != "":
			return fmt.Errorf("collector %q sets answer, which says what a collector with request.then reads; it has no request.then", c.Name)
		}
		return nil
	}
	if err := checkChainBeside(c); err != nil {
		return err
	}
	switch c.Answer {
	case "":
		c.Answer = model.ChainAnswerLast
	case model.ChainAnswerLast, model.ChainAnswerAll:
	default:
		return fmt.Errorf("collector %q answer is %q; want last or all", c.Name, c.Answer)
	}
	if len(own.Then) == 0 || len(own.Then) > model.MaxChainSteps {
		return fmt.Errorf("collector %q request.then has %d steps; a chain has 1 to %d steps after its first request, 13 requests in all", c.Name, len(own.Then), model.MaxChainSteps)
	}
	values := map[string]*chainValue{}
	if err := readChainValues(c, model.ChainFirst, "request.extract", own.Extract, values); err != nil {
		return err
	}
	if err := checkFirstRequestRefs(c); err != nil {
		return err
	}
	names := map[string]bool{}
	fanOut := ""
	for i := range own.Then {
		step := &own.Then[i]
		at := fmt.Sprintf("request.then[%d]", i)
		if err := checkStepName(c, step.Name, at, names); err != nil {
			return err
		}
		at = fmt.Sprintf("request.then[%d] (%q)", i, step.Name)
		if step.ForEach != "" {
			if fanOut != "" {
				return fmt.Errorf("collector %q %s sets for_each, as step %q does; a chain has at most one fan-out", c.Name, at, fanOut)
			}
			fanOut = step.Name
		}
		if err := validateChainStep(c, step, at, values); err != nil {
			return err
		}
		if err := readChainValues(c, step.Name, at+".extract", step.Extract, values); err != nil {
			return err
		}
	}
	for _, name := range model.SortedKeys(values) {
		if v := values[name]; !v.used {
			return fmt.Errorf("collector %q reads the value %q in step %q, which no later step sends or fans out over; leave it out", c.Name, name, v.step)
		}
	}
	if last := own.Then[len(own.Then)-1]; last.ForEach != "" && c.Answer == model.ChainAnswerLast {
		return fmt.Errorf("collector %q ends with the fan-out step %q under answer: last; a fan-out's answers are not one response, so set answer: all, or add a step after it", c.Name, last.Name)
	}
	c.Request.Own = own
	return nil
}

// checkChainBeside refuses what a chain does not work beside.
func checkChainBeside(c *model.Collector) error {
	own := httpRequest(c)
	switch {
	case own.Pagination != nil:
		return fmt.Errorf("collector %q sets request.pagination beside request.then; a chain's requests are not paged", c.Name)
	case c.Cache.Revalidate != nil && *c.Cache.Revalidate:
		return fmt.Errorf("collector %q sets cache.revalidate beside request.then; one answer's validators cannot speak for the chain's answers", c.Name)
	case len(c.Request.RedirectTrustedHosts) > 0:
		return fmt.Errorf("collector %q sets request.redirect_trusted_hosts beside request.then; every request of a chain, redirects included, stays on the target's origin", c.Name)
	}
	return nil
}

// checkStepName checks a step's name and notes it.
func checkStepName(c *model.Collector, name, at string, names map[string]bool) error {
	switch {
	case name == "":
		return fmt.Errorf("collector %q %s has no name; a step is named with letters, digits and _, such as last_build", c.Name, at)
	case name == model.ChainFirst:
		return fmt.Errorf("collector %q %s is named %q, the name of the collector's own request; choose another", c.Name, at, name)
	case !chainStepName.MatchString(name):
		return fmt.Errorf("collector %q %s is named %q; a step's name is lower-case letters, digits and _, and does not start with a digit", c.Name, at, name)
	case names[name]:
		return fmt.Errorf("collector %q has two steps named %q; a step's name is unique in the collector", c.Name, name)
	}
	names[name] = true
	return nil
}

// readChainValues checks the values a step reads, and notes them.
func readChainValues(c *model.Collector, step, at string, extract map[string]model.ChainExtract, values map[string]*chainValue) error {
	if extract == nil {
		return nil
	}
	if len(extract) == 0 || len(extract) > model.MaxChainValues {
		return fmt.Errorf("collector %q %s reads %d values; a step reads 1 to %d", c.Name, at, len(extract), model.MaxChainValues)
	}
	for _, name := range model.SortedKeys(extract) {
		if !chainNamePattern.MatchString(name) {
			return fmt.Errorf("collector %q %s names a value %q; a value's name is letters, digits and _, and does not start with a digit", c.Name, at, name)
		}
		if earlier, ok := values[name]; ok {
			return fmt.Errorf("collector %q %s reads the value %q, which step %q reads already; give it another name", c.Name, at, name, earlier.step)
		}
		e := extract[name]
		if err := checkLoginExtract(e.Place(), at+"."+name); err != nil {
			return fmt.Errorf("collector %q %w", c.Name, err)
		}
		values[name] = &chainValue{step: step, json: e.JSON != ""}
	}
	return nil
}

// checkFirstRequestRefs refuses a chain placeholder in the collector's own
// request, which no step has read a value for yet, and a placeholder of a
// reserved parameter.
func checkFirstRequestRefs(c *model.Collector) error {
	texts := map[string]string{"request.path": c.Request.Path}
	if _, body := methodBody(c); body != "" {
		texts["request.body"] = body
	}
	for name, value := range c.Request.Headers {
		texts["request.headers."+name] = value
	}
	for name, value := range c.Request.Query {
		texts["request.query."+name] = value
	}
	for _, where := range model.SortedKeys(texts) {
		_, refs, err := rewriteChainRefs(where, texts[where])
		if err != nil {
			return fmt.Errorf("collector %q %w", c.Name, err)
		}
		if len(refs) > 0 {
			return fmt.Errorf("collector %q %s has %s, but it is the chain's first request, which no step has read a value for; send values in the steps of request.then", c.Name, where, refText(refs[0]))
		}
		if strings.Contains(texts[where], "{{"+chainParamPrefix) {
			return fmt.Errorf("collector %q %s has a placeholder named %s…, a name the chain keeps for itself", c.Name, where, chainParamPrefix)
		}
	}
	return nil
}

// refText is how a chain placeholder is written.
func refText(r chainRef) string {
	switch r.Kind {
	case "chain":
		return "{{chain." + r.Name + "}}"
	case "each_field":
		return "{{each." + r.Name + "}}"
	}
	return "{{" + r.Kind + "}}"
}

// validateChainStep checks one step and fills in its defaults.
func validateChainStep(c *model.Collector, step *model.ChainStep, at string, values map[string]*chainValue) error {
	if err := checkStepRequest(c, step, at); err != nil {
		return err
	}
	if err := checkStepFanOut(c, step, at, values); err != nil {
		return err
	}
	fields, err := stepFields(*step, at)
	if err != nil {
		return fmt.Errorf("collector %q %w", c.Name, err)
	}
	for _, f := range fields {
		for _, ref := range f.refs {
			switch ref.Kind {
			case "chain":
				v, ok := values[ref.Name]
				if !ok {
					return fmt.Errorf("collector %q %s sends {{chain.%s}}, which no step before it reads; read it with extract in the first request or an earlier step", c.Name, f.where, ref.Name)
				}
				v.used = true
			default:
				if step.ForEach == "" {
					return fmt.Errorf("collector %q %s sends %s, which only a fan-out step has; set for_each, or send a value with {{chain.<name>}}", c.Name, f.where, refText(ref))
				}
			}
		}
		if strings.Contains(f.text, "{{"+chainParamPrefix) && strings.Count(f.text, "{{"+chainParamPrefix) > len(f.refs) {
			return fmt.Errorf("collector %q %s has a placeholder named %s…, a name the chain keeps for itself", c.Name, f.where, chainParamPrefix)
		}
		if err := f.check(); err != nil {
			return fmt.Errorf("collector %q %w", c.Name, err)
		}
	}
	return nil
}

// checkStepRequest checks what a step sends: its method, path or url,
// headers, query, body and accepted statuses.
func checkStepRequest(c *model.Collector, step *model.ChainStep, at string) error {
	bodies := 0
	for _, set := range []bool{step.Body != "", step.Form != nil, step.JSON != nil} {
		if set {
			bodies++
		}
	}
	if bodies > 1 {
		return fmt.Errorf("collector %q %s sets %d of body, form and json; a step sends at most one body", c.Name, at, bodies)
	}
	switch {
	case step.Method == "" && bodies > 0:
		step.Method = http.MethodPost
	case step.Method == "":
		step.Method = http.MethodGet
	}
	step.Method = strings.ToUpper(step.Method)
	if !slices.Contains(chainMethods, step.Method) {
		return fmt.Errorf("collector %q %s has the method %q; a step uses GET, HEAD, POST, PUT or PATCH", c.Name, at, step.Method)
	}
	if bodies > 0 && (step.Method == http.MethodGet || step.Method == http.MethodHead) {
		return fmt.Errorf("collector %q %s sends a body with %s; use POST, PUT or PATCH", c.Name, at, step.Method)
	}
	if step.URL != "" {
		if step.Path != "" {
			return fmt.Errorf("collector %q %s sets both path and url; a step is sent to one of them", c.Name, at)
		}
		if !chainURLPattern.MatchString(step.URL) {
			return fmt.Errorf("collector %q %s url is %q; a url is one placeholder of a link a step read, alone, such as \"{{chain.next}}\", and a path on the target is written with path", c.Name, at, step.URL)
		}
	}
	if err := checkStepPath(c, step.Path, at); err != nil {
		return err
	}
	if err := checkStepHeaders(c, step.Headers, at); err != nil {
		return err
	}
	if err := checkQueryNames(step.Query); err != nil {
		return fmt.Errorf("collector %q %s %w", c.Name, at, err)
	}
	for name := range step.Query {
		if strings.Contains(name, "{{") {
			return fmt.Errorf("collector %q %s query name %q has a placeholder; placeholders stand only in query values", c.Name, at, name)
		}
	}
	for _, form := range []map[string]string{step.Form, step.JSON} {
		for name := range form {
			if name == "" || strings.Contains(name, "{{") {
				return fmt.Errorf("collector %q %s has a form or json field named %q; a field's name is written as text, without a placeholder", c.Name, at, name)
			}
		}
	}
	if step.AcceptStatus != nil {
		if err := normalizeAcceptedStatuses(step.AcceptStatus); err != nil {
			return fmt.Errorf("collector %q %s.accept_status %w", c.Name, at, err)
		}
	}
	return nil
}

// checkStepPath refuses a path that names a scheme or a host: a step asks
// the target alone.
func checkStepPath(c *model.Collector, path, at string) error {
	if path == "" {
		return nil
	}
	parsed, err := url.Parse(path)
	if strings.HasPrefix(path, "//") || strings.Contains(path, "://") || err == nil && (parsed.Scheme != "" || parsed.Host != "") {
		return fmt.Errorf("collector %q %s path %q names a scheme or a host; a step's path is a path on the target, since a chain asks one target only", c.Name, at, path)
	}
	return nil
}

// checkStepHeaders checks a step's header names, and refuses one that
// carries the collector's credentials, which every step sends as they are.
func checkStepHeaders(c *model.Collector, headers map[string]string, at string) error {
	if err := checkHeaderNames(headers); err != nil {
		return fmt.Errorf("collector %q %s.headers %w", c.Name, at, err)
	}
	tokenHeader := ""
	if l := c.Request.Login; l != nil && l.Header != "" {
		tokenHeader = http.CanonicalHeaderKey(l.Header)
	}
	for name := range headers {
		if strings.Contains(name, "{{") {
			return fmt.Errorf("collector %q %s.headers name %q has a placeholder; placeholders stand only in header values", c.Name, at, name)
		}
		if err := checkHeaderValue(headers[name]); err != nil {
			return fmt.Errorf("collector %q %s.headers %s %w", c.Name, at, name, err)
		}
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "Authorization" || canonical == tokenHeader {
			return fmt.Errorf("collector %q %s.headers sets %s, which carries the collector's credentials; every step sends the collector's own", c.Name, at, canonical)
		}
	}
	return nil
}

// checkStepFanOut checks for_each and the keys of a fan-out, and fills in
// their defaults.
func checkStepFanOut(c *model.Collector, step *model.ChainStep, at string, values map[string]*chainValue) error {
	if step.ForEach == "" {
		if step.Concurrency != 0 || step.MaxItems != 0 || step.OnItemError != "" {
			return fmt.Errorf("collector %q %s sets concurrency, max_items or on_item_error without for_each; they apply to a fan-out step", c.Name, at)
		}
		return nil
	}
	v, ok := values[step.ForEach]
	switch {
	case !ok:
		return fmt.Errorf("collector %q %s for_each names %q, which no step before it reads; name a json value of an earlier step", c.Name, at, step.ForEach)
	case !v.json:
		return fmt.Errorf("collector %q %s for_each names %q, which is not a json value; a fan-out goes over a list a jq expression reads, such as {json: '[.jobs[].name]'}", c.Name, at, step.ForEach)
	}
	v.used = true
	switch {
	case step.Concurrency == 0:
		step.Concurrency = model.DefaultChainConcurrency
	case step.Concurrency < 1 || step.Concurrency > model.MaxChainConcurrency:
		return fmt.Errorf("collector %q %s concurrency is %d; want 1 to %d", c.Name, at, step.Concurrency, model.MaxChainConcurrency)
	}
	switch {
	case step.MaxItems == 0:
		step.MaxItems = model.DefaultChainMaxItems
	case step.MaxItems < 1 || step.MaxItems > model.MaxChainMaxItems:
		return fmt.Errorf("collector %q %s max_items is %d; want 1 to %d", c.Name, at, step.MaxItems, model.MaxChainMaxItems)
	}
	switch step.OnItemError {
	case "":
		step.OnItemError = model.ChainItemFail
	case model.ChainItemFail, model.ChainItemSkip:
	default:
		return fmt.Errorf("collector %q %s on_item_error is %q; want fail or skip", c.Name, at, step.OnItemError)
	}
	return nil
}

// stepField is a templated part of a step: a templateField as the request
// writes it, the placeholders rewritten, and the chain placeholders it had.
type stepField struct {
	templateField
	refs []chainRef
}

// check finds the field's placeholders, as its place finds them, and
// checks the default of each of the collector's own placeholders, as every
// templated field's are checked (templateField.check). A value of the
// chain is checked when it is sent.
func (f stepField) check() error {
	placeholders, err := f.parse()
	if f.kind == "path" || f.kind == "url" {
		placeholders, err = parsePlaceholders(f.where, f.text, true, false)
	}
	if err != nil {
		return err
	}
	for _, p := range placeholders {
		if !p.HasDefault || strings.HasPrefix(p.Name, chainParamPrefix) {
			continue
		}
		if _, err := f.write(p, p.Default); err != nil {
			return fmt.Errorf("%w; that value is the placeholder's default, so every probe that leaves %s out would fail — change the default", err, p.Name)
		}
	}
	return nil
}

// stepFields lists the parts of a step that may hold placeholders, each
// rewritten (rewriteChainRefs). The path's kind is "path" and the url's
// "url"; a form value's is "query" and a json value's "arg", each filled
// in as text, which the step's body then encodes (stepBody).
func stepFields(step model.ChainStep, at string) ([]stepField, error) {
	var out []stepField
	add := func(where, text, kind string) error {
		rewritten, refs, err := rewriteChainRefs(where, text)
		if err != nil {
			return err
		}
		if HasPathParams(rewritten) {
			out = append(out, stepField{templateField{where, rewritten, kind}, refs})
		}
		return nil
	}
	parts := []struct{ where, text, kind string }{
		{at + ".path", step.Path, "path"},
		{at + ".url", step.URL, "url"},
		{at + ".body", step.Body, "body"},
	}
	for _, name := range model.SortedKeys(step.Headers) {
		parts = append(parts, struct{ where, text, kind string }{at + ".headers." + name, step.Headers[name], "header"})
	}
	for _, name := range model.SortedKeys(step.Query) {
		parts = append(parts, struct{ where, text, kind string }{at + ".query." + name, step.Query[name], "query"})
	}
	for _, name := range model.SortedKeys(step.Form) {
		parts = append(parts, struct{ where, text, kind string }{at + ".form." + name, step.Form[name], "query"})
	}
	for _, name := range model.SortedKeys(step.JSON) {
		parts = append(parts, struct{ where, text, kind string }{at + ".json." + name, step.JSON[name], "arg"})
	}
	for _, p := range parts {
		if err := add(p.where, p.text, p.kind); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// chainTemplates are the templated parts of a collector's steps, for the
// checks every collector's templated parts get: which probe parameters the
// collector takes, which it was given and does not use, and whether it
// reads the clock (RequestType.Templates).
func chainTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	var out []templateField
	for i, step := range httpRequest(c).Then {
		fields, err := stepFields(step, fmt.Sprintf("request.then[%d]", i))
		if err != nil {
			continue // refused at load (validateChain)
		}
		for _, f := range fields {
			out = append(out, f.templateField)
		}
	}
	return out
}

// chainKeyRules is what the configuration schema says of request.extract
// and request.then. A value used before a step reads it, a value no step
// uses, and the origin of a path, the exporter alone refuses.
func chainKeyRules() map[string]map[string]any {
	oneOf := func(places ...string) []any {
		out := make([]any, len(places))
		for i, place := range places {
			out[i] = map[string]any{"required": []string{place}}
		}
		return out
	}
	extract := func(prefix, what string) map[string]map[string]any {
		return map[string]map[string]any{
			prefix:                 {"minProperties": 1, "maxProperties": model.MaxChainValues, "propertyNames": map[string]any{"pattern": chainNamePattern.String()}, "description": what},
			prefix + ".*":          {"oneOf": oneOf("regex", "json", "form", "header", "cookie")},
			prefix + ".*.regex":    {"description": "A regular expression on the body: its first group, or the whole match without one."},
			prefix + ".*.json":     {"description": "A jq expression on the body read as JSON, giving text, a number, or a list, which for_each goes over."},
			prefix + ".*.form":     {"description": "A field of the body read as a form."},
			prefix + ".*.header":   {"description": "A header of the answer."},
			prefix + ".*.cookie":   {"description": "The value of a cookie the answer sets."},
			prefix + ".*.optional": {"description": "A value not found, or empty, is the empty text instead of failing the step."},
			prefix + ".*.secret":   {"description": "The value is shown as *** wherever the exporter shows it: the debug report and the workflow diagram, a step's URL and headers, an error and a span."},
		}
	}
	rules := extract("extract", "http, with request.then: the values read from the first request's answer, by name, at most 16, each from exactly one of regex, json, form, header and cookie. A step sends one as {{chain.<name>}}. See docs/REQUESTS.md#chained-requests.")
	for key, rule := range extract("then[].extract", "The values read from the step's answer, as request.extract reads them.") {
		rules[key] = rule
	}
	steps := map[string]map[string]any{
		"then":           {"minItems": 1, "maxItems": model.MaxChainSteps, "description": "http: 1 to 12 more requests to the same target, made in order after the collector's own, each able to send values read from the answers before it. Every request goes to the target's origin, redirects included. Not with request.pagination, cache.revalidate or request.redirect_trusted_hosts. See docs/REQUESTS.md#chained-requests."},
		"then[]":         {"required": []string{"name"}, "allOf": []any{map[string]any{"not": map[string]any{"required": []string{"path", "url"}}}, map[string]any{"not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"body", "form"}}, map[string]any{"required": []string{"body", "json"}}, map[string]any{"required": []string{"form", "json"}}}}}, map[string]any{"if": map[string]any{"not": map[string]any{"required": []string{"for_each"}}}, "then": map[string]any{"not": map[string]any{"anyOf": oneOf("concurrency", "max_items", "on_item_error")}}}}},
		"then[].name":    {"pattern": model.ChainStepNamePattern, "not": map[string]any{"const": model.ChainFirst}, "description": "The step's name: lower-case letters, digits and _, unique in the collector, and not first, the collector's own request. It names the step in errors, metrics and the answer document."},
		"then[].method":  {"enum": SchemaOptionalEnum(chainMethodSpellings()), "description": "GET, HEAD, POST, PUT or PATCH. GET when left out, or POST when the step has a body."},
		"then[].path":    {"description": "A path on the target, joined to it as request.path is, with placeholders. It names no scheme and no host: a chain asks one target only. The collector's request.path when left out."},
		"then[].url":     {"pattern": SchemaOptionalPattern(chainURLPattern.String()), "description": "A link a step read, as one placeholder alone, such as \"{{chain.next}}\", sent in place of path. A relative link is resolved against the answer it came from; it must lead to the target's origin."},
		"then[].query":   {"propertyNames": SchemaQueryNames(), "description": "Query parameters of the step, with placeholders. A name is not empty and holds no control character."},
		"then[].headers": {"propertyNames": map[string]any{"allOf": []any{SchemaHeaderNames(), map[string]any{"not": map[string]any{"pattern": "^" + SchemaCaseless("Authorization") + "$"}}}}, "description": "Headers added to the collector's, each replacing one of the same name, with placeholders. A name is an HTTP token, not Accept-Encoding, and not Authorization nor the header a login sends its token in, which carry the collector's credentials."},
		"then[].body":    {"description": "The step's body, with placeholders, written as request.body is."},
		"then[].form":    {"description": "The step's body as a form, each field's value with placeholders."},
		"then[].json":    {"description": "The step's body as a JSON object of text, each value with placeholders."},
		"then[].accept_status": {
			"items":       map[string]any{"type": []string{"integer", "string"}, "minimum": 100, "maximum": 599, "pattern": "^[1-5][xX][xX]$|^[1-5][0-9][0-9]$|^" + AcceptAnyStatus + "$"},
			"anyOf":       []any{map[string]any{"maxItems": 1}, map[string]any{"items": map[string]any{"not": map[string]any{"const": AcceptAnyStatus}}}},
			"description": "The statuses that count as an answer for this step, written as request.accept_status is; the collector's when left out.",
		},
		"then[].forwarded_headers": {"description": "Whether the step is sent the forwarded headers: header_<name> probe parameters and those request.forward_headers lets through. true, the default, sends them; false sends none of them, but a forwarded Authorization (request.forward_authorization), the collector's credential, and a forwarded Host, which keeps the step on the first request's virtual host."},
		"then[].for_each":          {"description": "The name of a list value an earlier step read with json: one request per entry, with {{each}}, {{each.<field>}} and {{each_index}}. At most one step of a chain fans out."},
		"then[].concurrency":       {"minimum": 1, "maximum": model.MaxChainConcurrency, "description": "With for_each: how many of the step's requests run at once, 1 to 16, 4 when left out."},
		"then[].max_items":         {"minimum": 1, "maximum": model.MaxChainMaxItems, "description": "With for_each: how many entries are asked for, 1 to 1000, 100 when left out. The rest are left out, counted in http_exporter_chain_items_cut_total; the trip still succeeds."},
		"then[].on_item_error":     {"enum": SchemaOptionalEnum(model.ChainItemErrors), "description": "With for_each: fail, the default, fails the trip at an item's failure; skip keeps the item in the answer document as an error and goes on."},
	}
	for key, rule := range steps {
		rules[key] = rule
	}
	return rules
}

// chainMethodSpellings are the ways a step's method may be written: in
// upper case or lower case.
func chainMethodSpellings() []string {
	var out []string
	for _, m := range chainMethods {
		out = append(out, m, strings.ToLower(m))
	}
	return out
}
