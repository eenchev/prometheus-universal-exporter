package fetch

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// TargetOverrides translates the target request block into the same per-scrape
// override structure the /probe endpoint produces.
func TargetOverrides(t *model.StaticTarget) RequestOverrides {
	out := RequestOverrides{Method: t.Request.Method, Timeout: time.Duration(t.Request.Timeout), Params: t.Params, From: t.Request.From, Until: t.Request.Until}
	if len(t.Request.Targets) > 0 {
		out.Targets = append([]string(nil), t.Request.Targets...)
	}
	if t.Request.PathSet {
		out.PathSet = true
		out.Path = t.Request.Path
	}
	if t.Request.BodySet {
		body := t.Request.Body
		out.Body = &body
	}
	if t.Request.InsecureSkipVerify != nil {
		value := *t.Request.InsecureSkipVerify
		out.InsecureSkipVerify = &value
	}
	if t.Request.FollowRedirects != nil {
		value := *t.Request.FollowRedirects
		out.FollowRedirects = &value
	}
	if t.Request.EnableHTTP2 != nil {
		value := *t.Request.EnableHTTP2
		out.EnableHTTP2 = &value
	}
	// Each retry key the target sets replaces the collector's; the others
	// keep it.
	if retry := t.Request.Retry; retry != nil {
		if retry.Attempts != nil {
			attempts := *retry.Attempts
			out.RetryAttempts = &attempts
		}
		if retry.Backoff != nil {
			backoff := time.Duration(*retry.Backoff)
			out.RetryBackoff = &backoff
		}
		if retry.NonIdempotent != nil {
			nonIdempotent := *retry.NonIdempotent
			out.RetryNonIdempotent = &nonIdempotent
		}
	}
	return out
}

// TargetHeaders builds the headers sent to the target. Static targets are operator
// configuration rather than caller input, so they are applied directly instead
// of through the collector's forwarding allowlist. Credentials are resolved
// here so that they are part of the cache key and can never be shared with a
// request that did not present them.
func TargetHeaders(t *model.StaticTarget) (http.Header, error) {
	out := make(http.Header)
	for name, value := range t.Request.Headers {
		out.Set(name, value)
	}
	username, password := "", ""
	if t.Request.BasicAuth != nil {
		username, password = t.Request.BasicAuth.Username, t.Request.BasicAuth.Password
	}
	if t.Request.BasicAuthFile != nil {
		value, err := ReadCredentialFile(t.Request.BasicAuthFile.Username)
		if err != nil {
			return nil, fmt.Errorf("reading basic auth username file: %w", err)
		}
		username = value
		value, err = ReadCredentialFile(t.Request.BasicAuthFile.Password)
		if err != nil {
			return nil, fmt.Errorf("reading basic auth password file: %w", err)
		}
		password = value
		if username == "" || password == "" {
			return nil, errors.New("basic auth credential files must not be empty")
		}
	}
	if username != "" || password != "" {
		request := &http.Request{Header: make(http.Header)}
		request.SetBasicAuth(username, password)
		out.Set("Authorization", request.Header.Get("Authorization"))
	}
	token := t.Request.BearerToken
	if t.Request.BearerTokenFile != "" {
		value, err := ReadCredentialFile(t.Request.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading bearer token file: %w", err)
		}
		if value == "" {
			return nil, fmt.Errorf("bearer token file %s is empty", t.Request.BearerTokenFile)
		}
		token = value
	}
	if token != "" {
		out.Set("Authorization", "Bearer "+token)
	}
	return out, nil
}
