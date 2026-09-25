package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http/httpguts"
)

// HTTPResponse is what a fetch returned, whatever the request type: a
// status, headers and a body, or, for a localfile collector reading a
// directory, the files it read. Target and Collector say what was fetched,
// Duration how long it took.
type HTTPResponse struct {
	StatusCode int
	// NoStatus says the answer has no status of its own, as a local file's
	// has not: StatusCode is only what lets it through the status check.
	NoStatus bool
	// GRPCCode is a grpc call's status code: 0 for OK, or a code the
	// collector accepts (request.accept_codes).
	GRPCCode  *int
	Headers   http.Header
	Body      []byte
	Target    string
	Collector string
	Duration  time.Duration
	// Directory is set instead of Body by a localfile collector reading a
	// directory: every file it read, each to be decoded on its own.
	Directory *DirectoryRead
}

// Status is the answer's status as rules read it, $status in jq and yq and
// response.status_code in Python: the HTTP status, a grpc call's status code,
// or nil for an answer without one, such as a local file's.
func (r *HTTPResponse) Status() any {
	switch {
	case r == nil || r.NoStatus:
		return nil
	case r.GRPCCode != nil:
		return *r.GRPCCode
	case r.StatusCode != 0:
		return r.StatusCode
	}
	return nil
}

// GraphiteContentType is the Content-Type a local file of carbon plaintext
// lines is given by its extension, .graphite or .carbon, so decoder.type auto
// reads it with the graphite decoder.
const GraphiteContentType = "text/x-graphite"

// RequestOverrides are the probe parameters that change a collector's request
// for one probe. An unset field leaves the collector's setting in force.
type RequestOverrides struct {
	Method             string
	Path               string
	PathSet            bool
	Timeout            time.Duration
	Body               *string
	InsecureSkipVerify *bool
	RetryAttempts      *int
	// RetryNonIdempotent is a static target's retry.non_idempotent; no
	// probe parameter sets it.
	RetryNonIdempotent *bool
	RetryBackoff       *time.Duration
	FollowRedirects    *bool
	EnableHTTP2        *bool
	// Targets, From and Until are a static target's request.targets, from
	// and until, replacing a graphite collector's; no probe parameter sets
	// them. Targets is nil when the target does not set it.
	Targets []string
	From    string
	Until   string
	// Message replaces a grpc collector's request.message: the message
	// probe parameter, or a static target's request.message. Metadata is a
	// static target's request.metadata, sent besides the collector's, and
	// RetryCodes its retry.codes; no probe parameter sets them.
	Message    *string
	Metadata   map[string]string
	RetryCodes []string
	// AcceptStatus and AcceptCodes are a static target's request.accept_status
	// and accept_codes, replacing the collector's when not nil; no probe
	// parameter sets them.
	AcceptStatus []string
	AcceptCodes  []string
	// formPost sends the query as a form body with POST instead of in the
	// URL, for a graphite request whose expressions are too long for a URL
	// (requesttype_graphite.go). Only the type's own fetch sets it.
	formPost bool
	// Params are the param_<name> probe parameters, bound into the
	// {{param_<name>}} placeholders of the collector's request.path.
	Params map[string]string
}

// parseBoolOverride reads an optional boolean probe parameter. An absent
// parameter leaves the collector setting in force; a present one must be
// exactly true or false so a typo cannot quietly select a default.
func parseBoolOverride(values url.Values, name string) (*bool, error) {
	value, ok := values[name]
	if !ok {
		return nil, nil
	}
	raw := ""
	if len(value) > 0 {
		raw = strings.TrimSpace(value[0])
	}
	var parsed bool
	switch strings.ToLower(raw) {
	case "true":
		parsed = true
	case "false":
		parsed = false
	default:
		return nil, fmt.Errorf("invalid %s override %q; want true or false", name, raw)
	}
	return &parsed, nil
}

