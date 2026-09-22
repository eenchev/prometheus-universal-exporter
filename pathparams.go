package main

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Path parameters let one collector serve targets whose paths differ by a
// value only the scrape knows — a tenant, a region, an API version:
//
//	request:
//	  path: /api/{{param_tenant}}/v{{param_version:2}}/status
//
// probed with /probe?target=...&collector=...&param_tenant=acme requests
// /api/acme/v2/status. The placeholder is named exactly like the probe
// parameter that fills it, so a configuration and the monitor that scrapes it
// can be read side by side.
//
// The syntax is chosen not to meet ${NAME}, the environment reference
// --config.export-env substitutes. Environment references are expanded once,
// textually, when the file is read; path parameters are bound on every probe.
// The two compose: {{param_tenant:${DEFAULT_TENANT}}} takes its default from the
// environment and its value from the probe.
//
// `{{` always opens a placeholder in request.path. A path never needs literal
// double braces, and treating them as text would send an unfilled placeholder
// to the target — so anything after `{{` that is not a well-formed placeholder
// is rejected when the configuration is loaded.

// pathParamPrefix is shared by placeholders and the probe parameters that fill
// them. A probe parameter without it can never be a path parameter, so the two
// namespaces cannot collide with target, collector, path, method and the rest.
const pathParamPrefix = "param_"

var pathParamName = regexp.MustCompile(`^param_[A-Za-z0-9_]+$`)

// pathPlaceholder is one {{param_name}} or {{param_name:default}} in a path.
type pathPlaceholder struct {
	Name       string
	Default    string
	HasDefault bool
	start, end int // byte offsets of the whole placeholder, braces included
}

// hasPathParams is the cheap test every caller makes first, so a path with no
// placeholders takes exactly the code path it always did.
func hasPathParams(path string) bool {
	return strings.Contains(path, "{{")
}

// parsePathParams finds the placeholders in a path. The error names the
// problem precisely because it is reported at startup, against a file someone
// has to go and edit.
func parsePathParams(path string) ([]pathPlaceholder, error) {
	var out []pathPlaceholder
	for offset := 0; ; {
		open := strings.Index(path[offset:], "{{")
		if open < 0 {
			return out, nil
		}
		open += offset
		closing := strings.Index(path[open+2:], "}}")
		if closing < 0 {
			return nil, fmt.Errorf("request.path has an unclosed placeholder at %q; write {{param_name}} or {{param_name:default}}", path[open:])
		}
		closing += open + 2
		inner := path[open+2 : closing]
		name, def, hasDefault := strings.Cut(inner, ":")
		if !pathParamName.MatchString(name) {
			return nil, fmt.Errorf("request.path placeholder {{%s}} is not a path parameter; placeholders are named %s<name>, with letters, digits and underscores, e.g. {{param_tenant}}", inner, pathParamPrefix)
		}
		// A default ends at the first }}, so braces inside it can only come
		// from something the author did not mean as a default. The common case
		// is an environment reference left unexpanded because
		// --config.export-env is off: {{param_x:${X}}} would otherwise bind the
		// default "${X" and leave a stray brace in the path.
		if strings.ContainsAny(def, "{}") {
			return nil, fmt.Errorf("request.path placeholder {{%s}} has a default containing a brace; if it is an environment reference, run with --config.export-env so it is expanded first", inner)
		}
		out = append(out, pathPlaceholder{Name: name, Default: def, HasDefault: hasDefault, start: open, end: closing + 2})
		offset = closing + 2
	}
}

// pathParamValues reads the path parameters from a probe's query string. A
// parameter given twice is an error rather than first-wins: which of two
// tenants a scrape was meant for is not something to guess.
func pathParamValues(values url.Values) (map[string]string, error) {
	var out map[string]string
	for key, given := range values {
		if !strings.HasPrefix(key, pathParamPrefix) {
			continue
		}
		if len(given) > 1 {
			return nil, fmt.Errorf("probe parameter %s is given %d times; give it once", key, len(given))
		}
		if out == nil {
			out = map[string]string{}
		}
		value := ""
		if len(given) == 1 {
			value = given[0]
		}
		out[key] = value
	}
	return out, nil
}

