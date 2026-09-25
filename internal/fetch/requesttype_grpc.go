//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The grpc request type calls one unary gRPC method on the target and hands
// the answer to the transforms as JSON:
//
//	request:
//	  type: grpc
//	  rpc: acme.queue.v1.QueueService/GetStats  # required: package.Service/Method
//	  message: '{"queue": {{param_queue|json}}}' # the request, in the protobuf JSON mapping
//	  metadata:
//	    x-tenant: "{{param_tenant:default}}"
//	  descriptors: reflection                  # required: reflection, protoset or proto
//
// The probe's target is the server, host:port, plaintext unless grpcs:// or
// a tls block asks for TLS. The message types come from descriptors the
// configuration names (grpcdescriptors.go), so no service needs code
// generated for it: the server's reflection service, a descriptor set file,
// or .proto sources compiled when the configuration loads. The health
// service, grpc.health.v1.Health, is built in and needs none.
//
// The call is made on a connection kept per target and TLS settings
// (grpcconn.go) and answered as JSON (grpccall.go); everything after it —
// decoding, transforms, limits, caching — is every type's.
//
// This file and the grpc*.go files are the only ones that import grpc-go and
// the protobuf runtime, all behind this type's build constraint, so a build
// without the type does not link them.

// The descriptor sources.
const (
	descriptorsReflection = "reflection"
	descriptorsProtoset   = "protoset"
	descriptorsProto      = "proto"
)

var descriptorSources = []string{descriptorsReflection, descriptorsProtoset, descriptorsProto}

// grpcHealthService is the service whose types are built into the exporter,
// so a collector calling it may leave descriptors out.
const grpcHealthService = "grpc.health.v1.Health"

// grpcCodeNames are the gRPC status codes by number, as a configuration
// names them.
var grpcCodeNames = []string{
	"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED",
	"NOT_FOUND", "ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED",
	"FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED",
	"INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED",
}

// grpcDefaultRetryCodes are retried when retry.codes is left out: the server
// did not start the call, so repeating it cannot repeat what it did.
var grpcDefaultRetryCodes = []string{"UNAVAILABLE"}

