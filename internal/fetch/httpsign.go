//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: HMAC-SHA1 is what some APIs sign with; it is the API's choice, not a hash of ours
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Some APIs take no token: each request is signed with a secret the two
// sides share, and the server works out the same signature to know the
// request is the account's and was not changed on its way. request.signature
// signs an http collector's requests so, with an HMAC:
//
//	request:
//	  query:
//	    symbol: BTCEUR
//	    timestamp: "{{unix_ms}}"
//	  signature:
//	    secret_file: /etc/exporter/api-secret
//	    payload: "{{query}}"
//	    query_param: signature
//
// payload says what is signed, of the request as it is sent: its text, with
// {{method}}, {{path}}, {{query}}, {{body}}, {{host}}, {{url}} and
// {{header:Name}} in it for those parts. The signature is put in a header
// or in a query parameter, which is added last, so it signs the query
// before it. A timestamp an API wants signed is a clock placeholder in a
// header or the query, which the payload then holds as part of them.
//
// request.aws_sigv4 signs with AWS Signature Version 4 instead
// (httpsigv4.go). Either signs every request of a trip, a retry and each
// page of a paginated answer its own.

// SignatureConfig is request.signature.
type SignatureConfig struct {
	// Algorithm is the HMAC's hash: hmac-sha256, the default, hmac-sha1,
	// hmac-sha384 or hmac-sha512.
	Algorithm string `yaml:"algorithm"`
	// SecretFile holds the secret, and SecretEncoding says how the file
	// writes it: text, the default, base64 or hex.
	SecretFile     string `yaml:"secret_file"`
	SecretEncoding string `yaml:"secret_encoding"`
	// Payload is what is signed.
	Payload string `yaml:"payload"`
	// Encoding is how the signature is written: hex, the default, base64
	// or base64url.
	Encoding string `yaml:"encoding"`
	// Header is the header the signature is sent in, after Prefix, and
	// QueryParam the query parameter; one of the two.
	Header     string `yaml:"header"`
	Prefix     string `yaml:"prefix"`
	QueryParam string `yaml:"query_param"`
}

var (
	signatureAlgorithms = []string{"hmac-sha256", "hmac-sha1", "hmac-sha384", "hmac-sha512"}
	signatureEncodings  = []string{"hex", "base64", "base64url"}
	signatureSecretForm = []string{"text", "base64", "hex"}
	// signaturePart is a placeholder of a payload.
	signaturePart  = regexp.MustCompile(`\{\{\s*([a-z_]+)(?::([^}]*))?\s*\}\}`)
	signatureParts = []string{"method", "path", "query", "body", "host", "url", "header"}
)

// signatureKeyRules is what the configuration schema says of the block's
// keys.
func signatureKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"signature":                 {"required": []string{"payload"}, "description": "http: sign each request with an HMAC of a shared secret, sent in a header or a query parameter, as APIs that take no token ask. See docs/AUTHENTICATION.md#signing-requests."},
		"signature.algorithm":       {"enum": SchemaOptionalEnum(signatureAlgorithms), "description": "The HMAC's hash: hmac-sha256, the default, hmac-sha1, hmac-sha384 or hmac-sha512."},
		"signature.secret_file":     {"description": "Required. The file of the secret the requests are signed with, read at each request."},
		"signature.secret_encoding": {"enum": SchemaOptionalEnum(signatureSecretForm), "description": "How the file writes the secret: text, the default, base64 or hex."},
		"signature.payload":         {"description": "Required. What is signed, of the request as it is sent: text with {{method}}, {{path}}, {{query}}, {{body}}, {{host}}, {{url}} and {{header:Name}} in it, such as \"{{query}}\" or \"{{header:X-Timestamp}}{{method}}{{path}}{{body}}\"."},
		"signature.encoding":        {"enum": SchemaOptionalEnum(signatureEncodings), "description": "How the signature is written: hex, the default, base64 or base64url."},
		"signature.header":          {"description": "The header the signature is sent in. Exactly one of header and query_param."},
		"signature.prefix":          {"description": "What the signature is sent after in its header, such as \"sha256=\"."},
		"signature.query_param":     {"description": "The query parameter the signature is sent in, added after every other, so the payload's {{query}} is the query before it."},
	}
}

