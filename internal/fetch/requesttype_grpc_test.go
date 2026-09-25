//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const getStats = grpctest.Service + "/GetStats"

// grpcCollector calls GetStats with descriptors from the server's
// reflection service.
func grpcCollector() model.Collector {
	return model.Collector{
		Name:      "queue",
		Request:   model.RequestConfig{Type: RequestTypeGRPC, RPC: getStats, Descriptors: "reflection"},
		Transform: model.TransformConfig{Type: "jq"},
	}
}

func validGRPC(t *testing.T, c model.Collector) *model.Collector {
	t.Helper()
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func wantRefused(t *testing.T, c model.Collector, fragment string) {
	t.Helper()
	err := ValidateRequest(&c)
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("got %v, want an error containing %q", err, fragment)
	}
}

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
		{"negative attempts", func(c *model.Collector) { c.Request.Retry.Attempts = -1 }, "must not be negative"},
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

// The health service's types are built in: it needs no descriptors, and
// its streaming Watch is refused at load.
func TestGRPCHealthNeedsNoDescriptors(t *testing.T) {
	c := model.Collector{Name: "health", Request: model.RequestConfig{Type: RequestTypeGRPC, RPC: "grpc.health.v1.Health/Check"}, Transform: model.TransformConfig{Type: "jq"}}
	checked := validGRPC(t, c)
	c.Request.RPC = "grpc.health.v1.Health/Watch"
	wantRefused(t, c, "server streaming method")
	c.Request.RPC = "grpc.health.v1.Health/Check"
	c.Request.Message = `{"servce": "x"}`
	wantRefused(t, c, `unknown field "servce"`)

	server := grpctest.Start(t, grpctest.Options{})
	resp, err := FetchCollector(context.Background(), server.Addr, checked, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"status":"SERVING"}` {
		t.Fatalf("body %s", resp.Body)
	}
}

// protoset reads a descriptor set, and the method is checked at load: it
// must exist, be unary, and take the message.
func TestGRPCProtoset(t *testing.T) {
	dir := t.TempDir()
	set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), false)
	c := grpcCollector()
	c.Request.Descriptors, c.Request.ProtosetFile = "protoset", set
	c.Request.Message = `{"queue": "orders", "include_shards": true}`
	validGRPC(t, c)

	for _, tc := range []struct{ rpc, message, fragment string }{
		{"acme.queue.v1.Missing/Get", "", "the service acme.queue.v1.Missing is not in protoset_file"},
		{grpctest.Service + "/Get", "", "has no method Get; it has GetStats, Watch, Push, Sync"},
		{grpctest.Service + "/Watch", "", "is a server streaming method"},
		{grpctest.Service + "/Push", "", "is a client streaming method"},
		{grpctest.Service + "/Sync", "", "is a bidirectional streaming method"},
		{getStats, `{"queues": "x"}`, `does not fit acme.queue.v1.GetStatsRequest: (line 1:2): unknown field "queues"`},
		{getStats, `{"limit": "many"}`, "does not fit"},
	} {
		c := c
		c.Request.RPC, c.Request.Message = tc.rpc, tc.message
		wantRefused(t, c, tc.fragment)
	}
	// A placeholder with no default stands in with a value no probe need
	// send, so the message is checked only for being JSON.
	c.Request.Message = `{"limit": {{param_limit|json}}}`
	validGRPC(t, c)

	// A set without an import it needs names it.
	full := grpctest.WriteProtoset(t, filepath.Join(dir, "full.pb"), true)
	c.Request.Message, c.Request.ProtosetFile = "", full
	validGRPC(t, c)
	missing := filepath.Join(dir, "missing.pb")
	if err := os.WriteFile(missing, []byte("not a descriptor set"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Request.ProtosetFile = missing
	wantRefused(t, c, "is not a FileDescriptorSet")
	c.Request.ProtosetFile = filepath.Join(dir, "absent.pb")
	wantRefused(t, c, "reading protoset_file")
}

// A descriptor set is read again when it changes on disk.
func TestGRPCProtosetIsReadAgainWhenItChanges(t *testing.T) {
	dir := t.TempDir()
	set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), false)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(set, old, old); err != nil {
		t.Fatal(err)
	}
	c := grpcCollector()
	c.Request.Descriptors, c.Request.ProtosetFile = "protoset", set
	checked := validGRPC(t, c)
	if _, err := staticMethod(checked); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(set, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := staticMethod(checked); err == nil || !strings.Contains(err.Error(), "is not in protoset_file") {
		t.Fatalf("an emptied protoset still answered: %v", err)
	}
}

// proto compiles .proto sources, resolving imports in proto_import_paths,
// the well-known types built in; a compile error names the file and line.
func TestGRPCProtoSources(t *testing.T) {
	dir := t.TempDir()
	queue := grpctest.WriteSources(t, dir)
	c := grpcCollector()
	c.Request.Descriptors, c.Request.ProtoFiles, c.Request.ProtoImportPaths = "proto", []string{queue}, []string{dir}
	c.Request.Message = `{"queue": "orders"}`
	checked := validGRPC(t, c)
	method, err := staticMethod(checked)
	if err != nil || method.path() != "/"+getStats {
		t.Fatalf("%v %v", method.path(), err)
	}

	// Without import paths, each file's directory is one, and queue.proto's
	// import by its path from the root is not found there.
	c.Request.ProtoImportPaths = nil
	wantRefused(t, c, "acme/queue/v1/shard.proto")
	// A file outside every import path.
	c.Request.ProtoImportPaths = []string{filepath.Join(dir, "elsewhere")}
	wantRefused(t, c, "is not under any of proto_import_paths")

	broken := filepath.Join(t.TempDir(), "broken.proto")
	if err := os.WriteFile(broken, []byte("syntax = \"proto3\";\npackage x;\nmessage M {\n  strin a = 1;\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Request.RPC, c.Request.ProtoFiles, c.Request.ProtoImportPaths = "x.S/M", []string{broken}, nil
	wantRefused(t, c, "broken.proto:4:")
}

// Targets: host:port, dns:///, grpc:// and grpcs://, with a port, and
// nothing else.
func TestGRPCTargets(t *testing.T) {
	c := validGRPC(t, grpcCollector())
	for target, want := range map[string]string{
		"queue.internal:9090":         "grpc://queue.internal:9090/" + getStats,
		"dns:///queue.internal:9090":  "grpc://queue.internal:9090/" + getStats,
		"grpc://10.0.0.5:9090":        "grpc://10.0.0.5:9090/" + getStats,
		"grpcs://queue.internal:9090": "grpcs://queue.internal:9090/" + getStats,
		"grpcs://[::1]:9090/":         "grpcs://[::1]:9090/" + getStats,
	} {
		if err := CheckTarget(c, target, true); err != nil {
			t.Errorf("%s: %v", target, err)
		}
		if label, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || label != want {
			t.Errorf("%s: label %q, %v; want %q", target, label, err, want)
		}
		if DisplayTarget(c, target) != target {
			t.Errorf("%s: displayed as %q", target, DisplayTarget(c, target))
		}
	}
	if RequestMethodFor(c, RequestOverrides{}) != http.MethodPost || FetchStage(c) != "grpc" {
		t.Fatal("method or stage")
	}
	for target, fragment := range map[string]string{
		"queue.internal":                "is not host:port",
		"http://queue.internal:9090":    "has the scheme http://",
		"queue.internal:9090/x":         "has a path",
		"grpc://user@queue.internal:90": "has a path, query or user",
		"queue.internal:0":              "want a number from 1 to 65535",
	} {
		if err := CheckTarget(c, target, false); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%s: %v, want %q", target, err, fragment)
		}
		if DisplayTarget(c, target) != "<invalid target>" {
			t.Errorf("%s displayed as %q", target, DisplayTarget(c, target))
		}
	}
	// A tls block makes a bare target TLS, and contradicts grpc://.
	tlsCollector := grpcCollector()
	tlsCollector.Request.TLS = model.TLSConfig{ServerName: "queue.test"}
	withTLS := validGRPC(t, tlsCollector)
	if label, _ := RequestLabelFor("queue.internal:9090", withTLS, RequestOverrides{}); !strings.HasPrefix(label, "grpcs://") {
		t.Fatalf("label %q", label)
	}
	if err := CheckTarget(withTLS, "grpc://queue.internal:9090", true); err == nil || !strings.Contains(err.Error(), "asks for plaintext") {
		t.Fatalf("grpc:// with tls: %v", err)
	}
}

// The probe parameters a grpc collector takes, and those it refuses.
func TestGRPCProbeParameters(t *testing.T) {
	c := validGRPC(t, grpcCollector())
	if err := CheckOverrideParams(c, url.Values{"message": {"{}"}, "timeout": {"1s"}, "retry_attempts": {"1"}, "param_q": {"x"}, "header_x": {"y"}}); err != nil {
		t.Fatal(err)
	}
	err := CheckOverrideParams(c, url.Values{"method": {"GET"}, "path": {"/x"}, "body": {"b"}, "from": {"-1h"}})
	if err == nil || !strings.Contains(err.Error(), "body, from, method, path do not apply") {
		t.Fatalf("%v", err)
	}
	overrides, err := ParseRequestOverrides(url.Values{"message": {`{"queue": "a"}`}})
	if err != nil || overrides.Message == nil || *overrides.Message != `{"queue": "a"}` {
		t.Fatalf("%+v %v", overrides, err)
	}
}

// A static target may set its own message, metadata and retry codes.
func TestGRPCStaticTargetRequest(t *testing.T) {
	dir := t.TempDir()
	c := grpcCollector()
	c.Request.Descriptors, c.Request.ProtosetFile = "protoset", grpctest.WriteProtoset(t, filepath.Join(dir, "q.pb"), false)
	checked := validGRPC(t, c)
	attempts := 2
	good := &model.StaticTarget{Name: "t", Target: "q:1", Request: model.TargetRequestConfig{
		Message: `{"queue": "b"}`, Metadata: map[string]string{"x-tenant": "b"},
		Retry: &model.TargetRetryConfig{Attempts: &attempts, Codes: []string{"aborted"}},
	}}
	if err := CheckTargetRequest(good, checked); err != nil {
		t.Fatal(err)
	}
	if good.Request.Retry.Codes[0] != "ABORTED" {
		t.Fatalf("codes %v", good.Request.Retry.Codes)
	}
	overrides := TargetOverrides(good)
	if *overrides.Message != `{"queue": "b"}` || overrides.Metadata["x-tenant"] != "b" || overrides.RetryCodes[0] != "ABORTED" {
		t.Fatalf("%+v", overrides)
	}
	for fragment, request := range map[string]model.TargetRequestConfig{
		"request.message is not JSON":             {Message: "{"},
		`unknown field "queues"`:                  {Message: `{"queues": "b"}`},
		"request.metadata X-A":                    {Metadata: map[string]string{"X-A": "b"}},
		"is not a gRPC status code":               {Retry: &model.TargetRetryConfig{Codes: []string{"NOPE"}}},
		"retry.non_idempotent, which does not":    {Retry: &model.TargetRetryConfig{NonIdempotent: new(bool)}},
		"request.method, which does not apply to": {Method: "GET"},
	} {
		target := &model.StaticTarget{Name: "t", Target: "q:1", Request: request}
		if err := CheckTargetRequest(target, checked); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%+v: %v, want %q", request, err, fragment)
		}
	}
}

// statsAnswer answers GetStats with a queue whose second shard is empty.
func statsAnswer(_ context.Context, _, request string) (string, error) {
	var in struct {
		Queue string `json:"queue"`
	}
	_ = json.Unmarshal([]byte(request), &in)
	return `{"shards": [{"id": "` + in.Queue + `-1", "depth": 12}, {"id": "` + in.Queue + `-2"}], "total": "12345678901234", "state": "STATE_OK", "updated": "2026-09-24T10:00:00Z"}`, nil
}

// A call end to end: the message with its placeholders, the metadata and
// credentials, the deadline, and the answer as JSON with zero values,
// the .proto's field names, 64-bit integers as strings and enums as names.
func TestGRPCCall(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := grpcCollector()
	c.Request.Message = `{"queue": {{param_queue|json}}, "include_shards": true}`
	c.Request.Metadata = map[string]string{"x-tenant": "{{param_tenant:default}}", "x-static": "one"}
	c.Request.BearerTokenFile = token
	checked := validGRPC(t, c)
	overrides := RequestOverrides{Params: map[string]string{"param_queue": "orders"}, Timeout: 5 * time.Second}
	forwarded := http.Header{"X-Request-Id": {"r1"}, "Grpc-Timeout": {"1S"}, "Host": {"elsewhere"}}
	resp, err := FetchCollector(context.Background(), server.Addr, checked, overrides, forwarded)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"shards":[{"id":"orders-1","depth":"12"},{"id":"orders-2","depth":"0"}],"total":"12345678901234","state":"STATE_OK","updated":"2026-09-24T10:00:00Z","empty_count":0}`
	if string(resp.Body) != want {
		t.Fatalf("body\n%s\nwant\n%s", resp.Body, want)
	}
	if resp.StatusCode != http.StatusOK || resp.Headers.Get("Content-Type") != "application/json" || resp.Target != server.Addr {
		t.Fatalf("%d %v", resp.StatusCode, resp.Headers)
	}
	calls := server.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	call := calls[0]
	if call.Request != `{"queue":"orders","include_shards":true}` {
		t.Fatalf("request %s", call.Request)
	}
	md := call.Metadata
	if md.Get("x-tenant")[0] != "default" || md.Get("x-static")[0] != "one" || md.Get("authorization")[0] != "Bearer s3cret" || md.Get("x-request-id")[0] != "r1" {
		t.Fatalf("metadata %v", md)
	}
	if call.Deadline <= 0 || call.Deadline > 5*time.Second {
		t.Fatalf("deadline %s did not reach the server", call.Deadline)
	}

	// The probe's message replaces the collector's, as it is.
	message := `{"queue": "direct"}`
	if _, err := FetchCollector(context.Background(), server.Addr, checked, RequestOverrides{Message: &message}, nil); err != nil {
		t.Fatal(err)
	}
	if got := server.Calls()[1].Request; got != `{"queue":"direct"}` {
		t.Fatalf("request %s", got)
	}
}

