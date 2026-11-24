//go:build !select_request_types || request_type_http

package fetch

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// request.aws_sigv4 signs an http collector's requests with AWS Signature
// Version 4, so the collector reads an AWS API as it reads any other: a
// CloudWatch statistic, an SQS queue's depth, a cost report.
//
//	request:
//	  method: POST
//	  headers:
//	    Content-Type: application/x-amz-json-1.0
//	    X-Amz-Target: AmazonSQS.GetQueueAttributes
//	  body: '{"QueueUrl": "https://sqs.eu-west-1.amazonaws.com/123456789012/jobs", "AttributeNames": ["All"]}'
//	  aws_sigv4:
//	    service: sqs
//	    region: eu-west-1
//
// The credentials are those the AWS SDK's default chain finds, as for an
// s3file collector, a role for the service account, EKS Pod Identity, the
// instance's role or the environment, unless access_key_id_file and
// secret_access_key_file name a static access key. The SDK's signer and
// credential providers are the modules the s3file request type links, so
// the key is in the builds that have both types and in no other
// (httpsigv4.go, httpsigv4_none.go); its keys are every build's, so the
// schema is one.

// SigV4Config is request.aws_sigv4.
type SigV4Config struct {
	// Service is the name the API signs under, such as sqs, monitoring,
	// ce, ecs, lambda or execute-api.
	Service string `yaml:"service"`
	// Region is the region signed for; the SDK's own configuration, such
	// as AWS_REGION, says it when left out.
	Region string `yaml:"region"`
	// AccessKeyIDFile, SecretAccessKeyFile and SessionTokenFile are a
	// static access key's files, in place of the SDK's chain.
	AccessKeyIDFile     string `yaml:"access_key_id_file"`
	SecretAccessKeyFile string `yaml:"secret_access_key_file"`
	SessionTokenFile    string `yaml:"session_token_file"`
}

// sigV4Name is a service's or a region's name.
var sigV4Name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validateSigV4 checks request.aws_sigv4 when the configuration loads.
func validateSigV4(c *model.Collector, s *SigV4Config) error {
	if s == nil {
		return nil
	}
	where := fmt.Sprintf("collector %q request.aws_sigv4", c.Name)
	if err := sigV4Built(); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	r := c.Request
	switch {
	case !sigV4Name.MatchString(s.Service):
		return fmt.Errorf("%s.service is %q; name the service the API signs under, in lower case, such as sqs, monitoring, ce or execute-api", where, s.Service)
	case s.Region != "" && !sigV4Name.MatchString(s.Region):
		return fmt.Errorf("%s.region is %q; want a region's name, such as eu-west-1, or leave it out for the one the AWS SDK's configuration says", where, s.Region)
	case (s.AccessKeyIDFile == "") != (s.SecretAccessKeyFile == ""):
		return fmt.Errorf("%s sets one of access_key_id_file and secret_access_key_file without the other; a static access key is the two together, and without either the AWS SDK's default chain finds the credentials", where)
	case s.SessionTokenFile != "" && s.AccessKeyIDFile == "":
		return fmt.Errorf("%s sets session_token_file without access_key_id_file and secret_access_key_file; the token is the temporary access key's", where)
	case strings.Contains(s.AccessKeyIDFile+s.SecretAccessKeyFile+s.SessionTokenFile, "{{"):
		return fmt.Errorf("%s names a file with a placeholder; the credentials are the configuration's", where)
	case r.BasicAuth != nil || r.BasicAuthFile != nil || r.BearerToken != "" || r.BearerTokenFile != "" || r.ForwardAuthorization:
		return fmt.Errorf("%s is set beside a credential sent in Authorization (basic_auth, bearer_token or forward_authorization); the signature is the request's Authorization", where)
	case r.Login != nil && strings.EqualFold(LoginHeader(r.Login), "Authorization"):
		return fmt.Errorf("%s is set beside a login whose token is sent in Authorization; the signature is the request's Authorization", where)
	}
	for name := range r.Headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-Amz-Date") || strings.EqualFold(name, "X-Amz-Security-Token") {
			return fmt.Errorf("%s is set beside request.headers.%s, which the signing sets itself", where, name)
		}
	}
	return nil
}

// sigV4KeyRules is what the configuration schema says of the block's keys.
func sigV4KeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"aws_sigv4":                        {"required": []string{"service"}, "description": "http: sign each request with AWS Signature Version 4, with the credentials the AWS SDK's default chain finds, so the collector reads an AWS API. In the builds that have the s3file request type. See docs/AUTHENTICATION.md#signing-requests-for-aws."},
		"aws_sigv4.service":                {"pattern": `^[a-z0-9][a-z0-9-]*$`, "description": "Required. The name the API signs under, such as sqs, monitoring (CloudWatch), ce (Cost Explorer), ecs, lambda or execute-api."},
		"aws_sigv4.region":                 {"pattern": `^([a-z0-9][a-z0-9-]*)?$`, "description": "The region signed for, such as eu-west-1; the AWS SDK's configuration, such as AWS_REGION, says it when left out."},
		"aws_sigv4.access_key_id_file":     {"description": "The file of a static access key's id, with secret_access_key_file, in place of the credentials the AWS SDK's default chain finds."},
		"aws_sigv4.secret_access_key_file": {"description": "The file of a static access key's secret, with access_key_id_file."},
		"aws_sigv4.session_token_file":     {"description": "The file of a temporary access key's session token, with the other two."},
	}
}
