//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// signedCollector is an http collector that signs its requests as s says,
// edited by edit first.
func signedCollector(t *testing.T, s SignatureConfig, edit func(*model.Collector)) *model.Collector {
	t.Helper()
	c := testutil.Collector("api", "text")
	if edit != nil {
		edit(&c)
	}
	httpOwn(&c.Request, func(own *HTTPConfig) { own.Signature = &s })
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// A collector with request.signature signs each request with an HMAC of
// its secret over the payload, made of the request as it is sent. In a
// query parameter the signature is added last and signs the query before
// it, as an exchange's API asks; a retry is signed as the first request
// was.
func TestARequestIsSignedInItsQuery(t *testing.T) {
	secret := "s3cret-key"
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		query, signature, found := strings.Cut(r.URL.RawQuery, "&signature=")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(query))
		if !found || signature != hex.EncodeToString(mac.Sum(nil)) || query != "recvWindow=5000&symbol=BTC%2FEUR" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	defer target.Close()
	c := signedCollector(t, SignatureConfig{SecretFile: testutil.WriteFile(t, "secret", secret+"\n"), Payload: "{{query}}", QueryParam: "signature"}, func(c *model.Collector) {
		c.Request.Path, c.Request.Query = "/api/v3/account", map[string]string{"symbol": "BTC/EUR", "recvWindow": "5000"}
		c.Request.Retry = model.RetryConfig{Attempts: 1, Backoff: model.Duration(1)}
	})
	resp, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != "value=1\n" || requests.Load() != 2 {
		t.Fatalf("%v %v after %d requests", resp, err, requests.Load())
	}
}

// In a header the signature follows its prefix, in the encoding asked for,
// of a secret the file writes in base64; the payload names the method, the
// path, the body, the host and a header of the request, each as it is
// sent.
func TestARequestIsSignedInAHeader(t *testing.T) {
	secret := []byte{0x00, 0xff, 0x10, 0x80, 's', 'k'}
	var problem atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha512.New, secret)
		mac.Write([]byte(r.Header.Get("X-Timestamp") + "POST/v1/orders/a%20b" + string(body) + r.Host + "|"))
		want := "v1=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if got := r.Header.Get("X-Signature"); got != want || string(body) != `{"q":1}` || r.Header.Get("X-Timestamp") != "1700000000" {
			problem.Store("signature " + got + ", want " + want)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	defer target.Close()
	c := signedCollector(t, SignatureConfig{Algorithm: "hmac-sha512", SecretEncoding: "base64", Encoding: "base64", Header: "X-Signature", Prefix: "v1=",
		SecretFile: testutil.WriteFile(t, "secret", base64.StdEncoding.EncodeToString(secret)),
		Payload:    "{{header:X-Timestamp}}{{method}}{{path}}{{body}}{{ host }}|{{header:X-Absent}}"}, func(c *model.Collector) {
		c.Request.Path, c.Request.Headers = "/v1/orders/a b", map[string]string{"X-Timestamp": "1700000000", "Content-Type": "application/json"}
		httpOwn(&c.Request, func(own *HTTPConfig) { own.Method, own.Body = http.MethodPost, `{"q":1}` })
	})
	if resp, err := FetchCollector(context.Background(), target.URL, c, RequestOverrides{}, nil); err != nil || string(resp.Body) != "value=1\n" {
		t.Fatalf("%v %v %v", resp, err, problem.Load())
	}
}

// What is wrong with request.signature is said when the configuration
// loads, and a secret that cannot be read, or is not written as
// secret_encoding says, fails the scrape before anything is sent, without
// the secret.
func TestARequestSignatureIsChecked(t *testing.T) {
	for want, s := range map[string]SignatureConfig{
		`request.signature.algorithm is "md5"`:              {Algorithm: "md5", SecretFile: "/s", Payload: "x", Header: "X-Sig"},
		"request.signature has no secret_file":              {Payload: "x", Header: "X-Sig"},
		`request.signature.secret_encoding is "base32"`:     {SecretFile: "/s", SecretEncoding: "base32", Payload: "x", Header: "X-Sig"},
		`request.signature.encoding is "binary"`:            {SecretFile: "/s", Encoding: "binary", Payload: "x", Header: "X-Sig"},
		"request.signature has no payload":                  {SecretFile: "/s", Header: "X-Sig"},
		"needs exactly one of header and query_param":       {SecretFile: "/s", Payload: "x"},
		"request.signature needs exactly one of header and": {SecretFile: "/s", Payload: "x", Header: "X-Sig", QueryParam: "sig"},
		`request.signature.header "X Sig" is not a header`:  {SecretFile: "/s", Payload: "x", Header: "X Sig"},
		"sets prefix with query_param":                      {SecretFile: "/s", Payload: "x", QueryParam: "sig", Prefix: "v1="},
		`query_param "a&b" is no name`:                      {SecretFile: "/s", Payload: "x", QueryParam: "a&b"},
		"request.signature.payload has {{timestamp}}":       {SecretFile: "/s", Payload: "{{timestamp}}{{body}}", Header: "X-Sig"},
		`payload names the header "X Sig"`:                  {SecretFile: "/s", Payload: "{{header:X Sig}}", Header: "X-Sig"},
		"payload has {{body:x}}; only {{header:Name}} take": {SecretFile: "/s", Payload: "{{body:x}}", Header: "X-Sig"},
		"payload has a {{ that is none of":                  {SecretFile: "/s", Payload: "{{body|json}}", Header: "X-Sig"},
		`header "Content-Type" is no header to send a sign`: {SecretFile: "/s", Payload: "x", Header: "Content-Type"},
	} {
		c := testutil.Collector("api", "text")
		httpOwn(&c.Request, func(own *HTTPConfig) { own.Signature = &s })
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	both := testutil.Collector("api", "text")
	httpOwn(&both.Request, func(own *HTTPConfig) {
		own.Signature, own.AWSSigV4 = &SignatureConfig{SecretFile: "/s", Payload: "x", Header: "X-Sig"}, &SigV4Config{Service: "sqs"}
	})
	if err := ValidateRequest(&both); err == nil || !strings.Contains(err.Error(), "a request is signed one way") {
		t.Errorf("both: %v", err)
	}
	inAuthorization := testutil.Collector("api", "text")
	inAuthorization.Request.BearerToken = "t"
	httpOwn(&inAuthorization.Request, func(own *HTTPConfig) {
		own.Signature = &SignatureConfig{SecretFile: "/s", Payload: "x", Header: "authorization"}
	})
	if err := ValidateRequest(&inAuthorization); err == nil || !strings.Contains(err.Error(), "sends the signature in Authorization, which the collector's credential") {
		t.Errorf("beside a credential: %v", err)
	}

	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer target.Close()
	for want, s := range map[string]SignatureConfig{
		"reading request.signature.secret_file":        {SecretFile: "/nonexistent/secret"},
		"is empty":                                     {SecretFile: testutil.WriteFile(t, "empty", "\n")},
		"is not hex, as secret_encoding says it is":    {SecretFile: testutil.WriteFile(t, "nothex", "s3cret-zz"), SecretEncoding: "hex"},
		"is not base64, as secret_encoding says it is": {SecretFile: testutil.WriteFile(t, "notb64", "s3cret!!"), SecretEncoding: "base64"},
	} {
		s.Payload, s.Header = "{{body}}", "X-Sig"
		_, err := FetchCollector(context.Background(), target.URL, signedCollector(t, s, nil), RequestOverrides{}, nil)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "s3cret") || requests.Load() != 0 {
			t.Errorf("err=%v after %d requests, want %q", err, requests.Load(), want)
		}
	}
}
