//go:build (!select_request_types || request_type_s3file) && (!select_request_types || request_type_http || request_type_kafka)

package fetch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

// The AWS SDK's configuration, as request.aws_sigv4 (httpsigv4.go) and a
// kafka collector's AWS_MSK_IAM login (kafkaawsiam.go) sign with it. Both
// are in the builds that have the s3file request type, which links the
// SDK.

// errNoAWSRegion is what sigV4Configuration says when neither the
// collector nor the SDK's configuration names a region; each caller words
// it with its own key.
var errNoAWSRegion = errors.New("no AWS region is known")

// sigV4Configs are the SDK's configurations, by region asked for: each
// loaded once, with the provider of the credentials, which keeps them until
// they expire.
var sigV4Configs = struct {
	sync.Mutex
	loaded map[string]*aws.Config
}{loaded: map[string]*aws.Config{}}

// sigV4Now is the clock requests are signed by; a test sets it.
var sigV4Now = time.Now

// sigV4Configuration is the SDK's configuration for region, "" for the one
// the SDK's own configuration says.
func sigV4Configuration(ctx context.Context, region string) (*aws.Config, error) {
	sigV4Configs.Lock()
	defer sigV4Configs.Unlock()
	if cfg := sigV4Configs.loaded[region]; cfg != nil {
		return cfg, nil
	}
	var options []func(*config.LoadOptions) error
	if region != "" {
		options = append(options, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("loading the AWS SDK's configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errNoAWSRegion
	}
	sigV4Configs.loaded[region] = &cfg
	return &cfg, nil
}