// ParseRequestOverrides reads the overrides from a probe's query parameters,
// refusing a malformed one.
func ParseRequestOverrides(values url.Values) (RequestOverrides, error) {
	overrides := RequestOverrides{}
	if method := strings.TrimSpace(values.Get("method")); method != "" {
		overrides.Method = strings.ToUpper(method)
		switch overrides.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
		default:
			return overrides, fmt.Errorf("unsupported request method override %q", method)
		}
	}
	if path, ok := values["path"]; ok {
		overrides.PathSet = true
		if len(path) > 0 {
			overrides.Path = path[0]
		}
	}
	if timeout := strings.TrimSpace(values.Get("timeout")); timeout != "" {
		parsed, err := time.ParseDuration(timeout)
		if err != nil || parsed <= 0 {
			return overrides, fmt.Errorf("invalid request timeout override %q", timeout)
		}
		overrides.Timeout = parsed
	}
	if body, ok := values["body"]; ok {
		value := ""
		if len(body) > 0 {
			value = body[0]
		}
		overrides.Body = &value
	}
	if message, ok := values["message"]; ok {
		value := ""
		if len(message) > 0 {
			value = message[0]
		}
		overrides.Message = &value
	}
	insecure, err := parseBoolOverride(values, "insecure_skip_verify")
	if err != nil {
		return overrides, err
	}
	overrides.InsecureSkipVerify = insecure
	followRedirects, err := parseBoolOverride(values, "follow_redirects")
	if err != nil {
		return overrides, err
	}
	overrides.FollowRedirects = followRedirects
	enableHTTP2, err := parseBoolOverride(values, "enable_http2")
	if err != nil {
		return overrides, err
	}
	overrides.EnableHTTP2 = enableHTTP2
	if value, ok := values["retry_attempts"]; ok {
		raw := ""
		if len(value) > 0 {
			raw = strings.TrimSpace(value[0])
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return overrides, fmt.Errorf("invalid retry_attempts override %q; want a non-negative integer", raw)
		}
		overrides.RetryAttempts = &parsed
	}
	if value, ok := values["retry_backoff"]; ok {
		raw := ""
		if len(value) > 0 {
			raw = strings.TrimSpace(value[0])
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 0 {
			return overrides, fmt.Errorf("invalid retry_backoff override %q; want a non-negative Go duration", raw)
		}
		overrides.RetryBackoff = &parsed
	}
	for _, window := range []struct {
		name  string
		value *string
	}{{"from", &overrides.From}, {"until", &overrides.Until}} {
		value, ok := values[window.name]
		if !ok {
			continue
		}
		raw := ""
		if len(value) > 0 {
			raw = strings.TrimSpace(value[0])
		}
		if raw == "" || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
			return overrides, fmt.Errorf("invalid %s override %q; want a Graphite time such as -1h, now or 1727000000", window.name, raw)
		}
		*window.value = raw
	}
	params, err := pathParamValues(values)
	if err != nil {
		return overrides, err
	}
	overrides.Params = params
	return overrides, nil
}

// resolveRequestURL builds the URL a scrape actually requests. Both fetch and
// the verbose self-metric label go through it, so a label can never describe a
// different URL than the one that was fetched.
func resolveRequestURL(target string, c *model.Collector, overrides RequestOverrides) (*url.URL, error) {
	return buildRequestURL(target, c, overrides, true)
}

