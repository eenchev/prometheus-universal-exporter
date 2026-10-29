package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// The defaults block of a file (model.CollectorDefaults) is given to the
// collectors of that file as they are read, before anything is validated,
// so a collector is checked, compared on a reload and keyed in the cache as
// if it wrote every value it took: moving a value from the collectors into
// defaults changes no collector, and changing a default changes each
// collector that takes it. The defaults of one file never reach the
// collectors of another, the configuration's included.
//
// A value of the block is checked on its own first, where it does not
// depend on the collector, and refused naming the block, once rather than
// once for each collector; what does depend on the collector — whether its
// request type takes a key, and a value the type holds to rules of its own,
// a bucket for an allowed target of s3file, say — is checked with each
// collector, whose error then says the value came from defaults.

// applyDefaults gives the collectors of a file, decoded from the document
// whose root mapping is root, the file's defaults block, and records on
// each the keys it took (Collector.FromDefaults). own says the file is the
// configuration itself rather than a collector file. A value of the block
// that is wrong in itself is refused, and the block is then given to none;
// so is a key that applies to no collector of the file, which would be a
// setting nothing uses.
func applyDefaults(defaults *model.CollectorDefaults, root *yaml.Node, collectors []model.Collector, own bool) error {
	if defaults == nil {
		return nil
	}
	if err := checkDefaults(defaults); err != nil {
		return err
	}
	if len(collectors) == 0 {
		if own {
			return errors.New("defaults apply to the collectors of their own file, and the configuration file defines none; a collector file takes a defaults block of its own")
		}
		return nil
	}
	nodes := collectorNodes(root)
	if len(nodes) != len(collectors) {
		// Never the case for a document that decoded.
		return nil
	}
	taken := map[string]bool{}
	takes := map[string]bool{}
	socketed := false
	for i := range collectors {
		x := &collectors[i]
		applies := func(key string) bool { return defaultApplies(x, key) }
		written := model.DefaultableKeys(nodes[i])
		// A collector that turns failure_backoff off takes none of the
		// block's keys of it, as if it wrote each.
		if model.BackoffSwitchedOff(x.FailureBackoff) {
			for _, key := range defaults.Keys {
				if key == "failure_backoff" || strings.HasPrefix(key, "failure_backoff.") {
					written = append(written, key)
				}
			}
		}
		// The block's proxy credentials are for a proxy: a collector that
		// names its own proxy, or goes direct, takes none of them when the
		// block names the proxy they are for, which would send them to a
		// proxy they were not meant for. Credentials given without a proxy
		// are for the collectors that name their own, and one that goes
		// direct has no proxy to send them to.
		if slices.Contains(written, "request.proxy_url") && (x.Request.ProxyURL == model.ProxyDirect || slices.Contains(defaults.Keys, "request.proxy_url")) {
			written = append(written, "request.proxy_basic_auth_file")
		}
		// A collector whose requests go to a Unix socket takes no proxy,
		// which the socket stands in place of (fetch/unixsocket.go).
		if fetch.CollectorPolicy(x).UnixSocket != "" {
			written = append(written, "request.proxy_url", "request.proxy_basic_auth_file")
			socketed = true
		}
		x.FromDefaults = defaults.ApplyTo(x, written, applies)
		for _, key := range defaults.Keys {
			taken[key] = taken[key] || slices.Contains(x.FromDefaults, key)
			takes[key] = takes[key] || applies(key)
		}
	}
	var errs []error
	refused := map[string]bool{}
	for _, key := range defaults.Keys {
		if taken[key] || (key == "request.tls" || key == "failure_backoff") && slices.ContainsFunc(defaults.Keys, func(inner string) bool {
			return strings.HasPrefix(inner, key+".") && taken[inner]
		}) {
			continue
		}
		refused[key] = true
		// A key of a block refused already is refused with it: removing
		// the block removes the key.
		if i := strings.LastIndex(key, "."); i >= 0 && refused[key[:i]] {
			continue
		}
		if takes[key] {
			how := "sets " + key + " itself"
			if key == "failure_backoff" || strings.HasPrefix(key, "failure_backoff.") {
				how += ", or turns the backoff off"
			}
			if socketed && (key == "request.proxy_url" || key == "request.proxy_basic_auth_file") {
				how += ", or sends its requests to its request.unix_socket"
			}
			if key == "request.proxy_basic_auth_file" {
				how += ", or goes direct with request.proxy_url: " + model.ProxyDirect
				if slices.Contains(defaults.Keys, "request.proxy_url") {
					how = "sets " + key + " itself, or names a request.proxy_url of its own, and the credentials are for defaults.request.proxy_url alone"
				}
			}
			errs = append(errs, fmt.Errorf("defaults.%s applies to no collector of the file: each collector it would apply to %s; remove it from defaults", key, how))
			continue
		}
		errs = append(errs, fmt.Errorf("defaults.%s applies to no collector of the file: %s does not apply to request.type %s of its collectors; remove it from defaults", key, key, joinRequestTypes(collectors)))
	}
	return model.JoinProblems(errs...)
}

