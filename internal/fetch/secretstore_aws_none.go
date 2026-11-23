//go:build select_request_types && !request_type_s3file

package fetch

import (
	"errors"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// errNoAWSStore is what a build without the s3file request type says of an
// aws secret store, which finds its credentials with the AWS SDK that type
// links (secretstore_aws.go).
var errNoAWSStore = errors.New("is of type aws, and this build has no aws secret store, which comes with the s3file request type; build with s3file among REQUEST_TYPES")

func checkAWSStore(*model.SecretStore) error { return errNoAWSStore }

func newAWSBackend(model.SecretStore, time.Duration) (secretBackend, error) {
	return nil, errNoAWSStore
}
