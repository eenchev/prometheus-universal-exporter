//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// What gRPC says of a call that failed reaches the call as a status
// message, text and nothing else, so what belongs to one connection or one
// answer is left out of that text in the forms gRPC and Go write it. A
// server that resets every connection during the TLS handshake fails the
// calls with the port each connection was made from; one whose answer is
// over the response limit, with the size that answer had; one whose
// certificate has expired, with the time of the attempt. Each is recognised
// by one text, with the mark for that part, and stays the call's status. A
// refused connection, which names only where it went, is recognised by its
// text as it is. So is what the server itself answers a call with, which
// reaches the call as a status message too: a server that says which of its
// replicas is lagging, as two addresses with an arrow between them, fails
// the calls with another failure for another replica.
func TestAGRPCCallsFailureIsRecognisedWithoutWhatBelongsToOneConnection(t *testing.T) {
	t.Run("a reset during the TLS handshake", func(t *testing.T) {
		target := resettingTarget(t)
		c := validGRPC(t, protosetCollector(t))
		a, b := fetchFailures(t, c, "grpcs://"+target)
		// A call made while the channel waits to connect again is answered
		// with the failure of the connection before it.
		for range 100 {
			if a.Error() != b.Error() {
				break
			}
			time.Sleep(50 * time.Millisecond)
			_, b = fetchFailures(t, c, "grpcs://"+target)
		}
		if a.Error() == b.Error() || !strings.Contains(a.Error(), "read tcp 127.0.0.1:") {
			t.Fatalf("the failures read\n%v\n%v\nwant each with the port its connection was made from", a, b)
		}
		want := `gRPC call failed: UNAVAILABLE: connection error: desc = "transport: authentication handshake failed: read tcp ` + model.MovingMark + `->` + target + `: read: connection reset by peer"`
		if model.SameFailureText(a) != want || model.SameFailureText(b) != want {
			t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", a, b, model.SameFailureText(a), model.SameFailureText(b), want)
		}
		var status *CallStatusError
		if !errors.As(a, &status) || status.CodeName != "UNAVAILABLE" {
			t.Errorf("%v is no longer the call's status", a)
		}
	})
	t.Run("an answer over the limit", func(t *testing.T) {
		var answers atomic.Int64
		server := grpctest.Start(t, grpctest.Options{Answer: func(context.Context, string, string) (string, error) {
			return `{"shards":[{"id":"` + strings.Repeat("x", 5000+int(answers.Add(1))) + `"}]}`, nil
		}})
		c := protosetCollector(t)
		c.Request.MaxResponseBytes = 1000
		a, b := fetchFailures(t, validGRPC(t, c), server.Addr)
		if a.Error() == b.Error() || !strings.HasPrefix(a.Error(), "gRPC call failed: RESOURCE_EXHAUSTED: grpc: received message larger than max (50") || !errors.Is(a, model.ErrLimitExceeded) {
			t.Fatalf("the failures read\n%v\n%v\nwant each with the size of its answer, as a limit", a, b)
		}
		want := "gRPC call failed: RESOURCE_EXHAUSTED: grpc: received message larger than max (" + model.MovingMark + " vs. 1000)"
		if model.SameFailureText(a) != want || model.SameFailureText(b) != want {
			t.Errorf("the failures\n%v\n%v\nare recognised by\n%s\n%s\nwant both by\n%s", a, b, model.SameFailureText(a), model.SameFailureText(b), want)
		}
	})
	t.Run("an expired certificate", func(t *testing.T) {
		certificate := expiredCertificate(t)
		listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					_ = conn.(*tls.Conn).Handshake()
					_ = conn.Close()
				}()
			}
		}()
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		a, _ := fetchFailures(t, validGRPC(t, protosetCollector(t)), "grpcs://"+listener.Addr().String())
		said := `gRPC call failed: UNAVAILABLE: connection error: desc = "transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time `
		validTo := " is after " + leaf.NotAfter.Format(time.RFC3339) + `"`
		if !strings.HasPrefix(a.Error(), said+time.Now().Format("2006-01-02T")) || !strings.HasSuffix(a.Error(), validTo) {
			t.Fatalf("the failure reads %v, want it to say %q, the time and %q", a, said, validTo)
		}
		if want := said + model.MovingMark + validTo; model.SameFailureText(a) != want {
			t.Errorf("the failure %v is recognised by\n%s\nwant\n%s", a, model.SameFailureText(a), want)
		}
	})
	t.Run("a status the server sends", func(t *testing.T) {
		var calls atomic.Int64
		server := grpctest.Start(t, grpctest.Options{Answer: func(context.Context, string, string) (string, error) {
			return "", status.Errorf(codes.Internal, "replica 10.0.0.%d:5432->10.0.0.9:5432 is lagging", calls.Add(1))
		}})
		a, b := fetchFailures(t, validGRPC(t, protosetCollector(t)), server.Addr)
		for i, err := range []error{a, b} {
			want := "gRPC call failed: INTERNAL: replica 10.0.0." + string(rune('1'+i)) + ":5432->10.0.0.9:5432 is lagging"
			if err.Error() != want || model.SameFailureText(err) != want {
				t.Errorf("the server's status fails call %d with\n%v\nrecognised by\n%s\nwant both\n%s", i+1, err, model.SameFailureText(err), want)
			}
		}
	})
	t.Run("a refused connection", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		nobody := listener.Addr().String()
		_ = listener.Close()
		a, _ := fetchFailures(t, validGRPC(t, protosetCollector(t)), nobody)
		want := `gRPC call failed: UNAVAILABLE: connection error: desc = "transport: Error while dialing: dial tcp ` + nobody + `: connect: connection refused"`
		if a.Error() != want || model.SameFailureText(a) != want {
			t.Errorf("a refused connection fails with\n%v\nrecognised by\n%s\nwant both\n%s", a, model.SameFailureText(a), want)
		}
	})
}
