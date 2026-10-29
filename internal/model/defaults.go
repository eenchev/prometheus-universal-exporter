package model

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// A file of many similar collectors repeats the same limits, retries and
// target policies in each. A top-level defaults block, in the configuration
// and in each collector file, writes them once for the collectors of that
// file. YAML merge keys cannot do it: a key written beside <<: replaces the
// merged one whole, so a collector that sets one limit of a merged limits
// block loses the others, and a merge brings in whole blocks, not the keys
// nested in them.
//
// The keys defaults may set are listed here (DefaultsKeys) rather than
// being every key of a collector: each is a setting that is the same for
// collectors of different request types and decoders, so one block can serve
// a whole file. The load gives each collector of the file every key of the
// block it does not write itself, key by key at every depth: a collector that
// writes limits.max_metrics keeps the defaults' other limits. A list, such as
// request.allowed_targets, is one key and replaced whole, never joined with
// the collector's.

// CollectorDefaults is the defaults block of a configuration file or of a
// collector file.
type CollectorDefaults struct {
	Request        DefaultsRequest       `yaml:"request"`
	Limits         Limits                `yaml:"limits"`
	ErrorHandling  ErrorHandling         `yaml:"error_handling"`
	FailureBackoff *FailureBackoffConfig `yaml:"failure_backoff"`
	Cache          CacheConfig           `yaml:"cache"`
	// Keys are the keys the block sets (DefaultableKeys), by their paths
	// in a collector, in the order the file has them, which is the order
	// they are given to a collector in.
	Keys []string `yaml:"-"`
}

// DefaultsRequest is the request block of defaults: the keys of a
// collector's request block that defaults may set.
type DefaultsRequest struct {
	TLS              TLSConfig   `yaml:"tls"`
	Retry            RetryConfig `yaml:"retry"`
	MaxResponseBytes ByteSize    `yaml:"max_response_bytes"`
	AllowedTargets   []string    `yaml:"allowed_targets"`
	DeniedTargets    []string    `yaml:"denied_targets"`
	// ProxyURL and ProxyBasicAuthFile are taken by the collectors whose
	// request type takes them, http and graphite: the proxy is where the
	// exporter runs more than what a collector asks, and a file of
	// collectors behind one proxy names it once.
	ProxyURL           string              `yaml:"proxy_url"`
	ProxyBasicAuthFile *ProxyBasicAuthFile `yaml:"proxy_basic_auth_file"`
}

// DefaultsKeys are the keys defaults may set, as blocks of a collector and
// keys of its request block: every key of each.
var DefaultsKeys = []string{"request.tls", "request.retry", "request.max_response_bytes", "request.allowed_targets", "request.denied_targets", "request.proxy_url", "request.proxy_basic_auth_file", "limits", "error_handling", "failure_backoff", "cache"}

// defaultsBlocks are the blocks of a collector defaults may set keys of, and
// defaultsRequestKeys the keys of its request block.
var (
	defaultsBlocks      = []string{"request", "limits", "error_handling", "failure_backoff", "cache"}
	defaultsRequestKeys = []string{"tls", "retry", "max_response_bytes", "allowed_targets", "denied_targets", "proxy_url", "proxy_basic_auth_file"}
)

