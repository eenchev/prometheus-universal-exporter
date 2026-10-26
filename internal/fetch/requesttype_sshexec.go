//go:build request_type_sshexec

package fetch

import (
	"context"
	"errors"
	"net/http"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The sshexec request type reads TODO(sshexec): say what the type reads, what
// a collector writes, what its target is, what a scrape sends and what the
// answer handed to the decoder is, and why the type does what it does, as
// the comment opening each other requesttype_<name>.go does.
//
//	request:
//	  type: sshexec

// SshexecConfig is a sshexec collector's own request keys, those no other type
// takes, each a field with its yaml key, which a collector's request holds
// in Own (requestkeys.go); a key several types take is one of
// model.RequestConfig's common part, and goes in Fields instead.
// TODO(sshexec): the keys, and, once there are any, an accessor reading them
// from a collector, as tcpRequest reads TCPConfig's.
type SshexecConfig struct{}

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeSshexec,
		// Fields are the keys of model.RequestConfig's common part the
		// type takes, such as tls, max_response_bytes, allowed_targets and
		// denied_targets. TODO(sshexec)
		Fields:   []string{},
		Config:   SshexecConfig{},
		KeyRules: sshexecKeyRules,
		// Overrides are the probe parameters a probe of the type may set,
		// and TargetFields the keys a static target's request may set: the
		// timeout and the param_<name> parameters, which every type takes,
		// until the type has more. TODO(sshexec)
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    sshexecRequester{},
		// UnusedParams words the refusal of a probe parameter nothing
		// uses, naming where a placeholder can stand: the label values
		// alone until a key of the type takes one, which joins them here
		// and in Templates. TODO(sshexec)
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		// The optional hooks and the settings of what the rest of the
		// exporter knows of the type are left at their zero values, which
		// say the type has none of them (RequestType): Stage, the stage
		// named in logs, "http" unset; TargetHint and NoTarget, what the
		// collectors page says of a target; JSONAnswer, Decoder, Rows and
		// FileExtensions, how its answer is decoded; Families, its own
		// self-metric families; SchemaRule, its required keys in the
		// configuration schema; Templates, the keys that take a
		// placeholder; CheckFiles, NamedFiles and ReadFiles, the files it
		// reads; Follow, CloseConnections, Connections, ConnectionLimit and
		// ConnectionPools, the connections it keeps. TODO(sshexec): set those
		// the type has.
	})
}

// errNotImplementedSshexec is what a scrape of a sshexec collector fails with
// until the type is written.
var errNotImplementedSshexec = errors.New("the sshexec request type is not implemented yet")

// sshexecRequester is the sshexec request type's Requester.
type sshexecRequester struct{}

// Validate checks a sshexec collector's request and fills in its defaults.
// TODO(sshexec): the type's required keys, defaults and rules between keys,
// each refusal naming the collector and the key; until then every request
// is accepted, and its scrape fails.
func (sshexecRequester) Validate(*model.Collector) error { return nil }

// Fetch performs one request of a sshexec collector. TODO(sshexec): reach the
// target through the target policy, within ctx's deadline, and hand the
// answer back as an HTTPResponse, held to the response limit.
func (sshexecRequester) Fetch(_ context.Context, _ string, _ *model.Collector, _ RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return nil, errNotImplementedSshexec
}

// CheckTarget checks a sshexec collector's target. TODO(sshexec): what a
// target is, refused before anything is sent when it is not one; until
// then any target is taken.
func (sshexecRequester) CheckTarget(*model.Collector, string, bool) error { return nil }

// Label is the url label of a sshexec request. TODO(sshexec): the target as
// the verbose self-metrics label it, without credentials.
func (sshexecRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	return target, nil
}

// Method is the http_method label of a sshexec request. TODO(sshexec): what a
// scrape does, in capitals, as tlscert's HANDSHAKE.
func (sshexecRequester) Method(*model.Collector, RequestOverrides) string { return "" }

// Display renders a sshexec target for logs and labels. TODO(sshexec): without
// credentials.
func (sshexecRequester) Display(target string) string { return target }

// sshexecKeyRules is what the configuration schema says of the type's own
// keys beyond their Go types, by the path of each under the request block
// (RequestType.KeyRules). TODO(sshexec): each key's description, and its
// pattern, bounds or allowed values.
func sshexecKeyRules() map[string]map[string]any {
	return map[string]map[string]any{}
}
