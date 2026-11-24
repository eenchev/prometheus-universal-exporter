//go:build request_type_smtp || request_type_mqtt || request_type_ftp || request_type_imap || request_type_pop3 || request_type_sip || request_type_amqp || request_type_nats

package fetch

import (
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// netPassword reads request.password_file, "" when the collector names
// none; an empty file is a failure, since a login with no password is not
// what a collector that names a file means.
func netPassword(c *model.Collector) (string, error) {
	if c.Request.PasswordFile == "" {
		return "", nil
	}
	password, err := ReadCredentialFile(c.Request.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("reading request.password_file: %w", err)
	}
	if password == "" {
		return "", fmt.Errorf("request.password_file %s is empty", c.Request.PasswordFile)
	}
	return password, nil
}
