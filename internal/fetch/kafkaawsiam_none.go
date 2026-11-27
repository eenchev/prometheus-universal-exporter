//go:build request_type_kafka && select_request_types && !request_type_s3file

package fetch

import (
	"context"
	"errors"
)

// errNoKafkaAWS is what a build without the s3file request type says of
// an AWS_MSK_IAM login, which signs its token with the AWS SDK that type
// links (kafkaawsiam.go).
var errNoKafkaAWS = errors.New("this build has no AWS_MSK_IAM login, which signs with the AWS SDK that comes with the s3file request type; build with s3file among REQUEST_TYPES")

// kafkaAWSBuilt says this build has no AWS_MSK_IAM login.
func kafkaAWSBuilt() error { return errNoKafkaAWS }

func kafkaMSKToken(context.Context, string) (string, error) { return "", errNoKafkaAWS }
