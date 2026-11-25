package fetch

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The credential keys, basic_auth, basic_auth_file, bearer_token and
// bearer_token_file, mean the same for every type that sends them: an
// Authorization header over HTTP, the authorization metadata over gRPC.

// collectorAuthorization is requestAuthorization for a request that also
// carries forwarded headers: a static target's own, or those a probe
// forwards. An Authorization among them is sent instead of the collector's,
// so the collector's is not built at all. A static target with its own
// credential must not fail because the collector's credential file, which
// it never sends, is missing or empty.
func collectorAuthorization(c *model.Collector, forwarded ...http.Header) (string, error) {
	for _, headers := range forwarded {
		for name, values := range headers {
			if len(values) > 0 && strings.EqualFold(name, "Authorization") {
				return "", nil
			}
		}
	}
	return requestAuthorization(c)
}

// CheckBasicUsername refuses a Basic credential's username that holds a
// colon. The credential is the username and the password joined by a colon
// (RFC 7617), and a server splits it at the first one. A username with a
// colon reaches the server as a shorter username plus a password that
// starts with the rest of it. The server then refuses it on every scrape,
// or the exporter's own web.basic_auth never matches it. The value itself
// is never shown.
func CheckBasicUsername(username string) error {
	if strings.Contains(username, ":") {
		return errors.New(`holds a colon (":"), which ends the username of a Basic credential (RFC 7617): the server reads the username up to it and the rest as the start of the password, so a Basic username cannot hold one`)
	}
	return nil
}

// requestAuthorization is the Authorization value a collector's credential
// keys make, or "" when it sets none. Credential files are read on every
// call, so a rotated secret is picked up at the next request.
func requestAuthorization(c *model.Collector) (string, error) {
	var username, password string
	if c.Request.BasicAuth != nil {
		username, password = c.Request.BasicAuth.Username, c.Request.BasicAuth.Password
	}
	if c.Request.BasicAuthFile != nil {
		var err error
		if username, err = ReadCredentialFile(c.Request.BasicAuthFile.Username); err != nil {
			return "", fmt.Errorf("reading basic auth username file: %w", err)
		}
		if password, err = ReadCredentialFile(c.Request.BasicAuthFile.Password); err != nil {
			return "", fmt.Errorf("reading basic auth password file: %w", err)
		}
		if username == "" || password == "" {
			return "", errors.New("basic auth credential files must not be empty")
		}
		if err := CheckBasicUsername(username); err != nil {
			return "", fmt.Errorf("basic auth username file %s %w", c.Request.BasicAuthFile.Username, err)
		}
	}
	if username != "" || password != "" {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password)), nil
	}
	token := c.Request.BearerToken
	if c.Request.BearerTokenFile != "" {
		value, err := ReadCredentialFile(c.Request.BearerTokenFile)
		if err != nil {
			return "", fmt.Errorf("reading bearer token file: %w", err)
		}
		if value == "" {
			return "", fmt.Errorf("bearer token file %s is empty", c.Request.BearerTokenFile)
		}
		token = value
	}
	if token != "" {
		return "Bearer " + token, nil
	}
	return "", nil
}
