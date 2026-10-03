package exporter

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a probe's query may be, and what of it makes the probe's cache and
// coalescing key (probeparams.go, cache.go).

// Every request type's parameters are refused when repeated, whatever the
// type: the names are read from the type itself, so one added to a type
// later is covered without a line here.
func TestEveryRequestTypesParametersAreReadOnce(t *testing.T) {
	for name, rt := range fetch.RequestTypes {
		reads := probeRequestParam(&model.Collector{Request: model.RequestConfig{Type: name}})
		for _, override := range rt.Overrides {
			key := override
			if strings.HasSuffix(key, "_") {
				key += "x"
			}
			err := checkProbeParams(url.Values{key: {"1", "2"}}, reads)
			switch {
			case override == headerParamPrefix:
				if err != nil {
					t.Errorf("%s: a repeated %s is refused: %v", name, key, err)
				}
			case err == nil || !strings.Contains(err.Error(), "probe parameter "+key+" is given 2 times"):
				t.Errorf("%s: a repeated %s is not refused by name: %v", name, key, err)
			}
		}
		// A parameter the type does not read is the probe's to ignore.
		if err := checkProbeParams(url.Values{"no_such_parameter": {"1", "2"}}, reads); err != nil {
			t.Errorf("%s: a repeated parameter the probe does not read is refused: %v", name, err)
		}
	}
}

// probeParameterValues are two valid values of every probe parameter some
// request type accepts, for TestEveryProbeParameterIsPartOfTheKey.
var probeParameterValues = map[string][2]string{
	"method":               {"POST", "PUT"},
	"path":                 {"/a", "/b"},
	"timeout":              {"5s", "6s"},
	"body":                 {"a", "b"},
	"message":              {`{"a": 1}`, `{"a": 2}`},
	"insecure_skip_verify": {"true", "false"},
	"follow_redirects":     {"true", "false"},
	"enable_http2":         {"true", "false"},
	"retry_attempts":       {"1", "2"},
	"retry_backoff":        {"1s", "2s"},
	"from":                 {"-1h", "-2h"},
	"until":                {"now", "-1min"},
	fetch.PathParamPrefix:  {"a", "b"},
}

// The key is made of the parsed parameters, field by field, so a parameter a
// request type accepts and the key leaves out would let two different
// requests share an entry. Every parameter of every registered type changes
// the key, by its presence and by its value. A parameter added to a type
// without a value here fails the test, which is the reminder to key it.
func TestEveryProbeParameterIsPartOfTheKey(t *testing.T) {
	c := testutil.Collector("text", "text")
	key := func(query url.Values) string {
		t.Helper()
		overrides, err := fetch.ParseRequestOverrides(query)
		if err != nil {
			t.Fatal(err)
		}
		return probeCacheKey(&c, "http://h", probeKeyQuery("text", "http://h", overrides), nil)
	}
	bare := key(url.Values{})
	for name, rt := range fetch.RequestTypes {
		for _, override := range rt.Overrides {
			if override == headerParamPrefix {
				// Keyed as the headers they become, when forwarded.
				continue
			}
			values, known := probeParameterValues[override]
			if !known {
				t.Errorf("%s accepts the probe parameter %s, which this test has no values for: add them, and the parameter to probeKeyQuery", name, override)
				continue
			}
			parameter := override
			if strings.HasSuffix(parameter, "_") {
				parameter += "x"
			}
			one, other := key(url.Values{parameter: {values[0]}}), key(url.Values{parameter: {values[1]}})
			if one == bare || other == bare || one == other {
				t.Errorf("%s: %s is not part of the key: without it %.8s, with %q %.8s, with %q %.8s", name, parameter, bare, values[0], one, values[1], other)
			}
		}
	}
	// A path parameter left empty is one not given, where it is bound and
	// here.
	if key(url.Values{"param_x": {""}}) != bare {
		t.Error("an empty param_x makes a key of its own")
	}
	// Present and empty is not absent for the parameters that replace
	// something wholesale.
	for _, parameter := range []string{"path", "body", "message"} {
		if key(url.Values{parameter: {""}}) == bare {
			t.Errorf("an empty %s shares the key of a probe without it", parameter)
		}
	}
}

// A static target's scrape and the probe that makes the same request share a
// cache entry, however the probe spells it: both keys are written the same
// way.
func TestAStaticTargetAndTheProbeOfItsRequestShareAKey(t *testing.T) {
	yes, attempts, backoff := true, 2, model.Duration(time.Second)
	target := model.StaticTarget{
		Name: "one", Collector: "text", Target: "http://h",
		Params: map[string]string{"param_region": "eu"},
		Request: model.TargetRequestConfig{
			Method: "POST", Path: "/api", PathSet: true, Body: "payload", BodySet: true,
			Timeout: model.Duration(5 * time.Second), InsecureSkipVerify: &yes, FollowRedirects: &yes, EnableHTTP2: &yes,
			Retry: &model.TargetRetryConfig{Attempts: &attempts, Backoff: &backoff},
		},
	}
	query, err := url.ParseQuery("method=post&path=/api&body=payload&timeout=5000ms&insecure_skip_verify=TRUE&follow_redirects=true&enable_http2=true&retry_attempts=02&retry_backoff=1000ms&param_region=eu")
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := fetch.ParseRequestOverrides(query)
	if err != nil {
		t.Fatal(err)
	}
	if probe, static := probeKeyQuery("text", "http://h", overrides).Encode(), targetCacheQuery(&target).Encode(); probe != static {
		t.Fatalf("the probe is keyed by\n%s\nand the static target by\n%s", probe, static)
	}
}
