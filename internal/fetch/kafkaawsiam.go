//go:build request_type_kafka && (!select_request_types || request_type_s3file)

package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// A kafka collector's AWS_MSK_IAM login, to an Amazon MSK cluster that
// authenticates with IAM. The login is OAUTHBEARER's, whose token is a URL
// presigned with AWS Signature Version 4 for the action
// kafka-cluster:Connect, as AWS's aws-msk-iam-sasl-signer libraries make
// it: the broker checks the signature and the IAM policy of the
// credentials that made it. The credentials are those the AWS SDK's
// default chain finds, as for request.aws_sigv4 and an s3file collector,
// so the login is in the builds that have the s3file request type, which
// links the SDK (kafkaawsiam_none.go).

// kafkaMSKExpiry is how long a token is good for, in seconds, as AWS's
// signers make it; a scrape makes one and uses it at once.
const kafkaMSKExpiry = "900"

// kafkaAWSBuilt says this build has the AWS_MSK_IAM login.
func kafkaAWSBuilt() error { return nil }

// kafkaMSKToken is the token of an AWS_MSK_IAM login for region, "" for
// the one the SDK's configuration says.
func kafkaMSKToken(ctx context.Context, region string) (string, error) {
	cfg, err := sigV4Configuration(ctx, region)
	if errors.Is(err, errNoAWSRegion) {
		return "", errors.New("no AWS region is known for the AWS_MSK_IAM login; set request.aws_region, or AWS_REGION")
	}
	if err != nil {
		return "", err
	}
	credentials, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("finding AWS credentials for the AWS_MSK_IAM login: %w", err)
	}
	query := url.Values{"Action": {"kafka-cluster:Connect"}, "X-Amz-Expires": {kafkaMSKExpiry}}
	endpoint := url.URL{Scheme: "https", Host: "kafka." + cfg.Region + ".amazonaws.com", Path: "/", RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("the AWS_MSK_IAM login's region %q: %w", cfg.Region, err)
	}
	empty := sha256.Sum256(nil)
	signed, _, err := v4.NewSigner().PresignHTTP(ctx, credentials, req, hex.EncodeToString(empty[:]), "kafka-cluster", cfg.Region, sigV4Now().UTC())
	if err != nil {
		return "", fmt.Errorf("signing the AWS_MSK_IAM login's token: %w", err)
	}
	presigned, err := url.Parse(signed)
	if err != nil {
		return "", fmt.Errorf("signing the AWS_MSK_IAM login's token: %w", err)
	}
	// The broker logs the client that made the token by its User-Agent,
	// which is not signed.
	params := presigned.Query()
	params.Set("User-Agent", kafkaClientID)
	presigned.RawQuery = params.Encode()
	return base64.RawURLEncoding.EncodeToString([]byte(presigned.String())), nil
}