// buildRequestURL is resolveRequestURL with the choice of whether to bind path
// parameters. The self-metric label is built with bind false, so it shows the
// placeholder rather than the value; everything else is identical, which keeps
// the label describing the URL that was fetched.
func buildRequestURL(target string, c *model.Collector, overrides RequestOverrides, bind bool) (*url.URL, error) {
	u, err := url.Parse(normalizeTarget(target))
	if err != nil {
		return nil, fmt.Errorf("invalid target: %w", err)
	}
	if err := checkScheme(c, u.Scheme); err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("target has no host")
	}
	requestPath := c.Request.Path
	if overrides.PathSet {
		requestPath = overrides.Path
	}
	// Path parameters are bound only in the collector's own request.path. A
	// path probe parameter replaces it wholesale and is already the scrape's
	// own choice, so it is used exactly as given.
	var bound []string
	if bind && !overrides.PathSet && HasPathParams(requestPath) {
		requestPath, bound, err = bindPathParams(requestPath, overrides.Params)
		if err != nil {
			return nil, err
		}
	}
	if requestPath != "" {
		// The target's path is joined in both its forms, so an escape it
		// was written with, such as %2F inside a segment, is sent as
		// written rather than decoded into a new segment.
		rawBase := strings.TrimSuffix(u.EscapedPath(), "/")
		base := strings.TrimSuffix(u.Path, "/")
		p := strings.TrimPrefix(requestPath, "/")
		u.Path = path.Join("/", base, p)
		u.RawPath = path.Join("/", rawBase, (&url.URL{Path: p}).EscapedPath())
		if strings.HasSuffix(requestPath, "/") {
			u.Path += "/"
			u.RawPath += "/"
		}
		// A raw form the default escaping gives anyway is not kept; one
		// that does not decode to the path is ignored by Go, which then
		// escapes the path itself.
		if u.RawPath == (&url.URL{Path: u.Path}).EscapedPath() {
			u.RawPath = ""
		}
	}
	if len(bound) > 0 {
		applyPathParams(u, bound)
	}
	query := c.Request.Query
	if bind {
		// Placeholders in query values are filled in (requesttemplate.go);
		// the label, built with bind false, drops the query anyway.
		query, err = renderedValues(query, "query", "request.query.", overrides.Params)
		if err != nil {
			return nil, err
		}
	}
	q := u.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	// What the type builds itself goes last, over request.query; the
	// label drops the query, so it is built only for the request.
	if rt := requestTypeOf(c); bind && rt != nil && rt.Query != nil {
		own, err := rt.Query(c, overrides)
		if err != nil {
			return nil, err
		}
		for k, v := range own {
			q[k] = v
		}
	}
	u.RawQuery = q.Encode()
	return u, nil
}

// checkScheme refuses a target scheme request.allowed_schemes does not
// allow, http and https when it is unset.
func checkScheme(c *model.Collector, scheme string) error {
	allowed := c.Request.AllowedSchemes
	if len(allowed) == 0 {
		allowed = []string{"http", "https"}
	}
	for _, s := range allowed {
		if strings.EqualFold(s, scheme) {
			return nil
		}
	}
	return fmt.Errorf("target scheme %q is not allowed; request.allowed_schemes allows %s", scheme, strings.Join(allowed, ", "))
}

// checkURLPath refuses a URL path that holds a query or a fragment: path.Join
// would escape the ? and the #, and the target would be asked for a path
// with %3F in it. Query parameters belong in request.query.
func checkURLPath(p string) error {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		return fmt.Errorf("%q has a %c in it; a path is joined onto the target as a path, so it would be sent escaped as %s — put query parameters under request.query", p, p[i], url.PathEscape(string(p[i])))
	}
	return nil
}

// checkHeaderNames refuses a header name Go would refuse to send, failing
// every scrape, and two names that are one header once canonicalised, such
// as X-Tenant and x-tenant, of which a scrape would send either, by chance.
func checkHeaderNames(headers map[string]string) error {
	seen := map[string]string{}
	for _, name := range model.SortedKeys(headers) {
		if strings.Contains(name, "{{") {
			// A placeholder in a name is refused with its own message
			// (validateRequestTemplates).
			continue
		}
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("%q is not a header name; a name is letters, digits and !#$%%&'*+-.^_`|~, without spaces", name)
		}
		canonical := http.CanonicalHeaderKey(name)
		if other, ok := seen[canonical]; ok {
			return fmt.Errorf("%q and %q are the same header, whose names are not case-sensitive; set it once", other, name)
		}
		seen[canonical] = name
	}
	return nil
}

// checkHeaderValue refuses a header value with a control character other
// than tab, which Go refuses to send, failing every scrape.
func checkHeaderValue(value string) error {
	for _, r := range value {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return fmt.Errorf("has the control character %q in its value, which a header may not hold", r)
		}
	}
	return nil
}