// UnmarshalYAML reads a defaults block. A key defaults may not set is
// refused, naming it, whether it is a key of a collector or of nothing; the
// file's decoder does not look into a type that decodes itself, so the keys
// of its blocks are checked here as it would check them. Each block is
// decoded on its own, so the error of one that checks its own keys (tls,
// cache, failure_backoff), which may end the decoding of everything around
// it, loses none of the others, and every problem is reported in the order
// of the file.
func (d *CollectorDefaults) UnmarshalYAML(n *yaml.Node) error {
	type plain CollectorDefaults
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return notAMapping(n, "model.CollectorDefaults")
	}
	var problems []keyProblem
	refuse := func(key *yaml.Node, path string, known bool) {
		why := fmt.Sprintf("unknown key %q in defaults", path)
		if known {
			why = "defaults cannot set " + path
		}
		problems = append(problems, keyProblem{key.Line, key.Column, fmt.Sprintf("line %d: %s; defaults set the keys of %s", key.Line, why, joinWithAnd(DefaultsKeys))})
	}
	seen := map[string]bool{}
	var out plain
	blocks := reflect.ValueOf(&out).Elem()
	collector := reflect.TypeOf(Collector{})
	for _, entry := range MappingEntries(n) {
		block := KeyName(entry.Key)
		if !slices.Contains(defaultsBlocks, block) {
			_, known := fieldByKey(reflect.New(collector).Elem(), block)
			refuse(entry.Key, block, known && block != "-")
			continue
		}
		switch block {
		case "request":
			if value := resolveAlias(entry.Value); value != nil && value.Kind == yaml.MappingNode {
				view := reflect.New(RequestViewType()).Elem()
				for _, inner := range MappingEntries(value) {
					key := KeyName(inner.Key)
					if !slices.Contains(defaultsRequestKeys, key) {
						_, known := fieldByKey(view, key)
						refuse(inner.Key, "request."+key, known && key != "-")
						continue
					}
					switch key {
					case "retry":
						problems = append(problems, unknownKeys(inner.Value, reflect.TypeOf(RetryConfig{}), "model.RetryConfig")...)
					case "proxy_basic_auth_file":
						problems = append(problems, unknownKeys(inner.Value, reflect.TypeOf(ProxyBasicAuthFile{}), "model.ProxyBasicAuthFile")...)
					}
				}
			}
		case "limits":
			problems = append(problems, unknownKeys(entry.Value, reflect.TypeOf(Limits{}), "model.Limits")...)
		case "error_handling":
			problems = append(problems, unknownKeys(entry.Value, reflect.TypeOf(ErrorHandling{}), "model.ErrorHandling")...)
		}
		// Each value of the block is decoded on its own, so that a
		// message the decoder gives with its line alone is placed at its
		// key (placedDecodeProblems): on a line of several, it is said
		// where the file has it, not where the block's mapping begins.
		field, _ := fieldByKey(blocks, block)
		for _, problem := range placedDecodeProblems(entry.Value, field.Type(), keyAt{"", entry.Key.Line, entry.Key.Column}) {
			seen[problem.message] = true
			problems = append(problems, problem)
		}
	}
	// The block decoded whole is what the collectors take; what it finds
	// that no block on its own did, a key written twice, is reported too.
	for _, message := range errorMessages(n.Decode(&out)) {
		if !seen[message] {
			problems = append(problems, placedProblem(n, message))
		}
	}
	if len(problems) > 0 {
		sortProblems(problems)
		messages := make([]string, 0, len(problems))
		for _, problem := range problems {
			messages = append(messages, problem.message)
		}
		return &yaml.TypeError{Errors: messages}
	}
	*d = CollectorDefaults(out)
	d.Keys = DefaultableKeys(n)
	return nil
}

// errorMessages are the messages of a decoding error: each of a
// *yaml.TypeError's, or the error's own.
func errorMessages(err error) []string {
	if typeErr, ok := err.(*yaml.TypeError); ok { //nolint:errorlint // the decoder's own, as it returns it
		return typeErr.Errors
	}
	if err != nil {
		return []string{err.Error()}
	}
	return nil
}

// linePrefix is the line a decoding message starts by naming.
var linePrefix = regexp.MustCompile(`^line (\d+):`)

// placedProblem is a message of decoding value, placed at the line it names,
// and on value's own line at value's column, so it sorts among the keys
// refused around it.
func placedProblem(value *yaml.Node, message string) keyProblem {
	value = resolveAlias(value)
	line, column := value.Line, value.Column
	if m := linePrefix.FindStringSubmatch(message); m != nil {
		if named, err := strconv.Atoi(m[1]); err == nil && named != line {
			line, column = named, 0
		}
	}
	return keyProblem{line, column, message}
}

