//go:build select_request_types && !request_type_http

package main

// A build with -tags select_request_types keeps only the request types named by
// request_type_<name> tags. With none of them, every collector would fail
// validation, so the build fails instead, on the undefined name below.
//
// When a request type is added, its tag joins the constraint above; a test
// keeps the constraint and knownRequestTypes in step.
var _ = select_request_types_needs_at_least_one_request_type_tag