// requestLabelURL renders a resolved URL for a metric label. Credentials in the
// userinfo and the whole query string are dropped: a collector's request.query
// or a probe parameter can carry a token or a tenant identifier, and a metric
// label is persisted by Prometheus and passed on to anything federating from it.
func requestLabelURL(u *url.URL) string {
	labelled := *u
	labelled.User = nil
	labelled.RawQuery = ""
	labelled.ForceQuery = false
	labelled.Fragment = ""
	labelled.RawFragment = ""
	return labelled.String()
}

// requestMethod reports the method a scrape will use.
func requestMethod(c *model.Collector, overrides RequestOverrides) string {
	if overrides.Method != "" {
		return overrides.Method
	}
	return c.Request.Method
}

func fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded ...http.Header) (*HTTPResponse, error) {
	u, err := resolveRequestURL(target, c, overrides)
	if err != nil {
		return nil, err
	}
	tlsSettings := c.Request.TLS
	if overrides.InsecureSkipVerify != nil {
		tlsSettings.InsecureSkipVerify = *overrides.InsecureSkipVerify
	}
	followRedirects := c.Request.FollowRedirects
	if overrides.FollowRedirects != nil {
		followRedirects = *overrides.FollowRedirects
	}
	// Go only negotiates HTTP/2 on a custom transport when it is asked to, so
	// this defaults to the protocol the exporter has always used. It applies to
	// HTTPS targets: cleartext HTTP/2 is not negotiated.
	enableHTTP2 := c.Request.EnableHTTP2
	if overrides.EnableHTTP2 != nil {
		enableHTTP2 = *overrides.EnableHTTP2
	}
	// The connection pool is shared with every request made with the same
	// TLS and HTTP/2 settings (transport.go).
	client, err := HTTPClient(TransportSettings{TLS: tlsSettings, EnableHTTP2: enableHTTP2}, followRedirects, 0)
	if err != nil {
		return nil, err
	}
	// The collector's allowed_targets and denied_targets, checked here, on
	// every redirect and on every connection the request makes, with the
	// transport's own idea of what goes through a proxy.
	var proxy func(*http.Request) (*url.URL, error)
	if transport, ok := client.Transport.(*http.Transport); ok {
		proxy = transport.Proxy
	}
	ctx, err = withTargetPolicy(ctx, c, u, proxy)
	if err != nil {
		return nil, err
	}
	requestContext := ctx
	cancel := func() {}
	if overrides.Timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, overrides.Timeout)
	}
	defer cancel()
	method := c.Request.Method
	if overrides.Method != "" {
		method = overrides.Method
	}
	requestBody := c.Request.Body
	if overrides.Body != nil {
		requestBody = *overrides.Body
	} else if HasPathParams(requestBody) {
		requestBody, err = templateField{"request.body", requestBody, "body"}.render(overrides.Params)
		if err != nil {
			return nil, err
		}
	}
	headers, err := renderedValues(c.Request.Headers, "header", "request.headers.", overrides.Params)
	if err != nil {
		return nil, err
	}
	retryAttempts := c.Request.Retry.Attempts
	retryBackoff := time.Duration(c.Request.Retry.Backoff)
	if overrides.RetryAttempts != nil {
		retryAttempts = *overrides.RetryAttempts
	}
	if overrides.RetryBackoff != nil {
		retryBackoff = *overrides.RetryBackoff
	}
	if retryAttempts < 0 {
		retryAttempts = 0
	}
	retryAnyMethod := c.Request.Retry.NonIdempotent
	if overrides.RetryNonIdempotent != nil {
		retryAnyMethod = *overrides.RetryNonIdempotent
	}
	contentType := ""
	if overrides.formPost {
		// The query, the type's own parameters and request.query alike,
		// goes in the body. The request only reads, so it is retried as a
		// GET would be.
		method, requestBody, contentType = http.MethodPost, u.RawQuery, "application/x-www-form-urlencoded"
		u.RawQuery = ""
		retryAnyMethod = true
	}
	if !retryAnyMethod && !IdempotentMethod(method) {
		retryAttempts = 0
	}
	if retryBackoff < 0 {
		retryBackoff = 0
	}

	authorization, err := requestAuthorization(c)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	limit := responseLimit(c)
	for attempt := 0; attempt <= retryAttempts; attempt++ {
		var reqBody io.Reader
		if requestBody != "" {
			reqBody = strings.NewReader(requestBody)
		}
		req, err := http.NewRequestWithContext(requestContext, method, u.String(), reqBody)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			setRequestHeader(req, k, v)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		if len(forwarded) > 0 {
			for k, v := range forwarded[0] {
				if strings.EqualFold(k, "Host") {
					if len(v) > 0 {
						req.Host = v[len(v)-1]
					}
					continue
				}
				req.Header[k] = append([]string(nil), v...)
			}
		}

		traceRequest(requestContext, req.Method, req.URL.String(), req.Header, req.Host, false)
		resp, err := client.Do(req)
		if err != nil {
			traceOutcome(requestContext, "error: "+err.Error())
			// A refused target is refused again on every attempt.
			if attempt < retryAttempts && requestContext.Err() == nil && !errors.Is(err, ErrTargetRefused) {
				if waitErr := waitRetry(requestContext, retryBackoff); waitErr != nil {
					return nil, fmt.Errorf("HTTP request failed: %w (the wait before retrying was cut short: %w)", err, waitErr)
				}
				continue
			}
			return nil, fmt.Errorf("HTTP request failed: %w", err)
		}
		traceOutcome(requestContext, resp.Status)
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("reading response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing response: %w", closeErr)
		}
		if int64(len(body)) > limit {
			return nil, model.MarkError(fmt.Errorf("response size %d exceeds limit %d", len(body), limit), model.ErrLimitExceeded)
		}
		response := &HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body, Target: target, Collector: c.Name, Duration: time.Since(start)}
		// An answer the collector accepts is its answer, not a failure to
		// retry, even a 503 it asked to read.
		if retryableStatus(resp.StatusCode) && !AcceptedStatus(c, overrides, resp.StatusCode) && attempt < retryAttempts {
			// A wait the deadline, a shutdown or the caller cut short keeps
			// what the target answered: the status and body say why the
			// scrape failed, where the bare context error would only say
			// that time ran out.
			if waitRetry(requestContext, retryBackoff) != nil {
				return response, nil
			}
			continue
		}
		return response, nil
	}
	// Every attempt returns or continues, and the last cannot continue.
	panic("unreachable")
}

