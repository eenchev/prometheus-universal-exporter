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

// A grpc target whose host is written with a character outside ASCII is
// refused as an http target's is (canonicalHost): in every form a target
// takes, behind a proxy and without one, whatever the collector's lists,
// and before a connection is set up for it. It was judged as the ASCII
// form it converts to, which is not the name grpc-go, given the target as
// it is written, would have asked a resolver or a proxy for. The name in
// its xn-- form is judged as it is written, and refused by a list that
// names it.
func TestGRPCAHostOutsideASCIIIsRefused(t *testing.T) {
	restoreProxy, restoreResolve := proxyOverride, resolveHost
	t.Cleanup(func() { proxyOverride, resolveHost = restoreProxy, restoreResolve })
	resolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	none := grpcCollector()
	names := grpcCollector()
	names.Request.AllowedTargets = []string{"origin.test", "*.example", "127.0.0.1"}
	denied := grpcCollector()
	denied.Request.DeniedTargets = []string{"xn--rigin-qr33a.test", "xn--bcher-kva.example"}
	connections := grpcConns.size()
	for _, proxied := range []bool{true, false} {
		proxyOverride = func(*url.URL) bool { return proxied }
		for name, c := range map[string]*model.Collector{"no lists": validGRPC(t, none), "the mapped names allowed": validGRPC(t, names), "the unmapped names denied": validGRPC(t, denied)} {
			for _, target := range []string{
				"ｏrigin.test:443",
				"dns:///ｏrigin.test:443",
				"grpc://origin。test:443",
				"grpcs://bücher.example:443",
				"BÜCHER.example:443",
				"bu\u0308cher.example:443",
				"１２７.０.０.１:443",
				"a\u200db.example:443",
				"[fe80::1%ｅth0]:443",
			} {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
				cancel()
				if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), ", a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example") {
					t.Errorf("%s, behind a proxy %v: %s: err=%v, want it refused for a character outside ASCII", name, proxied, target, err)
				}
			}
		}
	}
	if got := grpcConns.size(); got != connections {
		t.Errorf("%d connections were set up for the refused targets", got-connections)
	}
	proxyOverride = func(*url.URL) bool { return true }
	_, err := FetchCollector(context.Background(), "grpcs://XN--bcher-kva.example:443", validGRPC(t, denied), RequestOverrides{}, nil)
	if want := `target xn--bcher-kva.example refused: it matches "xn--bcher-kva.example" in request.denied_targets`; err == nil || err.Error() != want {
		t.Errorf("the xn-- form of a denied name: err=%v, want %q", err, want)
	}
	_, err = FetchCollector(context.Background(), "ｏrigin.test:443", validGRPC(t, names), RequestOverrides{}, nil)
	if want := "target ｏrigin.test refused: it has U+FF4F 'ｏ', a character outside ASCII: write an internationalised name in its ASCII form, as xn--bcher-kva.example for bücher.example"; err == nil || err.Error() != want {
		t.Errorf("err=%v, want %q", err, want)
	}
}
