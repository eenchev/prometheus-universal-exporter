//go:build !select_request_types || request_type_grpc

package fetch

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
)

// attemptsToConnectHave gives an attempt to connect to a grpc target limit,
// on the connections made from now on, and the tests' half minute again when
// the test ends; a limit of zero is the exporter's own.
func attemptsToConnectHave(t *testing.T, limit time.Duration) {
	t.Helper()
	SetGRPCConnectTimeout(limit)
	t.Cleanup(func() { SetGRPCConnectTimeout(testsGRPCConnectTimeout) })
}

// An attempt to connect to a grpc target has twenty seconds in the exporter,
// after waits of a second that grow to five at most: what a connection is
// made with when nothing has set another limit, value for value what it was
// before a test could. In the tests it has half a minute, which TestMain
// sets, and the waits are the exporter's.
func TestAnAttemptToConnectHasTwentySecondsInTheExporterAndHalfAMinuteInTheTests(t *testing.T) {
	exporters := grpc.ConnectParams{
		Backoff:           backoff.Config{BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second},
		MinConnectTimeout: 20 * time.Second,
	}
	inTests := exporters
	inTests.MinConnectTimeout = 30 * time.Second
	if got := grpcConnectParams(); got != inTests {
		t.Errorf("a connection of the tests is made with %+v, want %+v: the exporter's waits and the half minute TestMain sets", got, inTests)
	}
	attemptsToConnectHave(t, 0)
	if got := grpcConnectParams(); got != exporters {
		t.Errorf("a connection made with no limit set is made with %+v, want the exporter's %+v", got, exporters)
	}
}

// A target that takes the connection and never answers the HTTP/2 preface is
// given up at the limit of an attempt to connect, long before the probe's
// deadline: the call fails as UNAVAILABLE, naming the server preface that
// did not come, no sooner than the limit, and the target has been sent the
// client's preface.
//
// The target never writes, so nothing but the limit ends the attempt however
// slow the machine, and the limit stays short; the probe itself has a
// minute. grpc-go gives an attempt the longer of the limit and the wait
// before the next attempt, so the wait is shorter still here, where under
// the exporter's second it would be what the attempt has. What a connection
// is made with is read as well, so a limit that was not the one set fails
// the test at once rather than by its length.
func TestAGRPCTargetThatNeverAnswersThePrefaceIsGivenUpAtTheConnectLimit(t *testing.T) {
	const limit = 200 * time.Millisecond
	attemptsToConnectHave(t, limit)
	previous := grpcBackoff
	t.Cleanup(func() { grpcBackoff = previous })
	grpcBackoff = backoff.Config{BaseDelay: time.Millisecond, Multiplier: 1, MaxDelay: time.Millisecond}
	if got := grpcConnectParams().MinConnectTimeout; got != limit {
		t.Fatalf("a connection made now has %s to connect in, want the %s set", got, limit)
	}

	listener := grpctest.Listen(t)
	greeted := make(chan string, 1)
	ended := make(chan struct{})
	t.Cleanup(func() {
		close(ended)
		_ = listener.Close()
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				preface := make([]byte, len("PRI * HTTP/2.0"))
				n, _ := io.ReadFull(conn, preface)
				select {
				case greeted <- string(preface[:n]):
				default:
				}
				<-ended
			}(conn)
		}
	}()

	c := model.Collector{Name: "health", Request: model.RequestConfig{Type: RequestTypeGRPC, RPC: "grpc.health.v1.Health/Check"}, Transform: model.TransformConfig{Type: "jq"}}
	checked := validGRPC(t, c)
	addr := listener.Addr().String()
	// The connection goes on trying for as long as it is kept, so it is
	// closed with the test.
	t.Cleanup(func() {
		grpcConns.mu.Lock()
		defer grpcConns.mu.Unlock()
		key := grpcConnKey{dial: addr, policy: policyOf(checked)}
		if entry := grpcConns.entries[key]; entry != nil {
			delete(grpcConns.entries, key)
			grpcConns.retireLocked(entry)
		}
	})
	took, err := probeGRPC(addr, checked, time.Minute, RequestOverrides{})
	if err == nil {
		t.Fatal("a call to a target that never answers the preface was answered")
	}
	if !strings.Contains(err.Error(), "UNAVAILABLE") || !strings.Contains(err.Error(), "server preface") {
		t.Errorf("the call failed with %v, want it UNAVAILABLE for the server preface that did not come", err)
	}
	if took < limit {
		t.Errorf("the call failed after %s, sooner than the %s an attempt to connect has", took, limit)
	}
	select {
	case preface := <-greeted:
		if preface != "PRI * HTTP/2.0" {
			t.Errorf("the target was sent %q, want the start of the HTTP/2 preface", preface)
		}
	default:
		t.Error("the target was sent nothing: the attempt ended before the preface")
	}
}
