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
var grpcCodeNames = model.GRPCCodeNames

// grpcDefaultRetryCodes are retried when retry.codes is left out: the server
// did not start the call, so repeating it cannot repeat what it did.
var grpcDefaultRetryCodes = []string{"UNAVAILABLE"}

// grpcRPC is the shape of request.rpc: a service's full name, its package
// first if it has one, a slash, and a method name.
var grpcRPC = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*\.)*[A-Za-z_][A-Za-z0-9_]*/[A-Za-z_][A-Za-z0-9_]*$`)

// grpcMetadataKey is what a metadata key may be made of: lower-case letters,
// digits and - _ . , as gRPC requires of a header name.
var grpcMetadataKey = regexp.MustCompile(`^[0-9a-z_.-]+$`)

// GRPCConfig is a grpc collector's own request keys: the method called,
// package.Service/Method; the request message in the protobuf JSON
// mapping; the request metadata; where the message types come from,
// reflection, protoset (ProtosetFile) or proto (ProtoFiles, compiled with
// ProtoImportPaths); and the gRPC status codes, other than OK, whose calls
// are answers rather than failures, such as [NOT_FOUND].
type GRPCConfig struct {
	RPC              string            `yaml:"rpc"`
	Message          string            `yaml:"message"`
	Metadata         map[string]string `yaml:"metadata"`
	Descriptors      string            `yaml:"descriptors"`
	ProtosetFile     string            `yaml:"protoset_file"`
	ProtoFiles       []string          `yaml:"proto_files"`
	ProtoImportPaths []string          `yaml:"proto_import_paths"`
	AcceptCodes      []string          `yaml:"accept_codes"`
}

// grpcRequest is a grpc collector's own request keys: the zero GRPCConfig
// of a collector built in code whose request has not been validated, which
// sets them (ValidateRequest).
func grpcRequest(c *model.Collector) GRPCConfig {
	own, _ := c.Request.Own.(GRPCConfig)
	return own
}

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeGRPC,
		Fields: []string{
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"forward_authorization", "forward_headers",
			"tls", "retry", "max_response_bytes",
			"allowed_targets", "denied_targets",
		},
		Config:   GRPCConfig{},
		KeyRules: grpcKeyRules,
		Policy: func(c *model.Collector) RequestPolicy {
			return RequestPolicy{AcceptCodes: grpcRequest(c).AcceptCodes}
		},
		Overrides: []string{
			"timeout", "insecure_skip_verify", "retry_attempts", "retry_backoff",
			"header_", PathParamPrefix, "message",
		},
		TargetFields: []string{
			"message", "metadata", "timeout", "insecure_skip_verify", "retry",
			"basic_auth", "basic_auth_file", "bearer_token", "bearer_token_file",
			"accept_codes", "tls",
		},
		Requester:          grpcRequester{},
		StatusCodes:        true,
		JSONAnswer:         true,
		TargetHint:         func(*model.Collector) string { return "host:port" },
		CheckTargetRequest: checkGRPCTargetRequest,
		Stage:              "grpc",
		Templates:          grpcTemplates,
		UnusedParams: func(c *model.Collector, overrides RequestOverrides, unused string) error {
			if overrides.Message != nil {
				return fmt.Errorf("probe parameters %s are not used: the message probe parameter replaces request.message, and nothing else in the request and no label value of the collector names them", unused)
			}
			return unusedParamsIn(c, unused, "its request.message, its metadata values or its label values")
		},
		SchemaRule: grpcSchemaRule,
		ReadFiles:  grpcReadFiles,
		NamedFiles: func(c *model.Collector) []string {
			own := grpcRequest(c)
			return append([]string{own.ProtosetFile}, own.ProtoFiles...)
		},
		CheckFiles: checkGRPCFiles,
	})
	templateFilterRules[grpcMessageKind] = grpcMessageFilter
	// The message probe parameter replaces request.message whole.
	templateReplacers[grpcMessageKind] = func(overrides RequestOverrides) bool { return overrides.Message != nil }
}

// grpcMessageKind is the kind of templated field of a grpc collector's
// request.message (templateFilterRules): it takes the body's filters and is
// written as the body is, but only by the filters that write JSON.
const grpcMessageKind = "message"

// grpcMessageField is a grpc collector's message as a templated field.
func grpcMessageField(text string) templateField {
	return templateField{"request.message", text, grpcMessageKind}
}

// grpcMessageFilter says why a placeholder of a message may not have its
// filter: a message is JSON, so only the filters that write JSON apply.
func grpcMessageFilter(where string, p pathPlaceholder) error {
	switch p.Filter {
	case "", "json", "number", "raw":
		return nil
	}
	return fmt.Errorf("%s placeholder {{%s}} has the filter |%s, which does not write JSON; a message takes |json for a string, |number for a number, or |raw", where, p.Name, p.Filter)
}

// grpcRequester is the grpc request type's Requester.
type grpcRequester struct{}

// Validate checks a grpc collector's request and fills in its defaults.
func (grpcRequester) Validate(c *model.Collector) error {
	return validateGRPCRequest(c)
}

// Fetch performs one request of a grpc collector.
func (grpcRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchGRPC(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks a grpc collector's target.
func (grpcRequester) CheckTarget(c *model.Collector, target string, static bool) error {
	return checkGRPCTarget(c, target, static)
}

// Label is the url label of a grpc request.
func (grpcRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	return grpcLabel(target, c, overrides)
}

// Method is the http_method label of a grpc request.
func (grpcRequester) Method(*model.Collector, RequestOverrides) string { return http.MethodPost }

// Display renders a grpc target for logs and labels.
func (grpcRequester) Display(target string) string {
	return grpcDisplay(target)
}

// grpcReadFiles is what compiling a collector's proto_files looked at
// (fileSets.protoLooked). A descriptor set is one file, which the request
// names, and reflection reads none.
func grpcReadFiles(c *model.Collector) (paths []string, read string) {
	if len(grpcRequest(c).ProtoFiles) == 0 || grpcRequest(c).Descriptors != descriptorsProto {
		return nil, ""
	}
	return descriptorFiles.protoLooked(grpcRequest(c).ProtoFiles, grpcRequest(c).ProtoImportPaths)
}

// validateGRPCRequest holds the grpc type's rules: the method's shape, the
// message and metadata, the descriptor source, the credentials and retries.
// With descriptors protoset or proto the method is looked up now, so a
// method that does not exist, streams, or takes another message fails the
// configuration rather than every scrape.
func validateGRPCRequest(x *model.Collector) error {
	r := &x.Request
	own := grpcRequest(x)
	own.RPC = strings.TrimPrefix(strings.TrimSpace(own.RPC), "/")
	x.Request.Own = own
	switch {
	case own.RPC == "":
		return fmt.Errorf("collector %q has no request.rpc; a grpc collector calls one method, written package.Service/Method, such as grpc.health.v1.Health/Check", x.Name)
	case strings.Contains(own.RPC, "{{"):
		return fmt.Errorf("collector %q request.rpc %q has a placeholder; a collector calls one method, so rpc takes none", x.Name, own.RPC)
	case !grpcRPC.MatchString(own.RPC):
		return fmt.Errorf("collector %q request.rpc %q is not a method; write the service's full name, a slash and the method, such as acme.queue.v1.QueueService/GetStats", x.Name, own.RPC)
	}
	if err := validateCredentials(x); err != nil {
		return err
	}
	if err := checkTLSSettings(r.TLS); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkGRPCMetadata(own.Metadata, true, r.BasicAuth != nil || r.BasicAuthFile != nil || r.BearerToken != "" || r.BearerTokenFile != ""); err != nil {
		return fmt.Errorf("collector %q request.metadata %w", x.Name, err)
	}
	if err := checkForwardHeaders(r.ForwardHeaders); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	for _, name := range r.ForwardHeaders {
		if err := checkGRPCMetadataName(strings.ToLower(strings.TrimSpace(name))); err != nil {
			return fmt.Errorf("collector %q request.forward_headers %q: %w", x.Name, name, err)
		}
	}
	if r.Retry.Attempts < 0 || r.Retry.Attempts > MaxRetryAttempts {
		return fmt.Errorf("collector %q request.retry.attempts must be from 0 to %d", x.Name, MaxRetryAttempts)
	}
	if r.Retry.Backoff < 0 {
		return fmt.Errorf("collector %q request.retry.backoff must not be negative", x.Name)
	}
	if err := normalizeRetryCodes(r.Retry.Codes); err != nil {
		return fmt.Errorf("collector %q request.retry.codes %w", x.Name, err)
	}
	if err := normalizeRetryCodes(own.AcceptCodes); err != nil {
		return fmt.Errorf("collector %q request.accept_codes %s", x.Name, strings.Replace(err.Error(), "never retried", "always accepted", 1))
	}
	// The placeholders of the message and the metadata values.
	for _, f := range requestTemplates(x, RequestOverrides{}) {
		if err := f.check(); err != nil {
			return fmt.Errorf("collector %q: %w", x.Name, err)
		}
	}
	message, typed, err := messageForCheck(own.Message)
	if err != nil {
		return fmt.Errorf("collector %q request.message %w", x.Name, err)
	}
	if err := validateDescriptorKeys(x); err != nil {
		return err
	}
	own = grpcRequest(x)
	if own.Descriptors == descriptorsReflection {
		// The server is asked at the first call; nothing more is known now.
		return nil
	}
	if x.DescriptorFilesUnread && own.Descriptors != "" {
		// The descriptor files are not opened (config.WithoutNamedFiles):
		// the replicas that mount them check the method and the message.
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
// keys go only with it. descriptors is taken as written, in lower case and
// without blanks around it, the one spelling the schemas take.
func validateDescriptorKeys(x *model.Collector) error {
	r := grpcRequest(x)
	service, _, _ := strings.Cut(r.RPC, "/")
	switch {
	case r.Descriptors == "" && service == grpcHealthService:
		// Its types are built in.
	case r.Descriptors == "":
		return fmt.Errorf("collector %q has no request.descriptors; say where the message types of %s come from: reflection, to ask the server's reflection service; protoset, to read a descriptor set from protoset_file; or proto, to compile the .proto files in proto_files", x.Name, service)
	case !slices.Contains(descriptorSources, r.Descriptors):
		return fmt.Errorf("collector %q request.descriptors is %q; want reflection, protoset or proto, in lower case", x.Name, r.Descriptors)
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
	f := grpcMessageField(text)
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
// read at load, fit the method; its metadata; its retry codes; and its own
// tls block, which a grpc:// target, asking for plaintext, contradicts.
func checkGRPCTargetRequest(c *model.Collector, t *model.StaticTarget) error {
	credentials := c.Request.BasicAuth != nil || c.Request.BasicAuthFile != nil || c.Request.BearerToken != "" || c.Request.BearerTokenFile != "" ||
		t.Request.BasicAuth != nil || t.Request.BasicAuthFile != nil || t.Request.BearerToken != "" || t.Request.BearerTokenFile != ""
	if err := checkGRPCMetadata(t.Request.Metadata, false, credentials); err != nil {
		return fmt.Errorf("request.metadata %w", err)
	}
	// The target may be in force, so copies are checked: its codes were
	// written in upper case when the file was loaded (NormalizeTargetRequest).
	if retry := t.Request.Retry; retry != nil {
		if err := normalizeRetryCodes(slices.Clone(retry.Codes)); err != nil {
			return fmt.Errorf("request.retry.codes %w", err)
		}
	}
	if err := normalizeRetryCodes(slices.Clone(t.Request.AcceptCodes)); err != nil {
		return fmt.Errorf("request.accept_codes %s", strings.Replace(err.Error(), "never retried", "always accepted", 1))
	}
	if err := checkTargetTLS(t); err != nil {
		return err
	}
	if a, err := parseGRPCTarget(t.Target); err == nil && a.plain && t.Request.TLS != nil {
		return errors.New("target asks for plaintext with grpc://, but sets request.tls of its own; use grpcs:// or a bare host:port")
	}
	if t.Request.Message == "" {
		return nil
	}
	if !json.Valid([]byte(t.Request.Message)) {
		return errors.New("request.message is not JSON; write the message in the protobuf JSON mapping")
	}
	if (grpcRequest(c).Descriptors == descriptorsProtoset || grpcRequest(c).Descriptors == descriptorsProto) && !c.DescriptorFilesUnread || grpcRequest(c).Descriptors == "" {
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
	// hostPort is the server's host:port, which the connection is made to
	// (grpcConnCache.get) and labels and logs name.
	hostPort string
	// tls says grpcs:// asked for TLS; plain says grpc:// asked for
	// plaintext. Neither leaves it to the collector's tls block.
	tls, plain bool
}

// parseGRPCTarget reads a target: host:port, dns:///host:port, or a
// grpc:// or grpcs:// URL of host:port. The port is required: gRPC has no
// port of its own to fall back on. The host unix is refused.
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
	// grpc-go dials an address unix:<path> as a unix socket, whatever
	// resolver produced it, and behind a proxy the address is the host:port
	// as written, never resolved: unix:8443 would be the socket 8443 on the
	// exporter's own machine, reached without the proxy or allowed_targets.
	// Every other host is safe as a name: grpc-go is handed dns:///host:port
	// (grpcConnCache.get), so unix-abstract or passthrough is only a name.
	if strings.EqualFold(host, "unix") {
		return a, fmt.Errorf("target %q names the host unix, which grpc-go would dial as a unix socket on the exporter's machine; a grpc target is a TCP server, host:port, such as queue.internal:9090", raw)
	}
	a.hostPort = net.JoinHostPort(host, port)
	return a, nil
}

// useTLS says whether a call to a goes over TLS: grpcs://, or a tls block,
// even an empty one (model.TLSConfig.Set), on a target that did not ask
// for plaintext with grpc://, which the two together contradict.
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
func grpcLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	c = withTargetAccount(c, overrides)
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
	return scheme + "://" + a.hostPort + "/" + grpcRequest(c).RPC, nil
}

// grpcDisplay renders a target for logs and labels: as written, since a
// grpc target holds no credentials, when it is one at all.
func grpcDisplay(target string) string {
	if _, err := parseGRPCTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// fetchGRPC makes a scrape's call (grpccall.go), with a static target's
// own tls block in place of the collector's: the connection is kept by its
// TLS settings (grpcConnKey), so targets with blocks of their own share no
// connection.
func fetchGRPC(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return callGRPC(ctx, target, withTargetAccount(c, overrides), overrides, forwarded)
}

// checkGRPCFiles reads the tls block's files as a connection reads them,
// the collector's or those of a static target's own block, for --dry-run.
// Loading the configuration reads none of them, since a file may be
// mounted after the exporter starts.
func checkGRPCFiles(c *model.Collector, t *model.StaticTarget) []string {
	return accountFileWarnings(c, t)
}

// grpcSchemaRule is what a grpc collector's request requires, as the
// exporter does (validateGRPCRequest): rpc, written with something other
// than "", and where its message types come from, descriptors, unless it
// calls the health service, whose types are built in
// (validateDescriptorKeys). The files of one source of descriptors go with
// it alone, as the exporter has them: protoset_file with descriptors:
// protoset, which requires it, and proto_files and proto_import_paths with
// descriptors: proto, which requires proto_files. The schema took either
// beside any source, and a source without its files.
func grpcSchemaRule() map[string]any {
	rule := SchemaWrittenKey("rpc")
	rule["if"] = map[string]any{"properties": map[string]any{"rpc": map[string]any{"not": map[string]any{"pattern": `^/?grpc\.health\.v1\.Health/`}}}}
	rule["then"] = SchemaWrittenKey("descriptors")
	source := func(name string) map[string]any {
		return map[string]any{"properties": map[string]any{"descriptors": map[string]any{"const": name}}, "required": []string{"descriptors"}}
	}
	listed := func(key string) map[string]any {
		return map[string]any{"properties": map[string]any{key: map[string]any{"minItems": 1}}, "required": []string{key}}
	}
	protosetFile := SchemaWrittenKey("protoset_file")
	protosetFile["properties"].(map[string]any)["protoset_file"].(map[string]any)["not"] = map[string]any{"anyOf": []any{map[string]any{"const": ""}, SchemaOnlyBlanks()}}
	rule["allOf"] = []any{
		map[string]any{"if": SchemaWrittenKey("protoset_file"), "then": source(descriptorsProtoset)},
		map[string]any{"if": map[string]any{"anyOf": []any{listed("proto_files"), listed("proto_import_paths")}}, "then": source(descriptorsProto)},
		map[string]any{"if": source(descriptorsProtoset), "then": protosetFile},
		map[string]any{"if": source(descriptorsProto), "then": listed("proto_files")},
	}
	return rule
}

// grpcTemplates are a grpc collector's message, which takes the body's
// filters that write JSON (grpcMessageKind), unless the probe replaced it,
// and its metadata values, which take the header rule.
func grpcTemplates(c *model.Collector, overrides RequestOverrides) []templateField {
	var out []templateField
	if overrides.Message == nil && HasPathParams(grpcRequest(c).Message) {
		out = append(out, grpcMessageField(grpcRequest(c).Message))
	}
	for _, name := range model.SortedKeys(grpcRequest(c).Metadata) {
		if value := grpcRequest(c).Metadata[name]; HasPathParams(value) {
			out = append(out, templateField{"request.metadata." + name, value, "header"})
		}
	}
	return out
}

// grpcKeyRules is what the configuration schema says of a grpc
// collector's own keys.
func grpcKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"rpc":                {"pattern": SchemaOptionalPattern(`^/?([A-Za-z_][A-Za-z0-9_]*\.)*[A-Za-z_][A-Za-z0-9_]*/[A-Za-z_][A-Za-z0-9_]*$`), "description": "grpc, required: the method called, package.Service/Method, such as grpc.health.v1.Health/Check. See docs/GRPC.md."},
		"message":            {"description": "grpc: the request message in the protobuf JSON mapping. Defaults to {}. May contain {{param_name}} placeholders, written as |json for a string, |number for a number, or |raw. The message probe parameter replaces it."},
		"metadata":           {"propertyNames": map[string]any{"pattern": "^[0-9a-z_.-]+$"}, "description": "grpc: request metadata. Keys are lower-case; -bin keys and the ones gRPC reserves are refused. Values may contain {{param_name}} placeholders."},
		"descriptors":        {"enum": SchemaOptionalEnum([]string{"reflection", "protoset", "proto"}), "description": "grpc, required but for grpc.health.v1.Health: where the message types come from. reflection asks the server's reflection service; protoset reads protoset_file; proto compiles proto_files."},
		"protoset_file":      {"description": "grpc with descriptors: protoset: a FileDescriptorSet with its imports, as protoc --descriptor_set_out --include_imports or buf build -o writes it. Read again when it changes."},
		"proto_files":        {"description": "grpc with descriptors: proto: the .proto files that define the service, compiled when the configuration loads and again when one changes."},
		"proto_import_paths": {"description": "grpc with descriptors: proto: the directories imports are resolved in, as protoc -I. Defaults to the directory of each file. The well-known types are built in."},
		"accept_codes":       {"items": map[string]any{"type": "string", "pattern": SchemaCaselessOneOf(model.GRPCCodeNames[1:]...)}, "description": "grpc: the gRPC status codes other than OK whose calls are answers rather than failures, by name, such as [NOT_FOUND]. Such a call is not retried; its rules see an empty object, the code as $status and the status message as $headers[\"grpc-message\"]."},
	}
}
