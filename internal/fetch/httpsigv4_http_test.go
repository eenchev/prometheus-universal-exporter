//go:build !select_request_types || (request_type_http && request_type_s3file)

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sigV4Expected is the signature AWS works out of a request, as its
// documentation writes Signature Version 4: the canonical request of the
// method, the path, the query and the signed headers, the string to sign of
// it, and the key derived from the secret, the date, the region and the
// service.
func sigV4Expected(r *http.Request, body []byte, signedHeaders, secret, region, service string) string {
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	mac := func(key []byte, text string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(text))
		return m.Sum(nil)
	}
	var headers strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		value := r.Header.Get(name)
		if name == "host" {
			value = r.Host
		}
		headers.WriteString(name)
		headers.WriteString(":")
		headers.WriteString(strings.TrimSpace(value))
		headers.WriteString("\n")
	}
	canonical := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, headers.String(), signedHeaders, sum(body)}, "\n")
	date := r.Header.Get("X-Amz-Date")
	scope := date[:8] + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", date, scope, sum([]byte(canonical))}, "\n")
	key := mac(mac(mac(mac([]byte("AWS4"+secret), date[:8]), region), service), "aws4_request")
	return hex.EncodeToString(mac(key, toSign))
}

// A collector with request.aws_sigv4 signs each request with AWS Signature
// Version 4, for its service and region, with the static access key of its
// files: the Authorization names the key and the scope, and its signature
// is the one AWS works out of the request as it was sent, body and headers
// included; a session token is sent beside it.
func TestARequestIsSignedForAWS(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	sigV4Now = func() time.Time { return at }
	t.Cleanup(func() { sigV4Now = time.Now })
	var problem atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		authorization := r.Header.Get("Authorization")
		credential, rest, _ := strings.Cut(strings.TrimPrefix(authorization, "AWS4-HMAC-SHA256 Credential="), ", SignedHeaders=")
		signedHeaders, signature, _ := strings.Cut(rest, ", Signature=")
		want := sigV4Expected(r, body, signedHeaders, "wJalrXUtnFEMI/s3cret", "eu-west-1", "sqs")
		if credential != "AKIDEXAMPLE/20261124/eu-west-1/sqs/aws4_request" || signature != want || r.Header.Get("X-Amz-Date") != "20261124T120000Z" || r.Header.Get("X-Amz-Security-Token") != "session-token" ||
			!strings.Contains(signedHeaders, "host") || !strings.Contains(signedHeaders, "x-amz-target") || string(body) != `{"QueueUrl":"q"}` {
			problem.Store("Authorization " + authorization + ", want the signature " + want)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	defer target.Close()
	c := testutil.Collector("queue", "text")
	c.Request.Headers = map[string]string{"X-Amz-Target": "AmazonSQS.GetQueueAttributes", "Content-Type": "application/x-amz-json-1.0"}
	httpOwn(&c.Request, func(own *HTTPConfig) {
		own.Method, own.Body = http.MethodPost, `{"QueueUrl":"q"}`
		own.AWSSigV4 = &SigV4Config{Service: "sqs", Region: "eu-west-1", AccessKeyIDFile: testutil.WriteFile(t, "id", "AKIDEXAMPLE\n"),
			SecretAccessKeyFile: testutil.WriteFile(t, "secret", "wJalrXUtnFEMI/s3cret\n"), SessionTokenFile: testutil.WriteFile(t, "token", "session-token\n")}
	})
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if resp, err := FetchCollector(context.Background(), target.URL, &c, RequestOverrides{}, nil); err != nil || string(resp.Body) != "value=1\n" {
		t.Fatalf("%v %v %v", resp, err, problem.Load())
	}
	// A key file that is missing fails the scrape, naming the key and not
	// the secret, before anything is sent.
	httpOwn(&c.Request, func(own *HTTPConfig) {
		missing := *own.AWSSigV4
		missing.SessionTokenFile = "/nonexistent/token"
		own.AWSSigV4 = &missing
	})
	if _, err := FetchCollector(context.Background(), target.URL, &c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "request.aws_sigv4: reading session_token_file") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("a missing file: %v", err)
	}
}

// What is wrong with request.aws_sigv4 is said when the configuration
// loads: a service or a region that is no name, half an access key, and a
// credential or a header the signing sets itself beside it.
func TestAWSRequestSigningIsChecked(t *testing.T) {
	for want, edit := range map[string]func(*model.Collector, *SigV4Config){
		`request.aws_sigv4.service is ""`:                       func(*model.Collector, *SigV4Config) {},
		`request.aws_sigv4.service is "SQS"`:                    func(_ *model.Collector, s *SigV4Config) { s.Service = "SQS" },
		`request.aws_sigv4.region is "EU West"`:                 func(_ *model.Collector, s *SigV4Config) { s.Service, s.Region = "sqs", "EU West" },
		"sets one of access_key_id_file and secret_access_key_": func(_ *model.Collector, s *SigV4Config) { s.Service, s.AccessKeyIDFile = "sqs", "/id" },
		"sets session_token_file without access_key_id_file":    func(_ *model.Collector, s *SigV4Config) { s.Service, s.SessionTokenFile = "sqs", "/t" },
		"is set beside a credential sent in Authorization":      func(c *model.Collector, s *SigV4Config) { s.Service, c.Request.BearerToken = "sqs", "t" },
		"is set beside request.headers.x-amz-date, which the sig": func(c *model.Collector, s *SigV4Config) {
			s.Service, c.Request.Headers = "sqs", map[string]string{"x-amz-date": "now"}
		},
	} {
		c := testutil.Collector("queue", "text")
		s := SigV4Config{}
		edit(&c, &s)
		httpOwn(&c.Request, func(own *HTTPConfig) { own.AWSSigV4 = &s })
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}
