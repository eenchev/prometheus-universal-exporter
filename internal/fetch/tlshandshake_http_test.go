//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// handshakesHave gives the TLS handshakes of the pools built during the test
// limit, and those built after it the tests' half minute again; 0 is the
// exporter's own limit. The test has pools of its own, so none built under
// limit is left to another test.
func handshakesHave(t *testing.T, limit time.Duration) {
	t.Helper()
	freshPools(t)
	SetTLSHandshakeTimeout(limit)
	t.Cleanup(func() { SetTLSHandshakeTimeout(testsTLSHandshakeTimeout) })
}

// A TLS handshake has ten seconds in the exporter, which is the limit of a
// pool built when nothing has set another, and half a minute in the tests,
// which TestMain sets. A pool keeps the limit it was built with.
func TestATLSHandshakeHasTenSecondsInTheExporterAndHalfAMinuteInTheTests(t *testing.T) {
	freshPools(t)
	inTests, err := transports.get(TransportSettings{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if inTests.TLSHandshakeTimeout != 30*time.Second {
		t.Errorf("a pool of the tests gives a TLS handshake %s, want the half minute TestMain sets", inTests.TLSHandshakeTimeout)
	}
	handshakesHave(t, 0)
	own, err := transports.get(TransportSettings{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if own.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("a pool built with no limit set gives a TLS handshake %s, want the exporter's 10s", own.TLSHandshakeTimeout)
	}
	if inTests.TLSHandshakeTimeout != 30*time.Second {
		t.Errorf("the pool built before has %s for a handshake now, want what it was built with", inTests.TLSHandshakeTimeout)
	}
}

// A target that takes the connection and never answers the TLS handshake is
// given up at the handshake's limit, long before the probe's deadline: the
// fetch fails saying so, no sooner than the limit, and the target has been
// sent the start of the handshake.
//
// The target never writes, so nothing but the limit ends the handshake
// however slow the machine, and the limit stays short; the fetch itself has a
// minute. The pool the fetch was made on is read as well, so a limit that
// was not the one set fails the test at once rather than by its length.
func TestATLSHandshakeTheTargetNeverAnswersEndsAtItsLimit(t *testing.T) {
	const limit = 100 * time.Millisecond
	handshakesHave(t, limit)
	greeted := make(chan int, 1)
	silent := connectionTarget(t, func(conn net.Conn) {
		n, _ := conn.Read(make([]byte, 16))
		select {
		case greeted <- n:
		default:
		}
		// Held open until the client gives up and closes it.
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
	})
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.AllowedSchemes = []string{"https"}
		c.Request.TLS.InsecureSkipVerify = true
	})
	pool, err := transports.get(TransportSettings{TLS: c.Request.TLS, EnableHTTP2: c.Request.EnableHTTP2, policy: policyOf(c)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if pool.TLSHandshakeTimeout != limit {
		t.Fatalf("the collector's pool gives a TLS handshake %s, want the %s set", pool.TLSHandshakeTimeout, limit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	_, err = FetchCollector(ctx, "https://"+silent, c, RequestOverrides{}, nil)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "TLS handshake timeout") || ctx.Err() != nil {
		t.Fatalf("the fetch ended with %v after %s, want it failed by the TLS handshake's limit", err, took)
	}
	if took < limit {
		t.Errorf("the fetch failed after %s, sooner than the handshake's limit of %s", took, limit)
	}
	select {
	case n := <-greeted:
		if n == 0 {
			t.Error("the target was sent nothing before the handshake was given up")
		}
	case <-time.After(30 * time.Second):
		t.Error("the target never had the connection")
	}
}
