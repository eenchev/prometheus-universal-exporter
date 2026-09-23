package main

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

func parseRequestOverrides(values url.Values) (RequestOverrides, error) {
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

// resolveRequestURL builds the URL a scrape actually requests. Both fetch and
// the verbose self-metric label go through it, so a label can never describe a
// different URL than the one that was fetched.
func resolveRequestURL(target string, c *Collector, overrides RequestOverrides) (*url.URL, error) {
	return buildRequestURL(target, c, overrides, true)
}

// buildRequestURL is resolveRequestURL with the choice of whether to bind path
// parameters. The self-metric label is built with bind false, so it shows the
// placeholder rather than the value; everything else is identical, which keeps
// the label describing the URL that was fetched.
func buildRequestURL(target string, c *Collector, overrides RequestOverrides, bind bool) (*url.URL, error) {
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
	if bind && !overrides.PathSet && hasPathParams(requestPath) {
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
	q := u.Query()
	for k, v := range c.Request.Query {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u, nil
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
func requestMethod(c *Collector, overrides RequestOverrides) string {
	if overrides.Method != "" {
		return overrides.Method
	}
	return c.Request.Method
}

func fetch(ctx context.Context, target string, c *Collector, overrides RequestOverrides, forwarded ...http.Header) (*HTTPResponse, error) {
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
	client, err := httpClient(transportSettings{TLS: tlsSettings, EnableHTTP2: enableHTTP2}, followRedirects, 0)
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
		username, readErr := readCredentialFile(c.Request.BasicAuthFile.Username)
		if readErr != nil {
			return nil, fmt.Errorf("reading basic auth username file: %w", readErr)
		}
		password, readErr := readCredentialFile(c.Request.BasicAuthFile.Password)
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
		for k, v := range c.Request.Headers {
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
			return nil, markError(fmt.Errorf("response size %d exceeds limit %d", len(body), limit), errLimitExceeded)
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

// responseLimit is the most a collector reads from its target: the smaller of
// limits.max_response_bytes and request.max_response_bytes, 10 MiB when
// neither is set. Every request type reads through it.
func responseLimit(c *Collector) int64 {
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

func readCredentialFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
