//go:build request_type_amqp

package fetch

import (
	"context"
	"errors"
	"net/http"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The amqp request type reads TODO(amqp): say what the type reads, what
// a collector writes, what its target is, what a scrape sends and what the
// answer handed to the decoder is, and why the type does what it does, as
// the comment opening each other requesttype_<name>.go does.
//
//	request:
//	  type: amqp

// AMQPConfig is an amqp collector's own request keys, those no other type
// takes, each a field with its yaml key, which a collector's request holds
// in Own (requestkeys.go); a key several types take is one of
// model.RequestConfig's common part, and goes in Fields instead.
// TODO(amqp): the keys, and, once there are any, an accessor reading them
// from a collector, as tcpRequest reads TCPConfig's.
type AMQPConfig struct{}

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeAMQP,
		// Fields are the keys of model.RequestConfig's common part the
		// type takes, such as tls, max_response_bytes, allowed_targets and
		// denied_targets. TODO(amqp)
		Fields:   []string{},
		Config:   AMQPConfig{},
		KeyRules: amqpKeyRules,
		// Overrides are the probe parameters a probe of the type may set,
		// and TargetFields the keys a static target's request may set: the
		// timeout and the param_<name> parameters, which every type takes,
		// until the type has more. TODO(amqp)
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    amqpRequester{},
		// TargetHint names what a target of the type is and shows its
		// forms, as the collectors page and the debug page ask for one, or
		// NoTarget says it takes none; AnswerHint says what a response of
		// it pasted on the debug page is. Both are required. TODO(amqp)
		TargetHint: func(*model.Collector) (string, string) { return "Server", "host:port" },
		AnswerHint: "Paste the JSON document the exporter makes of the answer, as the Response of a run shows it",
		// UnusedParams words the refusal of a probe parameter nothing
		// uses, naming where a placeholder can stand: the label values
		// alone until a key of the type takes one, which joins them here
		// and in Templates. TODO(amqp)
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
		// ConnectionPools, the connections it keeps. TODO(amqp): set those
		// the type has.
	})
}

// errNotImplementedAMQP is what a scrape of an amqp collector fails with
// until the type is written.
var errNotImplementedAMQP = errors.New("the amqp request type is not implemented yet")

// amqpRequester is the amqp request type's Requester.
type amqpRequester struct{}

// Validate checks an amqp collector's request and fills in its defaults.
// TODO(amqp): the type's required keys, defaults and rules between keys,
// each refusal naming the collector and the key; until then every request
// is accepted, and its scrape fails.
func (amqpRequester) Validate(*model.Collector) error { return nil }

// Fetch performs one request of an amqp collector. TODO(amqp): reach the
// target through the target policy, within ctx's deadline, and hand the
// answer back as an HTTPResponse, held to the response limit.
func (amqpRequester) Fetch(_ context.Context, _ string, _ *model.Collector, _ RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return nil, errNotImplementedAMQP
}

// CheckTarget checks an amqp collector's target. TODO(amqp): what a
// target is, refused before anything is sent when it is not one; until
// then any target is taken.
func (amqpRequester) CheckTarget(*model.Collector, string, bool) error { return nil }

// Label is the url label of an amqp request. TODO(amqp): the target as
// the verbose self-metrics label it, without credentials.
func (amqpRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	return target, nil
}

// Method is the http_method label of an amqp request. TODO(amqp): what a
// scrape does, in capitals, as tlscert's HANDSHAKE.
func (amqpRequester) Method(*model.Collector, RequestOverrides) string { return "" }

// Display renders an amqp target for logs and labels. TODO(amqp): without
// credentials.
func (amqpRequester) Display(target string) string { return target }

// amqpKeyRules is what the configuration schema says of the type's own
// keys beyond their Go types, by the path of each under the request block
// (RequestType.KeyRules). TODO(amqp): each key's description, and its
// pattern, bounds or allowed values.
func amqpKeyRules() map[string]map[string]any {
	return map[string]map[string]any{}
}
