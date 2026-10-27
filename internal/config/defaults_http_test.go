//go:build !select_request_types || (request_type_http && request_type_dns)

package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The defaults block of a file (defaults.go): what the collectors of that
// file take for the keys they do not write.

// defaultsCollector is a collector of the request type, http or dns, the
// latter in any case, with more written under it, each line indented as a
// key of the collector.
func defaultsCollector(name, requestType string, more ...string) string {
	var b strings.Builder
	b.WriteString("  - name: " + name + "\n")
	switch strings.ToLower(requestType) {
	case "http":
		b.WriteString("    request: {type: http, path: /" + name + "}\n    transform: {type: regex}\n    metrics:\n      - name: " + name + "_value\n        expression: 'value=(\\d+)'\n")
	case "dns":
		b.WriteString("    request: {type: " + requestType + "}\n    transform: {type: jq}\n    metrics:\n      - name: " + name + "_value\n        expression: .answers | length\n")
	}
	for _, line := range more {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// collectorNamed is the collector of c with the name.
func collectorNamed(t *testing.T, c *model.Config, name string) *model.Collector {
	t.Helper()
	for i := range c.Collectors {
		if c.Collectors[i].Name == name {
			return &c.Collectors[i]
		}
	}
	t.Fatalf("no collector %q in %v", name, testutil.CollectorNames(c))
	return nil
}

// A collector takes every key of defaults it does not write, key by key at
// every depth, and a list whole: one that writes limits.max_metrics keeps
// the defaults' other limits, and one that writes allowed_targets keeps its
// own list alone.
func TestDefaultsApplyKeyByKey(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", `defaults:
  request:
    retry: {attempts: 2, backoff: 3s}
    allowed_targets: [example.com, example.org]
    tls: {insecure_skip_verify: true}
    max_response_bytes: 2MiB
  limits: {max_metrics: 500, max_labels_per_metric: 7}
  error_handling: {on_fetch_error: log, allow_missing_keys: true}
  failure_backoff: {initial: 1m, after: 4}
  cache: {ttl: 30s}
collectors:
`+defaultsCollector("plain", "http")+defaultsCollector("own", "http",
		"limits: {max_metrics: 100}",
		"error_handling: {on_decode_error: ignore, on_fetch_error: ''}",
		"failure_backoff: {after: 9}",
		"cache: {ttl: 0s}",
	)+`  - name: own_request
    request:
      type: http
      path: /x
      allowed_targets: [example.net]
      retry: {attempts: 5}
      tls: {server_name: target.example}
    transform: {type: regex}
    metrics:
      - name: own_request_value
        expression: 'value=(\d+)'
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	plain := collectorNamed(t, c, "plain")
	if plain.Limits.MaxMetrics != 500 || plain.Limits.MaxLabelsPerMetric != 7 || plain.Limits.MaxHelpLength != 2000 {
		t.Errorf("plain limits = %+v, want the defaults' and the default of the rest", plain.Limits)
	}
	if plain.Request.Retry.Attempts != 2 || plain.Request.Retry.Backoff != model.Duration(3*time.Second) || !slices.Equal(plain.Request.AllowedTargets, []string{"example.com", "example.org"}) || plain.Request.MaxResponseBytes != 2<<20 {
		t.Errorf("plain request = %+v", plain.Request)
	}
	if !plain.Request.TLS.Set || !plain.Request.TLS.InsecureSkipVerify {
		t.Errorf("plain tls = %+v, want the defaults' tls, written", plain.Request.TLS)
	}
	if plain.ErrorHandling != (model.ErrorHandling{OnFetchError: "log", OnDecodeError: "fail", OnTransformError: "fail", AllowMissingKeys: true}) {
		t.Errorf("plain error_handling = %+v", plain.ErrorHandling)
	}
	if b := plain.FailureBackoff; b == nil || *b.After != 4 || *b.Initial != model.Duration(time.Minute) {
		t.Errorf("plain failure_backoff = %+v", b)
	}
	if plain.Cache.TTL != model.Duration(30*time.Second) {
		t.Errorf("plain cache = %+v", plain.Cache)
	}

	own := collectorNamed(t, c, "own")
	if own.Limits.MaxMetrics != 100 || own.Limits.MaxLabelsPerMetric != 7 {
		t.Errorf("own limits = %+v, want its max_metrics and the defaults' max_labels_per_metric", own.Limits)
	}
	if own.ErrorHandling.OnDecodeError != "ignore" || own.ErrorHandling.OnFetchError != "log" {
		t.Errorf("own error_handling = %+v", own.ErrorHandling)
	}
	if b := own.FailureBackoff; b == nil || *b.After != 9 || *b.Initial != model.Duration(time.Minute) {
		t.Errorf("own failure_backoff = %+v, want its after and the defaults' initial", b)
	}
	// A key written with the zero value is written: cache.ttl 0s answers
	// every probe from the target. One written "" is left out, as
	// on_fetch_error above, which takes the defaults' log.
	if own.Cache.TTL != 0 {
		t.Errorf("own cache = %+v, want its own ttl 0s", own.Cache)
	}

	ownRequest := collectorNamed(t, c, "own_request")
	if !slices.Equal(ownRequest.Request.AllowedTargets, []string{"example.net"}) {
		t.Errorf("allowed_targets = %v, want the collector's list alone", ownRequest.Request.AllowedTargets)
	}
	if ownRequest.Request.Retry.Attempts != 5 || ownRequest.Request.Retry.Backoff != model.Duration(3*time.Second) {
		t.Errorf("retry = %+v, want its attempts and the defaults' backoff", ownRequest.Request.Retry)
	}
	if tls := ownRequest.Request.TLS; tls.ServerName != "target.example" || !tls.InsecureSkipVerify {
		t.Errorf("tls = %+v, want its server_name and the defaults' insecure_skip_verify", tls)
	}

	// What each took is recorded, for the errors and the dry run.
	want := []string{"request.retry.backoff", "request.tls.insecure_skip_verify", "request.max_response_bytes", "limits.max_metrics", "limits.max_labels_per_metric", "error_handling.on_fetch_error", "error_handling.allow_missing_keys", "failure_backoff", "failure_backoff.initial", "failure_backoff.after", "cache.ttl"}
	if !slices.Equal(ownRequest.FromDefaults, want) {
		t.Errorf("own_request took %q, want %q", ownRequest.FromDefaults, want)
	}
}

// The defaults of a file are its own collectors': the configuration's do not
// reach the collectors of a collector file, nor a collector file's those of
// the configuration or of another collector file.
func TestDefaultsApplyToTheCollectorsOfTheirFileAlone(t *testing.T) {
	dir := t.TempDir()
	team := testutil.WriteIn(t, dir, "team.yaml", "defaults:\n  limits: {max_metrics: 300}\ncollectors:\n"+defaultsCollector("team", "http"))
	testutil.WriteIn(t, dir, "other.yaml", "collectors:\n"+defaultsCollector("other", "http"))
	c, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [team.yaml, other.yaml]\ndefaults:\n  limits: {max_labels_per_metric: 5}\ncollectors:\n"+defaultsCollector("own", "http")))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]model.Limits{
		"own":   {MaxMetrics: 10000, MaxLabelsPerMetric: 5},
		"team":  {MaxMetrics: 300, MaxLabelsPerMetric: 20},
		"other": {MaxMetrics: 10000, MaxLabelsPerMetric: 20},
	} {
		got := collectorNamed(t, c, name).Limits
		if got.MaxMetrics != want.MaxMetrics || got.MaxLabelsPerMetric != want.MaxLabelsPerMetric {
			t.Errorf("%s: max_metrics %d, max_labels_per_metric %d; want %d and %d", name, got.MaxMetrics, got.MaxLabelsPerMetric, want.MaxMetrics, want.MaxLabelsPerMetric)
		}
	}
	if c.CollectorSources["team"] != team {
		t.Errorf("sources = %v", c.CollectorSources)
	}
}

// A key defaults cannot set is refused naming it and its line, whether it
// is a key of a collector or of nothing, in the configuration and in a
// collector file, which the error names.
func TestDefaultsRefuseTheKeysTheyCannotSet(t *testing.T) {
	collectors := "collectors:\n" + defaultsCollector("a", "http")
	for name, tc := range map[string]struct{ defaults, want string }{
		"a collector key":         {"defaults:\n  metrics: []\n", `line 2: defaults cannot set metrics; defaults set the keys of request.tls, request.retry, request.max_response_bytes, request.allowed_targets, request.denied_targets, limits, error_handling, failure_backoff and cache`},
		"an unknown key":          {"defaults:\n  limitz: {}\n", `line 2: unknown key "limitz" in defaults; defaults set the keys of`},
		"a request key":           {"defaults:\n  request: {path: /x}\n", "line 2: defaults cannot set request.path;"},
		"the request type":        {"defaults:\n  request:\n    type: http\n", "line 3: defaults cannot set request.type;"},
		"a key of a type's own":   {"defaults:\n  request:\n    record_type: A\n", "line 3: defaults cannot set request.record_type;"},
		"an unknown request key":  {"defaults:\n  request:\n    retri: {}\n", `line 3: unknown key "request.retri" in defaults`},
		"an unknown limit":        {"defaults:\n  limits: {max_metricz: 1}\n", `line 2: unknown key "max_metricz" in limits`},
		"an unknown retry key":    {"defaults:\n  request:\n    retry: {attempt: 1}\n", `line 3: unknown key "attempt" in retry`},
		"an unknown cache key":    {"defaults:\n  cache: {tll: 1m}\n", `line 2: cache has the unknown key "tll"`},
		"an unknown backoff key":  {"defaults:\n  failure_backoff: {start: 1m}\n", `line 2: failure_backoff has the unknown key "start"`},
		"a value of another kind": {"defaults:\n  limits: {max_metrics: many}\n", "line 2: expected a whole number"},
		"not a mapping":           {"defaults: 5\n", "line 1: defaults must be a mapping"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(testutil.WriteFile(t, "config.yaml", tc.defaults+collectors))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("configuration: err = %v, want %q", err, tc.want)
			}
			dir := t.TempDir()
			file := testutil.WriteIn(t, dir, "team.yaml", tc.defaults+collectors)
			_, err = Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [team.yaml]\n"))
			if err == nil || !strings.Contains(err.Error(), "collector file "+file+": "+tc.want) {
				t.Errorf("collector file: err = %v, want %q naming the file", err, tc.want)
			}
		})
	}
}