// A message that does not fit the method fails at the message stage,
// before the target is called, and made no call.
func TestGRPCMessageThatDoesNotFit(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1"})
	c := validGRPC(t, grpcCollector())
	message := `{"queue": 5}`
	_, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{Message: &message}, nil)
	var stage *StageError
	if !errors.As(err, &stage) || FetchErrorStage(c, err) != "message" || !strings.Contains(err.Error(), "does not fit acme.queue.v1.GetStatsRequest") {
		t.Fatalf("%v", err)
	}
	if code, ok := GRPCStatusCode(c, err); code != -1 || !ok {
		t.Fatalf("code %d", code)
	}
	if len(server.Calls()) != 0 {
		t.Fatal("the target was called")
	}
}

// A status other than OK fails the fetch with its code and message.
func TestGRPCStatusErrors(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		return "", status.Error(codes.PermissionDenied, "tenant may not read queues")
	}})
	c := validGRPC(t, grpcCollector())
	_, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil)
	var callErr *CallStatusError
	if !errors.As(err, &callErr) || callErr.CodeName != "PERMISSION_DENIED" || err.Error() != "gRPC call failed: PERMISSION_DENIED: tenant may not read queues" {
		t.Fatalf("%v", err)
	}
	if code, ok := GRPCStatusCode(c, err); code != 7 || !ok || FetchErrorStage(c, err) != "grpc" {
		t.Fatalf("code %d stage %s", code, FetchErrorStage(c, err))
	}
	if code, ok := GRPCStatusCode(c, nil); code != 0 || !ok {
		t.Fatal("OK is 0")
	}

	// A server that is not there is UNAVAILABLE.
	server.Stop()
	_, err = FetchCollector(context.Background(), server.Addr, c, RequestOverrides{Timeout: 2 * time.Second}, nil)
	if code, _ := GRPCStatusCode(c, err); code != int(codes.Unavailable) {
		t.Fatalf("%v", err)
	}
}

