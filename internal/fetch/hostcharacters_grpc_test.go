//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A grpc target is host:port, with no URL around it, and its host goes
// through the same policy as an http target's: one with a character no host
// name has — a '%' that a proxy might decode into a denied name or the cloud
// metadata address, a comma, a semicolon — is refused before any connection,
// in every form a target takes, whatever the collector's lists. Before, with
// no address rule and a proxy in between, such a host was left to the proxy
// to resolve.
func TestGRPCAHostWithACharacterNoNameHasIsRefused(t *testing.T) {
	restoreProxy, restoreResolve := proxyOverride, resolveHost
	t.Cleanup(func() { proxyOverride, resolveHost = restoreProxy, restoreResolve })
	proxyOverride = func(*url.URL) bool { return true }
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	none := grpcCollector()
	names := grpcCollector()
	names.Request.AllowedTargets = []string{"*.example"}
	names.Request.DeniedTargets = []string{"internal.example"}
	networks := grpcCollector()
	networks.Request.DeniedTargets = []string{"10.0.0.0/8"}
	for name, c := range map[string]*model.Collector{"no lists": validGRPC(t, none), "lists of names": validGRPC(t, names), "a denied network": validGRPC(t, networks)} {
		for _, target := range []string{
			"intern%61l.example:443",
			"dns:///intern%61l.example:443",
			"grpc://intern%61l.example:443",
			"grpcs://%69nternal.example:443",
			"%31%36%39.254.169.254:443",
			"169.254.169.254%00:443",
			"dns:///169%2e254.169.254:443",
			"a,b.example:443",
			"a;b.example:443",
			"a=b.example:443",
			"a b.example:443",
			"a+b.example:443",
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
			cancel()
			if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "a character that no host name or address has") {
				t.Errorf("%s: %s: err=%v, want it refused for its host", name, target, err)
			}
		}
	}
}