// A value of defaults that is wrong whatever collector takes it is refused
// once, naming the block's key, rather than once for each collector, and
// the collector file it is in.
func TestDefaultsRefuseABadValueNamingTheBlock(t *testing.T) {
	collectors := "collectors:\n" + defaultsCollector("a", "http") + defaultsCollector("b", "http")
	for name, tc := range map[string]struct{ defaults, want string }{
		"a negative limit":   {"limits: {max_metrics: -1}", "defaults.limits.max_metrics is -1, and a limit must not be negative"},
		"a small response":   {"request: {max_response_bytes: 10}", "defaults.request.max_response_bytes is 10 bytes, under the least, 1KiB"},
		"an error policy":    {"error_handling: {on_fetch_error: panic}", `defaults.error_handling.on_fetch_error has invalid value "panic"; want fail, log or ignore`},
		"retry attempts":     {"request: {retry: {attempts: 11}}", "defaults.request.retry.attempts must be from 0 to 10"},
		"a backoff":          {"failure_backoff: {initial: 0s}", "defaults.failure_backoff.initial must be a positive duration"},
		"a backoff's max":    {"failure_backoff: {initial: 2m, max: 1m}", "defaults.failure_backoff.max 1m0s must not be shorter than failure_backoff.initial 2m0s"},
		"a negative ttl":     {"cache: {ttl: -0s, stale_if_error: 1m}\n  limits: {script_timeout: 10us}", "defaults.limits.script_timeout is 10µs, under the least, 1ms"},
		"a script's memory":  {"limits: {max_script_memory: 1MiB}", "defaults.limits.max_script_memory must be 0, for no limit, or at least 32MiB"},
		"two bad values too": {"limits: {max_metrics: -1}\n  error_handling: {on_decode_error: maybe}", "defaults.limits.max_metrics is -1, and a limit must not be negative; leave it out, or 0, for the default\ndefaults.error_handling.on_decode_error has invalid value \"maybe\""},
	} {
		t.Run(name, func(t *testing.T) {
			document := "defaults:\n  " + tc.defaults + "\n" + collectors
			_, err := Load(testutil.WriteFile(t, "config.yaml", document))
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || strings.Contains(err.Error(), `collector "`) {
				t.Errorf("configuration: err = %v, want %q, once", err, tc.want)
			}
			dir := t.TempDir()
			file := testutil.WriteIn(t, dir, "team.yaml", document)
			_, err = Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [team.yaml]\n"))
			first, _, _ := strings.Cut(tc.want, "\n")
			if err == nil || !strings.HasPrefix(err.Error(), "collector file "+file+": "+first) || strings.Contains(err.Error(), `collector "`) {
				t.Errorf("collector file: err = %v, want %q naming the file", err, first)
			}
		})
	}
}