// UNAVAILABLE is retried by default, other codes only when retry.codes
// lists them, and a static target's codes replace the collector's.
func TestGRPCRetries(t *testing.T) {
	var failures atomic.Int32
	var code atomic.Int32
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(ctx context.Context, m, r string) (string, error) {
		if failures.Add(-1) >= 0 {
			return "", status.Error(codes.Code(code.Load()), "not now") //nolint:gosec // G115: a small test code
		}
		return statsAnswer(ctx, m, r)
	}})
	c := grpcCollector()
	c.Request.Retry.Attempts = 2
	checked := validGRPC(t, c)
	calls := func() int { return len(server.Calls()) }
	try := func(n int32, with codes.Code, overrides RequestOverrides) error {
		failures.Store(n)
		code.Store(int32(with)) //nolint:gosec // G115: a small test code
		_, err := FetchCollector(context.Background(), server.Addr, checked, overrides, nil)
		return err
	}
	if err := try(2, codes.Unavailable, RequestOverrides{}); err != nil || calls() != 3 {
		t.Fatalf("UNAVAILABLE twice: %v after %d calls", err, calls())
	}
	before := calls()
	if err := try(1, codes.Aborted, RequestOverrides{}); err == nil || calls()-before != 1 {
		t.Fatalf("ABORTED was retried: %v after %d calls", err, calls()-before)
	}
	before = calls()
	if err := try(1, codes.Aborted, RequestOverrides{RetryCodes: []string{"ABORTED"}}); err != nil || calls()-before != 2 {
		t.Fatalf("ABORTED listed: %v after %d calls", err, calls()-before)
	}
	before = calls()
	attempts := 0
	if err := try(1, codes.Unavailable, RequestOverrides{RetryAttempts: &attempts}); err == nil || calls()-before != 1 {
		t.Fatalf("no retries: %v after %d calls", err, calls()-before)
	}
}

