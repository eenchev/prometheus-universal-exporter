//go:build request_type_smtp || request_type_imap || request_type_pop3

package fetch

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

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

// A static target's own password_file is refused when its collector logs
// in with xoauth2, which would read the file and send the collector's
// token for the target's account; with a password login it is the
// target's account, as before.
func TestATargetOfAnXOAuth2CollectorNamesNoPasswordOfItsOwn(t *testing.T) {
	for _, typ := range BuiltRequestTypes() {
		rt := RequestTypes[typ]
		if !slices.Contains(rt.Fields, "xoauth2") {
			continue
		}
		target := &model.StaticTarget{Name: "other", Request: model.TargetRequestConfig{Username: "other@example.com", PasswordFile: "/password"}}
		withToken := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: typ, Username: "me@example.com", XOAuth2: &model.XOAuth2Config{TokenFile: "/token"}}}
		withPassword := &model.Collector{Name: "mail", Request: model.RequestConfig{Type: typ, Username: "me@example.com", PasswordFile: "/password"}}
		for _, c := range []*model.Collector{withToken, withPassword} {
			if err := ValidateRequest(c); err != nil {
				t.Fatal(err)
			}
		}
		if err := CheckTargetRequest(target, withToken); err == nil || !strings.Contains(err.Error(), `target "other": request.password_file is set, but collector "mail" logs in with xoauth2`) {
			t.Errorf("%s: err=%v, want the target's password_file refused", typ, err)
		}
		if err := CheckTargetRequest(target, withPassword); err != nil {
			t.Errorf("%s: a target's account under a password login: %v", typ, err)
		}
	}
}
