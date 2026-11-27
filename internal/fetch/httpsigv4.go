//go:build (!select_request_types || request_type_http) && (!select_request_types || request_type_s3file)

package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// sigV4Built says this build signs with AWS Signature Version 4.
func sigV4Built() error { return nil }

// sigV4Credentials are the credentials a request of s is signed with: the
// static access key of its files, read at each request, or those the SDK's
// chain finds.
func sigV4Credentials(ctx context.Context, s *SigV4Config) (aws.Credentials, string, error) {
	if s.AccessKeyIDFile != "" {
		values := [3]string{}
		for i, key := range []struct{ name, file string }{{"access_key_id_file", s.AccessKeyIDFile}, {"secret_access_key_file", s.SecretAccessKeyFile}, {"session_token_file", s.SessionTokenFile}} {
			if key.file == "" {
				continue
			}
			value, err := ReadCredentialFile(key.file)
			if err != nil {
				return aws.Credentials{}, "", fmt.Errorf("reading %s: %w", key.name, err)
			}
			if value == "" {
				return aws.Credentials{}, "", fmt.Errorf("%s %s is empty", key.name, key.file)
			}
			values[i] = value
		}
		region := s.Region
		if region == "" {
			cfg, err := sigV4Configuration(ctx, "")
			if err != nil {
				return aws.Credentials{}, "", sigV4RegionError(err)
			}
			region = cfg.Region
		}
		return aws.Credentials{AccessKeyID: values[0], SecretAccessKey: values[1], SessionToken: values[2], Source: "request.aws_sigv4"}, region, nil
	}
	cfg, err := sigV4Configuration(ctx, s.Region)
	if err != nil {
		return aws.Credentials{}, "", sigV4RegionError(err)
	}
	credentials, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, "", fmt.Errorf("finding AWS credentials: %w", err)
	}
	return credentials, cfg.Region, nil
}

// sigV4RegionError words a configuration without a region for
// request.aws_sigv4.
func sigV4RegionError(err error) error {
	if errors.Is(err, errNoAWSRegion) {
		return errors.New("no AWS region is known; set request.aws_sigv4.region, or AWS_REGION")
	}
	return err
}

// signWithSigV4 signs req, whose body is body, for the service and region
// of s: its Authorization, X-Amz-Date and, of temporary credentials,
// X-Amz-Security-Token are set, over the request as it stands.
func signWithSigV4(ctx context.Context, s *SigV4Config, req *http.Request, body string) error {
	credentials, region, err := sigV4Credentials(ctx, s)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(body))
	// The headers of a request signed before, as a retry's are not, since
	// each attempt is a new request; they are dropped all the same.
	req.Header.Del("Authorization")
	req.Header.Del("X-Amz-Date")
	req.Header.Del("X-Amz-Security-Token")
	if err := v4.NewSigner().SignHTTP(ctx, credentials, req, hex.EncodeToString(hash[:]), s.Service, region, sigV4Now()); err != nil {
		return fmt.Errorf("signing the request: %w", err)
	}
	return nil
}