// An answer over max_response_bytes is refused as a limit.
func TestGRPCResponseLimit(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		return `{"shards": [{"id": "` + strings.Repeat("x", 2048) + `"}]}`, nil
	}})
	c := grpcCollector()
	c.Request.MaxResponseBytes = 1024
	_, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, c), RequestOverrides{}, nil)
	var callErr *CallStatusError
	if !errors.As(err, &callErr) || callErr.CodeName != "RESOURCE_EXHAUSTED" || !errors.Is(err, model.ErrLimitExceeded) {
		t.Fatalf("%v", err)
	}
}

// Reflection v1, the v1alpha fallback, and a server with neither; answers
// are kept, and asked again after the method was not found.
func TestGRPCReflection(t *testing.T) {
	c := validGRPC(t, grpcCollector())
	for _, version := range []string{"v1", "v1alpha", "both"} {
		server := grpctest.Start(t, grpctest.Options{Reflection: version, Answer: statsAnswer})
		if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil); err != nil {
			t.Fatalf("%s: %v", version, err)
		}
	}
	bare := grpctest.Start(t, grpctest.Options{})
	_, err := FetchCollector(context.Background(), bare.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "has no reflection service") || !strings.Contains(err.Error(), "use descriptors: protoset or proto") {
		t.Fatalf("%v", err)
	}
	if code, _ := GRPCStatusCode(c, err); code != int(codes.Unimplemented) {
		t.Fatalf("code %d", code)
	}

	// The answer is kept: a second call does not ask again.
	var unimplemented atomic.Bool
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(ctx context.Context, m, r string) (string, error) {
		if unimplemented.CompareAndSwap(true, false) {
			return "", status.Error(codes.Unimplemented, "unknown method")
		}
		return statsAnswer(ctx, m, r)
	}})
	key := reflectedKey{conn: grpcConnKey{dial: server.Addr, policy: policyOf(c)}, service: grpctest.Service}
	if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	first := reflectionAnswers.fetchedAt(key)
	if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if first.IsZero() || !reflectionAnswers.fetchedAt(key).Equal(first) {
		t.Fatal("the reflection answer was not kept")
	}
	// UNIMPLEMENTED asks again and calls again, once, beside the retries.
	unimplemented.Store(true)
	before := len(server.Calls())
	if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(server.Calls())-before != 2 || reflectionAnswers.fetchedAt(key).Equal(first) {
		t.Fatalf("%d calls; asked again: %v", len(server.Calls())-before, !reflectionAnswers.fetchedAt(key).Equal(first))
	}
	// An answer older than the TTL is asked for again.
	reflectionAnswers.mu.Lock()
	reflectionAnswers.sweepLocked(time.Now().Add(reflectionTTL + time.Minute))
	reflectionAnswers.mu.Unlock()
	if !reflectionAnswers.fetchedAt(key).IsZero() {
		t.Fatal("an expired answer was kept")
	}

	// A service the reflection service does not know.
	missing := grpcCollector()
	missing.Request.RPC = "acme.queue.v1.Missing/Get"
	_, err = FetchCollector(context.Background(), server.Addr, validGRPC(t, missing), RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not know the service acme.queue.v1.Missing") {
		t.Fatalf("%v", err)
	}
}