// setRequestHeader sets a header of a request, Host included: Go sends the
// request's Host field and ignores a Host header, so a Host a collector or a
// static target sets — to reach a virtual host by an IP address, or through
// an ingress — goes there.
func setRequestHeader(req *http.Request, name, value string) {
	if strings.EqualFold(name, "Host") {
		req.Host = value
		return
	}
	req.Header.Set(name, value)
}

// defaultResponseLimit is the response limit of a collector that sets neither
// limits.max_response_bytes nor request.max_response_bytes.
const defaultResponseLimit = 10 << 20

// responseLimit is the most a collector reads from its target: the smaller of
// limits.max_response_bytes and request.max_response_bytes when both are set,
// the one that is set otherwise, and 10 MiB when neither is. Neither is
// filled in with the default, so either alone may raise the limit past it.
// Every request type reads through it.
func responseLimit(c *model.Collector) int64 {
	limit := c.Limits.MaxResponseBytes
	if limit <= 0 || c.Request.MaxResponseBytes > 0 && c.Request.MaxResponseBytes < limit {
		limit = c.Request.MaxResponseBytes
	}
	if limit <= 0 {
		limit = defaultResponseLimit
	}
	return int64(limit)
}

// IdempotentMethod reports whether sending a request with method twice has the
// effect of sending it once, so a failed one may be retried without asking:
// GET, HEAD, OPTIONS, TRACE, PUT and DELETE (RFC 9110). An empty method is
// GET.
func IdempotentMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500 && status <= 599
}