// signatureHash is the hash of an algorithm.
func signatureHash(algorithm string) func() hash.Hash {
	switch algorithm {
	case "hmac-sha1":
		return sha1.New
	case "hmac-sha384":
		return sha512.New384
	case "hmac-sha512":
		return sha512.New
	}
	return sha256.New
}

// validateSignature checks request.signature when the configuration loads.
func validateSignature(c *model.Collector, s *SignatureConfig) error {
	if s == nil {
		return nil
	}
	where := fmt.Sprintf("collector %q request.signature", c.Name)
	switch {
	case s.Algorithm != "" && !slices.Contains(signatureAlgorithms, s.Algorithm):
		return fmt.Errorf("%s.algorithm is %q; want hmac-sha256, the default, hmac-sha1, hmac-sha384 or hmac-sha512", where, s.Algorithm)
	case strings.TrimSpace(s.SecretFile) == "" && !c.RequestNotMade:
		return fmt.Errorf("%s has no secret_file; name the file of the secret the requests are signed with", where)
	case strings.Contains(s.SecretFile, "{{"):
		return fmt.Errorf("%s.secret_file has a placeholder; the secret is the configuration's", where)
	case s.SecretEncoding != "" && !slices.Contains(signatureSecretForm, s.SecretEncoding):
		return fmt.Errorf("%s.secret_encoding is %q; it is how the file writes the secret: text, the default, base64 or hex", where, s.SecretEncoding)
	case s.Encoding != "" && !slices.Contains(signatureEncodings, s.Encoding):
		return fmt.Errorf("%s.encoding is %q; it is how the signature is written: hex, the default, base64 or base64url", where, s.Encoding)
	case s.Payload == "":
		return fmt.Errorf("%s has no payload; say what is signed, such as \"{{query}}\" or \"{{header:X-Timestamp}}{{method}}{{path}}{{body}}\"", where)
	case (s.Header == "") == (s.QueryParam == ""):
		return fmt.Errorf("%s needs exactly one of header and query_param, where the signature is sent", where)
	case s.Header != "" && !httpguts.ValidHeaderFieldName(s.Header):
		return fmt.Errorf("%s.header %q is not a header name", where, s.Header)
	case s.QueryParam != "" && s.Prefix != "":
		return fmt.Errorf("%s sets prefix with query_param; a prefix goes before the signature in a header", where)
	case strings.ContainsFunc(s.QueryParam, func(r rune) bool { return r <= 0x20 || r == 0x7f || r == '&' || r == '=' || r == '#' || r == '?' }):
		return fmt.Errorf("%s.query_param %q is no name of a query parameter", where, s.QueryParam)
	}
	if err := checkHeaderValue(s.Prefix); err != nil {
		return fmt.Errorf("%s.prefix %w", where, err)
	}
	switch http.CanonicalHeaderKey(s.Header) {
	case "Host", "Accept-Encoding", "Content-Length", "Content-Type", "Transfer-Encoding", "Connection":
		return fmt.Errorf("%s.header %q is no header to send a signature in", where, s.Header)
	}
	for _, part := range signaturePart.FindAllStringSubmatch(s.Payload, -1) {
		switch {
		case !slices.Contains(signatureParts, part[1]):
			return fmt.Errorf("%s.payload has {{%s}}; the parts of a request it may name are {{method}}, {{path}}, {{query}}, {{body}}, {{host}}, {{url}} and {{header:Name}}", where, part[1])
		case part[1] == "header" && !httpguts.ValidHeaderFieldName(strings.TrimSpace(part[2])):
			return fmt.Errorf("%s.payload names the header %q, which is no header name; write {{header:Name}}", where, part[2])
		case part[1] != "header" && part[2] != "":
			return fmt.Errorf("%s.payload has {{%s:%s}}; only {{header:Name}} takes a name", where, part[1], part[2])
		}
	}
	if strings.Contains(signaturePart.ReplaceAllString(s.Payload, ""), "{{") {
		return fmt.Errorf("%s.payload has a {{ that is none of {{method}}, {{path}}, {{query}}, {{body}}, {{host}}, {{url}} and {{header:Name}}; a timestamp to sign goes in a header or the query, as a clock placeholder there, and is signed as part of it", where)
	}
	if r := c.Request; s.Header != "" && strings.EqualFold(s.Header, "Authorization") && (r.BasicAuth != nil || r.BasicAuthFile != nil || r.BearerToken != "" || r.BearerTokenFile != "" || r.Login != nil && strings.EqualFold(LoginHeader(r.Login), "Authorization")) {
		return fmt.Errorf("%s sends the signature in Authorization, which the collector's credential or login is sent in; one request has one", where)
	}
	return nil
}

