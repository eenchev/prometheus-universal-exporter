//go:build select_request_types && request_type_http && !request_type_s3file

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A build without the s3file request type links no AWS SDK, and refuses
// request.aws_sigv4 when the configuration loads, saying which type brings
// it.
func TestABuildWithoutS3FileRefusesAWSSigning(t *testing.T) {
	c := testutil.Collector("queue", "text")
	httpOwn(&c.Request, func(own *HTTPConfig) { own.AWSSigV4 = &SigV4Config{Service: "sqs"} })
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "this build does not sign with AWS Signature Version 4, which comes with the s3file request type") {
		t.Errorf("err=%v", err)
	}
}
