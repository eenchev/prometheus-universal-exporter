//go:build request_type_kafka && (!select_request_types || request_type_s3file)

package fetch

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/eenchev/prometheus-universal-exporter/internal/kafkatest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// kafkaAWSEnvironment gives the AWS SDK a static access key from the
// environment and nothing else to find, region as AWS_REGION, and loads
// its configuration again, at a fixed clock.
func kafkaAWSEnvironment(t *testing.T, region string) {
	t.Helper()
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/s3cret", "AWS_SESSION_TOKEN": "session-token",
		"AWS_REGION": region, "AWS_DEFAULT_REGION": "", "AWS_PROFILE": "", "AWS_CONFIG_FILE": "/nonexistent/config",
		"AWS_SHARED_CREDENTIALS_FILE": "/nonexistent/credentials", "AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(name, value)
	}
	reset := func() {
		sigV4Configs.Lock()
		sigV4Configs.loaded = map[string]*aws.Config{}
		sigV4Configs.Unlock()
	}
	reset()
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	sigV4Now = func() time.Time { return at }
	t.Cleanup(func() {
		sigV4Now = time.Now
		reset()
	})
}

// kafkaMSKCheck is what an MSK broker checks of an AWS_MSK_IAM token, as
// AWS documents Signature Version 4 for a presigned URL: the URL of the
// action kafka-cluster:Connect at kafka.<region>.amazonaws.com, the key,
// the scope, the time and the session token in its query, and the
// signature of its canonical request, the query but the signature and the
// User-Agent; "" for a token that passes.
func kafkaMSKCheck(token, region string) string {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "not base64url: " + err.Error()
	}
	u, err := url.Parse(string(raw))
	if err != nil {
		return "not a URL: " + err.Error()
	}
	q := u.Query()
	for key, want := range map[string]string{
		"Action": "kafka-cluster:Connect", "X-Amz-Algorithm": "AWS4-HMAC-SHA256", "X-Amz-Credential": "AKIDEXAMPLE/20261127/" + region + "/kafka-cluster/aws4_request",
		"X-Amz-Date": "20261127T120000Z", "X-Amz-Expires": "900", "X-Amz-SignedHeaders": "host", "X-Amz-Security-Token": "session-token", "User-Agent": "prometheus-universal-exporter",
	} {
		if got := q.Get(key); got != want {
			return key + " is " + got + ", want " + want
		}
	}
	if u.Scheme != "https" || u.Host != "kafka."+region+".amazonaws.com" || u.Path != "/" {
		return "the URL is " + u.Scheme + "://" + u.Host + u.Path
	}
	escape := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	keys := []string{}
	for key := range q {
		if key != "X-Amz-Signature" && key != "User-Agent" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, escape(key)+"="+escape(q.Get(key)))
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	mac := func(key []byte, text string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(text))
		return m.Sum(nil)
	}
	canonical := strings.Join([]string{"GET", "/", strings.Join(pairs, "&"), "host:" + u.Host + "\n", "host", sum(nil)}, "\n")
	scope := "20261127/" + region + "/kafka-cluster/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", "20261127T120000Z", scope, sum([]byte(canonical))}, "\n")
	key := mac(mac(mac(mac([]byte("AWS4wJalrXUtnFEMI/s3cret"), "20261127"), region), "kafka-cluster"), "aws4_request")
	if want := hex.EncodeToString(mac(key, toSign)); q.Get("X-Amz-Signature") != want {
		return "the signature is " + q.Get("X-Amz-Signature") + ", want " + want
	}
	return ""
}

