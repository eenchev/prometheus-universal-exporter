package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http/httpguts"
)

// HTTPResponse is what a fetch returned, whatever the request type. It holds
// a status, headers and a body. For a localfile collector reading a
// directory, it holds the files it read instead. Target and Collector say
// what was fetched, and Duration how long it took.
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
	// RedirectWithheld says where the answer came from when a redirect led
	// there and the request's headers were not sent to it. That happens when the
	// host is neither the request's own origin nor one of the collector's
	// request.redirect_trusted_hosts (redirecttrust.go). The text is the one the
	// error of a failing status continues with: "from <host>, where a redirect
	// led: ...". It is "" otherwise.
	RedirectWithheld string
	// NotRetried says why an answer with a retried status was not retried
	// though attempts were left, in the words its failing status's error
	// continues with. Under request.retry.retry_after, this is when its
	// Retry-After asked for a wait longer than the probe's time left. "" otherwise.
	NotRetried string
	// Directory is set instead of Body by a localfile collector reading a
	// directory: every file it read, each to be decoded on its own.
	Directory *DirectoryRead
	// Rows is a sql collector's answer: the rows its statement returned. Each
	// row is a map of its columns to their values. A column whose value is NULL
	// is left out. Body holds the same rows as JSON, for whatever shows an
	// answer. The decoder passes Rows to the transforms unchanged
	// (requesttype_sql.go).
	Rows []any
	// NamedRows is, of a sql collector of several statements
	// (request.statements), each one's rows, as Rows are, by its name; Rows
	// and Columns are then nil.
	NamedRows map[string]any
	// Columns holds the names of a sql collector's columns, in the statement's
	// order. It includes a column that is NULL in every row, which no row of Rows
	// has. A csv rule that reads such a column then reads empty cells, not a
	// column missing from the answer.
	Columns []string
	// Pages is what an http collector's request.pagination fetched: each page
	// requested, and whether it stopped at max_pages with another page still to
	// come. Body is then the pages merged into one document, and Headers are the
	// first page's. When a page after the first was answered with a status the
	// collector does not accept, StatusCode, Headers and Body are that page's
	// instead (pagination.go). Pages is nil without request.pagination.
	Pages *PagesRead
	// Conditional says the request carried the validators of a cached response
	// (revalidate.go). NotModified says the target answered it with 304 Not
	// Modified. The response is then the cached one, with its status and body,
	// and its headers updated from the 304's.
	Conditional, NotModified bool
	// Trip is what the request an http or graphite collector's answer
	// came from was besides its answer (HTTPTrip); nil for every other
	// request type, and for a response the debug page was given.
	Trip *HTTPTrip
}

// PagesRead is what request.pagination fetched for one trip.
type PagesRead struct {
	// Pages is every page asked for, in order, the first included.
	Pages []PageRead
	// MoreLeft says the trip stopped at request.pagination.max_pages while
	// the last page named another, or, for page and offset, held items.
	MoreLeft bool
	// Merged says the response is every page, all accepted, merged into one
	// document, or the first page alone when it was the only one. When unset,
	// the response is the last page's, whose status the collector does not
	// accept.
	Merged bool
}

