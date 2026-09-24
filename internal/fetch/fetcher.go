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

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

type HTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Target     string
	Collector  string
	Duration   time.Duration
	// Directory is set instead of Body by a localfile collector reading a
	// directory: every file it read, each to be decoded on its own.
	Directory *DirectoryRead
}

type RequestOverrides struct {
	Method             string
	Path               string
	PathSet            bool
	Timeout            time.Duration
	Body               *string
	InsecureSkipVerify *bool
	RetryAttempts      *int
	RetryBackoff       *time.Duration
	FollowRedirects    *bool
	EnableHTTP2        *bool
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
	params, err := pathParamValues(values)
	if err != nil {
		return overrides, err
	}
	overrides.Params = params
	return overrides, nil
}

// ResolveRequestURL builds the URL a scrape actually requests. Both fetch and
// the verbose self-metric label go through it, so a label can never describe a
// different URL than the one that was fetched.
func ResolveRequestURL(target string, c *model.Collector, overrides RequestOverrides) (*url.URL, error) {
	return buildRequestURL(target, c, overrides, true)
}

// buildRequestURL is ResolveRequestURL with the choice of whether to bind path
// parameters. The self-metric label is built with bind false, so it shows the
// placeholder rather than the value; everything else is identical, which keeps
// the label describing the URL that was fetched.
func buildRequestURL(target string, c *model.Collector, overrides RequestOverrides, bind bool) (*url.URL, error) {
	u, err := url.Parse(normalizeTarget(target))
	if err != nil {
		return nil, fmt.Errorf("invalid target: %w", err)
	}
	allowed := c.Request.AllowedSchemes
	if len(allowed) == 0 {
		allowed = []string{"http", "https"}
	}
	ok := false
	for _, s := range allowed {
		if strings.EqualFold(s, u.Scheme) {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("target scheme %q is not allowed", u.Scheme)
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
		base := strings.TrimSuffix(u.Path, "/")
		p := strings.TrimPrefix(requestPath, "/")
		u.Path = path.Join("/", base, p)
		if strings.HasSuffix(requestPath, "/") {
			u.Path += "/"
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
	u.RawQuery = q.Encode()
	return u, nil
}

// RequestLabelURL renders a resolved URL for a metric label. Credentials in the
// userinfo and the whole query string are dropped: a collector's request.query
// or a probe parameter can carry a token or a tenant identifier, and a metric
// label is persisted by Prometheus and passed on to anything federating from it.
func RequestLabelURL(u *url.URL) string {
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
	u, err := ResolveRequestURL(target, c, overrides)
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
	if retryBackoff < 0 {
		retryBackoff = 0
	}

	var basicUsername, basicPassword string
	if c.Request.BasicAuth != nil {
		basicUsername = c.Request.BasicAuth.Username
		basicPassword = c.Request.BasicAuth.Password
	}
	if c.Request.BasicAuthFile != nil {
		username, readErr := ReadCredentialFile(c.Request.BasicAuthFile.Username)
		if readErr != nil {
			return nil, fmt.Errorf("reading basic auth username file: %w", readErr)
		}
		password, readErr := ReadCredentialFile(c.Request.BasicAuthFile.Password)
		if readErr != nil {
			return nil, fmt.Errorf("reading basic auth password file: %w", readErr)
		}
		if username == "" || password == "" {
			return nil, errors.New("basic auth credential files must not be empty")
		}
		basicUsername = username
		basicPassword = password
	}
	bearerToken := c.Request.BearerToken
	if c.Request.BearerTokenFile != "" {
		token, readErr := os.ReadFile(c.Request.BearerTokenFile)
		if readErr != nil {
			return nil, fmt.Errorf("reading bearer token file: %w", readErr)
		}
		bearerToken = strings.TrimSpace(string(token))
		if bearerToken == "" {
			return nil, fmt.Errorf("bearer token file %s is empty", c.Request.BearerTokenFile)
		}
	}
	start := time.Now()
	limit := ResponseLimit(c)
	for attempt := 0; attempt <= retryAttempts; attempt++ {
		var reqBody io.Reader
		if requestBody != "" {
			reqBody = strings.NewReader(requestBody)
		}
		req, err := http.NewRequestWithContext(requestContext, method, u.String(), reqBody)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if basicUsername != "" || basicPassword != "" {
			req.SetBasicAuth(basicUsername, basicPassword)
		}
		if bearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+bearerToken)
		}
		if len(forwarded) > 0 {
			for k, v := range forwarded[0] {
				req.Header[k] = append([]string(nil), v...)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			if attempt < retryAttempts && requestContext.Err() == nil {
				if waitErr := waitRetry(requestContext, retryBackoff); waitErr != nil {
					return nil, fmt.Errorf("HTTP request failed: %w", err)
				}
				continue
			}
			return nil, fmt.Errorf("HTTP request failed: %w", err)
		}
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
		if retryableStatus(resp.StatusCode) && attempt < retryAttempts {
			if waitErr := waitRetry(requestContext, retryBackoff); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		return &HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body, Target: target, Collector: c.Name, Duration: time.Since(start)}, nil
	}
	return nil, fmt.Errorf("HTTP request failed after %d attempts", retryAttempts+1)
}

// ResponseLimit is the most a collector reads from its target: the smaller of
// limits.max_response_bytes and request.max_response_bytes, 10 MiB when
// neither is set. Every request type reads through it.
func ResponseLimit(c *model.Collector) int64 {
	limit := c.Limits.MaxResponseBytes
	if limit <= 0 || c.Request.MaxResponseBytes > 0 && c.Request.MaxResponseBytes < limit {
		limit = c.Request.MaxResponseBytes
	}
	if limit <= 0 {
		limit = 10 << 20
	}
	return int64(limit)
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
// outside the localfile build tag because the probe and scheduled-scrape paths
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