// A request key of defaults applies to the collectors whose request type
// takes it, so collectors of several types share a file; one that applies
// to no collector of its file, for their types or because each sets it
// itself, is a setting nothing uses, and refused. So is a defaults block
// in a configuration file without collectors of its own: a collector file
// has a block of its own.
func TestADefaultRequestKeyAppliesToTheTypesThatTakeIt(t *testing.T) {
	c, err := Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request:\n    retry: {attempts: 3, statuses: [503]}\n    tls: {insecure_skip_verify: true}\n    allowed_targets: [example.com]\ncollectors:\n"+defaultsCollector("web", "http")+defaultsCollector("names", "dns")))
	if err != nil {
		t.Fatal(err)
	}
	web, names := collectorNamed(t, c, "web"), collectorNamed(t, c, "names")
	if web.Request.Retry.Attempts != 3 || !slices.Equal(web.Request.Retry.Statuses, []string{"503"}) || !web.Request.TLS.Set || !slices.Equal(web.Request.AllowedTargets, []string{"example.com"}) {
		t.Errorf("web request = %+v", web.Request)
	}
	if names.Request.Retry.Attempts != 0 || names.Request.TLS.Set || !slices.Equal(names.Request.AllowedTargets, []string{"example.com"}) {
		t.Errorf("names request = %+v, want allowed_targets alone", names.Request)
	}
	if want := []string{"request.allowed_targets"}; !slices.Equal(names.FromDefaults, want) {
		t.Errorf("names took %q, want %q", names.FromDefaults, want)
	}

	for name, tc := range map[string]struct{ document, want string }{
		"no type takes it": {"defaults:\n  request:\n    retry: {attempts: 3}\n    allowed_targets: [example.com]\ncollectors:\n" + defaultsCollector("names", "dns") + defaultsCollector("more", "DNS"),
			`defaults.request.retry.attempts applies to no collector of the file: request.retry.attempts does not apply to request.type "dns" of its collectors; remove it from defaults`},
		"a retry key of another type": {"defaults:\n  request:\n    retry: {codes: [ABORTED]}\ncollectors:\n" + defaultsCollector("web", "http") + defaultsCollector("names", "dns"),
			`defaults.request.retry.codes applies to no collector of the file: request.retry.codes does not apply to request.type "http" or "dns" of its collectors`},
		"each sets it": {"defaults:\n  limits: {max_metrics: 5, max_help_length: 9}\ncollectors:\n" + defaultsCollector("a", "http", "limits: {max_metrics: 6}") + defaultsCollector("b", "dns", "limits: {max_metrics: 7}"),
			"defaults.limits.max_metrics applies to no collector of the file: each collector it would apply to sets limits.max_metrics itself; remove it from defaults"},
		"each that takes it sets it": {"defaults:\n  request: {retry: {attempts: 1}}\ncollectors:\n" + defaultsCollector("names", "dns") + "  - name: web\n    request: {type: http, retry: {attempts: 2}}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n",
			"defaults.request.retry.attempts applies to no collector of the file: each collector it would apply to sets request.retry.attempts itself"},
		"no collectors of its own": {"defaults:\n  limits: {max_metrics: 5}\ncollector_files: [team.yaml]\n",
			"defaults apply to the collectors of their own file, and the configuration file defines none; a collector file takes a defaults block of its own"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.WriteIn(t, dir, "team.yaml", "collectors:\n"+defaultsCollector("team", "http"))
			_, err := Load(testutil.WriteIn(t, dir, "config.yaml", tc.document))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// A block written whole by a collector, tls here, takes the defaults'
	// keys of it all the same: request.tls is not refused for being
	// written by each, since its keys apply.
	if _, err := Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request: {tls: {insecure_skip_verify: true}}\ncollectors:\n  - name: web\n    request: {type: http, tls: {server_name: x.example}}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n")); err != nil {
		t.Error(err)
	}
}

// A problem validation finds with a collector in a value it took from
// defaults says so, since the value to change is written there.
func TestAProblemWithADefaultedValueSaysWhereItIsWritten(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  failure_backoff: {initial: 20m}\n  limits: {max_metrics: 10}\ncollectors:\n"+defaultsCollector("a", "http", "failure_backoff: {max: 5m}")))
	want := `collector "a" failure_backoff.max 5m0s must not be shorter than failure_backoff.initial 20m0s (failure_backoff.initial comes from the defaults of the file)`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	// A problem that names no key taken from defaults is left as it is.
	_, err = Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  limits: {max_metrics: 10}\ncollectors:\n"+defaultsCollector("a", "http", "failure_backoff: {initial: 20m, max: 5m}")))
	if want := `collector "a" failure_backoff.max 5m0s must not be shorter than failure_backoff.initial 20m0s`; err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	if !namesKey("request.tls.ca_file is missing.", "request.tls.ca_file") || namesKey("request.tls.ca_file is missing", "request.tls") || namesKey("limits.max_response_bytes", "request.max_response_bytes") {
		t.Error("namesKey does not name a key as a whole")
	}
}

// The request keys a collector takes from defaults come after its own in
// its request block's order, in the order the block has them, so of the
// keys the validation refuses the first the collector writes is named,
// whichever load it is.
func TestDefaultedRequestKeysComeAfterTheCollectorsOwn(t *testing.T) {
	document := "defaults:\n  request:\n    denied_targets: [bad.example]\n    retry: {backoff: 1s, attempts: 1}\ncollectors:\n" +
		"  - name: a\n    request:\n      path: /a\n      type: http\n      allowed_targets: [example.com]\n    transform: {type: regex}\n    metrics: [{name: v, expression: 'v=(\\d+)'}]\n"
	c, err := Load(testutil.WriteFile(t, "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"path", "type", "allowed_targets", "denied_targets", "retry", "retry.backoff", "retry.attempts"}; !slices.Equal(c.Collectors[0].Request.Order, want) {
		t.Errorf("order = %q, want %q", c.Collectors[0].Request.Order, want)
	}
	refused := strings.Replace(strings.Replace(document, "path: /a", "record_type: A", 1), "allowed_targets: [example.com]", "rpc: x.Y/Z", 1)
	for range 5 {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", refused)); err == nil || !strings.Contains(err.Error(), `collector "a" sets request.record_type, which does not apply to request.type "http"`) {
			t.Fatalf("err = %v, want record_type named, the first in the file", err)
		}
	}
}

// A reload compares collectors with their defaults: moving a value from the
// collectors into defaults changes none, and changing a default changes each
// collector that takes it, and no other.
func TestAReloadComparesCollectorsWithTheirDefaults(t *testing.T) {
	load := func(document string) *model.Config {
		t.Helper()
		c, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// collector is an http collector with its request's other keys and
	// more keys of its own.
	collector := func(name, request string, more ...string) string {
		var b strings.Builder
		b.WriteString("  - name: " + name + "\n    request: {type: http, path: /" + name + request + "}\n    transform: {type: regex}\n    metrics: [{name: " + name + "_value, expression: 'value=(\\d+)'}]\n")
		for _, line := range more {
			b.WriteString("    " + line + "\n")
		}
		return b.String()
	}
	shared := ", tls: {insecure_skip_verify: true}, retry: {attempts: 2}"
	written := load("collectors:\n" +
		collector("a", shared, "limits: {max_metrics: 50}", "failure_backoff: {initial: 1m}") +
		collector("b", shared, "limits: {max_metrics: 50}", "failure_backoff: {initial: 1m}") +
		collector("c", shared, "limits: {max_metrics: 70}", "failure_backoff: {initial: 1m}"))
	defaults := "defaults:\n  request: {tls: {insecure_skip_verify: true}, retry: {attempts: 2}}\n  limits: {max_metrics: 50}\n  failure_backoff: {initial: 1m}\n"
	moved := defaults + "collectors:\n" + collector("a", "") + collector("b", "") + collector("c", "", "limits: {max_metrics: 70}")
	if changes := collectorChanges(written, load(moved), nil); len(changes.added)+len(changes.removed)+len(changes.changed) != 0 {
		t.Errorf("moving values into defaults changed %+v", changes)
	}
	for i := range written.Collectors {
		if encodedDefinition(written, i) != encodedDefinition(load(moved), i) {
			t.Errorf("collector %s is written as YAML differently with its values in defaults:\n%s\n%s", written.Collectors[i].Name, encodedDefinition(written, i), encodedDefinition(load(moved), i))
		}
	}
	changes := collectorChanges(load(moved), load(strings.Replace(moved, "max_metrics: 50", "max_metrics: 60", 1)), nil)
	if len(changes.added)+len(changes.removed) != 0 || !slices.Equal(changes.changed, []string{"a", "b"}) {
		t.Errorf("changing a default changed %+v, want a and b, which take it", changes)
	}
	changes = collectorChanges(load(moved), load(strings.Replace(moved, "attempts: 2", "attempts: 3", 1)), nil)
	if !slices.Equal(changes.changed, []string{"a", "b", "c"}) {
		t.Errorf("changing a default request key changed %+v, want every collector", changes)
	}
}

// A ${NAME} in defaults is expanded with the configuration's, as anywhere
// in the file.
func TestDefaultsAreExpandedFromTheEnvironment(t *testing.T) {
	t.Setenv("PUE_TEST_DEFAULT_TARGET", "env.example")
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "team.yaml", "defaults:\n  request: {allowed_targets: ['${PUE_TEST_DEFAULT_TARGET}']}\ncollectors:\n"+defaultsCollector("team", "http"))
	c, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [team.yaml]\n"), WithEnvExpansion())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Collectors[0].Request.AllowedTargets; !slices.Equal(got, []string{"env.example"}) {
		t.Errorf("allowed_targets = %v", got)
	}
	// Without expansion, the reference is text, which is no host; the
	// error says the value is written in defaults.
	_, err = Load(filepath.Join(dir, "config.yaml"))
	if want := `request.allowed_targets entry "${PUE_TEST_DEFAULT_TARGET}" is not a host name`; err == nil || !strings.Contains(err.Error(), want) || !strings.HasSuffix(err.Error(), "(request.allowed_targets comes from the defaults of the file)") {
		t.Errorf("without expansion: err = %v, want %q, from defaults", err, want)
	}
}