// signatureSecret reads the secret of s, as its encoding says.
func signatureSecret(s *SignatureConfig) ([]byte, error) {
	text, err := ReadCredentialFile(s.SecretFile)
	if err != nil {
		return nil, fmt.Errorf("reading request.signature.secret_file: %w", err)
	}
	if text == "" {
		return nil, fmt.Errorf("request.signature.secret_file %s is empty", s.SecretFile)
	}
	switch s.SecretEncoding {
	case "base64":
		secret, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			if secret, err = base64.RawStdEncoding.DecodeString(text); err != nil {
				return nil, fmt.Errorf("request.signature.secret_file %s is not base64, as secret_encoding says it is", s.SecretFile)
			}
		}
		return secret, nil
	case "hex":
		secret, err := hex.DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("request.signature.secret_file %s is not hex, as secret_encoding says it is", s.SecretFile)
		}
		return secret, nil
	}
	return []byte(text), nil
}

// signWithHMAC signs req, whose body is body, as s says: the payload is
// made of the request as it stands, and the signature set in its header or
// added to its query.
func signWithHMAC(s *SignatureConfig, req *http.Request, body string) error {
	secret, err := signatureSecret(s)
	if err != nil {
		return err
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	payload := signaturePart.ReplaceAllStringFunc(s.Payload, func(written string) string {
		part := signaturePart.FindStringSubmatch(written)
		switch part[1] {
		case "method":
			return req.Method
		case "path":
			return req.URL.EscapedPath()
		case "query":
			return req.URL.RawQuery
		case "body":
			return body
		case "host":
			return host
		case "url":
			return req.URL.String()
		}
		return req.Header.Get(strings.TrimSpace(part[2]))
	})
	mac := hmac.New(signatureHash(s.Algorithm), secret)
	mac.Write([]byte(payload))
	sum := mac.Sum(nil)
	var signature string
	switch s.Encoding {
	case "base64":
		signature = base64.StdEncoding.EncodeToString(sum)
	case "base64url":
		signature = base64.RawURLEncoding.EncodeToString(sum)
	default:
		signature = hex.EncodeToString(sum)
	}
	if s.Header != "" {
		req.Header.Set(s.Header, s.Prefix+signature)
		return nil
	}
	if req.URL.RawQuery != "" {
		req.URL.RawQuery += "&"
	}
	req.URL.RawQuery += s.QueryParam + "=" + signature
	return nil
}

// signRequest signs a request of c before it is sent, when its collector
// signs its requests: with request.signature's HMAC, or with AWS Signature
// Version 4. A request that cannot be signed is not sent.
func signRequest(ctx context.Context, c *model.Collector, req *http.Request, body string) error {
	own := httpRequest(c)
	switch {
	case own.Signature != nil:
		if err := signWithHMAC(own.Signature, req, body); err != nil {
			return err
		}
	case own.AWSSigV4 != nil:
		if err := signWithSigV4(ctx, own.AWSSigV4, req, body); err != nil {
			return fmt.Errorf("request.aws_sigv4: %w", err)
		}
	}
	return nil
}

func init() { signHTTPRequest = signRequest }

// errBothSignatures refuses a collector that signs its requests two ways.
var errBothSignatures = errors.New("sets request.signature and request.aws_sigv4; a request is signed one way")
