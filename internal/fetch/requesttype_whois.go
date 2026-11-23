//go:build request_type_whois

package fetch

import (
	"context"
	"errors"
	"net/http"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The whois request type reads TODO(whois): say what the type reads, what
// a collector writes, what its target is, what a scrape sends and what the
// answer handed to the decoder is, and why the type does what it does, as
// the comment opening each other requesttype_<name>.go does.
//
//	request:
//	  type: whois

// WhoisConfig is a whois collector's own request keys, those no other type
// takes, each a field with its yaml key, which a collector's request holds
// in Own (requestkeys.go); a key several types take is one of
// model.RequestConfig's common part, and goes in Fields instead.
// TODO(whois): the keys, and, once there are any, an accessor reading them
// from a collector, as tcpRequest reads TCPConfig's.
type WhoisConfig struct{}

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeWhois,
		// Fields are the keys of model.RequestConfig's common part the
		// type takes, such as tls, max_response_bytes, allowed_targets and
		// denied_targets. TODO(whois)
		Fields:   []string{},
		Config:   WhoisConfig{},
		KeyRules: whoisKeyRules,
		// Overrides are the probe parameters a probe of the type may set,
		// and TargetFields the keys a static target's request may set: the
		// timeout and the param_<name> parameters, which every type takes,
		// until the type has more. TODO(whois)
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    whoisRequester{},
		// TargetHint names what a target of the type is and shows its
		// forms, as the collectors page and the debug page ask for one, or
		// NoTarget says it takes none; AnswerHint says what a response of
		// it pasted on the debug page is. Both are required. TODO(whois)
		TargetHint: func(*model.Collector) (string, string) { return "Server", "host:port" },
		AnswerHint: "Paste the JSON document the exporter makes of the answer, as the Response of a run shows it",
		// UnusedParams words the refusal of a probe parameter nothing
		// uses, naming where a placeholder can stand: the label values
		// alone until a key of the type takes one, which joins them here
		// and in Templates. TODO(whois)
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		// The optional hooks and the settings of what the rest of the
		// exporter knows of the type are left at their zero values, which
		// say the type has none of them (RequestType): Stage, the stage
		// named in logs, "http" unset; JSONAnswer, Decoder, Rows and
		// FileExtensions, how its answer is decoded; Families, its own
		// self-metric families; SchemaRule, its required keys in the
		// configuration schema; Templates, the keys that take a
		// placeholder; CheckFiles, NamedFiles and ReadFiles, the files it
		// reads; Follow, CloseConnections, Connections, ConnectionLimit and
		// ConnectionPools, the connections it keeps. TODO(whois): set those
		// the type has.
	})
}

// errNotImplementedWhois is what a scrape of a whois collector fails with
// until the type is written.
var errNotImplementedWhois = errors.New("the whois request type is not implemented yet")

// whoisRequester is the whois request type's Requester.
type whoisRequester struct{}

// Validate checks a whois collector's request and fills in its defaults.
// TODO(whois): the type's required keys, defaults and rules between keys,
// each refusal naming the collector and the key; until then every request
// is accepted, and its scrape fails.
func (whoisRequester) Validate(*model.Collector) error { return nil }

// Fetch performs one request of a whois collector. TODO(whois): reach the
// target through the target policy, within ctx's deadline, and hand the
// answer back as an HTTPResponse, held to the response limit.
func (whoisRequester) Fetch(_ context.Context, _ string, _ *model.Collector, _ RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return nil, errNotImplementedWhois
}

// CheckTarget checks a whois collector's target. TODO(whois): what a
// target is, refused before anything is sent when it is not one; until
// then any target is taken.
func (whoisRequester) CheckTarget(*model.Collector, string, bool) error { return nil }

// Label is the url label of a whois request. TODO(whois): the target as
// the verbose self-metrics label it, without credentials.
func (whoisRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	return target, nil
}

// Method is the http_method label of a whois request. TODO(whois): what a
// scrape does, in capitals, as tlscert's HANDSHAKE.
func (whoisRequester) Method(*model.Collector, RequestOverrides) string { return "" }

// Display renders a whois target for logs and labels. TODO(whois): without
// credentials.
func (whoisRequester) Display(target string) string { return target }

// whoisKeyRules is what the configuration schema says of the type's own
// keys beyond their Go types, by the path of each under the request block
// (RequestType.KeyRules). TODO(whois): each key's description, and its
// pattern, bounds or allowed values.
func whoisKeyRules() map[string]map[string]any {
	return map[string]map[string]any{}
}
