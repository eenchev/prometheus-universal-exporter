//go:build request_type_smtp || request_type_mqtt || request_type_ftp

package fetch

import "github.com/eenchev/prometheus-universal-exporter/internal/model"

// withAccount gives a collector of a test its account.
func withAccount(user, passwordFile string) func(*model.RequestConfig) {
	return func(r *model.RequestConfig) { r.Username, r.PasswordFile = user, passwordFile }
}