// TLS: grpcs:// with the CA, server_name for a certificate of another name,
// and insecure_skip_verify, which a probe may override.
func TestGRPCTLS(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{TLS: true, Reflection: "v1", Answer: statsAnswer})
	c := grpcCollector()
	c.Request.TLS = model.TLSConfig{CAFile: server.CAFile}
	if _, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, c), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	c.Request.TLS.ServerName = "queue.test"
	if _, err := FetchCollector(context.Background(), "grpcs://"+server.Addr, validGRPC(t, c), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	c.Request.TLS.ServerName = "wrong.test"
	wrong := validGRPC(t, c)
	_, err := FetchCollector(context.Background(), server.Addr, wrong, RequestOverrides{Timeout: 2 * time.Second}, nil)
	if code, _ := GRPCStatusCode(wrong, err); code != int(codes.Unavailable) {
		t.Fatalf("a certificate for another name was accepted: %v", err)
	}
	skip := true
	if _, err := FetchCollector(context.Background(), server.Addr, wrong, RequestOverrides{InsecureSkipVerify: &skip}, nil); err != nil {
		t.Fatal(err)
	}
	// Plaintext to a TLS server fails.
	_, err = FetchCollector(context.Background(), server.Addr, validGRPC(t, grpcCollector()), RequestOverrides{Timeout: 2 * time.Second}, nil)
	if err == nil {
		t.Fatal("plaintext reached a TLS server")
	}
}