// grpcRPC is the shape of request.rpc: a service's full name, its package
// first if it has one, a slash, and a method name.
var grpcRPC = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*\.)*[A-Za-z_][A-Za-z0-9_]*/[A-Za-z_][A-Za-z0-9_]*$`)

// grpcMetadataKey is what a metadata key may be made of: lower-case letters,
// digits and - _ . , as gRPC requires of a header name.
var grpcMetadataKey = regexp.MustCompile(`^[0-9a-z_.-]+$`)

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeGRPC,
		Fields: []string{
			"rpc", "message", "metadata",
			"descriptors", "protoset_file", "proto_files", "proto_import_paths",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"forward_authorization", "forward_headers",
			"tls", "retry", "max_response_bytes",
			"allowed_targets", "denied_targets", "accept_codes",
		},
		Overrides: []string{
			"timeout", "insecure_skip_verify", "retry_attempts", "retry_backoff",
			"header_", PathParamPrefix, "message",
		},
		TargetFields: []string{
			"message", "metadata", "timeout", "insecure_skip_verify", "retry",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"accept_codes",
		},
		StatusCodes:        true,
		Validate:           validateGRPCRequest,
		CheckTarget:        checkGRPCTarget,
		CheckTargetRequest: checkGRPCTargetRequest,
		Label:              grpcLabel,
		Method:             func(*model.Collector, RequestOverrides) string { return http.MethodPost },
		Display:            grpcDisplay,
		Stage:              "grpc",
		Fetch:              fetchGRPC,
	})
}

// validateGRPCRequest holds the grpc type's rules: the method's shape, the
// message and metadata, the descriptor source, the credentials and retries.
// With descriptors protoset or proto the method is looked up now, so a
// method that does not exist, streams, or takes another message fails the
// configuration rather than every scrape.
func validateGRPCRequest(x *model.Collector) error {
	r := &x.Request
	r.RPC = strings.TrimPrefix(strings.TrimSpace(r.RPC), "/")
	switch {
	case r.RPC == "":
		return fmt.Errorf("collector %q has no request.rpc; a grpc collector calls one method, written package.Service/Method, such as grpc.health.v1.Health/Check", x.Name)
	case strings.Contains(r.RPC, "{{"):
		return fmt.Errorf("collector %q request.rpc %q has a placeholder; a collector calls one method, so rpc takes none", x.Name, r.RPC)
	case !grpcRPC.MatchString(r.RPC):
		return fmt.Errorf("collector %q request.rpc %q is not a method; write the service's full name, a slash and the method, such as acme.queue.v1.QueueService/GetStats", x.Name, r.RPC)
	}
	if err := validateCredentials(x); err != nil {
		return err
	}
	if err := checkTLSSettings(r.TLS); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkGRPCMetadata(r.Metadata, true, r.BasicAuth != nil || r.BasicAuthFile != nil || r.BearerToken != "" || r.BearerTokenFile != ""); err != nil {
		return fmt.Errorf("collector %q request.metadata %w", x.Name, err)
	}
	for _, name := range r.ForwardHeaders {
		if err := checkGRPCMetadataName(strings.ToLower(strings.TrimSpace(name))); err != nil {
			return fmt.Errorf("collector %q request.forward_headers %q: %w", x.Name, name, err)
		}
	}
	if r.Retry.Attempts < 0 {
		return fmt.Errorf("collector %q request.retry.attempts must not be negative", x.Name)
	}
	if r.Retry.Backoff < 0 {
		return fmt.Errorf("collector %q request.retry.backoff must not be negative", x.Name)
	}
	if err := normalizeRetryCodes(r.Retry.Codes); err != nil {
		return fmt.Errorf("collector %q request.retry.codes %w", x.Name, err)
	}
	if err := normalizeRetryCodes(r.AcceptCodes); err != nil {
		return fmt.Errorf("collector %q request.accept_codes %s", x.Name, strings.Replace(err.Error(), "never retried", "always accepted", 1))
	}
	// The placeholders of the message and the metadata values.
	for _, f := range requestTemplates(x, RequestOverrides{}) {
		if _, err := f.parse(); err != nil {
			return fmt.Errorf("collector %q: %w", x.Name, err)
		}
	}
	message, typed, err := messageForCheck(r.Message)
	if err != nil {
		return fmt.Errorf("collector %q request.message %w", x.Name, err)
	}
	if err := validateDescriptorKeys(x); err != nil {
		return err
	}
	if r.Descriptors == descriptorsReflection {
		// The server is asked at the first call; nothing more is known now.
		return nil
	}
	method, err := staticMethod(x)
	if err != nil {
		return fmt.Errorf("collector %q: %w", x.Name, err)
	}
	if typed {
		if err := method.checkMessage(message); err != nil {
			return fmt.Errorf("collector %q request.message does not fit %s: %w", x.Name, method.input(), err)
		}
	}
	return nil
}

// validateDescriptorKeys checks descriptors and the keys of its sources:
// descriptors is required, but for the health service, and each source's
// keys go only with it.
func validateDescriptorKeys(x *model.Collector) error {
	r := &x.Request
	r.Descriptors = strings.ToLower(strings.TrimSpace(r.Descriptors))
	service, _, _ := strings.Cut(r.RPC, "/")
	switch {
	case r.Descriptors == "" && service == grpcHealthService:
		// Its types are built in.
	case r.Descriptors == "":
		return fmt.Errorf("collector %q has no request.descriptors; say where the message types of %s come from: reflection, to ask the server's reflection service; protoset, to read a descriptor set from protoset_file; or proto, to compile the .proto files in proto_files", x.Name, service)
	case !slices.Contains(descriptorSources, r.Descriptors):
		return fmt.Errorf("collector %q request.descriptors is %q; want reflection, protoset or proto", x.Name, r.Descriptors)
	}
	if r.ProtosetFile != "" && r.Descriptors != descriptorsProtoset {
		return fmt.Errorf("collector %q sets request.protoset_file, which applies only with descriptors: protoset", x.Name)
	}
	if (len(r.ProtoFiles) > 0 || len(r.ProtoImportPaths) > 0) && r.Descriptors != descriptorsProto {
		return fmt.Errorf("collector %q sets request.proto_files or proto_import_paths, which apply only with descriptors: proto", x.Name)
	}
	if r.Descriptors == descriptorsProtoset && strings.TrimSpace(r.ProtosetFile) == "" {
		return fmt.Errorf("collector %q has descriptors: protoset but no request.protoset_file; name the FileDescriptorSet that protoc --descriptor_set_out --include_imports or buf build -o writes", x.Name)
	}
	if r.Descriptors == descriptorsProto && len(r.ProtoFiles) == 0 {
		return fmt.Errorf("collector %q has descriptors: proto but no request.proto_files; list the .proto files that define %s", x.Name, service)
	}
	for i, file := range r.ProtoFiles {
		if strings.TrimSpace(file) == "" {
			return fmt.Errorf("collector %q request.proto_files[%d] is empty", x.Name, i)
		}
	}
	for i, dir := range r.ProtoImportPaths {
		if strings.TrimSpace(dir) == "" {
			return fmt.Errorf("collector %q request.proto_import_paths[%d] is empty", x.Name, i)
		}
	}
	return nil
}

// messageForCheck is the message as the configuration loads it: {} when
// left out, each placeholder standing in with its default, or with a value
// of its filter's kind when it has none. It must be JSON. typed says every
// placeholder had a default, so the message is what a probe without
// parameters sends, and can be checked against the input type; a stand-in
// could fail that check for a value no probe sends.
func messageForCheck(text string) (message string, typed bool, err error) {
	if strings.TrimSpace(text) == "" {
		return "{}", true, nil
	}
	f := templateField{"request.message", text, "body"}
	placeholders, err := f.parse()
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	previous := 0
	typed = true
	for _, p := range placeholders {
		value := p.Default
		if !p.HasDefault {
			typed = false
			switch p.Filter {
			case "json":
				value = "x"
			default:
				value = "0"
			}
		}
		written, err := f.write(p, value)
		if err != nil {
			return "", false, err
		}
		b.WriteString(text[previous:p.start])
		b.WriteString(written)
		previous = p.end
	}
	b.WriteString(text[previous:])
	message = b.String()
	if !json.Valid([]byte(message)) {
		return "", false, fmt.Errorf("is not JSON once its placeholders take their defaults: %s; write the message in the protobuf JSON mapping, and a string placeholder as {{param_x|json}}", message)
	}
	return message, typed, nil
}

// checkGRPCMetadata checks metadata keys and values. templated says the
// values may hold placeholders, whose values are checked when they are
// filled in; credentials says the credential keys already send
// authorization.
func checkGRPCMetadata(metadata map[string]string, templated, credentials bool) error {
	for _, key := range model.SortedKeys(metadata) {
		if err := checkGRPCMetadataName(key); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if key == "authorization" && credentials {
			return errors.New("authorization: the credential keys already send it; set one or the other")
		}
		value := metadata[key]
		if templated && HasPathParams(value) {
			continue
		}
		if err := checkHeaderValue(value); err != nil {
			return fmt.Errorf("%s %w", key, err)
		}
	}
	return nil
}

// checkGRPCMetadataName refuses a metadata key gRPC would not send as
// written: upper-case, binary (-bin, which carries bytes, not text), or one
// gRPC or HTTP/2 reserves for itself.
func checkGRPCMetadataName(key string) error {
	switch {
	case key == "":
		return errors.New("is empty")
	case strings.HasPrefix(key, ":"), strings.HasPrefix(key, "grpc-"), key == "content-type", key == "te", key == "host", key == "connection", key == "user-agent":
		return errors.New("is reserved by gRPC and HTTP/2, which set it themselves")
	case strings.HasSuffix(key, "-bin"):
		return errors.New("is binary metadata (-bin), whose value is bytes; only text metadata can be configured")
	case !grpcMetadataKey.MatchString(key):
		return errors.New("must be lower-case letters, digits and - _ . , as gRPC metadata keys are")
	}
	return nil
}

// normalizeRetryCodes upper-cases retry.codes in place and refuses a name
// that is not a status code, and OK, which is not a failure.
func normalizeRetryCodes(codes []string) error {
	for i, code := range codes {
		code = strings.ToUpper(strings.TrimSpace(code))
		codes[i] = code
		switch {
		case code == "OK":
			return errors.New("lists OK, which is a success and never retried")
		case !slices.Contains(grpcCodeNames, code):
			return fmt.Errorf("lists %q, which is not a gRPC status code; use names such as UNAVAILABLE, RESOURCE_EXHAUSTED or ABORTED", code)
		}
	}
	return nil
}

// checkGRPCTargetRequest checks what a static target's request sets for a
// grpc collector: its message, which must be JSON and, with descriptors
// read at load, fit the method; its metadata; and its retry codes.
func checkGRPCTargetRequest(c *model.Collector, t *model.StaticTarget) error {
	credentials := c.Request.BasicAuth != nil || c.Request.BasicAuthFile != nil || c.Request.BearerToken != "" || c.Request.BearerTokenFile != "" ||
		t.Request.BasicAuth != nil || t.Request.BasicAuthFile != nil || t.Request.BearerToken != "" || t.Request.BearerTokenFile != ""
	if err := checkGRPCMetadata(t.Request.Metadata, false, credentials); err != nil {
		return fmt.Errorf("request.metadata %w", err)
	}
	if retry := t.Request.Retry; retry != nil {
		if err := normalizeRetryCodes(retry.Codes); err != nil {
			return fmt.Errorf("request.retry.codes %w", err)
		}
	}
	if err := normalizeRetryCodes(t.Request.AcceptCodes); err != nil {
		return fmt.Errorf("request.accept_codes %s", strings.Replace(err.Error(), "never retried", "always accepted", 1))
	}
	if t.Request.Message == "" {
		return nil
	}
	if !json.Valid([]byte(t.Request.Message)) {
		return errors.New("request.message is not JSON; write the message in the protobuf JSON mapping")
	}
	if c.Request.Descriptors == descriptorsProtoset || c.Request.Descriptors == descriptorsProto || c.Request.Descriptors == "" {
		method, err := staticMethod(c)
		if err != nil {
			return err
		}
		if err := method.checkMessage(t.Request.Message); err != nil {
			return fmt.Errorf("request.message does not fit %s: %w", method.input(), err)
		}
	}
	return nil
}

// grpcAddress is a grpc target as it is dialled.
type grpcAddress struct {
	// dial is what the connection is made to: host:port, or a dns:///
	// target as given.
	dial string
	// hostPort is the server's host:port, for labels and logs.
	hostPort string
	// tls says grpcs:// asked for TLS; plain says grpc:// asked for
	// plaintext. Neither leaves it to the collector's tls block.
	tls, plain bool
}

// parseGRPCTarget reads a target: host:port, dns:///host:port, or a
// grpc:// or grpcs:// URL of host:port. The port is required: gRPC has no
// port of its own to fall back on.
func parseGRPCTarget(raw string) (grpcAddress, error) {
	raw = strings.TrimSpace(raw)
	var a grpcAddress
	rest := raw
	switch {
	case strings.HasPrefix(rest, "grpcs://"):
		a.tls, rest = true, strings.TrimPrefix(rest, "grpcs://")
	case strings.HasPrefix(rest, "grpc://"):
		a.plain, rest = true, strings.TrimPrefix(rest, "grpc://")
	case strings.HasPrefix(rest, "dns:///"):
		rest = strings.TrimPrefix(rest, "dns:///")
	case strings.Contains(rest, "://"):
		scheme, _, _ := strings.Cut(rest, "://")
		return a, fmt.Errorf("target %q has the scheme %s://; a grpc target is host:port, dns:///host:port, grpc://host:port or grpcs://host:port", raw, scheme)
	}
	rest = strings.TrimSuffix(rest, "/")
	if strings.ContainsAny(rest, "/?#@") {
		return a, fmt.Errorf("target %q has a path, query or user in it; a grpc target is only host:port, and the method is request.rpc", raw)
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil || host == "" {
		return a, fmt.Errorf("target %q is not host:port; a grpc target names the server and its port, such as queue.internal:9090", raw)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return a, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	a.hostPort = net.JoinHostPort(host, port)
	a.dial = a.hostPort
	if strings.HasPrefix(raw, "dns:///") {
		a.dial = "dns:///" + a.hostPort
	}
	return a, nil
}

// useTLS says whether a call to a goes over TLS: grpcs://, or a tls block
// on a target that did not ask for plaintext with grpc://, which the two
// together contradict.
func (a grpcAddress) useTLS(c *model.Collector) (bool, error) {
	configured := c.Request.TLS != model.TLSConfig{}
	if a.plain && configured {
		return false, errors.New("target asks for plaintext with grpc://, but the collector sets request.tls; use grpcs:// or a bare host:port")
	}
	return a.tls || configured, nil
}

// checkGRPCTarget checks a probe's or a static target's address.
func checkGRPCTarget(c *model.Collector, target string, _ bool) error {
	a, err := parseGRPCTarget(target)
	if err != nil {
		return err
	}
	_, err = a.useTLS(c)
	return err
}

// grpcLabel is the url label of a call: grpc://host:port/package.Service/Method,
// or grpcs:// over TLS.
func grpcLabel(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	a, err := parseGRPCTarget(target)
	if err != nil {
		return "", err
	}
	secure, err := a.useTLS(c)
	if err != nil {
		return "", err
	}
	scheme := "grpc"
	if secure {
		scheme = "grpcs"
	}
	return scheme + "://" + a.hostPort + "/" + c.Request.RPC, nil
}

// grpcDisplay renders a target for logs and labels: as written, since a
// grpc target holds no credentials, when it is one at all.
func grpcDisplay(target string) string {
	if _, err := parseGRPCTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// fetchGRPC makes a scrape's call (grpccall.go).
func fetchGRPC(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return callGRPC(ctx, target, c, overrides, forwarded)
}
