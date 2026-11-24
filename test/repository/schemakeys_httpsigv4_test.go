//go:build !select_request_types || (request_type_http && request_type_s3file)

package repository

import (
	"strings"
	"testing"
)

// An http collector signs with AWS Signature Version 4 only in a build that
// also has the s3file request type, which brings the signer. In such a build
// the keys of request.aws_sigv4 get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnAWSSigV4KeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, httpSigV4SchemaKeys())
}

// awsSignedCollector is a configuration of one collector that signs its
// requests for AWS, with a comment where a row writes a key of the block.
var awsSignedCollector = strings.Replace(jqCollector, "      # request\n", "      aws_sigv4:\n        service: sqs\n        # aws_sigv4\n", 1)

// httpSigV4SchemaKeys are the keys of an http collector's request.aws_sigv4:
// a service is required and a name, a region a name or left out.
func httpSigV4SchemaKeys() []schemaKey {
	return []schemaKey{
		{key: "collectors[].request.aws_sigv4.service", document: awsSignedCollector, at: "        service: sqs\n", setting: "        service: %s\n", valid: "monitoring", invalid: "SQS"},
		{key: "collectors[].request.aws_sigv4.region", document: awsSignedCollector, at: "        # aws_sigv4\n", setting: "        region: %s\n", valid: "eu-west-1", invalid: `"EU West"`, absent: true, empty: true},
	}
}
