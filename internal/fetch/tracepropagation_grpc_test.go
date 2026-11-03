//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"encoding/hex"
	"slices"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
)

// A gRPC call carries the trip's traceparent and tracestate as its metadata,
// under the span id recorded with the call, and one request.metadata sets
// is sent as it is, with no tracestate added.
func TestAGRPCCallCarriesItsTraceparent(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	c := grpcCollector()
	editGRPC(&c, func(r *GRPCConfig) { r.Message = `{"queue": "orders"}` })
	checked := validGRPC(t, c)
	propagated := [16]byte{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c}
	ctx := WithTracePropagation(context.Background(), TracePropagation{TraceID: propagated, State: "vendor=x"})
	ctx, trace := WithSpanTrace(ctx)
	if _, err := FetchCollector(ctx, server.Addr, checked, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	span := trace.Requests()[0].SpanID
	md := server.Calls()[0].Metadata
	want := "00-0af7651916cd43dd8448eb211c80319c-" + hex.EncodeToString(span[:]) + "-00"
	if span == [8]byte{} || len(md.Get("traceparent")) != 1 || md.Get("traceparent")[0] != want || len(md.Get("tracestate")) != 1 || md.Get("tracestate")[0] != "vendor=x" {
		t.Errorf("the call's metadata is %v, want traceparent %s and tracestate vendor=x", md, want)
	}

	configured := "00-11111111111111111111111111111111-2222222222222222-01"
	editGRPC(&c, func(r *GRPCConfig) { r.Metadata = map[string]string{"traceparent": configured} })
	checked = validGRPC(t, c)
	if _, err := FetchCollector(ctx, server.Addr, checked, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	md = server.Calls()[1].Metadata
	if len(md.Get("traceparent")) != 1 || md.Get("traceparent")[0] != configured || len(md.Get("tracestate")) != 0 {
		t.Errorf("request.metadata's traceparent arrives as %v", md)
	}
}

// A tracestate request.metadata sets without a traceparent is dropped when
// the exporter sends a traceparent of its own or its caller's, as an HTTP
// request's header is, so that it never travels beside a traceparent it was
// not written for: the exporter's tracestate, or none, goes in its place.
// With nothing to propagate the operator's is sent as it is.
func TestAGRPCCallDropsTheOperatorsLoneTracestateBesideTheExportersTraceparent(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	c := grpcCollector()
	editGRPC(&c, func(r *GRPCConfig) {
		r.Message = `{"queue": "orders"}`
		r.Metadata = map[string]string{"tracestate": "operator=1"}
	})
	checked := validGRPC(t, c)
	caller := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	for i, tc := range []struct {
		propagation TracePropagation
		parent      bool
		state       []string
	}{
		{TracePropagation{TraceID: [16]byte{1}}, true, nil},
		{TracePropagation{TraceID: [16]byte{1}, State: "vendor=x"}, true, []string{"vendor=x"}},
		{TracePropagation{Parent: caller}, true, nil},
		{TracePropagation{Parent: caller, ParentState: "vendor=y"}, true, []string{"vendor=y"}},
		{TracePropagation{}, false, []string{"operator=1"}},
	} {
		ctx := WithTracePropagation(context.Background(), tc.propagation)
		if _, err := FetchCollector(ctx, server.Addr, checked, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		md := server.Calls()[i].Metadata
		if (len(md.Get("traceparent")) == 1) != tc.parent || !slices.Equal(md.Get("tracestate"), tc.state) {
			t.Errorf("%+v: the call's metadata is %v, want tracestate %v", tc.propagation, md, tc.state)
		}
	}
}
