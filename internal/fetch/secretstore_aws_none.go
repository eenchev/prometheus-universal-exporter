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

// The type is registered all the same, so that its name and keys are the
// schema's in every build, and refuses every entry.
func init() {
	registerSecretStoreType(&SecretStoreType{
		Name:     "aws",
		Keys:     awsStoreKeys,
		Check:    func(*model.SecretStore) error { return errNoAWSStore },
		New:      func(model.SecretStore, time.Duration) (SecretBackend, error) { return nil, errNoAWSStore },
		KeyRules: awsStoreKeyRules,
	})
}
