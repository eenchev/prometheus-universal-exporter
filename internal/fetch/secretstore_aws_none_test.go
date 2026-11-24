//go:build select_request_types && !request_type_s3file

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A build without the s3file request type has no aws secret store, and
// says so when a configuration names one.
func TestABuildWithoutS3FileRefusesTheAWSSecretStore(t *testing.T) {
	err := ValidateSecretStores([]model.SecretStore{{Name: "aws", Type: "aws", Region: "eu-west-1"}})
	if err == nil || !strings.Contains(err.Error(), "this build has no aws secret store, which comes with the s3file request type") {
		t.Fatalf("err=%v", err)
	}
}
