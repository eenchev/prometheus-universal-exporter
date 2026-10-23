package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Every collector declares how it reaches its data with request.type: http
// asks a URL, localfile reads a file from the exporter's own filesystem,
// graphite asks a Graphite render API for series, grpc calls a unary gRPC
// method, sshfile reads a file from another host over SFTP, sql runs a
// statement on a database, dns resolves a name, tlscert reads the
// certificates of a TLS handshake, s3file reads an object from S3 or a
// store that speaks its API, kubernetes lists objects from a Kubernetes API
// server, redis reads INFO and read-only commands from a Redis or Valkey
// server. The type is required rather than defaulted so that no configuration means
// "http" by accident, and so that each type can own its rules:
//
//   - Fields: the request keys it accepts. A key another type owns is an error
//     for this one, so a configuration cannot carry settings that are quietly
//     ignored.
//   - Overrides: the /probe parameters it accepts. A parameter that belongs to
//     some other type is rejected with 400 rather than ignored.
//   - TargetFields: the keys a static target's request block may set.
//   - Validate: the type's required fields, defaults and cross-field rules.
//   - Fetch: how a scrape actually gets its bytes. Everything after it —
//     decoding, transforms, limits, caching — is shared by every type.
//   - What a target is: whether a probe or static target may leave it
//     out, how it is checked, how it appears in logs, and the url and
//     http_method labels of the verbose self-metrics. Left unset, these are
//     http's.

// The request types in the source tree.
const (
	RequestTypeHTTP       = "http"
	RequestTypeLocalFile  = "localfile"
	RequestTypeGraphite   = "graphite"
	RequestTypeGRPC       = "grpc"
	RequestTypeSSHFile    = "sshfile"
	RequestTypeSQL        = "sql"
	RequestTypeDNS        = "dns"
	RequestTypeTLSCert    = "tlscert"
	RequestTypeS3File     = "s3file"
	RequestTypeKubernetes = "kubernetes"
	RequestTypeRedis      = "redis"
)

// knownRequestTypes is every request type in the source tree, whether or not
// this binary was built with it, so a configuration that names a type the
// build left out is told that, rather than that the type does not exist. A
// test keeps it in step with the requesttype_<name>.go files.
var knownRequestTypes = []string{RequestTypeHTTP, RequestTypeLocalFile, RequestTypeGraphite, RequestTypeGRPC, RequestTypeSSHFile, RequestTypeSQL, RequestTypeDNS, RequestTypeTLSCert, RequestTypeS3File, RequestTypeKubernetes, RequestTypeRedis}

// optInRequestTypes are the types of knownRequestTypes that a default build
// leaves out, as it leaves out the sql type's opt-in drivers: s3file links
// the AWS SDK, which few deployments want in the binary, and redis, which
// needs nothing outside the standard library, is kept out of the binary of
// every deployment that has no Redis to read. A build carries one only when
// asked, with its request_type_<name> tag, and its file is built
// under that tag alone. A test keeps the list in step with the files'
// constraints.
var optInRequestTypes = []string{RequestTypeS3File, RequestTypeRedis}

