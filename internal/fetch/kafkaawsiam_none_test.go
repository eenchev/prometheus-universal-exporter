//go:build request_type_kafka && select_request_types && !request_type_s3file

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A build without the s3file request type links no AWS SDK, and refuses an
// AWS_MSK_IAM login when the configuration loads, saying which type brings
// it.
func TestABuildWithoutS3FileRefusesAnMSKLogin(t *testing.T) {
	c := &model.Collector{Name: "kafka", Request: model.RequestConfig{Type: RequestTypeKafka, Own: KafkaConfig{SASLMechanism: kafkaAWSMSKIAM}}}
	if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), "this build has no AWS_MSK_IAM login, which signs with the AWS SDK that comes with the s3file request type") {
		t.Errorf("err=%v", err)
	}
}