// DefaultableKeys are the keys a collector, or a defaults block, written as
// the mapping n, sets of those defaults may set, by their paths, in the
// order the decoder reads them (DecodedEntries): the keys of each block,
// such as limits.max_metrics, and request.tls and failure_backoff
// themselves, since writing either block, even empty, is a setting of its
// own — TLS spoken, a backoff with its defaults. A key written "" is left
// out (leftOut).
func DefaultableKeys(n *yaml.Node) []string {
	var keys []string
	inner := func(path string, n *yaml.Node) {
		for _, entry := range DecodedEntries(n) {
			if key := KeyName(entry.Key); key != "" && !leftOut(entry.Value) {
				keys = append(keys, path+"."+key)
			}
		}
	}
	for _, entry := range DecodedEntries(n) {
		switch block := KeyName(entry.Key); block {
		case "limits", "error_handling", "cache":
			inner(block, entry.Value)
		case "failure_backoff":
			keys = append(keys, block)
			inner(block, entry.Value)
		case "request":
			for _, field := range DecodedEntries(entry.Value) {
				switch key := KeyName(field.Key); key {
				case "tls":
					keys = append(keys, "request.tls")
					inner("request.tls", field.Value)
				case "retry":
					inner("request.retry", field.Value)
				case "max_response_bytes", "allowed_targets", "denied_targets", "proxy_url", "proxy_basic_auth_file":
					if !leftOut(field.Value) {
						keys = append(keys, "request."+key)
					}
				}
			}
		}
	}
	return keys
}

// leftOut says whether a value written is the key left out: "", which an
// optional key takes as left out, so that a collector that writes an error
// policy "" takes the defaults' policy, and a policy "" in defaults gives
// none.
func leftOut(value *yaml.Node) bool {
	value = resolveAlias(value)
	return value != nil && value.Kind == yaml.ScalarNode && value.ShortTag() == stringTag && value.Value == ""
}

// ApplyTo gives x the value of each key of the block (Keys) that x does
// not write itself — written, the DefaultableKeys of its mapping — and
// that applies says applies to it, a copy of each, and returns those keys.
// A key of the request block given to x joins its Order after the keys x
// writes, so of several keys the validation refuses it names one x writes
// first, and the others in the order the block has them.
func (d *CollectorDefaults) ApplyTo(x *Collector, written []string, applies func(key string) bool) []string {
	var given []string
	from := reflect.ValueOf(d).Elem()
	to := reflect.ValueOf(x).Elem()
	for _, key := range d.Keys {
		if slices.Contains(written, key) || !applies(key) {
			continue
		}
		path := strings.Split(key, ".")
		switch key {
		case "request.tls":
			x.Request.TLS.Set = true
		case "failure_backoff":
			if x.FailureBackoff == nil {
				x.FailureBackoff = &FailureBackoffConfig{}
			}
		default:
			pathValue(to, path).Set(copyValue(pathValue(from, path)))
		}
		given = append(given, key)
		if rest, ok := strings.CutPrefix(key, "request."); ok {
			top, _, _ := strings.Cut(rest, ".")
			for _, order := range []string{top, rest} {
				if !slices.Contains(x.Request.Order, order) {
					x.Request.Order = append(x.Request.Order, order)
				}
			}
		}
	}
	return given
}

// pathValue is the field of v at the path of yaml keys, making each nil
// pointer on the way to it, so the field can be set.
func pathValue(v reflect.Value, path []string) reflect.Value {
	for _, key := range path {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		field, ok := fieldByKey(v, key)
		if !ok {
			panic("model: no key " + strings.Join(path, ".") + " in " + v.Type().String())
		}
		v = field
	}
	return v
}

// copyValue is a copy of v that shares no list or pointer with it, so a
// collector that changes a value it was given, as the validation fills in
// and normalises them, changes no other collector's.
func copyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(copyValue(v.Elem()))
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(copyValue(v.Index(i)))
		}
		return out
	}
	return v
}

// RequestKeyRefused says whether a request block of the type refuses the
// key, by its path (retry.codes for codes in retry), as one the type does
// not take (RequestKeys.Refuses); false for a type the build does not
// have, which the validation refuses the block for.
func RequestKeyRefused(requestType, key string) bool {
	refuses := currentRequestLayout().refuses
	return refuses != nil && refuses(strings.ToLower(strings.TrimSpace(requestType)), key)
}