// RequestType is how one request.type reaches its data. Fields lists the
// request keys it accepts, Overrides the probe parameters that may change them
// and TargetFields the keys a static target may set; Validate checks a
// collector's request and fills in its defaults, and Fetch performs one.
// Each type registers itself in RequestTypes.
type RequestType struct {
	Name         string
	Fields       []string
	Overrides    []string // a name ending in "_" is a prefix, such as header_
	TargetFields []string
	Validate     func(c *model.Collector) error
	Fetch        func(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error)

	// OptionalTarget lets a probe or a static target leave target out.
	OptionalTarget bool
	// CheckTarget validates a target before anything is fetched: a probe's
	// target, answered with 400 when it fails, and a static target's, at
	// load. static tells the two apart. Unset, any target is accepted.
	CheckTarget func(c *model.Collector, target string, static bool) error
	// Label and Method give the url and http_method labels of the verbose
	// self-metrics. Unset, they are http's.
	Label  func(target string, c *model.Collector, overrides RequestOverrides) (string, error)
	Method func(c *model.Collector, overrides RequestOverrides) string
	// Display renders a target for logs, error bodies and the target label of
	// a static target's health series. Unset, it is safeTarget.
	Display func(target string) string
	// Stage names the fetch stage in logs and error bodies. Unset, "http".
	Stage string
	// CheckOverride lets a collector refuse a probe parameter, or a static
	// target's request key, that its type accepts but its configuration has
	// no use for, such as path for a localfile collector reading a directory.
	// Unset, everything the type accepts is accepted.
	CheckOverride func(c *model.Collector, key string) error
	// Query adds the query parameters the type builds itself to the URL
	// of a request made over HTTP, such as graphite's repeated target, which
	// request.query, one value to a name, cannot hold. They are the type's
	// alone: a pair of the target's own query that names one is dropped
	// (withoutQueryKeys). Unset, it adds none.
	Query func(c *model.Collector, overrides RequestOverrides) (url.Values, error)
	// CheckTargetRequest checks the values of a static target's request
	// block, where CheckOverride sees only its keys. Unset, any value of an
	// accepted key is accepted.
	CheckTargetRequest func(c *model.Collector, t *model.StaticTarget) error
	// CheckPath checks the value of the path probe parameter, which is
	// refused with what it returns, before anything is sent. Unset, the
	// value is checked when the request is made.
	CheckPath func(what, path string) error
	// CheckFiles reads the files a collector's request names, or a static
	// target's, when t is set, as a request would read them, and says what
	// a request would find wrong with them, for --dry-run. It is the check
	// of files read at a request, which the configuration's loading does not
	// read. Unset, nothing is read.
	CheckFiles func(c *model.Collector, t *model.StaticTarget) []string
	// URLPath says the type's path is a URL path, joined onto the target,
	// which can hold no query or fragment; localfile's is a file's.
	URLPath bool
	// StatusCodes says the type retries by gRPC status code: request.retry
	// takes codes, and not non_idempotent, which is about HTTP methods.
	// Unset, it is the other way round.
	StatusCodes bool
	// ReadFiles says which paths checking a collector's request looked at on
	// disk, the files the request names and those that reading them led to,
	// and gives a mark of that reading (ReadFiles). Unset, checking a
	// request reads no file but those it names.
	ReadFiles func(c *model.Collector) (paths []string, read string)
	// Follow is told the collectors and static targets in force whenever
	// the per-collector state follows a configuration (FollowConfiguration),
	// so a type that keeps connections across scrapes closes those nothing
	// in force uses any more. Unset, the type keeps none.
	Follow func(collectors []model.Collector, targets []model.StaticTarget)
	// CloseConnections closes every connection the type keeps, at shutdown.
	// Unset, it keeps none.
	CloseConnections func()
	// Connections reports, of a collector, the connections the type keeps
	// open for it now and how many it has made, for the type's own
	// self-metrics. Unset, the type keeps none.
	Connections func(collector string) (open int, made uint64)
	// ConnectionLimit reports, of a collector with a limit on its
	// connections, how many of its scrapes waited for room at the limit and
	// how many failed there. Unset, the type has no such limit.
	ConnectionLimit func(collector string) (waited, failed uint64)
	// ConnectionPools reports, of a collector, what the type's pools of
	// connections hold now (PoolStats). Unset, the type keeps no pools.
	ConnectionPools func(collector string) PoolStats
	// Logins reports how many logins a collector's request.login made and
	// how many of them failed (login.go). Unset, the type does not log in.
	Logins func(collector string) (made, failed uint64)
}

// CollectorLogins reports how many logins a collector of requestType made
// and how many failed (RequestType.Logins); zero for a type that does not
// log in.
func CollectorLogins(requestType, collector string) (made, failed uint64) {
	if rt := RequestTypes[requestType]; rt != nil && rt.Logins != nil {
		return rt.Logins(collector)
	}
	return 0, 0
}

