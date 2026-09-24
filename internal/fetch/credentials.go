package fetch

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The credential keys, basic_auth, basic_auth_file, bearer_token and
// bearer_token_file, mean the same for every type that sends them: an
// Authorization header over HTTP, the authorization metadata over gRPC.

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
