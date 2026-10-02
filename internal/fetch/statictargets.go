package fetch

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A static target file in force is read by the scrapes while a reload checks
// it against the configuration just read (CheckTargetRequest), so that check
// never writes into it: the spellings a scrape compares — accept_status in
// lower case, accept_codes and retry.codes in upper case — are written once,
// by NormalizeTargetRequest, when the file is loaded and nothing reads it
// yet, and the check only reads.

// NormalizeTargetRequest writes a static target's request.accept_status,
// request.accept_codes and request.retry.codes as a scrape compares them:
// trimmed, statuses in lower case and codes in upper case. It is called once,
// when the target file is loaded; whether each entry is one the collector's
// request type takes is CheckTargetRequest's to say.
func NormalizeTargetRequest(t *model.StaticTarget) {
	for i, entry := range t.Request.AcceptStatus {
		t.Request.AcceptStatus[i] = strings.ToLower(strings.TrimSpace(entry))
	}
	for i, code := range t.Request.AcceptCodes {
		t.Request.AcceptCodes[i] = strings.ToUpper(strings.TrimSpace(code))
	}
	if retry := t.Request.Retry; retry != nil {
		for i, code := range retry.Codes {
			retry.Codes[i] = strings.ToUpper(strings.TrimSpace(code))
		}
	}
}

// checkAcceptStatus is normalizeAcceptStatus for a list that must not be
// written, a static target's: a copy is normalized and checked.
func checkAcceptStatus(entries []string) error {
	return normalizeAcceptStatus(slices.Clone(entries))
}

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
	if t.Request.Message != "" {
		message := t.Request.Message
		out.Message = &message
	}
	if len(t.Request.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(t.Request.Metadata))
		for key, value := range t.Request.Metadata {
			out.Metadata[key] = value
		}
	}
	if t.Request.AcceptStatus != nil {
		out.AcceptStatus = append([]string{}, t.Request.AcceptStatus...)
	}
	if t.Request.AcceptCodes != nil {
		out.AcceptCodes = append([]string{}, t.Request.AcceptCodes...)
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
		if retry.Codes != nil {
			out.RetryCodes = append([]string{}, retry.Codes...)
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