// bindPathParams resolves the placeholders in a path against the probe's
// parameters. It returns the path with each placeholder replaced by an opaque
// token, and the value for each token in order. The tokens survive path.Join,
// which would otherwise clean a value of ".." into a different path, and let
// applyPathParams escape each value as a single segment afterwards.
//
// A value comes from the probe, else from the default, else it is an error: a
// request sent with a placeholder left unfilled would reach a path nobody
// configured. An empty probe value counts as not given, so an empty monitor
// parameter falls back to the default instead of producing "//".
func bindPathParams(path string, params map[string]string) (string, []string, error) {
	placeholders, err := parsePathParams(path)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	values := make([]string, 0, len(placeholders))
	previous := 0
	for i, p := range placeholders {
		value, given := params[p.Name]
		if !given || value == "" {
			if !p.HasDefault {
				return "", nil, fmt.Errorf("request.path needs %s, which the probe did not supply and which has no default; add &%s=<value> to the probe, or give it a default as {{%s:<default>}}", p.Name, p.Name, p.Name)
			}
			value = p.Default
		}
		// "." and ".." are the two values escaping cannot make safe: both are
		// legal in a path segment, and a server resolving them would serve a
		// different path than the one configured.
		if value == "." || value == ".." {
			return "", nil, fmt.Errorf("path parameter %s must not be %q", p.Name, value)
		}
		b.WriteString(path[previous:p.start])
		b.WriteString(pathToken(i))
		values = append(values, value)
		previous = p.end
	}
	b.WriteString(path[previous:])
	return b.String(), values, nil
}

// pathToken marks where a bound value goes. NUL cannot occur in a configured
// path, and path.Join leaves it alone.
func pathToken(i int) string {
	return "\x00" + strconv.Itoa(i) + "\x00"
}

// applyPathParams substitutes the bound values into a URL whose path still
// carries the tokens. Each value is escaped as one path segment — a "/" in it
// becomes %2F rather than a new segment — and the URL keeps both forms, so Go
// sends the escaped one while Path still reads as the value.
func applyPathParams(u *url.URL, values []string) {
	u.RawPath = ""
	decoded := u.Path
	raw := u.EscapedPath()
	for i, value := range values {
		token := pathToken(i)
		decoded = strings.Replace(decoded, token, value, 1)
		raw = strings.Replace(raw, url.PathEscape(token), url.PathEscape(value), 1)
	}
	u.Path = decoded
	u.RawPath = raw
}

// checkPathParams validates a probe's path parameters against the collector
// before anything is fetched, so a missing value is the caller's 400 rather
// than a failed scrape of the target.
//
// A parameter the path does not use is rejected too. It is almost always a
// misspelling — param_tenat for param_tenant — and when the placeholder has a
// default, the misspelled scrape would otherwise succeed against the default
// tenant and report its numbers as the intended one's.
func checkPathParams(c *Collector, overrides RequestOverrides) error {
	used := map[string]bool{}
	if !overrides.PathSet && hasPathParams(c.Request.Path) {
		placeholders, err := parsePathParams(c.Request.Path)
		if err != nil {
			return err
		}
		for _, p := range placeholders {
			used[p.Name] = true
		}
		if _, _, err := bindPathParams(c.Request.Path, overrides.Params); err != nil {
			return err
		}
	}
	var unused []string
	for name := range overrides.Params {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	if len(unused) == 0 {
		return nil
	}
	sort.Strings(unused)
	if overrides.PathSet {
		return fmt.Errorf("probe parameters %s are not used: the path probe parameter replaces request.path, and path parameters are only bound in request.path", strings.Join(unused, ", "))
	}
	return fmt.Errorf("probe parameters %s are not used by collector %q, whose request.path is %q", strings.Join(unused, ", "), c.Name, c.Request.Path)
}

// requestLabel is the URL a verbose self-metric carries for a request. Path
// parameters stay as their placeholders: the value is exactly the kind of thing
// requestLabelURL keeps out of labels already — a tenant, an account — and one
// series per value would be unbounded besides.
func requestLabel(target string, c *Collector, overrides RequestOverrides) (string, error) {
	u, err := buildRequestURL(target, c, overrides, false)
	if err != nil {
		return "", err
	}
	label := requestLabelURL(u)
	if !overrides.PathSet && hasPathParams(c.Request.Path) {
		label = strings.NewReplacer("%7B%7B", "{{", "%7D%7D", "}}").Replace(label)
	}
	return label, nil
}