// applyOwnDefaults gives the configuration's collectors its defaults block,
// document being the configuration file as it was decoded.
func applyOwnDefaults(c *model.Config, document []byte) error {
	if c.Defaults == nil {
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(document, &doc); err != nil || len(doc.Content) == 0 {
		// Never the case for a document that decoded.
		return err
	}
	return applyDefaults(c.Defaults, doc.Content[0], c.Collectors, true)
}

// collectorNodes are the entries of the collectors list of a file's root
// mapping, as the decoder reads them, one for each collector it decoded.
func collectorNodes(root *yaml.Node) []*yaml.Node {
	var list *yaml.Node
	for _, entry := range model.DecodedEntries(root) {
		if model.KeyName(entry.Key) == collectorFileKey {
			list = entry.Value
		}
	}
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil
	}
	return list.Content
}

// defaultApplies says whether a key of defaults applies to a collector: a
// key of the request block when the collector's request type takes it,
// and every other key always.
func defaultApplies(x *model.Collector, key string) bool {
	rest, ok := strings.CutPrefix(key, "request.")
	if !ok {
		return true
	}
	top, _, _ := strings.Cut(rest, ".")
	if model.RequestKeyRefused(x.Request.Type, top) {
		return false
	}
	return top != "retry" || !model.RequestKeyRefused(x.Request.Type, rest)
}

