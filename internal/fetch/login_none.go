//go:build select_request_types && !request_type_http && !request_type_graphite

package fetch

import (
	"context"
	"net/http"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A build without the http and graphite types makes no request over HTTP
// that could log in (login.go): no collector it validates can set
// request.login, so a request never has one.

type requestLogin struct{}

func startLogin(context.Context, string, *model.Collector, RequestOverrides, []http.Header) (*requestLogin, error) {
	return nil, nil
}

func (*requestLogin) again(context.Context) (bool, error) { return false, nil }

func (*requestLogin) set(*http.Request) {}

func (*requestLogin) sends(string) bool { return false }

func (*requestLogin) carried(*http.Request) bool { return false }

// LoginFingerprint is "" in a build where no static target can log in.
func LoginFingerprint(*model.LoginConfig) string { return "" }
