package exporter

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What a probe's query may be, before any of it is read.
//
// A parameter the probe reads one value of — target, collector and every
// parameter that changes the request for this probe (method, timeout, path
// and the rest of the request type's RequestType.Overrides) — is refused when
// it is given twice, as a param_<name> given twice always was. The probe would
// use one value and ignore the other, and which of two targets or timeouts a
// scrape was meant for is not something to guess. header_<name> is the one
// that may repeat: every value is forwarded.
//
// A value is also bounded in length. What a probe names lives on after it has
// been answered: the failure log remembers a failing probe by its target for
// up to an hour (failurelog.go), and the verbose self-metrics keep a series
// per request URL (requeststats.go). Neither bounds what it is given, so a
// caller sending targets as long as the HTTP server lets a request be would
// have each failing probe leave that much behind, ten thousand times over.

// MaxProbeParameterBytes is the longest value target, collector and every
// other parameter a probe reads may have.
const MaxProbeParameterBytes = 8 << 10

// checkProbeParams refuses a parameter of query that reads is true for when
// its value is longer than MaxProbeParameterBytes, or when it is given more
// than once and is not a header_<name> parameter. The parameters are looked
// at in order of name, so the same query is always refused for the same one.
func checkProbeParams(query url.Values, reads func(key string) bool) error {
	keys := make([]string, 0, len(query))
	for key := range query {
		if reads(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := query[key]
		if len(values) > 1 && !headerParam(key) {
			return fmt.Errorf("probe parameter %s is given %d times; give it once", key, len(values))
		}
		for _, value := range values {
			if len(value) > MaxProbeParameterBytes {
				return fmt.Errorf("probe parameter %s is %d bytes long; a probe parameter's value may be at most %d bytes", key, len(value), MaxProbeParameterBytes)
			}
		}
	}
	return nil
}

// probeIdentityParam reports whether key is one of the two parameters every
// probe has, whatever its collector's request type.
func probeIdentityParam(key string) bool {
	return key == "target" || key == "collector"
}

// probeRequestParam returns the test for the parameters a probe of c reads
// besides those two: the ones its request type accepts, and header_<name> in
// any case of the prefix, which is how forwardedHeaders reads it.
func probeRequestParam(c *model.Collector) func(key string) bool {
	var overrides []string
	if rt := fetch.RequestTypes[c.Request.Type]; rt != nil {
		overrides = rt.Overrides
	}
	return func(key string) bool {
		if headerParam(key) {
			return requestParam(overrides, headerParamPrefix)
		}
		return requestParam(overrides, key)
	}
}

// headerParamPrefix starts a probe parameter naming a header to forward.
const headerParamPrefix = "header_"

// headerParam reports whether key is a header_<name> parameter.
func headerParam(key string) bool {
	return len(key) > len(headerParamPrefix) && strings.EqualFold(key[:len(headerParamPrefix)], headerParamPrefix)
}

// requestParam reports whether key is one of overrides, where a name ending
// in _ is a prefix, as CheckOverrideParams reads them.
func requestParam(overrides []string, key string) bool {
	for _, name := range overrides {
		if strings.HasSuffix(name, "_") && strings.HasPrefix(key, name) || key == name {
			return true
		}
	}
	return false
}
