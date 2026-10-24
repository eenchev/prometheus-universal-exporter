//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A grpc static target's own tls block replaces the collector's: a
// collector that calls in plaintext reaches a TLS server through a target
// that trusts its CA, and targets whose blocks differ share no connection,
// a connection being kept by its TLS settings. The label of such a target
// is grpcs://.
func TestAGRPCStaticTargetCallsWithItsOwnTLS(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{TLS: true, Reflection: "v1", Answer: statsAnswer})
	c := validGRPC(t, grpcCollector())
	connections := func() int {
		grpcConns.mu.Lock()
		defer grpcConns.mu.Unlock()
		n := 0
		for key := range grpcConns.entries {
			if key.dial == server.Addr {
				n++
			}
		}
		return n
	}
	before := connections()
	for _, settings := range []model.TLSConfig{{CAFile: server.CAFile, Set: true}, {CAFile: server.CAFile, ServerName: "queue.test", Set: true}} {
		target := &model.StaticTarget{Name: "x", Collector: c.Name, Target: server.Addr, Request: model.TargetRequestConfig{TLS: &settings}}
		if _, err := FetchCollector(context.Background(), server.Addr, c, TargetOverrides(target), nil); err != nil {
			t.Fatalf("%+v: %v", settings, err)
		}
		if label, err := RequestLabelFor(server.Addr, c, TargetOverrides(target)); err != nil || !strings.HasPrefix(label, "grpcs://") {
			t.Errorf("the label of a target with its own tls block: %q, %v", label, err)
		}
	}
	if got := connections() - before; got != 2 {
		t.Errorf("two targets with blocks of their own made %d connections, want one each", got)
	}
	if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{Timeout: 2 * time.Second}, nil); err == nil {
		t.Error("the collector's plaintext reached the TLS server")
	}
}

// A grpc static target's own tls block is checked as the collector's is,
// with no placeholder in it, and a grpc:// target asks for plaintext,
// which its own block contradicts. --dry-run reads its files, as it now
// reads the collector's.
func TestAGRPCStaticTargetsTLSIsChecked(t *testing.T) {
	c := validGRPC(t, grpcCollector())
	ca := &model.TLSConfig{CAFile: "/ca.pem", Set: true}
	checkTargetRequests(t, c, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"a tls block":               {"queue.internal:443", model.TargetRequestConfig{TLS: ca}, ""},
		"grpcs:// with a tls block": {"grpcs://queue.internal:443", model.TargetRequestConfig{TLS: ca}, ""},
		"grpc:// with its own tls":  {"grpc://queue.internal:443", model.TargetRequestConfig{TLS: ca}, "asks for plaintext with grpc://, but sets request.tls of its own"},
		"half a client certificate": {"queue.internal:443", model.TargetRequestConfig{TLS: &model.TLSConfig{CertFile: "/c.pem", Set: true}}, "only one of cert_file and key_file"},
		"a placeholder in the tls":  {"queue.internal:443", model.TargetRequestConfig{TLS: &model.TLSConfig{CAFile: "{{param_ca}}", Set: true}}, "has a placeholder"},
		"a password":                {"queue.internal:443", model.TargetRequestConfig{PasswordFile: "/p"}, "sets request.password_file, which does not apply"},
	})
	checkTargetFilesAreRead(t, c, false)
	missing := grpcCollector()
	missing.Request.TLS = model.TLSConfig{CAFile: filepath.Join(t.TempDir(), "ca.pem")}
	if warnings := FileWarnings(validGRPC(t, missing), nil); len(warnings) != 1 || !strings.Contains(warnings[0], `collector "queue": request.tls: open `) {
		t.Errorf("the collector's missing CA: %q", warnings)
	}
}
