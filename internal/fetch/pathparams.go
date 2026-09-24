package fetch

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
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

// PathParamPrefix is shared by placeholders and the probe parameters that fill
// them. A probe parameter without it can never be a path parameter, so the two
// namespaces cannot collide with target, collector, path, method and the rest.
const PathParamPrefix = "param_"

// PathParamName matches the name of a path parameter, PathParamPrefix
// followed by letters, digits and underscores.
var PathParamName = regexp.MustCompile(`^param_[A-Za-z0-9_]+$`)

// pathPlaceholder is one {{param_name}} or {{param_name:default}} in a path,
// or, in a request body, header or query value, the same with a |filter
// (requesttemplate.go).
type pathPlaceholder struct {
	Name       string
	Default    string
	HasDefault bool
	// Filter is how the value is written into a request body; empty is raw.
	Filter     string
	start, end int // byte offsets of the whole placeholder, braces included
}

// HasPathParams is the cheap test every caller makes first, so a path with no
// placeholders takes exactly the code path it always did.
func HasPathParams(path string) bool {
	return strings.Contains(path, "{{")
}

// parsePathParams finds the placeholders in a path. The error names the
// problem precisely because it is reported at startup, against a file someone
// has to go and edit. In a path, `{{` always opens a placeholder.
func parsePathParams(path string) ([]pathPlaceholder, error) {
	return parsePlaceholders("request.path", path, true, false)
}

// pathParamValues reads the path parameters from a probe's query string. A
// parameter given twice is an error rather than first-wins: which of two
// tenants a scrape was meant for is not something to guess.
func pathParamValues(values url.Values) (map[string]string, error) {
	var out map[string]string
	for key, given := range values {
		if !strings.HasPrefix(key, PathParamPrefix) {
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
		value, err := placeholderValue("request.path", p, params)
		if err != nil {
			return "", nil, err
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

// CheckPathParams validates a probe's path parameters against the collector
// before anything is fetched, so a missing value is the caller's 400 rather
// than a failed scrape of the target.
//
// A parameter the path does not use is rejected too. It is almost always a
// misspelling — param_tenat for param_tenant — and when the placeholder has a
// default, the misspelled scrape would otherwise succeed against the default
// tenant and report its numbers as the intended one's.
func CheckPathParams(c *model.Collector, overrides RequestOverrides) error {
	unused, err := CheckRequestParams(c, overrides)
	if err != nil || len(unused) == 0 {
		return err
	}
	if overrides.PathSet {
		return fmt.Errorf("probe parameters %s are not used: the path probe parameter replaces request.path, and nothing else in the request names them", strings.Join(unused, ", "))
	}
	return fmt.Errorf("probe parameters %s are not used by collector %q: no placeholder in its request.path (%q), body, header or query values names them", strings.Join(unused, ", "), c.Name, c.Request.Path)
}

// requestLabel is the URL a verbose self-metric carries for a request. Path
// parameters stay as their placeholders: the value is exactly the kind of thing
// requestLabelURL keeps out of labels already — a tenant, an account — and one
// series per value would be unbounded besides.
func requestLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	u, err := buildRequestURL(target, c, overrides, false)
	if err != nil {
		return "", err
	}
	label := requestLabelURL(u)
	if !overrides.PathSet && HasPathParams(c.Request.Path) {
		label = strings.NewReplacer("%7B%7B", "{{", "%7D%7D", "}}").Replace(label)
	}
	return label, nil
}
