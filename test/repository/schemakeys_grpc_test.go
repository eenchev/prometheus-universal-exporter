//go:build !select_request_types || request_type_grpc

package repository

import (
	"strings"
	"testing"
)

// healthCollector is a configuration of one grpc collector that calls the
// health service, whose message types are built in, with a comment where a
// row writes a key of its request; queueCollector calls another service,
// and so has to say where its types come from.
const healthCollector = `collectors:
  - name: demo
    request:
      type: grpc
      rpc: grpc.health.v1.Health/Check
      # request
    transform:
      type: jq
    metrics:
      - name: serving
        expression: .status
`

var queueCollector = strings.Replace(healthCollector, "grpc.health.v1.Health/Check", "acme.queue.v1.QueueService/GetStats", 1)

// grpcSchemaKeys are the keys of a grpc collector that a schema holds to
// allowed values or a pattern. rpc is required, and written "" is as missing
// as left out. descriptors is left out of a call of the health service, and
// so written "", and required of any other: the schema took a collector
// without it, which the exporter refuses.
func grpcSchemaKeys() []schemaKey {
	request := "      # request\n"
	rpc := "      rpc: grpc.health.v1.Health/Check\n"
	return []schemaKey{
		{key: "collectors[].request.rpc", document: healthCollector, at: rpc, setting: "      rpc: %s\n", valid: "grpc.health.v1.Health/Check", invalid: "Check"},
		{key: "collectors[].request.rpc", of: "with a slash before it", document: healthCollector, at: rpc, setting: "      rpc: %s\n", valid: "/grpc.health.v1.Health/Check", invalid: "//Check"},
		{key: "collectors[].request.descriptors", document: healthCollector, at: request, setting: "      descriptors: %s\n", valid: "reflection", invalid: "files", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.descriptors", of: "of another service than health", document: queueCollector, at: request, setting: "      descriptors: %s\n", valid: "reflection", invalid: "files", absentWas: taken},
		{key: "collectors[].request.method", of: "of a grpc collector", document: healthCollector, at: request, setting: "      method: %s\n", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.metadata{}", document: healthCollector, at: request, setting: "      metadata: {%s: one}\n", valid: "x-tenant", invalid: "X-Tenant", absent: true},
		{key: "targets[].request.metadata{}", file: inTargetFile, configuration: healthCollector, document: "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: a.example:50051\n    # target\n", at: "    # target\n", setting: "    request:\n      metadata: {%s: one}\n", valid: "x-tenant", invalid: "X-Tenant", absent: true},
	}
}

// The keys of a grpc collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly: rpc is
// required and "" is no method; descriptors: "" is descriptors left out,
// taken of a call of the health service, with a slash before its name or
// without, and refused of any other, which must say where its message types
// come from; and method: "" is the key a grpc collector does not have.
func TestSchemaAndExporterAgreeOnGRPCKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, grpcSchemaKeys())
}
