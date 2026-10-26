//go:build !select_request_types || request_type_grpc

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A grpc collector does not log in: its credential is bearer_token_file or
// basic auth, and request.login is refused as a key of another type.
func TestAGRPCCollectorTakesNoLogin(t *testing.T) {
	c := model.Collector{Name: "rpc", Request: model.RequestConfig{Type: RequestTypeGRPC,
		Login: &model.LoginConfig{Type: "oauth2", URL: "https://idp.example/token", ClientID: "x", ClientSecretFile: "/s"}, Own: GRPCConfig{RPC: "grpc.health.v1.Health/Check"}}}
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `sets request.login, which does not apply to request.type "grpc"`) {
		t.Errorf("got %v", err)
	}
}
