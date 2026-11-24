//go:build (!select_request_types || request_type_http) && select_request_types && !request_type_s3file

package fetch

import (
	"context"
	"errors"
	"net/http"
)

// errNoSigV4 is what a build without the s3file request type says of
// request.aws_sigv4, which signs with the AWS SDK that type links
// (httpsigv4.go).
var errNoSigV4 = errors.New("this build does not sign with AWS Signature Version 4, which comes with the s3file request type; build with s3file among REQUEST_TYPES")

// sigV4Built says this build does not sign with AWS Signature Version 4.
func sigV4Built() error { return errNoSigV4 }

func signWithSigV4(context.Context, *SigV4Config, *http.Request, string) error { return errNoSigV4 }
