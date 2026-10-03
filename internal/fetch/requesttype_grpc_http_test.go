//go:build !select_request_types || (request_type_http && request_type_grpc)

package fetch

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The configuration's rules: rpc's shape, descriptors required but for the
// health service, each source's keys only with it, and the metadata, retry
// and message rules.
func TestGRPCValidation(t *testing.T) {
	c := grpcCollector()
	c.Request.RPC = " /" + getStats + " "
	if got := validGRPC(t, c).Request.RPC; got != getStats {
		t.Fatalf("rpc normalized to %q", got)
	}
	for _, tc := range []struct {
		name     string
		change   func(*model.Collector)
		fragment string
	}{
		{"no rpc", func(c *model.Collector) { c.Request.RPC = "" }, "has no request.rpc"},
		{"no method", func(c *model.Collector) { c.Request.RPC = grpctest.Service }, "is not a method"},
		{"placeholder rpc", func(c *model.Collector) { c.Request.RPC = "a.{{param_x}}/M" }, "has a placeholder"},
		{"no descriptors", func(c *model.Collector) { c.Request.Descriptors = "" }, "has no request.descriptors"},
		{"unknown source", func(c *model.Collector) { c.Request.Descriptors = "buf" }, "want reflection, protoset or proto"},
		{"protoset file without protoset", func(c *model.Collector) { c.Request.ProtosetFile = "x.pb" }, "applies only with descriptors: protoset"},
		{"proto files without proto", func(c *model.Collector) { c.Request.ProtoFiles = []string{"x.proto"} }, "apply only with descriptors: proto"},
		{"protoset without file", func(c *model.Collector) { c.Request.Descriptors = "protoset" }, "no request.protoset_file"},
		{"proto without files", func(c *model.Collector) { c.Request.Descriptors = "proto" }, "no request.proto_files"},
		{"upper-case metadata", func(c *model.Collector) { c.Request.Metadata = map[string]string{"X-Tenant": "a"} }, "lower-case"},
		{"binary metadata", func(c *model.Collector) { c.Request.Metadata = map[string]string{"trace-bin": "a"} }, "binary metadata"},
		{"reserved metadata", func(c *model.Collector) { c.Request.Metadata = map[string]string{"grpc-timeout": "1S"} }, "reserved"},
		{"content-type metadata", func(c *model.Collector) { c.Request.Metadata = map[string]string{"content-type": "x"} }, "reserved"},
		{"control character", func(c *model.Collector) { c.Request.Metadata = map[string]string{"x-a": "a\nb"} }, "control character"},
		{"authorization twice", func(c *model.Collector) {
			c.Request.Metadata = map[string]string{"authorization": "x"}
			c.Request.BearerToken = "t"
		}, "credential keys already send it"},
		{"forwarded reserved header", func(c *model.Collector) { c.Request.ForwardHeaders = []string{"Grpc-Timeout"} }, "reserved"},
		{"unknown code", func(c *model.Collector) { c.Request.Retry.Codes = []string{"UNAVAILABLE", "BUSY"} }, `"BUSY", which is not a gRPC status code`},
		{"OK retried", func(c *model.Collector) { c.Request.Retry.Codes = []string{"OK"} }, "lists OK"},
		{"non_idempotent", func(c *model.Collector) { c.Request.Retry.NonIdempotent = true }, "retry.non_idempotent, which does not apply"},
		{"negative attempts", func(c *model.Collector) { c.Request.Retry.Attempts = -1 }, "must be from 0 to 10"},
		{"not JSON", func(c *model.Collector) { c.Request.Message = `{"queue": }` }, "is not JSON"},
		{"unquoted string placeholder", func(c *model.Collector) { c.Request.Message = `{"queue": {{param_q:orders}}}` }, "is not JSON"},
		{"form filter", func(c *model.Collector) { c.Request.Message = `{"queue": "{{param_q|form}}"}` }, "does not write JSON"},
		{"http key", func(c *model.Collector) { c.Request.Path = "/x" }, "request.path, which does not apply"},
		{"both credentials", func(c *model.Collector) {
			c.Request.BearerToken = "t"
			c.Request.BasicAuth = &model.BasicAuth{Username: "u", Password: "p"}
		}, "cannot configure basic and bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := grpcCollector()
			tc.change(&c)
			wantRefused(t, c, tc.fragment)
		})
	}

	// Codes are names, in any case.
	c = grpcCollector()
	c.Request.Retry.Codes = []string{" unavailable", "Aborted"}
	if got := validGRPC(t, c).Request.Retry.Codes; strings.Join(got, ",") != "UNAVAILABLE,ABORTED" {
		t.Fatalf("codes %v", got)
	}
	// Placeholders: a string with |json, a number with |number, each
	// JSON once it takes its default or a stand-in.
	c = grpcCollector()
	c.Request.Message = `{"queue": {{param_q|json}}, "limit": {{param_limit|number}}, "include_shards": {{param_shards:true|raw}}}`
	c.Request.Metadata = map[string]string{"x-tenant": "{{param_tenant:default}}"}
	validGRPC(t, c)
	// retry.codes is grpc's alone.
	h := model.Collector{Name: "h", Request: model.RequestConfig{Type: RequestTypeHTTP, Retry: model.RetryConfig{Codes: []string{"UNAVAILABLE"}}}}
	if err := ValidateRequest(&h); err == nil || !strings.Contains(err.Error(), "applies only to request.type grpc") {
		t.Fatalf("an http collector took retry.codes: %v", err)
	}
}