// An AWS_MSK_IAM login sends, as OAUTHBEARER, a URL presigned for
// kafka-cluster:Connect with the credentials the AWS SDK finds, for the
// region of request.aws_region, else the SDK's, and every broker of the
// cluster takes it. A token the broker does not take is refused, as is a
// target without TLS, a broker without OAUTHBEARER, a listener without
// SASL, and a login with no region known.
func TestAKafkaScrapeLogsInToMSKWithIAM(t *testing.T) {
	k := kafkaCluster3(t, true)
	var region atomic.Value
	region.Store("eu-west-1")
	var refused atomic.Value
	k.Edit(func(c *kafkatest.Cluster) {
		c.Mechanisms = []string{"OAUTHBEARER"}
		c.CheckToken = func(token string) string {
			why := kafkaMSKCheck(token, region.Load().(string))
			if why != "" {
				refused.Store(why)
			}
			return why
		}
	})
	iam := func(r *KafkaConfig) *model.Collector {
		return kafkaCollector(t, func(c *model.Collector, own *KafkaConfig) {
			*own = *r
			own.SASLMechanism = kafkaAWSMSKIAM
			c.Request.TLS.CAFile = k.Brokers[0].CAFile
		})
	}
	target := "tls://" + k.Brokers[0].Addr
	kafkaAWSEnvironment(t, "us-east-2")
	doc, err := kafkaDoc(t, iam(&KafkaConfig{AWSRegion: "eu-west-1"}), target)
	if err != nil || len(doc.Brokers) != 3 || len(doc.Errors) != 1 {
		t.Fatalf("logged in with IAM: %v, %q, refused: %v", err, doc.Errors, refused.Load())
	}
	region.Store("us-east-2")
	if _, err := kafkaDoc(t, iam(&KafkaConfig{}), target); err != nil {
		t.Errorf("the SDK's region: %v, refused: %v", err, refused.Load())
	}
	region.Store("ap-south-1")
	if _, err := kafkaDoc(t, iam(&KafkaConfig{AWSRegion: "eu-west-1"}), target); err == nil || !strings.Contains(err.Error(), `refused the AWS_MSK_IAM login of the AWS credentials: {"status":"invalid_token"}`) {
		t.Errorf("a token not taken: %v", err)
	}
	if _, err := kafkaDoc(t, iam(&KafkaConfig{AWSRegion: "eu-west-1"}), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "MSK takes over TLS only: write the target tls://host:9098") {
		t.Errorf("a target without TLS: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) { c.Mechanisms = []string{"SCRAM-SHA-512"} })
	if _, err := kafkaDoc(t, iam(&KafkaConfig{AWSRegion: "eu-west-1"}), target); err == nil || !strings.Contains(err.Error(), "does not take the SASL mechanism OAUTHBEARER, which AWS_MSK_IAM logs in with (UNSUPPORTED_SASL_MECHANISM); it takes SCRAM-SHA-512") {
		t.Errorf("a listener without OAUTHBEARER: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) { c.Mechanisms = nil })
	if _, err := kafkaDoc(t, iam(&KafkaConfig{AWSRegion: "eu-west-1"}), target); err == nil || !strings.Contains(err.Error(), "takes no SASL login on this listener (ILLEGAL_SASL_STATE), as MSK's listeners without IAM access control do: reach port 9098") {
		t.Errorf("a listener without SASL: %v", err)
	}
	kafkaAWSEnvironment(t, "")
	if _, err := kafkaDoc(t, iam(&KafkaConfig{}), target); err == nil || !strings.Contains(err.Error(), "no AWS region is known for the AWS_MSK_IAM login; set request.aws_region, or AWS_REGION") {
		t.Errorf("no region: %v", err)
	}
}

// The region of an AWS_MSK_IAM login is request.aws_region, else that of
// the broker's MSK name, provisioned or serverless, else none, for the
// SDK's; and the keys are checked when the configuration loads.
func TestTheRegionOfAnMSKLogin(t *testing.T) {
	c := kafkaCollector(t, func(_ *model.Collector, r *KafkaConfig) { r.SASLMechanism = kafkaAWSMSKIAM })
	for host, want := range map[string]string{
		"b-1.prod.abc123.c2.kafka.eu-west-1.amazonaws.com":         "eu-west-1",
		"B-2.Prod.ABC123.C2.Kafka.US-EAST-1.amazonaws.com.":        "us-east-1",
		"boot-abc.c1.kafka-serverless.ap-south-1.amazonaws.com":    "ap-south-1",
		"kafka.internal.example.com":                               "",
		"b-1.prod.abc123.c2.kafka.eu-west-1.amazonaws.com.evil.io": "",
	} {
		if got := kafkaAWSRegion(c, host); got != want {
			t.Errorf("%s: region %q, want %q", host, got, want)
		}
	}
	if got := kafkaAWSRegion(kafkaCollector(t, func(_ *model.Collector, r *KafkaConfig) { r.SASLMechanism, r.AWSRegion = kafkaAWSMSKIAM, "eu-north-1" }), "b-1.x.c2.kafka.eu-west-1.amazonaws.com"); got != "eu-north-1" {
		t.Errorf("request.aws_region: %q", got)
	}
	for want, edit := range map[string]func(*model.Collector, *KafkaConfig){
		"sets request.sasl_mechanism AWS_MSK_IAM beside request.username": func(c *model.Collector, r *KafkaConfig) {
			r.SASLMechanism, c.Request.Username, c.Request.PasswordFile = kafkaAWSMSKIAM, "u", "/p"
		},
		"sets request.sasl_mechanism AWS_MSK_IAM beside": func(c *model.Collector, r *KafkaConfig) {
			r.SASLMechanism, c.Request.BearerTokenFile = kafkaAWSMSKIAM, "/t"
		},
	} {
		own := KafkaConfig{}
		c := &model.Collector{Name: "kafka", Request: model.RequestConfig{Type: RequestTypeKafka}}
		edit(c, &own)
		c.Request.Own = own
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
}