// The debug page reads one collector, which has no defaults: the key is
// one of a file, as collectors is.
func TestADebugPageCollectorHasNoDefaults(t *testing.T) {
	_, err := ParseCollector([]byte("defaults: {limits: {max_metrics: 5}}\nname: a\n"), nil)
	if want := `line 1: "defaults" is a key of the configuration, not of a collector`; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// A key a merge key brings into a collector is written by the collector, and
// one it brings into defaults is the block's, as the decoder reads both.
func TestDefaultsReadMergeKeysAsTheDecoderDoes(t *testing.T) {
	c, err := Load(testutil.WriteFile(t, "config.yaml", `x-limits: &limits {max_metrics: 40}
x-base: &base
  limits: {max_help_length: 30}
defaults:
  limits:
    <<: *limits
    max_label_value_length: 50
collectors:
  - <<: *base
    name: merged
    request: {type: http, path: /m}
    transform: {type: regex}
    metrics: [{name: m_value, expression: 'value=(\d+)'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Collectors[0].Limits; got.MaxMetrics != 40 || got.MaxLabelValueLength != 50 || got.MaxHelpLength != 30 {
		t.Errorf("limits = %+v", got)
	}
	if want := []string{"limits.max_label_value_length", "limits.max_metrics"}; !slices.Equal(c.Collectors[0].FromDefaults, want) {
		t.Errorf("took %q, want %q", c.Collectors[0].FromDefaults, want)
	}
}