// A status the collector accepts is an answer: an empty object, the code as
// the answer's status and its message in the headers, not retried; another
// status is still a failure.
func TestGRPCAcceptedCodes(t *testing.T) {
	var calls atomic.Int64
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		calls.Add(1)
		return "", status.Error(codes.NotFound, "no such queue")
	}})
	c := grpcCollector()
	c.Request.AcceptCodes = []string{"not_found"}
	c.Request.Retry.Attempts = 2
	checked := validGRPC(t, c)
	response, err := FetchCollector(context.Background(), server.Addr, checked, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status() != 5 || string(response.Body) != "{}" || response.Headers.Get("grpc-message") != "no such queue" || response.Headers.Get("grpc-status") != "5" {
		t.Fatalf("status %v body %s headers %v", response.Status(), response.Body, response.Headers)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("an accepted status was retried: %d calls", n)
	}
	if code, ok := GRPCStatusCode(checked, nil); !ok || code != 0 {
		t.Fatalf("%d %v", code, ok)
	}
	other := grpcCollector()
	other.Request.AcceptCodes = []string{"UNAVAILABLE"}
	if _, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, other), RequestOverrides{}, nil); err == nil {
		t.Fatal("a status the collector does not accept was an answer")
	}
	// A static target's accept_codes replace the collector's.
	target := &model.StaticTarget{Name: "t", Request: model.TargetRequestConfig{AcceptCodes: []string{"not_found"}}}
	NormalizeTargetRequest(target)
	if err := CheckTargetRequest(target, validGRPC(t, other)); err != nil {
		t.Fatal(err)
	}
	if response, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, other), TargetOverrides(target), nil); err != nil || response.Status() != 5 {
		t.Fatalf("a target's accept_codes: %v", err)
	}
	okTarget := &model.StaticTarget{Name: "ok", Request: model.TargetRequestConfig{AcceptCodes: []string{"OK"}}}
	if err := CheckTargetRequest(okTarget, validGRPC(t, other)); err == nil || !strings.Contains(err.Error(), "accept_codes") {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"OK", "NOPE"} {
		refused := grpcCollector()
		refused.Request.AcceptCodes = []string{bad}
		wantRefused(t, refused, "request.accept_codes")
	}
	http := model.Collector{Name: "h", Request: model.RequestConfig{Type: RequestTypeHTTP, AcceptCodes: []string{"NOT_FOUND"}}}
	if err := ValidateRequest(&http); err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("%v", err)
	}
}