// Connections are kept per target and TLS settings, and forgotten unused.
func TestGRPCConnectionsAreReused(t *testing.T) {
	cache := &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}}
	start := time.Now()
	a := grpcConnKey{dial: "127.0.0.1:1"}
	first, _, err := cache.get(a, start)
	if err != nil {
		t.Fatal(err)
	}
	again, _, _ := cache.get(a, start.Add(time.Minute))
	if first != again {
		t.Fatal("a second call made a new connection")
	}
	if _, _, err := cache.get(grpcConnKey{dial: "127.0.0.1:2"}, start.Add(time.Minute+transportIdleTTL/2)); err != nil {
		t.Fatal(err)
	}
	if cache.size() != 2 {
		t.Fatalf("%d connections", cache.size())
	}
	if _, _, err := cache.get(grpcConnKey{dial: "127.0.0.1:2"}, start.Add(time.Minute+transportIdleTTL+time.Second)); err != nil {
		t.Fatal(err)
	}
	if cache.size() != 1 {
		t.Fatalf("%d connections after the first went unused", cache.size())
	}
	// A missing CA is an error, not a connection.
	if _, _, err := cache.get(grpcConnKey{dial: "127.0.0.1:3", tls: true, settings: model.TLSConfig{CAFile: "/nonexistent"}}, start); err == nil {
		t.Fatal("a missing CA file made a connection")
	}
}

// fetchedAt is when the answer under key was fetched, zero when there is
// none.
func (r *reflected) fetchedAt(key reflectedKey) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.entries[key]; entry != nil {
		return entry.fetched
	}
	return time.Time{}
}

// The size limit bounds the JSON an answer becomes as well as the message:
// zero values written can make it much larger. The call itself was OK.
func TestGRPCTheJSONAnswerIsLimitedToo(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		return `{"shards": [` + strings.TrimSuffix(strings.Repeat(`{},`, 200), ",") + `]}`, nil
	}})
	c := grpcCollector()
	c.Request.MaxResponseBytes = 1024
	checked := validGRPC(t, c)
	_, err := FetchCollector(context.Background(), server.Addr, checked, RequestOverrides{}, nil)
	if err == nil || !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "bytes as JSON, over the response limit of 1024") {
		t.Fatalf("%v", err)
	}
	if code, _ := GRPCStatusCode(checked, err); code != 0 {
		t.Fatalf("code %d, want 0: the call was answered OK", code)
	}
}

// Probes of a new target that miss the reflection answer together share one
// question to the server.
func TestGRPCConcurrentMissesAskReflectionOnce(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	c := validGRPC(t, grpcCollector())
	const probes = 8
	errs := make(chan error, probes)
	for i := 0; i < probes; i++ {
		go func() {
			_, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil)
			errs <- err
		}()
	}
	for i := 0; i < probes; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := server.ReflectionStreams.Load(); n != 1 {
		t.Fatalf("%d reflection streams for %d concurrent probes, want 1", n, probes)
	}
}

