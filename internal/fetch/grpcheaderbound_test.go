//go:build !select_request_types || request_type_grpc

package fetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// headerListServer is a gRPC server, spoken to frame by frame, that answers
// every call OK with the headers it is given, whatever the bound the client
// advertises, as a server that does not read that setting does. It is
// what shows the client's own bound: grpc-go's server keeps to the bound
// and refuses to send more itself.
// calls counts the calls it answered.
func headerListServer(t *testing.T, fields []hpack.HeaderField, calls *atomic.Int32) string {
	t.Helper()
	listener := grpctest.Listen(t)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go answerWithHeaders(conn, fields, calls)
		}
	}()
	return listener.Addr().String()
}

func answerWithHeaders(conn net.Conn, fields []hpack.HeaderField, calls *atomic.Int32) {
	defer func() { _ = conn.Close() }()
	if _, err := io.ReadFull(conn, make([]byte, len(http2.ClientPreface))); err != nil {
		return
	}
	framer := http2.NewFramer(conn, conn)
	if framer.WriteSettings() != nil {
		return
	}
	block := func(fields ...hpack.HeaderField) []byte {
		var b bytes.Buffer
		encoder := hpack.NewEncoder(&b)
		for _, f := range fields {
			_ = encoder.WriteField(f)
		}
		return b.Bytes()
	}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() && framer.WriteSettingsAck() != nil {
				return
			}
		case *http2.HeadersFrame:
			id := frame.StreamID
			calls.Add(1)
			// In frames of the client's largest size. A client reading
			// past its bound stops at the field that goes past it, and
			// closes the connection at any frame after that one, so the
			// tests put the field past it last.
			head := block(append([]hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-type", Value: "application/grpc"}}, fields...)...)
			for from := 0; from < len(head); from += 16 << 10 {
				chunk, last := head[from:min(from+16<<10, len(head))], from+16<<10 >= len(head)
				if from == 0 {
					err = framer.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: chunk, EndHeaders: last})
				} else {
					err = framer.WriteContinuation(id, last, chunk)
				}
				if err != nil {
					return
				}
			}
			// An empty HealthCheckResponse, and status OK.
			if framer.WriteData(id, false, make([]byte, 5)) != nil {
				return
			}
			if framer.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: block(hpack.HeaderField{Name: "grpc-status", Value: "0"}), EndHeaders: true, EndStream: true}) != nil {
				return
			}
		}
	}
}

// fillerHeaders are n headers of size bytes each, as the HTTP/2 header
// list counts them (a field's name and value and 32).
func fillerHeaders(n, size int) []hpack.HeaderField {
	fields := make([]hpack.HeaderField, n)
	for i := range fields {
		name := "x-filler-" + strconv.Itoa(1000+i)
		fields[i] = hpack.HeaderField{Name: name, Value: strings.Repeat("x", size-len(name)-32)}
	}
	return fields
}

// A gRPC answer's headers are held to the bound an http answer's are,
// maxResponseHeaderBytes, where grpc-go's own is 16 MiB: an answer whose
// headers are over it fails the call as the http one fails, counted as a
// limit, in the words that say what to do, and is not retried, where one
// just under it is taken whole.
func TestAGRPCAnswersHeadersAreHeldToTheHTTPBound(t *testing.T) {
	health := model.Collector{Name: "health", Request: model.RequestConfig{Type: RequestTypeGRPC, RPC: "grpc.health.v1.Health/Check"}, Transform: model.TransformConfig{Type: "jq"}}
	health.Request.Retry.Codes = []string{"INTERNAL"}
	health.Request.Retry.Attempts = 2
	c := validGRPC(t, health)
	// Under the bound, with what the answer's own two fields take.
	under := fillerHeaders(63, 16<<10)
	var calls atomic.Int32
	resp, err := FetchCollector(context.Background(), headerListServer(t, under, &calls), c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatalf("an answer whose headers are under the bound failed: %v", err)
	}
	if got := resp.Headers.Get(under[62].Name); len(got) != len(under[62].Value) {
		t.Fatalf("the last header under the bound has %d bytes, want %d", len(got), len(under[62].Value))
	}
	// Over it in the last field.
	over := append(fillerHeaders(63, 16<<10), fillerHeaders(1, 20<<10)...)
	over[63].Name = "x-past"
	calls.Store(0)
	resp, err = FetchCollector(context.Background(), headerListServer(t, over, &calls), c, RequestOverrides{}, nil)
	if err == nil {
		t.Fatalf("an answer whose headers are past the bound was taken, %d bytes of its last", len(resp.Headers.Get("x-past")))
	}
	var call *CallStatusError
	if !errors.As(err, &call) || call.CodeName != "INTERNAL" || !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("got %v, want an INTERNAL status counted as a limit", err)
	}
	want := "gRPC call failed: INTERNAL: the answer's headers or trailers are larger than 1048576 bytes, the most the exporter reads of a response's headers, whatever max_response_bytes allows its body; the target has to send fewer or smaller headers"
	if err.Error() != want {
		t.Errorf("the failure reads\n%v\nwant\n%s", err, want)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the call was made %d times, want once: INTERNAL is among retry.codes, and headers over the bound are not retried", n)
	}
}

// A server that keeps to the bound the client advertises, as grpc-go's
// does, does not send headers past it: the call fails rather than take
// them, which it did up to grpc-go's own 16 MiB.
func TestAGRPCServerKeepsToTheHeaderBoundTheExporterAdvertises(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(ctx context.Context, m, r string) (string, error) {
		if err := grpc.SetHeader(ctx, metadata.Pairs("x-big", big)); err != nil {
			return "", err
		}
		return statsAnswer(ctx, m, r)
	}})
	resp, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, grpcCollector()), RequestOverrides{}, nil)
	if err == nil {
		t.Fatalf("a header of %d bytes was taken, past the bound of %d", len(resp.Headers.Get("x-big")), maxResponseHeaderBytes)
	}
}

// The reflection question's answer is held to the bound as the call's is,
// and fails the probe in the same words, saying it was the question's.
func TestAReflectionAnswerPastTheHeaderBoundIsALimit(t *testing.T) {
	err := reflectionError(context.Background(), status.Error(codes.Internal, "peer header list size exceeded limit"))
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.HasPrefix(err.Error(), "gRPC call failed: INTERNAL: asking the reflection service: the answer's headers or trailers are larger than 1048576 bytes") {
		t.Errorf("got %v", err)
	}
	if other := reflectionError(context.Background(), status.Error(codes.Internal, "something else")); errors.Is(other, model.ErrLimitExceeded) {
		t.Errorf("another INTERNAL status was counted as a limit: %v", other)
	}
}
