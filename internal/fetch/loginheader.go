package fetch

import (
	"net/http"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// How a request.login's token is sent (login.go), outside the build tags of
// the types that log in, since --dry-run shows it whatever the build.

// LoginHeader is the header a login's token is sent in: login.header,
// Cookie with token_cookie, Authorization otherwise.
func LoginHeader(l *model.LoginConfig) string {
	switch {
	case l.Header != "":
		return http.CanonicalHeaderKey(l.Header)
	case l.TokenCookie != "":
		return "Cookie"
	}
	return "Authorization"
}

// LoginPrefix is what the token is sent after: login.prefix, the cookie's
// name and = with token_cookie, "Bearer " in Authorization, and nothing in
// any other header.
func LoginPrefix(l *model.LoginConfig) string {
	switch {
	case l.TokenCookie != "":
		return l.TokenCookie + "="
	case l.Prefix != nil:
		return *l.Prefix
	case strings.EqualFold(LoginHeader(l), "Authorization"):
		return "Bearer "
	}
	return ""
}