// joinRequestTypes names the request types of the collectors, each once, in
// the order they first appear.
func joinRequestTypes(collectors []model.Collector) string {
	var types []string
	for i := range collectors {
		if t := strings.ToLower(strings.TrimSpace(collectors[i].Request.Type)); !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	quoted := make([]string, len(types))
	for i, t := range types {
		quoted[i] = fmt.Sprintf("%q", t)
	}
	return joinWords(quoted, "or")
}

// joinWords writes words as "a", "a or b" and "a, b or c".
func joinWords(words []string, last string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + last + " " + words[len(words)-1]
}

// checkDefaults refuses a value of a defaults block that is wrong whatever
// collector takes it, with the check a collector's value goes through,
// naming the key as defaults.<key>.
func checkDefaults(d *model.CollectorDefaults) error {
	// The checks name the collector they check; one without a name stands
	// for the block, and its name is replaced by it.
	x := &model.Collector{Limits: d.Limits, Cache: d.Cache}
	x.Request.MaxResponseBytes = d.Request.MaxResponseBytes
	var errs []error
	if d.FailureBackoff != nil {
		backoff := *d.FailureBackoff
		// A collector turns the backoff off for itself; the block
		// turning it off for every collector would give them what leaving
		// failure_backoff out of it does.
		if model.BackoffSwitchedOff(&backoff) {
			errs = append(errs, errors.New(`collector "" failure_backoff.enabled is false, which gives the collectors no backoff, as leaving failure_backoff out of defaults does; remove failure_backoff from defaults, and write failure_backoff: {enabled: false} in a collector to turn off the backoff the block gives it`))
			backoff.Enabled = nil
		}
		// A max without initial is held to the initial of each collector
		// that takes it, which may set one shorter than the default.
		if backoff.Initial == nil && backoff.Max != nil && *backoff.Max > 0 {
			backoff.Initial = backoff.Max
		}
		x.FailureBackoff = &backoff
	}
	errs = append(errs, checkLimitValues(x), validateCache(x), validateFailureBackoff(x))
	for _, policy := range []struct{ key, value string }{
		{"on_fetch_error", d.ErrorHandling.OnFetchError},
		{"on_decode_error", d.ErrorHandling.OnDecodeError},
		{"on_transform_error", d.ErrorHandling.OnTransformError},
	} {
		if policy.value != "" {
			errs = append(errs, normalizeErrorPolicy("", "error_handling."+policy.key, &policy.value))
		}
	}
	if retry := d.Request.Retry; retry.Attempts < 0 || retry.Attempts > fetch.MaxRetryAttempts {
		errs = append(errs, fmt.Errorf(`collector "" request.retry.attempts must be from 0 to %d`, fetch.MaxRetryAttempts))
	}
	if d.Request.Retry.Backoff < 0 {
		errs = append(errs, errors.New(`collector "" request.retry.backoff must not be negative`))
	}
	// A proxy_basic_auth_file may stand without a proxy_url here, for
	// collectors that name their own proxies.
	if err := fetch.CheckProxy(d.Request.ProxyURL, d.Request.ProxyBasicAuthFile); err != nil {
		errs = append(errs, fmt.Errorf(`collector "" %w`, err))
	}
	var problems []error
	for _, err := range errs {
		if err != nil {
			problems = append(problems, errors.New(strings.Replace(err.Error(), `collector "" `, "defaults.", 1)))
		}
	}
	return model.JoinProblems(problems...)
}

// withDefaultsNoted says, of each problem validation found with a
// collector, which of the keys it names the collector took from its file's
// defaults, which is where the value to change is written.
func withDefaultsNoted(err error, x *model.Collector) error {
	if err == nil || len(x.FromDefaults) == 0 {
		return err
	}
	problems := model.Problems{err}
	if several, ok := err.(model.Problems); ok { //nolint:errorlint // a Problems itself, as JoinProblems splices it
		problems = several
	}
	out := make(model.Problems, 0, len(problems))
	for _, problem := range problems {
		if note := defaultsNote(problem.Error(), x); note != "" {
			problem = fmt.Errorf("%w%s", problem, note)
		}
		out = append(out, problem)
	}
	return model.JoinProblems(out...)
}

// defaultsNoted is a warning validation gave about a collector, saying, as
// withDefaultsNoted does of a problem, which of the keys it names came from
// the file's defaults: a warning that the collector sets a key is about a
// value written there.
func defaultsNoted(warning string, x *model.Collector) string {
	return warning + defaultsNote(warning, x)
}

// defaultsNote is what withDefaultsNoted adds to text: which of the keys
// text names x took from its file's defaults; "" for none.
func defaultsNote(text string, x *model.Collector) string {
	var named []string
	for _, key := range x.FromDefaults {
		if namesKey(text, key) {
			named = append(named, key)
		}
	}
	switch len(named) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" (%s comes from the defaults of the file)", named[0])
	}
	return fmt.Sprintf(" (%s come from the defaults of the file)", joinWords(named, "and"))
}

// namesKey says whether text names a key by its path, as a whole: not
// limits.max_response_bytes for request.max_response_bytes, nor
// request.tls for request.tls.ca_file.
func namesKey(text, key string) bool {
	for start := 0; ; {
		i := strings.Index(text[start:], key)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(key)
		// A point after the key ends a sentence when no key goes on
		// after it.
		after := end == len(text) || !keyByte(text[end]) || text[end] == '.' && (end+1 == len(text) || !keyByte(text[end+1]))
		if (i == 0 || !keyByte(text[i-1])) && after {
			return true
		}
		start = i + 1
	}
}

// keyByte says whether c can be part of a key's path.
func keyByte(c byte) bool {
	return c == '_' || c == '.' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}
