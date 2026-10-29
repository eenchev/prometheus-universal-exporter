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

// tlsAt is where a row writes a key of a grpc target's own tls block.
const tlsAt = "        # tls\n"

// targetTLS is a target file of one grpc target whose own tls block holds
// keys, and tlsAt.
func targetTLS(keys string) string {
	return "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: a.example:50051\n    request:\n      tls:\n" + keys + tlsAt
}

// oneGRPCTarget is a target file of one target of the collector demo, with
// grpcTargetAt where a row writes the target's keys.
const (
	oneGRPCTarget = "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: a.example:50051\n" + grpcTargetAt
	grpcTargetAt  = "    # target\n"
)

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
		// A target's own tls block: a client certificate is both files
		// or neither, "" a file left out, and no value holds a
		// placeholder, being used as written.
		{key: "targets[].request.tls.ca_file", file: inTargetFile, configuration: healthCollector, document: targetTLS("        server_name: a.example\n"), at: tlsAt, setting: "        ca_file: %s\n", valid: "/ca.pem", invalid: `"/{{param_ca}}"`, absent: true, empty: true},
		{key: "targets[].request.tls.server_name", file: inTargetFile, configuration: healthCollector, document: targetTLS("        ca_file: /ca.pem\n"), at: tlsAt, setting: "        server_name: %s\n", valid: "a.example", invalid: `"{{param_host}}"`, absent: true, empty: true},
		{key: "targets[].request.tls.cert_file", file: inTargetFile, configuration: healthCollector, document: targetTLS("        ca_file: /ca.pem\n"), at: tlsAt, setting: "        key_file: /k.pem\n        cert_file: %s\n", valid: "/c.pem", invalid: `"/{{param_c}}"`, absent: true},
		{key: "targets[].request.tls.key_file", file: inTargetFile, configuration: healthCollector, document: targetTLS("        ca_file: /ca.pem\n"), at: tlsAt, setting: "        cert_file: /c.pem\n        key_file: %s\n", valid: "/k.pem", invalid: `"/{{param_k}}"`, absent: true},
		// The status codes a collector or a target retries or accepts are
		// names, in any case and with blanks around them or not, as the
		// exporter matches them: the schema took any text, an unknown
		// name and OK among it.
		{key: "collectors[].request.retry.codes[]", document: healthCollector, at: request, setting: "      retry:\n        codes: [%s]\n", valid: "UNAVAILABLE", invalid: "UNAVAILABLY", absent: true},
		{key: "collectors[].request.retry.codes[]", of: "in lower case with a blank", document: healthCollector, at: request, setting: "      retry:\n        codes: [%s]\n", valid: "' aborted'", invalid: "'aborted x'", absent: true},
		{key: "collectors[].request.retry.codes[]", of: "OK", document: healthCollector, at: request, setting: "      retry:\n        codes: [%s]\n", valid: "Data_Loss", invalid: "ok", absent: true},
		{key: "defaults.request.retry.codes[]", document: "# defaults\n" + healthCollector, at: "# defaults\n", setting: "defaults:\n  request:\n    retry:\n      codes: [%s]\n", valid: "unavailable", invalid: "NOPE", absent: true},
		{key: "collectors[].request.accept_codes[]", document: healthCollector, at: request, setting: "      accept_codes: [%s]\n", valid: "NOT_FOUND", invalid: "NOT-FOUND", absent: true},
		{key: "collectors[].request.accept_codes[]", of: "in lower case with a blank", document: healthCollector, at: request, setting: "      accept_codes: [%s]\n", valid: "'not_found '", invalid: "'not found'", absent: true},
		{key: "collectors[].request.accept_codes[]", of: "OK", document: healthCollector, at: request, setting: "      accept_codes: [%s]\n", valid: "INTERNAL", invalid: "OK", absent: true},
		{key: "targets[].request.retry.codes[]", file: inTargetFile, configuration: healthCollector, document: oneGRPCTarget, at: grpcTargetAt, setting: "    request:\n      retry:\n        codes: [%s]\n", valid: "' Aborted '", invalid: "ABORT", absent: true},
		{key: "targets[].request.retry.codes[]", of: "OK", file: inTargetFile, configuration: healthCollector, document: oneGRPCTarget, at: grpcTargetAt, setting: "    request:\n      retry:\n        codes: [%s]\n", valid: "ABORTED", invalid: "Ok", absent: true},
		{key: "targets[].request.accept_codes[]", file: inTargetFile, configuration: healthCollector, document: oneGRPCTarget, at: grpcTargetAt, setting: "    request:\n      accept_codes: [%s]\n", valid: "not_found", invalid: "NOTFOUND", absent: true},
		{key: "targets[].request.accept_codes[]", of: "OK", file: inTargetFile, configuration: healthCollector, document: oneGRPCTarget, at: grpcTargetAt, setting: "    request:\n      accept_codes: [%s]\n", valid: "NOT_FOUND", invalid: "' OK'", absent: true},
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