// PoolStats is what a collector's pools of connections hold: the
// connections idle in them, how many targets they are open to, how
// many keepalive checks of a connection failed, and the rows the last
// statement returned, Rows being set only once one has (a sql collector's,
// requesttype_sql.go).
type PoolStats struct {
	Idle              int
	Targets           int
	KeepaliveFailures uint64
	Rows              *int
}

// CollectorPools reports what the pools of a collector of requestType hold
// (RequestType.ConnectionPools); nothing for a type that keeps none.
func CollectorPools(requestType, collector string) PoolStats {
	if rt := RequestTypes[requestType]; rt != nil && rt.ConnectionPools != nil {
		return rt.ConnectionPools(collector)
	}
	return PoolStats{}
}

// FileWarnings reads the files the request of c names, or of the static
// target t when it is set, as a request would, and says what a request
// would find wrong with them (RequestType.CheckFiles); none for a type that
// reads no file at a request.
func FileWarnings(c *model.Collector, t *model.StaticTarget) []string {
	if rt := requestTypeOf(c); rt != nil && rt.CheckFiles != nil {
		return rt.CheckFiles(c, t)
	}
	return nil
}

// FollowConfiguration tells every built type that keeps connections across
// scrapes which collectors and static targets are in force now
// (RequestType.Follow).
func FollowConfiguration(collectors []model.Collector, targets []model.StaticTarget) {
	rateLimits.follow(collectors)
	for _, name := range BuiltRequestTypes() {
		if rt := RequestTypes[name]; rt.Follow != nil {
			rt.Follow(collectors, targets)
		}
	}
}

// CloseConnections closes every connection a built type keeps across
// scrapes, for the exporter's shutdown, and forgets the rate limits' buckets
// and counts with them.
func CloseConnections() {
	rateLimits.reset()
	for _, name := range BuiltRequestTypes() {
		if rt := RequestTypes[name]; rt.CloseConnections != nil {
			rt.CloseConnections()
		}
	}
}

// CollectorConnections reports the connections a collector of requestType
// has open now and how many it has made (RequestType.Connections); zero for
// a type that keeps none.
func CollectorConnections(requestType, collector string) (open int, made uint64) {
	if rt := RequestTypes[requestType]; rt != nil && rt.Connections != nil {
		return rt.Connections(collector)
	}
	return 0, 0
}

// CollectorConnectionLimit reports how many scrapes of a collector of
// requestType waited for room at its limit on connections, and how many
// failed there (RequestType.ConnectionLimit); zero for a type without one.
func CollectorConnectionLimit(requestType, collector string) (waited, failed uint64) {
	if rt := RequestTypes[requestType]; rt != nil && rt.ConnectionLimit != nil {
		return rt.ConnectionLimit(collector)
	}
	return 0, 0
}

// ReadFiles returns the paths that checking a collector's request looked at
// on disk when the files the request names lead to others: for a grpc
// collector with descriptors: proto, its .proto files, the files those
// import, and the places an imported file was looked for at and not found.
// The watch of the configuration stamps them while a reload is refused, so
// a reload refused for an imported file is tried again when that file
// changes or appears, as one refused for a file the configuration names is,
// and while the configuration is in force, so one changing is checked
// against the configuration at the next tick.
//
// The files are read now when they were never read, or changed since they
// were. read is a mark of the reading the paths are of: the same mark from a
// later call says nothing the reading looked at has changed between the
// reading and that call.
//
// The collector need not have been validated: the watch asks before the
// configuration is, to stamp the files before the validation reads them. Its
// request.type is therefore taken as ValidateRequest names it, whatever its
// case and the blanks around it, so that a collector written `type: GRPC`
// has the files it will have once it is validated. Of a validated collector,
// whose type is written as the types are named, this changes nothing.
func ReadFiles(c *model.Collector) (paths []string, read string) {
	if rt := RequestTypes[strings.ToLower(strings.TrimSpace(c.Request.Type))]; rt != nil && rt.ReadFiles != nil {
		return rt.ReadFiles(c)
	}
	return nil, ""
}

