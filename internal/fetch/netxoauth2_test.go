//go:build request_type_smtp || request_type_imap || request_type_pop3

package fetch

import "github.com/eenchev/prometheus-universal-exporter/internal/model"

// withLogin sets how a collector logs in, auth and xoauth2, after account
// has set its account, if it sets one.
func withLogin(auth string, xoauth2 *model.XOAuth2Config, account func(*model.RequestConfig)) func(*model.RequestConfig) {
	return func(r *model.RequestConfig) {
		if account != nil {
			account(r)
		}
		r.Auth, r.XOAuth2 = auth, xoauth2
	}
}
