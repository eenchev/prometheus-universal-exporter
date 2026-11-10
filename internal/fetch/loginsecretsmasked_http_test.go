//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A server that refuses a login may answer with what it was sent, a form
// with its fields filled in again, under names of its own: the secrets the
// login sent, and the values its steps read, are masked in the answer the
// error shows, as they are and as a form or JSON writes them, while a value the
// configuration writes, which is no secret, stays.
func TestTheSecretsALoginSentAreMaskedInARefusalsAnswer(t *testing.T) {
	freshLogins(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			_, _ = io.WriteString(w, `nonce=n0nce-read`)
		default:
			sent, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<input name="j_user" value="me@example.com"><input name="j_pw" value="hunter2 & co"> {"pw":"hunter2 \u0026 co"} you sent `+string(sent))
		}
	}))
	defer server.Close()
	files := secretFiles(t, map[string]string{"password": "hunter2 & co"})
	for name, login := range map[string]model.LoginConfig{
		"steps": {Type: "steps", Values: map[string]string{"username": "me@example.com"}, SecretFiles: files, Token: "token", Steps: []model.LoginStep{
			{URL: "/page", Extract: map[string]model.LoginExtract{"nonce": {Regex: `nonce=(\S+)`}}},
			{URL: "/signin", Form: map[string]string{"u": "{{login.username}}", "p": "{{login.password}}", "n": "{{login.nonce}}"}, Extract: map[string]model.LoginExtract{"token": {JSON: ".token"}}},
		}},
		"json": {Type: "json", URL: "/signin", Fields: map[string]string{"username": "me@example.com"}, FieldFiles: files, Token: ".token"},
	} {
		c := loginCollector(t, login)
		_, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		if err == nil {
			t.Fatalf("%s: the login was not refused", name)
		}
		text := err.Error()
		for _, secret := range []string{"hunter2", "n0nce-read", " co", "+co"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s: the error shows %s: %s", name, secret, text)
			}
		}
		if !strings.Contains(text, "HTTP status 403") || !strings.Contains(text, `value="me@example.com"`) || !strings.Contains(text, `value="<redacted>"`) {
			t.Errorf("%s: the error is %s", name, text)
		}
		logins.reset()
	}
}