// RequestTypes is the registry of the types built into this binary. Each type
// registers itself from its own requesttype_<name>.go file, whose build
// constraint decides whether the type is included:
//
//	//go:build !select_request_types || request_type_<name>
//
// A default build carries every type but the opt-in ones (optInRequestTypes),
// whose files are built under their own tag alone:
//
//	//go:build request_type_<name>
//
// so -tags request_type_s3file is a default build with s3file added, and
// -tags request_type_s3file,request_type_redis one with both. Building
// with -tags select_request_types,request_type_http carries only the listed
// ones; the others, and everything only they import, are left out of the
// binary.
//
// The map is a variable, rather than being built once, only so a test can
// register a fixture type to exercise the per-type rules with more than one
// type.
var RequestTypes = map[string]*RequestType{}

// registerRequestType adds a type to the registry. It is called from the
// init function of the type's file, so a duplicate is a programming error.
func registerRequestType(rt *RequestType) {
	if _, exists := RequestTypes[rt.Name]; exists {
		panic(fmt.Sprintf("request type %q registered twice", rt.Name))
	}
	RequestTypes[rt.Name] = rt
}

// BuiltRequestTypes lists the types this binary was built with, sorted.
func BuiltRequestTypes() []string {
	names := make([]string, 0, len(RequestTypes))
	for name := range RequestTypes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// supportedRequestTypes lists the built types for error messages.
func supportedRequestTypes() string {
	return strings.Join(BuiltRequestTypes(), ", ")
}

// suggestedRequestType is the type the message for a missing request.type
// shows: http, the one most collectors want, or, in a build that left http
// out, the first of the types it does carry, so that what the message says
// to write is never a type this build goes on to refuse.
func suggestedRequestType() string {
	built := BuiltRequestTypes()
	if len(built) == 0 || slices.Contains(built, RequestTypeHTTP) {
		return RequestTypeHTTP
	}
	return built[0]
}

// ValidateRequest checks a collector's request block: the type is present and
// known, every key set belongs to that type, and the type's own rules hold.
// The keys are checked before the type validates, so a default the type fills
// in, such as method GET, is never mistaken for something the author wrote.
func ValidateRequest(c *model.Collector) error {
	c.Request.Type = strings.ToLower(strings.TrimSpace(c.Request.Type))
	if c.Request.Type == "" {
		return fmt.Errorf("collector %q has no request.type; it is required, and the supported types are: %s (add `type: %s` to its request block)", c.Name, supportedRequestTypes(), suggestedRequestType())
	}
	rt, ok := RequestTypes[c.Request.Type]
	if !ok && slices.Contains(optInRequestTypes, c.Request.Type) {
		return fmt.Errorf("collector %q uses request.type %q, which this build of the exporter does not include; it was built with: %s. %s is an opt-in type, which the default build and the published image leave out: build with OPT_IN_REQUEST_TYPES=%s (make build, or the Dockerfile's build argument), which adds the tag request_type_%s", c.Name, c.Request.Type, supportedRequestTypes(), c.Request.Type, c.Request.Type, c.Request.Type)
	}
	if !ok && slices.Contains(knownRequestTypes, c.Request.Type) {
		return fmt.Errorf("collector %q uses request.type %q, which this build of the exporter does not include; it was built with: %s. Use a build that includes it: the default build includes every type, and a build with -tags select_request_types needs request_type_%s in its tags too", c.Name, c.Request.Type, supportedRequestTypes(), c.Request.Type)
	}
	if !ok {
		return fmt.Errorf("collector %q has unsupported request.type %q; the supported types are: %s", c.Name, c.Request.Type, supportedRequestTypes())
	}
	for _, key := range setKeys(reflect.ValueOf(c.Request), "type") {
		if !slices.Contains(rt.Fields, key) {
			return fmt.Errorf("collector %q sets request.%s, which does not apply to request.type %q", c.Name, key, rt.Name)
		}
	}
	if err := checkRetryKeys(rt, c.Request.Retry.Codes != nil, c.Request.Retry.Statuses != nil, c.Request.Retry.NonIdempotent, c.Request.Retry.RetryAfter); err != nil {
		return fmt.Errorf("collector %q %w", c.Name, err)
	}
	if c.Request.MaxResponseBytes < 0 {
		return fmt.Errorf("collector %q request.max_response_bytes must not be negative", c.Name)
	}
	if err := validateTargetPolicy(c); err != nil {
		return err
	}
	if err := validateAcceptStatus(c); err != nil {
		return err
	}
	if err := validateRetryStatuses(c); err != nil {
		return err
	}
	if err := validateRateLimit(c); err != nil {
		return err
	}
	return rt.Validate(c)
}

// checkRetryKeys refuses the retry key that does not apply to the type:
// codes, the gRPC status codes a grpc request retries, for the others, and
// statuses, non_idempotent and retry_after, about HTTP answers and methods,
// for grpc.
func checkRetryKeys(rt *RequestType, codes, statuses, nonIdempotent, retryAfter bool) error {
	if codes && !rt.StatusCodes {
		return fmt.Errorf("sets request.retry.codes, which applies only to request.type grpc; a %s request is retried on a failed connection and on the HTTP statuses retry.statuses lists, 408, 425, 429 and 5xx when it is left out", rt.Name)
	}
	if statuses && rt.StatusCodes {
		return fmt.Errorf("sets request.retry.statuses, which does not apply to request.type %q; retry.codes says which gRPC status codes are retried", rt.Name)
	}
	if nonIdempotent && rt.StatusCodes {
		return fmt.Errorf("sets request.retry.non_idempotent, which does not apply to request.type %q; retry.codes says which gRPC status codes are retried", rt.Name)
	}
	if retryAfter && rt.StatusCodes {
		return fmt.Errorf("sets request.retry.retry_after, which does not apply to request.type %q; a gRPC status carries no Retry-After header, and retry.backoff is the wait between its retries", rt.Name)
	}
	return nil
}

// requestTypeOf returns the registered type of a validated collector.
func requestTypeOf(c *model.Collector) *RequestType {
	return RequestTypes[c.Request.Type]
}

// knownOverrides is every /probe parameter some request type accepts. A
// parameter outside it is not an override at all and is ignored, as it always
// was — a monitor may carry parameters of its own — but one inside it that the
// collector's type does not accept is a mistake worth a 400.
func knownOverrides() []string {
	var all []string
	for _, rt := range RequestTypes {
		all = append(all, rt.Overrides...)
	}
	return all
}

// CheckOverrideParams rejects probe parameters that belong to a different
// request type than the collector's.
func CheckOverrideParams(c *model.Collector, values url.Values) error {
	rt := requestTypeOf(c)
	if rt == nil {
		return fmt.Errorf("collector %q has no registered request type", c.Name)
	}
	known := knownOverrides()
	var rejected []string
	for key := range values {
		if key == "target" || key == "collector" {
			continue
		}
		if matchesOverride(known, key) && !matchesOverride(rt.Overrides, key) {
			rejected = append(rejected, key)
			continue
		}
		if rt.CheckOverride != nil && matchesOverride(rt.Overrides, key) {
			if err := rt.CheckOverride(c, key); err != nil {
				return fmt.Errorf("probe parameter %s: %w", key, err)
			}
		}
		if key == "path" && rt.URLPath {
			if err := checkURLPath(values.Get(key)); err != nil {
				return fmt.Errorf("probe parameter path %w", err)
			}
		}
		if key == "path" && rt.CheckPath != nil {
			if err := rt.CheckPath("probe parameter path", values.Get(key)); err != nil {
				return err
			}
		}
	}
	if len(rejected) == 0 {
		return nil
	}
	sort.Strings(rejected)
	return fmt.Errorf("probe parameters %s do not apply to collector %q, whose request.type is %q", strings.Join(rejected, ", "), c.Name, rt.Name)
}

// CheckTargetRequest rejects a static target request block that sets keys
// its collector's request type does not accept. It writes nothing into the
// target, which may be in force and read by a scrape (NormalizeTargetRequest).
func CheckTargetRequest(t *model.StaticTarget, c *model.Collector) error {
	rt := requestTypeOf(c)
	if rt == nil {
		return fmt.Errorf("target %q uses collector %q, which has no registered request type", t.Name, c.Name)
	}
	keys := setKeys(reflect.ValueOf(&t.Request).Elem())
	if t.Request.PathSet && !slices.Contains(keys, "path") {
		keys = append(keys, "path")
	}
	if t.Request.BodySet && !slices.Contains(keys, "body") {
		keys = append(keys, "body")
	}
	for _, key := range keys {
		if !slices.Contains(rt.TargetFields, key) {
			return fmt.Errorf("target %q sets request.%s, which does not apply to collector %q, whose request.type is %q", t.Name, key, c.Name, rt.Name)
		}
		if rt.CheckOverride != nil {
			if err := rt.CheckOverride(c, key); err != nil {
				return fmt.Errorf("target %q sets request.%s: %w", t.Name, key, err)
			}
		}
	}
	if retry := t.Request.Retry; retry != nil {
		if err := checkRetryKeys(rt, retry.Codes != nil, retry.Statuses != nil, retry.NonIdempotent != nil, retry.RetryAfter != nil); err != nil {
			return fmt.Errorf("target %q %w", t.Name, err)
		}
	}
	if err := checkAcceptStatus(t.Request.AcceptStatus); err != nil {
		return fmt.Errorf("target %q request.accept_status %w", t.Name, err)
	}
	if err := checkTargetRetryStatuses(t, c); err != nil {
		return fmt.Errorf("target %q %w", t.Name, err)
	}
	if t.Request.RateLimit != nil {
		if err := checkRateLimit(t.Request.RateLimit); err != nil {
			return fmt.Errorf("target %q request.rate_limit%w", t.Name, err)
		}
	}
	if rt.URLPath && t.Request.PathSet {
		if err := checkURLPath(t.Request.Path); err != nil {
			return fmt.Errorf("target %q request.path %w", t.Name, err)
		}
	}
	if err := checkHeaderNames(t.Request.Headers); err != nil {
		return fmt.Errorf("target %q request.headers %w", t.Name, err)
	}
	for _, name := range model.SortedKeys(t.Request.Headers) {
		if err := checkHeaderValue(t.Request.Headers[name]); err != nil {
			return fmt.Errorf("target %q request.headers %s %w", t.Name, name, err)
		}
	}
	if err := checkInlineCredentials(t.Request.BearerToken, t.Request.BasicAuth); err != nil {
		return fmt.Errorf("target %q %w", t.Name, err)
	}
	if rt.CheckTargetRequest != nil {
		if err := rt.CheckTargetRequest(c, t); err != nil {
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
	}
	return nil
}

// FetchCollector gets a scrape's bytes the way the collector's type does.
func FetchCollector(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	rt := requestTypeOf(c)
	if rt == nil {
		return nil, fmt.Errorf("collector %q has no registered request type", c.Name)
	}
	response, err := rt.Fetch(ctx, target, c, overrides, forwarded)
	// However a type's fetch failed, the error goes to logs, probe answers
	// and debug reports without the credentials a URL in it carries, with
	// a URL as long as a scraper or a redirect made it shown by its start
	// (shortURLErrors), and is the same failure to the log whichever
	// connection it happened on.
	return response, sameFetchFailure(shortURLErrors(RedactURLErrors(err)))
}

// ErrMissingTarget is CheckTarget's answer to a target left out by a type
// that needs one; each caller words it for its own audience.
var ErrMissingTarget = errors.New("target is required")

// CheckTarget validates a probe's or a static target's target for the
// collector's type.
func CheckTarget(c *model.Collector, target string, static bool) error {
	rt := requestTypeOf(c)
	if rt == nil {
		return fmt.Errorf("collector %q has no registered request type", c.Name)
	}
	if strings.TrimSpace(target) == "" && !rt.OptionalTarget {
		return ErrMissingTarget
	}
	if rt.CheckTarget != nil {
		return rt.CheckTarget(c, target, static)
	}
	return nil
}

// RequestLabelFor is the url label of a request, by the collector's type.
func RequestLabelFor(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	if rt := requestTypeOf(c); rt != nil && rt.Label != nil {
		return rt.Label(target, c, overrides)
	}
	return requestLabel(target, c, overrides)
}

// RequestMethodFor is the http_method label of a request, by the collector's
// type.
func RequestMethodFor(c *model.Collector, overrides RequestOverrides) string {
	if rt := requestTypeOf(c); rt != nil && rt.Method != nil {
		return rt.Method(c, overrides)
	}
	return requestMethod(c, overrides)
}

// DisplayTarget renders a target for logs and labels, by the collector's type.
func DisplayTarget(c *model.Collector, target string) string {
	if rt := requestTypeOf(c); rt != nil && rt.Display != nil {
		return rt.Display(target)
	}
	return safeTarget(target)
}

// FetchStage names the stage a failed fetch is reported under.
func FetchStage(c *model.Collector) string {
	if rt := requestTypeOf(c); rt != nil && rt.Stage != "" {
		return rt.Stage
	}
	return "http"
}

// setKeys returns the yaml keys of a struct's fields that hold a non-zero
// value, skipping the keys named in skip and fields that are not read from
// YAML at all.
func setKeys(v reflect.Value, skip ...string) []string {
	var keys []string
	for _, field := range yamlKeysOf(v.Type()) {
		if slices.Contains(skip, field.key) {
			continue
		}
		if !v.Field(field.index).IsZero() {
			keys = append(keys, field.key)
		}
	}
	return keys
}

// Which field of a struct has which yaml key depends on the struct's type
// alone, while reading it out of the fields' tags cost more than half the
// check of a static target file against the configuration, which asks for
// every target (CheckTargetRequest). So the keys of a type are read once,
// the first time it is asked about, and kept by the type; what is set is
// still looked at in each value.

// yamlField is a struct field read from YAML: its index and its yaml key.
type yamlField struct {
	index int
	key   string
}

// yamlKeysByType holds the yamlFields of each struct type setKeys was asked
// about, by its reflect.Type: in field order, without the fields that have
// no yaml key or the key "-".
var yamlKeysByType sync.Map

// yamlKeysReadHook, set by tests, is called whenever the yaml keys of a
// struct type are read from its tags (yamlKeysOf), so a test can see that
// it is once for a type, not once for each value.
var yamlKeysReadHook atomic.Pointer[func()]

// yamlKeysOf is the fields of the struct type t that are read from YAML,
// with their keys.
func yamlKeysOf(t reflect.Type) []yamlField {
	if known, ok := yamlKeysByType.Load(t); ok {
		return known.([]yamlField)
	}
	if hook := yamlKeysReadHook.Load(); hook != nil {
		(*hook)()
	}
	var fields []yamlField
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if key == "" || key == "-" {
			continue
		}
		fields = append(fields, yamlField{index: i, key: key})
	}
	known, _ := yamlKeysByType.LoadOrStore(t, fields)
	return known.([]yamlField)
}

func matchesOverride(overrides []string, key string) bool {
	for _, name := range overrides {
		if strings.HasSuffix(name, "_") {
			if strings.HasPrefix(key, name) {
				return true
			}
			continue
		}
		if key == name {
			return true
		}
	}
	return false
}
