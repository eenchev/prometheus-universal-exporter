//go:build select_request_types && request_type_grpc && !request_type_http

package exporter

// grpcPlainCollector is empty in a build without http: the grpc fixture
// then carries its grpc collector alone.
const grpcPlainCollector = ""
