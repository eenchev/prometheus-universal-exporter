//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// watchCollector calls the queue service's server streaming Watch, reading
// at most most messages, with descriptors from the server's reflection.
func watchCollector(t *testing.T, most int) *model.Collector {
	t.Helper()
	c := grpcCollector()
	editGRPC(&c, func(r *GRPCConfig) {
		r.RPC, r.Message, r.StreamMessages = grpctest.Service+"/Watch", `{"queue": "orders"}`, most
	})
	return validGRPC(t, c)
}

// countingWatch sends n messages, each its total its number, and ends the
// stream, or holds it open after them when hold is set, until the call ends.
func countingWatch(n int, hold bool) func(context.Context, string, func(string) error) error {
	return func(ctx context.Context, _ string, send func(string) error) error {
		for i := 1; i <= n; i++ {
			if err := send(fmt.Sprintf(`{"total": %d}`, i)); err != nil {
				return err
			}
		}
		if hold {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
}

// With stream_messages, a server streaming method is called with the
// message, and its stream read until the server ends it, every message in
// the answer's messages and ended true, or until stream_messages have come,
// when the stream is cut and ended is false; a stream held open after them
// is not waited on.
func TestAGRPCCollectorReadsAServerStream(t *testing.T) {
	ended := grpctest.Start(t, grpctest.Options{Reflection: "v1", Watch: countingWatch(2, false)})
	resp, err := FetchCollector(context.Background(), ended.Addr, watchCollector(t, 5), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(resp.Body), `{"messages":[{"shards":[],"total":"1",`) || !strings.Contains(string(resp.Body), `"total":"2"`) || !strings.HasSuffix(string(resp.Body), `}],"ended":true}`) {
		t.Errorf("ended stream: %s", resp.Body)
	}
	if calls := ended.Calls(); len(calls) != 1 || calls[0].Method != "Watch" || calls[0].Request != `{"queue":"orders"}` {
		t.Errorf("calls %+v", calls)
	}
	held := grpctest.Start(t, grpctest.Options{Reflection: "v1", Watch: countingWatch(3, true)})
	start := time.Now()
	resp, err = FetchCollector(context.Background(), held.Addr, watchCollector(t, 2), RequestOverrides{Timeout: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(resp.Body), `"total"`) != 2 || !strings.HasSuffix(string(resp.Body), `],"ended":false}`) || time.Since(start) > 5*time.Second {
		t.Errorf("a stream cut at 2 messages after %v: %s", time.Since(start), resp.Body)
	}
}

// A stream the server ends with a status fails the call with it, as a
// unary call's status does, and one held open with fewer messages than
// stream_messages fails at the deadline; messages past the response limit
// together fail it as a limit exceeded.
func TestAGRPCServerStreamsFailures(t *testing.T) {
	failing := grpctest.Start(t, grpctest.Options{Reflection: "v1", Watch: func(_ context.Context, _ string, send func(string) error) error {
		if err := send(`{"total": 1}`); err != nil {
			return err
		}
		return status.Error(codes.ResourceExhausted, "too many watchers")
	}})
	_, err := FetchCollector(context.Background(), failing.Addr, watchCollector(t, 5), RequestOverrides{}, nil)
	var failed *CallStatusError
	if !errors.As(err, &failed) || failed.CodeName != "RESOURCE_EXHAUSTED" {
		t.Errorf("a stream ended with a status: %v", err)
	}
	held := grpctest.Start(t, grpctest.Options{Reflection: "v1", Watch: countingWatch(1, true)})
	if _, err := FetchCollector(context.Background(), held.Addr, watchCollector(t, 2), RequestOverrides{Timeout: 300 * time.Millisecond}, nil); err == nil || !strings.Contains(err.Error(), "DEADLINE_EXCEEDED") {
		t.Errorf("a stream held open short of stream_messages: %v", err)
	}
	many := grpctest.Start(t, grpctest.Options{Reflection: "v1", Watch: countingWatch(50, false)})
	small := watchCollector(t, 50)
	small.Request.MaxResponseBytes = 600
	if _, err := FetchCollector(context.Background(), many.Addr, small, RequestOverrides{}, nil); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the stream's messages") {
		t.Errorf("messages past the limit together: %v", err)
	}
}

// stream_messages is from 1 to 1000, and goes with a server streaming
// method alone: a unary one with it is refused, and a server streaming one
// without it, saying to set it; client and bidirectional streaming methods
// are refused either way.
func TestGRPCStreamMessagesIsChecked(t *testing.T) {
	set := grpctest.WriteProtoset(t, filepath.Join(t.TempDir(), "queue.pb"), false)
	c := grpcCollector()
	editGRPC(&c, func(r *GRPCConfig) { r.Descriptors, r.ProtosetFile = "protoset", set })
	for _, tc := range []struct {
		rpc      string
		most     int
		fragment string
	}{
		{grpctest.Service + "/Watch", 0, "set request.stream_messages to read the first messages of its stream"},
		{getStats, 3, "is a unary method; request.stream_messages is for a server streaming method"},
		{grpctest.Service + "/Push", 3, "is a client streaming method"},
		{grpctest.Service + "/Sync", 3, "is a bidirectional streaming method"},
		{grpctest.Service + "/Watch", 1001, "request.stream_messages is 1001"},
		{grpctest.Service + "/Watch", -1, "request.stream_messages is -1"},
	} {
		c := c
		editGRPC(&c, func(r *GRPCConfig) { r.RPC, r.StreamMessages = tc.rpc, tc.most })
		wantRefused(t, c, tc.fragment)
	}
	editGRPC(&c, func(r *GRPCConfig) { r.RPC, r.StreamMessages = grpctest.Service+"/Watch", 1000 })
	validGRPC(t, c)
	// The built-in health service's Watch, which sends the status at once
	// and then each change.
	health := model.Collector{Name: "health", Request: model.RequestConfig{Type: RequestTypeGRPC, Own: GRPCConfig{RPC: "grpc.health.v1.Health/Watch", StreamMessages: 1}}, Transform: model.TransformConfig{Type: "jq"}}
	server := grpctest.Start(t, grpctest.Options{})
	resp, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, health), RequestOverrides{Timeout: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"messages":[{"status":"SERVING"}],"ended":false}` {
		t.Errorf("health Watch: %s", resp.Body)
	}
}

// A server streaming method found by reflection is refused at the call
// without stream_messages, as at load with a descriptor set.
func TestAReflectedServerStreamingMethodNeedsStreamMessages(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Watch: countingWatch(1, false)})
	c := grpcCollector()
	editGRPC(&c, func(r *GRPCConfig) { r.RPC = grpctest.Service + "/Watch" })
	if _, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, c), RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "set request.stream_messages") {
		t.Errorf("Watch without stream_messages: %v", err)
	}
}

// A stream's messages are written in the answer as they were rendered, as
// a unary answer is: a <, > or & in a string is itself, not escaped again.
func TestAGRPCStreamsMessagesAreWrittenAsRendered(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Watch: func(_ context.Context, _ string, send func(string) error) error {
		return send(`{"shards": [{"id": "a<b&c>d"}]}`)
	}})
	resp, err := FetchCollector(context.Background(), server.Addr, watchCollector(t, 1), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), `"id":"a<b&c>d"`) {
		t.Errorf("answer %s", resp.Body)
	}
}

// A connection's phases are a call's only when they began after the call
// did: grpc-go reconnects on its own, between calls.
func TestAGRPCConnectionMadeBeforeTheCallIsNotItsPhase(t *testing.T) {
	var phases grpcPhases
	before := time.Now()
	phases.add("connection.connect", before)
	if got := phases.take(before.Add(time.Millisecond)); got != nil {
		t.Errorf("a connection made before the call: %+v", got)
	}
	phases.add("connection.connect", time.Now())
	phases.add("connection.tls", time.Now())
	if got := phases.take(before); len(got) != 2 {
		t.Errorf("a connection made during the call: %+v", got)
	}
	if got := phases.take(before); got != nil {
		t.Errorf("taken twice: %+v", got)
	}
}