// PageRead is one page request.pagination asked for: its URL, its status and
// the size of its body.
type PageRead struct {
	URL    string
	Status int
	Bytes  int
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
	NewConnection      *bool
	// IPFamily is the ip_family probe parameter, or a static target's
	// request.ip_family, "" when neither sets it.
	IPFamily string
	// Targets, From and Until are a static target's request.targets, from
	// and until, replacing a graphite collector's; no probe parameter sets
	// them. Targets is nil when the target does not set it.
	Targets []string
	From    string
	Until   string
	// Message replaces a grpc collector's request.message. It comes from the
	// message probe parameter or a static target's request.message. Metadata is
	// a static target's request.metadata, sent in addition to the collector's,
	// and RetryCodes is its retry.codes. No probe parameter sets these two.
	Message    *string
	Metadata   map[string]string
	RetryCodes []string
	// RetryStatuses is a static target's retry.statuses, replacing an http
	// or graphite collector's when not nil; no probe parameter sets it.
	RetryStatuses []string
	// RetryAfter is a static target's retry.retry_after; no probe
	// parameter sets it.
	RetryAfter *bool
	// AcceptStatus and AcceptCodes are a static target's request.accept_status
	// and accept_codes, replacing the collector's when not nil; no probe
	// parameter sets them.
	AcceptStatus []string
	AcceptCodes  []string
	// Username, BindDN and the credential files are a static target's
	// request.username, bind_dn, private_key_file, passphrase_file and
	// password_file. They replace the account of an sshfile, sql, redis or ldap
	// collector. TLS is its request.tls, and replaces the whole block of a
	// redis, ldap, tcp, sql, grpc, http or graphite collector. No probe parameter
	// sets them, so a probe can choose neither the account a scrape logs in as
	// nor the certificate it presents.
	Username       string
	BindDN         string
	PrivateKeyFile string
	PassphraseFile string
	PasswordFile   string
	TLS            *model.TLSConfig
	// CommunityFile, AuthPasswordFile and PrivPasswordFile are a static target's
	// and replace an snmp collector's community and v3 passwords. RequestTimeout
	// and Retries are its request.request_timeout and retries. No probe parameter
	// sets them.
	CommunityFile    string
	AuthPasswordFile string
	PrivPasswordFile string
	RequestTimeout   *time.Duration
	Retries          *int
	// SQLConnections is a static target's request.connections, whose keys
	// replace a sql collector's; no probe parameter sets it.
	SQLConnections *model.SQLConnections
	// formPost sends the query as a form body with POST instead of in the
	// URL, for a graphite request whose expressions are too long for a URL
	// (requesttype_graphite.go). Only the type's own fetch sets it.
	formPost bool
	// Params are the param_<name> probe parameters, bound into the
	// {{param_<name>}} placeholders of the collector's request.path.
	Params map[string]string
	// targetClock says Params are a static target's, of which a value
	// holds a clock placeholder, filled in at each trip (statictargets.go).
	targetClock bool
	// Login is a static target's request.login, replacing an http or
	// graphite collector's; no probe parameter sets it, so a probe can
	// neither change where a login goes nor what it sends (login.go).
	Login *model.LoginConfig
	// RateLimit is a static target's request.rate_limit, replacing an
	// http or graphite collector's; no probe parameter sets it, so a probe
	// cannot ask a target faster than its configuration allows.
	RateLimit *model.RateLimitConfig
	// pageURL is the URL of a page after the first that request.pagination
	// asks for, sent instead of the one the target and the request make
	// (pagination.go). Only the pagination sets it.
	pageURL *url.URL
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
	newConnection, err := parseBoolOverride(values, "new_connection")
	if err != nil {
		return overrides, err
	}
	overrides.NewConnection = newConnection
	if value, ok := values["ip_family"]; ok {
		raw := ""
		if len(value) > 0 {
			raw = strings.TrimSpace(value[0])
		}
		if raw == "" || checkIPFamily(raw) != nil {
			return overrides, fmt.Errorf("invalid ip_family override %q; want ip4, ip6, prefer_ip4 or prefer_ip6", raw)
		}
		overrides.IPFamily = raw
	}
	if value, ok := values["retry_attempts"]; ok {
		raw := ""
		if len(value) > 0 {
			raw = strings.TrimSpace(value[0])
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > MaxRetryAttempts {
			return overrides, fmt.Errorf("invalid retry_attempts override %q; want a whole number from 0 to %d", raw, MaxRetryAttempts)
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

// requestURL is the URL a request is sent to: the page request.pagination
// asks for, when it is one after the first, and resolveRequestURL's
// otherwise.
func requestURL(target string, c *model.Collector, overrides RequestOverrides) (*url.URL, error) {
	if overrides.pageURL != nil {
		u := *overrides.pageURL
		return &u, nil
	}
	return resolveRequestURL(target, c, overrides)
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
		// The path is joined in its escaped form: the target's as written, and the
		// request path with the escapes it was written with (escapedRequestPath).
		// So an escape such as %2F inside a segment is sent as written. It is not
		// decoded into a new segment, nor escaped a second time into %252F.
		raw := path.Join("/", strings.TrimSuffix(u.EscapedPath(), "/"), escapedRequestPath(strings.TrimPrefix(requestPath, "/"), len(bound) > 0))
		// A trailing slash is kept, once: request.path / on a target
		// without a path of its own is /, not //.
		if strings.HasSuffix(requestPath, "/") && !strings.HasSuffix(raw, "/") {
			raw += "/"
		}
		setEscapedPath(u, applyPathParams(raw, bound))
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
	// The target's own query is sent byte for byte as written. If it were
	// parsed and encoded again, a bare key would gain a =, a pair containing a ;
	// would be dropped, and the pairs would be sorted and escaped anew. Only
	// what could not be sent at all is escaped (sendableQuery). request.query is
	// added after it, encoded.
	added := make(url.Values, len(query))
	for k, v := range query {
		added.Set(k, v)
	}
	rawQuery := joinQuery(sendableQuery(u.RawQuery), added.Encode())
	// What the type builds itself goes last, and alone: the label drops the
	// query, so it is built only for the request.
	if rt := requestTypeOf(c); bind && rt != nil && rt.Query != nil {
		own, err := rt.Query(c, overrides)
		if err != nil {
			return nil, err
		}
		rawQuery = joinQuery(withoutQueryKeys(rawQuery, own), own.Encode())
	}
	u.RawQuery = rawQuery
	return u, nil
}

// escapedRequestPath returns a request path in the form it is sent in. A %
// followed by two hexadecimal digits is an escape the author wrote, and is
// kept as written: /projects/group%2Fproject asks for exactly that. Escaping
// the % again would ask for group%252Fproject, a path nobody wrote. Everything
// else is escaped as a path is, including a % that begins no escape, as %25.
// With tokens, the NUL bytes that mark where bound values go (pathToken) are
// left as they are for applyPathParams to find. An escape cannot be mistaken
// for one, since a written %00 stays the text %00.
func escapedRequestPath(p string, tokens bool) string {
	var b strings.Builder
	literal := 0
	flush := func(end int) {
		if end > literal {
			b.WriteString((&url.URL{Path: p[literal:end]}).EscapedPath())
		}
	}
	for i := 0; i < len(p); i++ {
		switch {
		case p[i] == '%' && i+2 < len(p) && isHex(p[i+1]) && isHex(p[i+2]):
			flush(i)
			b.WriteString(p[i : i+3])
			i += 2
			literal = i + 1
		case tokens && p[i] == 0:
			flush(i)
			b.WriteByte(0)
			literal = i + 1
		}
	}
	flush(len(p))
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// setEscapedPath gives u the path whose escaped form is raw. The URL keeps
// both forms, so Go sends the escaped one unchanged. A raw form that the
// default escaping produces anyway is not kept. raw is built from a parsed
// URL's escaped path and escapedRequestPath's output, so it always decodes.
func setEscapedPath(u *url.URL, raw string) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		decoded, raw = raw, ""
	}
	u.Path, u.RawPath = decoded, raw
	if raw == (&url.URL{Path: decoded}).EscapedPath() {
		u.RawPath = ""
	}
}

// sendableQuery returns a query as written, with only the few characters
// escaped that a URL cannot hold as they are. These are a space, a double
// quote, angle brackets and every byte outside ASCII. A target from a probe
// parameter may well hold a space, which would end the URL in the request
// line and be answered 400 Bad Request. They are escaped in place, the space
// as %20. Everything else stays as written, including a % that begins no
// escape.
func sendableQuery(raw string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c <= ' ' || c == '"' || c == '<' || c == '>' || c >= 0x7f:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// joinQuery puts two encoded queries together, either of which may be empty.
func joinQuery(first, second string) string {
	if first == "" || second == "" {
		return first + second
	}
	return first + "&" + second
}

// withoutQueryKeys returns the encoded query raw without the pairs whose name,
// in any case, is one of own: the parameters a request type sets itself.
// Every other pair stays as written. The type's own values are then what a
// server reads, whichever of two values it would take. Otherwise a graphite
// target that named target=, as a probe's may, would add an expression to the
// collector's. A pair is also dropped when a ; in it sets one of them, since
// servers that still split on ; would read it that way.
func withoutQueryKeys(raw string, own url.Values) string {
	if raw == "" || len(own) == 0 {
		return raw
	}
	pairs := strings.Split(raw, "&")
	kept := pairs[:0]
	for _, pair := range pairs {
		drop := false
		for _, part := range strings.Split(pair, ";") {
			name, _, _ := strings.Cut(part, "=")
			if decoded, err := url.QueryUnescape(name); err == nil {
				name = decoded
			}
			for key := range own {
				drop = drop || strings.EqualFold(name, key)
			}
		}
		if !drop {
			kept = append(kept, pair)
		}
	}
	return strings.Join(kept, "&")
}

// checkScheme refuses a target scheme request.allowed_schemes does not
// allow, http and https when it is unset.
func checkScheme(c *model.Collector, scheme string) error {
	return checkSchemeAllowed(c.Request.AllowedSchemes, scheme)
}

// checkSchemeAllowed is checkScheme with the collector's list, allowed, as
// it is: a redirect's scheme is checked against it too (checkRedirect).
func checkSchemeAllowed(allowed []string, scheme string) error {
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
		if canonical == "Accept-Encoding" {
			// Go decompresses an answer only when it asked for the compression
			// itself. With this header set, it would hand the decoder the compressed
			// bytes, and the response limit would count those rather than the
			// decoded bytes. Go already asks for gzip.
			return fmt.Errorf("%q may not be set: the exporter asks for gzip itself and decompresses the answer, which it would not do with the header set", name)
		}
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

// The reasons a header is never forwarded from a probe (UnforwardableReason).
const (
	connectionHeader = "it describes the connection to the exporter, not the request to the target"
	encodingHeader   = "the exporter asks the target for gzip itself and decompresses the answer, which it would not do with the header set; Prometheus sends Accept-Encoding: gzip on every scrape"
)

// unforwardableHeaders maps the headers a probe never forwards, whatever
// request.forward_headers lists, by canonical name, to the reason.
// Authorization has its own request.forward_authorization, Accept-Encoding is
// the exporter's to set, and the rest describe the connection to the
// exporter, not the request to the target. No Proxy- header is forwarded
// either, whatever follows the dash (UnforwardableReason).
var unforwardableHeaders = map[string]string{
	"Accept-Encoding":     encodingHeader,
	"Authorization":       "the probe's Authorization is forwarded by request.forward_authorization: true alone",
	"Connection":          connectionHeader,
	"Content-Length":      connectionHeader,
	"Host":                connectionHeader,
	"Proxy-Authenticate":  connectionHeader,
	"Proxy-Authorization": connectionHeader,
	"Te":                  connectionHeader,
	"Trailer":             connectionHeader,
	"Transfer-Encoding":   connectionHeader,
	"Upgrade":             connectionHeader,
}

// UnforwardablePrefix starts the name of every header that is never
// forwarded besides the ones listed (UnforwardableHeaderNames).
const UnforwardablePrefix = "Proxy-"

// UnforwardableReason says why the canonical header name is never forwarded
// from a probe, or returns "" when it may be. Both the probe's forwarding
// (the exporter's forwardableHeaders) and the load-time check of
// request.forward_headers (checkForwardHeaders) use it.
func UnforwardableReason(name string) string {
	if reason, ok := unforwardableHeaders[name]; ok {
		return reason
	}
	if strings.HasPrefix(name, UnforwardablePrefix) {
		return connectionHeader
	}
	return ""
}

// UnforwardableHeaderNames returns the canonical, sorted names of the headers
// a probe never forwards, except those starting with UnforwardablePrefix.
// The schemas use it to refuse them in request.forward_headers, as the
// configuration does.
func UnforwardableHeaderNames() []string {
	return model.SortedKeys(unforwardableHeaders)
}

// lineBreakAdvice ends the message for a credential refused because of a
// control character. That character is usually the line break at the end of
// the Secret or the environment variable it came from. --config.expand-env
// keeps that line break, and the key's file form trims it.
const lineBreakAdvice = "; a line break at its end is usually one the Secret or the environment variable it came from ends with: remove it, or read the credential from a file with the key's _file form, which trims it"

// checkInlineCredentials refuses a credential, written in the configuration
// or a static target, that holds a control character other than tab. A
// bearer_token is sent as it is, in the Authorization header or the
// authorization metadata. Go refuses to send it with such a character, so
// every scrape would fail. A basic_auth username or password is sent encoded,
// but may not hold a control character either (RFC 7617); one there would
// make every scrape fail at the target instead. A username may not hold a
// colon either (CheckBasicUsername). The value itself is never shown.
func checkInlineCredentials(bearerToken string, basic *model.BasicAuth) error {
	if err := checkHeaderValue(bearerToken); err != nil {
		return fmt.Errorf("request.bearer_token %w%s", err, lineBreakAdvice)
	}
	if basic == nil {
		return nil
	}
	for _, field := range []struct{ key, value string }{{"username", basic.Username}, {"password", basic.Password}} {
		if err := checkHeaderValue(field.value); err != nil {
			return fmt.Errorf("request.basic_auth.%s %s, which a Basic credential may not hold%s", field.key, strings.TrimSuffix(err.Error(), ", which a header may not hold"), lineBreakAdvice)
		}
	}
	if err := CheckBasicUsername(basic.Username); err != nil {
		return fmt.Errorf("request.basic_auth.username %w", err)
	}
	return nil
}

// MaxRetryAttempts is the most retries one trip may make, whatever the
// collector, a static target or a probe parameter asks for. Anyone who can
// reach /probe chooses retry_attempts and retry_backoff. Without a bound, one
// probe of a failing target could send it requests in a tight loop until its
// deadline.
const MaxRetryAttempts = 10

// requestLabelURL renders a resolved URL for a metric label. Credentials in the
// userinfo and the whole query string are dropped: a collector's request.query
// or a probe parameter can carry a token or a tenant identifier, and a metric
// label is persisted by Prometheus and passed on to anything federating from it.
func requestLabelURL(u *url.URL) string {
	return RedactURL(u, DropQuery)
}

// requestMethod reports the method a scrape will use.
func requestMethod(c *model.Collector, overrides RequestOverrides) string {
	if overrides.Method != "" {
		return overrides.Method
	}
	method, _ := methodBody(c)
	return method
}

// withTargetTLS returns c with a static target's own tls block in place of
// the collector's whole block, as used for a request of an http or graphite
// collector to that target. It returns c itself when the target names none.
// c is never written, since it belongs to the configuration.
func withTargetTLS(c *model.Collector, o RequestOverrides) *model.Collector {
	if o.TLS == nil {
		return c
	}
	x := *c
	x.Request.TLS = *o.TLS
	return &x
}

func fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded ...http.Header) (*HTTPResponse, error) {
	// A static target's own tls block replaces the collector's for every
	// request of the trip: its pages, its redirects, which are judged by
	// the client certificate it presents, and its login.
	c = withTargetTLS(c, overrides)
	u, err := requestURL(target, c, overrides)
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
	// new_connection sends every request of the trip on a connection made
	// for it, from a pool that keeps none (transport.go).
	newConnection := c.Request.NewConnection
	if overrides.NewConnection != nil {
		newConnection = *overrides.NewConnection
	}
	// ip_family is the family every connection of the trip is made over
	// (ipfamily.go).
	ipFamily := c.Request.IPFamily
	if overrides.IPFamily != "" {
		ipFamily = overrides.IPFamily
	}
	// The connection pool is shared by every request made with the same TLS
	// and HTTP/2 settings (transport.go). So a static target with its own tls
	// block gets its own pool, and a connection made with one client
	// certificate never carries a request that must present another.
	client, err := HTTPClient(withProxy(TransportSettings{TLS: tlsSettings, EnableHTTP2: enableHTTP2, policy: policyOf(c), newConnections: newConnection, ipFamily: ipFamily}, c), followRedirects, 0)
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
	ctx, err = withTargetPolicy(ctx, c, u, proxy, overrides.TLS != nil, ipFamily)
	if err != nil {
		return nil, err
	}
	// Every request the trip sends from here on, its login's, its
	// redirects' and its retries' among them, waits for its turn under the
	// rate limit (ratelimit.go).
	ctx = withRateLimit(ctx, c, overrides)
	requestContext := ctx
	cancel := func() {}
	if overrides.Timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, overrides.Timeout)
	}
	defer cancel()
	// The request's deadline, the probe's or its own, is passed to its dials as
	// a value, because net/http keeps values where it drops the deadline. A
	// prefer_ip4 or prefer_ip6 dial gives the first family half of the time
	// left (ipfamily.go).
	requestContext = withDialDeadline(requestContext)
	method, requestBody := methodBody(c)
	if overrides.Method != "" {
		method = overrides.Method
	}
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
	retryAfter := c.Request.Retry.RetryAfter
	if overrides.RetryAfter != nil {
		retryAfter = *overrides.RetryAfter
	}
	// wait waits backoff before a retry, after a failure no Retry-After
	// came with. Under retry.retry_after the debug report shows each wait,
	// so one Retry-After set can be told from the backoff.
	wait := func(backoff time.Duration, why string) error {
		err := waitRetry(requestContext, backoff)
		if retryAfter {
			if err != nil {
				traceWait(requestContext, fmt.Sprintf("then the wait of %s, %s, was cut short: %v", backoff, why, err))
			} else {
				traceWait(requestContext, fmt.Sprintf("then waited %s, %s", backoff, why))
			}
		}
		return err
	}

	// The login is made, or its token taken from those kept, once the
	// target has passed the collector's policy, so a target the policy
	// refuses never has a login made for it.
	login, err := startLogin(ctx, target, c, overrides, forwarded, hostOverride(headers, forwarded...))
	if err != nil {
		return nil, err
	}
	authorization := ""
	if !login.sends("Authorization") {
		// A static target's login sent in Authorization replaces the
		// collector's credential, as the target's own credential does.
		if authorization, err = collectorAuthorization(c, forwarded...); err != nil {
			return nil, err
		}
	}
	// A cached response is revalidated only by a request that may be
	// conditional (Revalidable) and that neither the collector nor the probe
	// has made conditional itself. No method counts as GET, as for a graphite
	// request, which leaves request.method unset.
	cached := revalidationFrom(ctx)
	if cached != nil && (overrides.pageURL != nil || (method != "" && method != http.MethodGet && method != http.MethodHead) || setsConditional(headers, forwarded)) {
		cached = nil
	}
	start := time.Now()
	limit := responseLimit(c)
	// protocolErrors counts the attempts that ended with their HTTP/2
	// connection closed for a protocol error before the answer's headers.
	protocolErrors := 0
	// Timing a request's phases costs a trace of nine hooks per attempt. That
	// cost is skipped for a collector whose rules read neither $timings nor
	// $remote_ip, the address the trace sees the connection made to
	// (model.Collector.TimingsUnread). A debug run times them regardless of the
	// rules, since its report shows them. So does a trip exported as a trace,
	// whose requests' spans show them (TracedRequest.Phases).
	timed := !c.TimingsUnread || debugRun(ctx) || spanTraced(ctx)
	for attempt := 0; attempt <= retryAttempts; attempt++ {
		var reqBody io.Reader
		if requestBody != "" {
			reqBody = strings.NewReader(requestBody)
		}
		// Each attempt times its own requests: what the answer carries is
		// of the attempt that brought it (HTTPTrip).
		attemptStart := time.Now()
		attemptContext := requestContext
		var timing *tripTrace
		if timed {
			timing = newTripTrace(requestContext, attemptStart)
			attemptContext = httptrace.WithClientTrace(requestContext, timing.hooks())
		}
		req, err := http.NewRequestWithContext(attemptContext, method, u.String(), reqBody)
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
		login.set(req)
		if len(forwarded) > 0 {
			for k, v := range forwarded[0] {
				if strings.EqualFold(k, "Accept-Encoding") {
					// Never forwarded (checkHeaderNames says why):
					// Prometheus asks the exporter for gzip on every
					// scrape, which is no reason to ask the target.
					continue
				}
				if strings.EqualFold(k, "Host") {
					if len(v) > 0 {
						req.Host = v[len(v)-1]
					}
					continue
				}
				req.Header[k] = append([]string(nil), v...)
			}
		}
		// Set on every attempt, so a retry and the request sent again
		// after a new login are as conditional as the first.
		if cached != nil {
			setConditional(req, cached)
		}

		// A request refused here is refused again on every attempt, so
		// it ends the trip.
		waited, err := waitRateLimit(requestContext, req.URL)
		if err != nil {
			return nil, err
		}
		span := propagateTrace(requestContext, req.Header, false)
		// Signed last, over the request as it is sent (httpsign.go).
		if signHTTPRequest != nil {
			if err := signHTTPRequest(requestContext, c, req, requestBody); err != nil {
				return nil, err
			}
		}
		traceRequest(requestContext, req.Method, req.URL.String(), req.Header, req.Host, false)
		traceSpanID(requestContext, span)
		traceAttempt(requestContext, attempt)
		traceRateLimitWait(requestContext, waited, false)
		resp, err := client.Do(req)
		if err != nil {
			timing.recordPhases()
			// Go's error quotes the whole URL, request.query included, and
			// of an answer that is no HTTP, its first line, which a
			// probe's caller is not told (answerquoted.go).
			err = withAnswerWithheld(RedactURLErrors(err))
			traceFailedOutcome(requestContext, "error: "+err.Error())
			// Headers over the bound are over it on every attempt, and
			// Go's own words for it name neither the limit's reason nor
			// what to do about it.
			if responseHeadersTooLarge(err) {
				return nil, model.MarkError(fmt.Errorf("the response headers are larger than %d bytes, the most the exporter reads of a response's headers, whatever max_response_bytes allows its body; the target has to send fewer or smaller headers", maxResponseHeaderBytes), model.ErrLimitExceeded)
			}
			// Over HTTP/2, the same answer usually ends as a connection the client
			// closed for a protocol error. That error says nothing about headers and
			// has other causes too, so it cannot be called a limit. Go hands that
			// one error to every request waiting on the connection, so the first one
			// may be caused by another request's answer. It is therefore retried
			// once, as one of the collector's retries, on a connection of its own
			// (onOwnConnection). The request whose answer closed the first
			// connection is retried at the same moment, and on a shared connection it
			// would close that one too. A second error is therefore caused by this
			// request's own answer. What broke the protocol twice is not sent again,
			// so the request ends there, and the error says where to look.
			if http2ProtocolError(err) {
				protocolErrors++
				if protocolErrors == 1 && attempt < retryAttempts && requestContext.Err() == nil {
					if waitErr := wait(retryBackoff, "retry.backoff"); waitErr != nil {
						return nil, fmt.Errorf("HTTP request failed: %w (the wait before retrying was cut short: %w)", err, waitErr)
					}
					client = onOwnConnection(client)
					continue
				}
				closed := "in answer to this request or to another on the same connection, and the request had no retry left to be sent again on a new one"
				if protocolErrors > 1 {
					closed = "as the connection before it was, so the request is not retried again"
				}
				return nil, fmt.Errorf("HTTP request failed: %w: the HTTP/2 connection was closed over what the target sent, %s; an answer whose headers are larger than %d bytes, the most the exporter reads of a response's headers, ends this way over HTTP/2, so look at the size of the target's response headers first", err, closed, maxResponseHeaderBytes)
			}
			// Each of these is refused again on every attempt: a refused target, a
			// redirect that would send the body or show the client certificate to a
			// host that is not trusted, a request the rate limit would delay past the
			// deadline, and proxy credentials that could not be read.
			var redirectRefused *redirectRefusedError
			var proxyCredentials *proxyCredentialsError
			if attempt < retryAttempts && requestContext.Err() == nil && !errors.Is(err, ErrTargetRefused) && !errors.As(err, &redirectRefused) && !errors.Is(err, errRateLimitRefused) && !errors.As(err, &proxyCredentials) {
				if waitErr := wait(retryBackoff, "retry.backoff"); waitErr != nil {
					return nil, fmt.Errorf("HTTP request failed: %w (the wait before retrying was cut short: %w)", err, waitErr)
				}
				continue
			}
			return nil, fmt.Errorf("HTTP request failed: %w", err)
		}
		traceHTTPOutcome(requestContext, resp)
		// A Content-Length over the limit refuses the answer before a byte
		// of it is read. A HEAD answer's Content-Length is the size of a
		// body it does not have, so it is not held against it.
		if method != http.MethodHead && resp.ContentLength > limit {
			_ = resp.Body.Close()
			return nil, model.MarkError(model.Errorf("response size %d exceeds limit %d", model.Size(resp.ContentLength), limit), model.ErrLimitExceeded)
		}
		// The Content-Length is the length of the body as it is read here. Go
		// removes it from an answer it decompresses itself, the only kind that is
		// decompressed at all (checkHeaderNames). A HEAD answer's Content-Length is
		// that of a body it does not have.
		declared := resp.ContentLength
		if method == http.MethodHead {
			declared = 0
		}
		var body []byte
		var readErr error
		switch {
		case resp.StatusCode == http.StatusSwitchingProtocols:
			// A 101 has no body. What follows it on the connection is the protocol it
			// switched to, such as WebSocket frames, which the server may send as
			// long as the connection is open. Go hands over the connection as the
			// body. It is closed unread, so the answer is just the status and headers,
			// and the probe ends at once instead of reading the protocol's traffic
			// until its deadline.
			body = []byte{}
		case declared > 0:
			body, readErr = readBody(resp.Body, limit, declared)
		case method == http.MethodHead || resp.ContentLength == 0:
			// An answer that has no body, or says it has none, is read
			// without the hint of the last answer's size: a buffer of that
			// size would be made for nothing.
			body, readErr = readUnsizedBody(resp.Body, limit)
		default:
			// Nothing says how long the body is, so the last answer of the
			// same collector and target does.
			body, readErr = readHintedBody(resp.Body, limit, bodySizeHintOf(c.Name, target))
		}
		closeErr := resp.Body.Close()
		trip := timing.trip(resp, attemptStart, time.Now())
		// A connection through a proxy is to the proxy, whose address is
		// not the target's: $remote_ip is null then.
		if trip.RemoteIP.IsValid() && resp.Request != nil && viaProxy(proxy, resp.Request.URL) {
			trip.RemoteIP = netip.Addr{}
		}
		if readErr != nil {
			readErr = RedactURLErrors(readErr)
			traceBodyError(requestContext, readErr)
			// A connection that broke while the body was being read is
			// retried as one that broke before the answer began, unless the
			// probe's own time is what ran out.
			if attempt < retryAttempts && requestContext.Err() == nil {
				if waitErr := wait(retryBackoff, "retry.backoff"); waitErr != nil {
					return nil, fmt.Errorf("reading response: %w (the wait before retrying was cut short: %w)", readErr, waitErr)
				}
				continue
			}
			return nil, fmt.Errorf("reading response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("closing response: %w", closeErr)
		}
		if int64(len(body)) > limit {
			// Reading stopped one byte past the limit, so the size is not
			// known and is not made up.
			return nil, model.MarkError(fmt.Errorf("response size exceeds limit %d", limit), model.ErrLimitExceeded)
		}
		// A 401 to a request that logged in, or a status in login.relogin_on, means
		// the target refuses the token, which may have been revoked or expired
		// early. The token is dropped and the request is sent once more with a
		// token from a new login. This is not an attempt of request.retry, and it is
		// made within the request's deadline. A 401 from where a redirect led
		// without the token, to another origin, refused no token, and is handled
		// like any other answer.
		if login.refuses(resp.StatusCode) && login.carried(resp.Request) {
			again, err := login.again(requestContext)
			if err != nil {
				return nil, err
			}
			if again {
				attempt--
				continue
			}
		}
		if cached != nil && resp.StatusCode == http.StatusNotModified {
			// The cached response is current: it is the answer, as if
			// it had been downloaded again.
			traceOutcomeNote(requestContext, notModifiedNote(cached))
			return &HTTPResponse{StatusCode: cached.StatusCode, Headers: revalidated(cached, resp.Header), Body: cached.Body, Target: target, Collector: c.Name, Duration: time.Since(start), Conditional: true, NotModified: true, Trip: trip}, nil
		}
		response := &HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body, Target: target, Collector: c.Name, Duration: time.Since(start), Conditional: cached != nil, Trip: trip}
		// The answer of a request a redirect led to says so when the
		// redirect left the collector's headers behind, so a 401 from
		// there can be told from one the target gave.
		if resp.Request != nil && resp.Request.Response != nil {
			response.RedirectWithheld = redirectWithheldFrom(requestContext)
		}
		// An answer the collector accepts is its answer, not a failure to retry,
		// even a 503 it asked to read. The load refuses a retry.statuses entry
		// that is fully accepted (checkRetryStatuses). So this matters only when a
		// class is retried and some of its statuses are accepted, or for the
		// default set.
		if retriedStatus(c, overrides, resp.StatusCode) && !AcceptedStatus(c, overrides, resp.StatusCode) && attempt < retryAttempts {
			// If the deadline, a shutdown or the caller cuts a wait short, what the
			// target answered is kept. The status and body say why the scrape failed,
			// while the bare context error would only say that time ran out.
			next := retryWait{wait: retryBackoff, why: "retry.backoff"}
			if retryAfter {
				next = retryAfterWait(requestContext, resp.Header.Get("Retry-After"), retryBackoff, time.Now())
			}
			if next.giveUp != "" {
				// Waiting until the deadline would only fail the same
				// way later, with no answer to show for it.
				response.NotRetried = next.giveUp
				traceWait(requestContext, "not retried: "+next.giveUp)
				return response, nil
			}
			if wait(next.wait, next.why) != nil {
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

// hostOverride returns the Host a request is sent with instead of its URL's
// host, as fetch sets it. A static target's Host comes with the forwarded
// headers and is applied last. Otherwise it is a Host among the collector's
// rendered request.headers. It is "" when neither sets one. A probe never
// forwards Host (UnforwardableReason). A relative login.url (loginURL) and a
// next page on that virtual host (pagination.go) are sent with it too.
func hostOverride(headers map[string]string, forwarded ...http.Header) string {
	host := ""
	for name, value := range headers {
		if strings.EqualFold(name, "Host") {
			host = value
		}
	}
	if len(forwarded) > 0 {
		for name, values := range forwarded[0] {
			if strings.EqualFold(name, "Host") && len(values) > 0 {
				host = values[len(values)-1]
			}
		}
	}
	return host
}

// defaultResponseLimit is the response limit of a collector that sets neither
// limits.max_response_bytes nor request.max_response_bytes.
const defaultResponseLimit = 10 << 20

// responseLimit is the most a collector reads from its target: the smaller of
// limits.max_response_bytes and request.max_response_bytes when both are set,
// the one that is set otherwise, and 10 MiB when neither is. Neither is
// filled in with the default, so either alone may raise the limit past it.
// Every request type reads through it.
//
// Each request type reads one byte past the limit to tell an answer of
// exactly the limit from a longer one. So the limit is at most
// maxResponseLimit, one under the largest number. Otherwise a limit written
// as that number, meaning "no limit", would wrap around to a read of nothing.
func responseLimit(c *model.Collector) int64 {
	limit := c.Limits.MaxResponseBytes
	if limit <= 0 || c.Request.MaxResponseBytes > 0 && c.Request.MaxResponseBytes < limit {
		limit = c.Request.MaxResponseBytes
	}
	if limit <= 0 {
		limit = defaultResponseLimit
	}
	return min(int64(limit), maxResponseLimit)
}

// ResponseLimit is responseLimit, for a response that was not fetched: the
// debug page holds a pasted body to the limit a fetched one would be held to
// (exporter/debugpage.go).
func ResponseLimit(c *model.Collector) int64 { return responseLimit(c) }

// maxResponseLimit is the largest response limit there is (responseLimit).
const maxResponseLimit = math.MaxInt64 - 1

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

// DefaultRetryStatuses are the statuses an http or graphite request retries
// when retry.statuses is left out: those that say the target could not
// answer now, and may on the next attempt — 408 Request Timeout, 425 Too
// Early, 429 Too Many Requests and every 5xx.
var DefaultRetryStatuses = []string{"408", "425", "429", "5xx"}

// RetryStatuses are the statuses a request of c retries: a static target's
// retry.statuses, else the collector's, else DefaultRetryStatuses. An empty
// list is no status at all, not the default.
func RetryStatuses(c *model.Collector, overrides RequestOverrides) []string {
	retried := c.Request.Retry.Statuses
	if overrides.RetryStatuses != nil {
		retried = overrides.RetryStatuses
	}
	if retried == nil {
		retried = DefaultRetryStatuses
	}
	return retried
}

// RetriesOf says, for the debug report, how a request of c with overrides,
// made over HTTP, is retried: at most how often, how far apart and on what.
// It is "" when no retry is made, that is when attempts is 0, or an http
// request's method is not idempotent and retry.non_idempotent is unset. It is
// also "" for a type that is not retried by HTTP status: one without
// request.retry, and grpc, which retries by code.
func RetriesOf(c *model.Collector, overrides RequestOverrides) string {
	rt := requestTypeOf(c)
	if rt == nil || rt.StatusCodes || !rt.Takes("retry") {
		return ""
	}
	attempts := c.Request.Retry.Attempts
	if overrides.RetryAttempts != nil {
		attempts = *overrides.RetryAttempts
	}
	backoff := time.Duration(c.Request.Retry.Backoff)
	if overrides.RetryBackoff != nil {
		backoff = *overrides.RetryBackoff
	}
	anyMethod := c.Request.Retry.NonIdempotent
	if overrides.RetryNonIdempotent != nil {
		anyMethod = *overrides.RetryNonIdempotent
	}
	// Only a type that takes request.method, http, sends a method that may
	// not be idempotent: graphite's method is its own, a GET or a form that
	// only reads, and is retried as a GET (fetch).
	if attempts <= 0 || rt.Takes("method") && !anyMethod && !IdempotentMethod(requestMethod(c, overrides)) {
		return ""
	}
	statuses := RetryStatuses(c, overrides)
	if len(statuses) == 0 {
		return fmt.Sprintf("up to %d, %s apart, on a failed connection alone: retry.statuses is empty", attempts, max(backoff, 0))
	}
	retries := fmt.Sprintf("up to %d, %s apart, on a failed connection and on an answer of %s that is not accepted", attempts, max(backoff, 0), strings.Join(statuses, ", "))
	retryAfter := c.Request.Retry.RetryAfter
	if overrides.RetryAfter != nil {
		retryAfter = *overrides.RetryAfter
	}
	if retryAfter {
		retries += ", such an answer's Retry-After replacing the backoff (retry.retry_after)"
	}
	return retries
}

// retriedStatus says whether an answer with status is retried, when it is
// not accepted and an attempt is left.
func retriedStatus(c *model.Collector, overrides RequestOverrides, status int) bool {
	return statusListed(RetryStatuses(c, overrides), status)
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

// signHTTPRequest signs a request of a collector that signs its requests,
// whose body is body, before it is sent; the http request type sets it
// (httpsign.go), and a build without the type signs none.
var signHTTPRequest func(ctx context.Context, c *model.Collector, req *http.Request, body string) error

// ReadCredentialFile reads a credential from the file at path, without the
// surrounding whitespace an editor or a Secret mount may leave. A path
// that is a reference to a secret store, secret://store/path#field, is
// read from the store instead, or from what the store answered a while ago
// (secretstore.go).
func ReadCredentialFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is empty")
	}
	if IsSecretReference(path) {
		return readSecret(path)
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

// safeTarget renders a target for logs, labels and error bodies with its
// credentials withheld: its userinfo, and the values of query parameters
// whose names read as credentials (MaskCredentialQueryValues). It must never
// fail, since it runs on the error paths, even for a target that is not a
// URL at all.
func safeTarget(raw string) string {
	u, err := url.Parse(normalizeTarget(raw))
	if err != nil {
		// Unparseable, so the credentials cannot be located to redact them;
		// the raw text is withheld rather than risk echoing a password.
		return "<invalid target>"
	}
	return RedactURL(u, MaskCredentialQueryValues)
}

// normalizeTarget gives a target without a scheme the default http:// one.
//
// Prometheus service discovery hands over __address__, which is host:port with
// no scheme, and the chart's monitors pass it straight through as target. It
// cannot simply be parsed and then checked for an empty scheme: url.Parse
// rejects 10.0.0.5:8080 outright ("first path segment cannot contain colon")
// and reads legacy.example:8080 as the scheme "legacy.example". So the
// decision is made on the text, before parsing: no "://" means no scheme.
// A scheme ends before the path, the query and the fragment begin, so a
// "://" after the first /, ? or # is part of those — the URL in
// host:8080/login?next=http://other/ — and names no scheme.
// A target that wants https says so explicitly.
func normalizeTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if scheme, _, found := strings.Cut(raw, "://"); found && !strings.ContainsAny(scheme, "/?#") {
		return raw
	}
	return "http://" + raw
}

// validateAcceptStatus checks request.accept_status at load.
func validateAcceptStatus(c *model.Collector) error {
	if err := normalizeAcceptedStatuses(c.Request.AcceptStatus); err != nil {
		return fmt.Errorf("collector %q request.accept_status %w", c.Name, err)
	}
	return nil
}

// normalizeAcceptStatus rewrites each entry in place as AcceptedStatus
// compares it (canonicalStatus). It refuses an entry that is neither a
// status from 100 to 599 written with its three digits nor a class such as
// 2xx. This is exactly what the schemas' pattern takes,
// ^[1-5][xX][xX]$|^[1-5][0-9][0-9]$. A status written as a number in YAML
// arrives as its three digits (wholeNumberText in internal/config), however
// YAML wrote the number. Text with a sign, leading zeros or blanks ("+503",
// "0503" or " 503 ") is refused, as the schemas refuse it. So a file the
// exporter takes is also one an editor's schema check takes.
func normalizeAcceptStatus(entries []string) error {
	for i, raw := range entries {
		entry := canonicalStatus(raw)
		entries[i] = entry
		if len(entry) == 3 && entry[0] >= '1' && entry[0] <= '5' && (entry[1:] == "xx" || isDigit(entry[1]) && isDigit(entry[2])) {
			continue
		}
		return fmt.Errorf("entry %q is not an HTTP status from 100 to 599 written with its three digits, such as 503, or a class such as 2xx", raw)
	}
	return nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// AcceptAnyStatus is the request.accept_status entry that accepts every
// status from 100 to 599. It is for a collector that reports whatever the
// target answered, such as an uptime check, without listing the five
// classes. Since it covers everything, it is written alone. retry.statuses,
// which lists what is not accepted, does not take it.
const AcceptAnyStatus = "any"

// normalizeAcceptedStatuses is normalizeAcceptStatus for
// request.accept_status, which also takes AcceptAnyStatus, in lower case as
// the schemas take it, as its only entry. Another entry beside it would say
// nothing, or would wrongly read as narrowing it.
func normalizeAcceptedStatuses(entries []string) error {
	if i := slices.Index(entries, AcceptAnyStatus); i >= 0 {
		if len(entries) > 1 {
			return fmt.Errorf("entry %q accepts every status from 100 to 599 and is written alone, without %q; take the other entries out", AcceptAnyStatus, entries[(i+1)%len(entries)])
		}
		return nil
	}
	return normalizeAcceptStatus(entries)
}

// canonicalStatus is an entry of request.accept_status in the form
// AcceptedStatus compares with a status. A class is written in lower case,
// 5xx for 5XX, since the schemas take either case. Any other entry is also
// written in lower case, for the load to take or refuse. The exception is
// AcceptAnyStatus in another case, such as "ANY". The schemas take any only
// in lower case, and a static target's entries are written so before they
// are checked, so it is kept as written for the load to refuse.
func canonicalStatus(raw string) string {
	if strings.EqualFold(raw, AcceptAnyStatus) {
		return raw
	}
	return strings.ToLower(raw)
}

// AcceptedStatus says whether a response with status is decoded: one of
// request.accept_status, a static target's replacing the collector's, or any
// 2xx when there is none.
func AcceptedStatus(c *model.Collector, overrides RequestOverrides, status int) bool {
	accepted := c.Request.AcceptStatus
	if overrides.AcceptStatus != nil {
		accepted = overrides.AcceptStatus
	}
	return acceptsStatus(accepted, status)
}

// acceptsStatus says whether accepted, a normalized request.accept_status,
// accepts status: every 2xx when it is empty, every status from 100 to 599
// when it is AcceptAnyStatus, and its entries otherwise. A status past 599,
// which no HTTP status class holds, is never accepted.
func acceptsStatus(accepted []string, status int) bool {
	switch {
	case len(accepted) == 0:
		return status >= 200 && status < 300
	case len(accepted) == 1 && accepted[0] == AcceptAnyStatus:
		return status >= 100 && status <= 599
	}
	return statusListed(accepted, status)
}

// statusListed says whether status is one of entries, each a status or a
// class such as 5xx as canonicalStatus writes it.
func statusListed(entries []string, status int) bool {
	code := strconv.Itoa(status)
	for _, entry := range entries {
		if entry == code || (len(code) == 3 && entry[0] == code[0] && entry[1:] == "xx") {
			return true
		}
	}
	return false
}

// validateRetryStatuses checks request.retry.statuses at load, as
// request.accept_status is checked, after accept_status is normalized.
func validateRetryStatuses(c *model.Collector) error {
	if err := normalizeAcceptStatus(c.Request.Retry.Statuses); err != nil {
		return fmt.Errorf("collector %q request.retry.statuses %w", c.Name, err)
	}
	if err := checkRetryStatuses(c.Request.Retry.Statuses, c.Request.AcceptStatus); err != nil {
		return fmt.Errorf("collector %q %w", c.Name, err)
	}
	return nil
}

// checkRetryStatuses refuses a normalized retry.statuses entry whose every
// status is accepted by accepted, also normalized. An accepted status is the
// answer and is never retried, so the entry could never take effect. A class
// with only some of its statuses accepted is kept, since the others are
// retried.
func checkRetryStatuses(retried, accepted []string) error {
	accepts := func(status int) bool { return acceptsStatus(accepted, status) }
	for _, entry := range retried {
		var least, most int
		if code, err := strconv.Atoi(entry); err == nil {
			least, most = code, code
		} else {
			least = int(entry[0]-'0') * 100
			most = least + 99
		}
		whole := true
		for status := least; status <= most && whole; status++ {
			whole = accepts(status)
		}
		if !whole {
			continue
		}
		if len(accepted) == 0 {
			return fmt.Errorf("request.retry.statuses entry %q is never retried: it is a 2xx, which a collector without request.accept_status accepts, and an accepted status is the answer, not a failure to retry; take it out of retry.statuses, or set accept_status without it", entry)
		}
		return fmt.Errorf("request.retry.statuses entry %q is never retried: request.accept_status accepts it, and an accepted status is the answer, not a failure to retry; take it out of one of the two lists", entry)
	}
	return nil
}