func waitRetry(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ReadCredentialFile reads a credential from the file at path, without the
// surrounding whitespace an editor or a Secret mount may leave.
func ReadCredentialFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// DirectoryRead is what reading a directory produced. It and FileRead live
// outside the localfile build tag because the probe and static target paths
// that consume them are shared by every request type.
type DirectoryRead struct {
	// Path is the directory, for logs.
	Path string
	// Files holds every matching file up to request.max_files, in name order.
	Files []FileRead
	// Matched counts the matching files; Skipped names those beyond
	// request.max_files, which were not looked at.
	Matched int
	Skipped []string
	// Bytes is how much was read across the files.
	Bytes int64
	// Listed counts the directory entries listed, and Truncated says the
	// listing stopped at its bound before the end of the directory.
	Listed    int
	Truncated bool
	// CutShort says the probe's deadline, or a shutdown, ended the read:
	// the files it had not read fail with that, and are no fault of theirs.
	CutShort bool
}

// FileRead is one file of a directory: a response to decode, or the reason
// there is none.
type FileRead struct {
	// Name is the file's name in the directory, its file label.
	Name     string
	ModTime  time.Time
	Response *HTTPResponse
	Err      error
}

// safeTarget renders a target for logs and error bodies with any credentials
// redacted. It must never fail: it runs on the error paths, including for a
// target that is not a URL at all.
func safeTarget(raw string) string {
	u, err := url.Parse(normalizeTarget(raw))
	if err != nil {
		// Unparseable, so the credentials cannot be located to redact them;
		// the raw text is withheld rather than risk echoing a password.
		return "<invalid target>"
	}
	if u.User != nil {
		u.User = url.UserPassword("redacted", "redacted")
	}
	return u.String()
}

// normalizeTarget gives a target without a scheme the default http:// one.
//
// Prometheus service discovery hands over __address__, which is host:port with
// no scheme, and the chart's monitors pass it straight through as target. It
// cannot simply be parsed and then checked for an empty scheme: url.Parse
// rejects 10.0.0.5:8080 outright ("first path segment cannot contain colon")
// and reads legacy.example:8080 as the scheme "legacy.example". So the
// decision is made on the text, before parsing: no "://" means no scheme.
// A target that wants https says so explicitly.
func normalizeTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	return "http://" + raw
}

// validateAcceptStatus checks request.accept_status at load.
func validateAcceptStatus(c *model.Collector) error {
	if err := normalizeAcceptStatus(c.Request.AcceptStatus); err != nil {
		return fmt.Errorf("collector %q request.accept_status %w", c.Name, err)
	}
	return nil
}

// normalizeAcceptStatus writes each entry in lower case, in place, and
// refuses one that is not a status from 100 to 599 or a class such as 2xx.
func normalizeAcceptStatus(entries []string) error {
	for i, raw := range entries {
		entry := strings.ToLower(strings.TrimSpace(raw))
		entries[i] = entry
		if len(entry) == 3 && entry[1:] == "xx" && entry[0] >= '1' && entry[0] <= '5' {
			continue
		}
		if code, err := strconv.Atoi(entry); err == nil && code >= 100 && code <= 599 {
			continue
		}
		return fmt.Errorf("entry %q is not an HTTP status from 100 to 599 or a class such as 2xx", raw)
	}
	return nil
}

// AcceptedStatus says whether a response with status is decoded: one of
// request.accept_status, a static target's replacing the collector's, or any
// 2xx when there is none.
func AcceptedStatus(c *model.Collector, overrides RequestOverrides, status int) bool {
	accepted := c.Request.AcceptStatus
	if overrides.AcceptStatus != nil {
		accepted = overrides.AcceptStatus
	}
	if len(accepted) == 0 {
		return status >= 200 && status < 300
	}
	code := strconv.Itoa(status)
	for _, entry := range accepted {
		if entry == code || (len(code) == 3 && entry[0] == code[0] && entry[1:] == "xx") {
			return true
		}
	}
	return false
}
