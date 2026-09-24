// Package grpctest runs an in-process gRPC server for the tests of the grpc
// request type: a queue service defined by .proto sources compiled at run
// time, so the tests need neither protoc nor generated code, with the
// health service and, as a test asks, reflection v1, v1alpha or none, over
// plaintext or TLS. It is imported only by tests, and its code is behind the
// grpc request type's build constraint, so a build without the type has
// nothing of it.
package grpctest