// A probe whose deadline ends while the shared reflection question is in
// flight fails alone: the question is not its own, and the probes still
// waiting get the answer.
func TestGRPCAShortProbeDoesNotFailTheSharedReflection(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer, ReflectionDelay: 300 * time.Millisecond})
	c := validGRPC(t, grpcCollector())
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	shortDone := make(chan error, 1)
	go func() {
		_, err := FetchCollector(short, server.Addr, c, RequestOverrides{}, nil)
		shortDone <- err
	}()
	// The short probe starts the question; the patient one joins it.
	for deadline := time.Now().Add(5 * time.Second); server.ReflectionStreams.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatalf("the patient probe failed with the short one: %v", err)
	}
	if err := <-shortDone; err == nil {
		t.Fatal("the short probe outlived its deadline")
	}
	if n := server.ReflectionStreams.Load(); n != 1 {
		t.Fatalf("%d reflection streams, want 1", n)
	}
}

// A slow read of one descriptor set keeps no other set waiting.
func TestGRPCDescriptorSetsAreReadApart(t *testing.T) {
	sets := &fileSets{slots: map[string]*fileSetSlot{}}
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _ = sets.get("slow", func() (*protoregistry.Files, []string, error) {
			close(started)
			<-release
			return new(protoregistry.Files), nil, nil
		})
	}()
	<-started
	done := make(chan struct{})
	go func() {
		_, _ = sets.get("fast", func() (*protoregistry.Files, []string, error) { return new(protoregistry.Files), nil, nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second set waited for the first set's read")
	}
	close(release)
}

// A grpc collector's allowed_targets and denied_targets refuse a server
// before any call, and allow one on the list.
func TestGRPCTargetPolicy(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	denied := grpcCollector()
	denied.Request.DeniedTargets = []string{"127.0.0.0/8"}
	c := validGRPC(t, denied)
	if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("err=%v", err)
	}
	if n := server.ReflectionStreams.Load(); n != 0 || len(server.Calls()) != 0 {
		t.Fatalf("a refused server was called: %d streams, %d calls", n, len(server.Calls()))
	}
	allowed := grpcCollector()
	allowed.Request.AllowedTargets = []string{"127.0.0.1"}
	if _, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, allowed), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	// The connection itself is checked: a policy whose name rule let the
	// call through still refuses the address the connection was made to.
	dial := grpcPolicyDialer(mustPolicy(t, []string{"10.0.0.0/8"}, nil), server.Addr, &atomic.Pointer[TargetRefusedError]{})
	if _, err := dial(context.Background(), server.Addr); !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("err=%v", err)
	}
}

func mustPolicy(t *testing.T, allowed, denied []string) *targetPolicy {
	t.Helper()
	p, err := compileTargetPolicy(allowed, denied)
	if err != nil {
		t.Fatal(err)
	}
	return p
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

// An OK call's status is 0.
func TestGRPCStatusOfAnAnswerIsZero(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	response, err := FetchCollector(context.Background(), server.Addr, validGRPC(t, grpcCollector()), RequestOverrides{}, nil)
	if err != nil || response.Status() != 0 {
		t.Fatalf("%v %v", err, response.Status())
	}
}

// A connection the policy refuses after the check before the call passed —
// a name that resolved elsewhere by the time grpc-go dialed it — is reported
// as the refusal, not as UNAVAILABLE, and not retried.
func TestGRPCARefusedConnectionIsARefusal(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	_, port, _ := net.SplitHostPort(server.Addr)
	restore := resolveHost
	t.Cleanup(func() { resolveHost = restore })
	resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "localhost" {
			return []netip.Addr{netip.MustParseAddr("203.0.113.1")}, nil
		}
		return restore(ctx, host)
	}
	c := grpcCollector()
	c.Request.DeniedTargets = []string{"127.0.0.0/8", "::1"}
	c.Request.Retry.Attempts = 2
	checked := validGRPC(t, c)
	_, err := FetchCollector(context.Background(), "localhost:"+port, checked, RequestOverrides{}, nil)
	if !errors.Is(err, ErrTargetRefused) {
		t.Fatalf("err=%v", err)
	}
	if n := server.ReflectionStreams.Load(); n != 0 {
		t.Fatalf("the refused server was called: %d streams", n)
	}
}
