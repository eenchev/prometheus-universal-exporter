//go:build !select_request_types || request_type_s3file

package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An aws store reads a secret from AWS Secrets Manager with its
// GetSecretValue call, naming the secret by its name or its ARN. Its
// SecretString, or its SecretBinary's bytes, is the secret's text.
//
// The credentials are those the AWS SDK's default chain finds, as for an
// s3file collector: the environment, a role for the service account (IRSA),
// EKS Pod Identity, or the instance's role. The SDK's configuration and
// credential providers are the modules the s3file request type links. The
// request is signed with the SDK's signer and sent as it is, so the store
// adds no module to the binary. It is in the builds that have s3file and in
// no other.

type awsBackend struct {
	region, endpoint string
	client           *http.Client

	mu  sync.Mutex
	cfg *aws.Config
}

func init() {
	registerSecretStoreType(&SecretStoreType{
		Name:     "aws",
		Keys:     awsStoreKeys,
		Check:    func(e *model.SecretStore) error { return checkSecretStoreURL("endpoint", e.Endpoint) },
		New:      newAWSBackend,
		KeyRules: awsStoreKeyRules,
	})
}

func newAWSBackend(e model.SecretStore, timeout time.Duration) (SecretBackend, error) {
	client, err := secretClient(model.TLSConfig{}, timeout)
	if err != nil {
		return nil, err
	}
	return &awsBackend{region: e.Region, endpoint: strings.TrimRight(e.Endpoint, "/"), client: client}, nil
}

// configuration is the SDK's configuration, loaded once: the region and
// the provider of the credentials, which keeps them until they expire.
func (b *awsBackend) configuration(ctx context.Context) (*aws.Config, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg != nil {
		return b.cfg, nil
	}
	var options []func(*config.LoadOptions) error
	if b.region != "" {
		options = append(options, config.WithRegion(b.region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("loading the AWS SDK's configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("no AWS region is known; set the store's region, or AWS_REGION")
	}
	b.cfg = &cfg
	return b.cfg, nil
}

func (b *awsBackend) Read(ctx context.Context, ref SecretReference) (SecretDocument, error) {
	cfg, err := b.configuration(ctx)
	if err != nil {
		return SecretDocument{}, err
	}
	credentials, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return SecretDocument{}, fmt.Errorf("finding AWS credentials: %w", err)
	}
	ask := map[string]string{"SecretId": ref.Path}
	switch {
	case strings.HasPrefix(ref.Version, "AWS"):
		ask["VersionStage"] = ref.Version
	case ref.Version != "":
		ask["VersionId"] = ref.Version
	}
	payload, err := json.Marshal(ask)
	if err != nil {
		return SecretDocument{}, err
	}
	endpoint := b.endpoint
	if endpoint == "" {
		domain := "amazonaws.com"
		if strings.HasPrefix(cfg.Region, "cn-") {
			domain = "amazonaws.com.cn"
		}
		endpoint = "https://secretsmanager." + cfg.Region + "." + domain
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/", bytes.NewReader(payload))
	if err != nil {
		return SecretDocument{}, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager.GetSecretValue")
	hash := sha256.Sum256(payload)
	if err := v4.NewSigner().SignHTTP(ctx, credentials, req, hex.EncodeToString(hash[:]), "secretsmanager", cfg.Region, time.Now()); err != nil {
		return SecretDocument{}, fmt.Errorf("signing the request: %w", err)
	}
	status, body, err := secretHTTP(b.client, req)
	if err != nil {
		return SecretDocument{}, err
	}
	if status != http.StatusOK {
		var refusal struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Upper   string `json:"Message"`
		}
		if json.Unmarshal(body, &refusal) == nil && refusal.Type != "" {
			return SecretDocument{}, fmt.Errorf("secrets manager answered %d: %s", status, secretStoreSaid([]byte(strings.TrimSpace(refusal.Type+" "+refusal.Message+refusal.Upper))))
		}
		return SecretDocument{}, fmt.Errorf("secrets manager answered %d: %s", status, secretStoreSaid(body))
	}
	var answer struct {
		SecretString *string `json:"SecretString"`
		SecretBinary string  `json:"SecretBinary"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return SecretDocument{}, errors.New("secrets manager's answer is not the JSON of a secret")
	}
	if answer.SecretString != nil {
		return SecretDocument{Text: *answer.SecretString}, nil
	}
	binary, err := base64.StdEncoding.DecodeString(answer.SecretBinary)
	if err != nil || len(binary) == 0 {
		return SecretDocument{}, errors.New("secrets manager's answer has neither a SecretString nor a SecretBinary")
	}
	return SecretDocument{Text: string(binary)}, nil
}
