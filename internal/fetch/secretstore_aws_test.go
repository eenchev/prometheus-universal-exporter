//go:build !select_request_types || request_type_s3file

package fetch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An aws store asks Secrets Manager for a secret's value with a request
// signed for the service and the region with the credentials the SDK's
// chain finds; the SecretString is the credential, or, with a field, a
// member of the JSON object it is, and ?version= names a stage or a
// version id.
func TestACredentialIsReadFromAWSSecretsManager(t *testing.T) {
	var mu sync.Mutex
	var asked []map[string]string
	aws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ask map[string]string
		_ = json.Unmarshal(body, &ask)
		mu.Lock()
		asked = append(asked, ask)
		mu.Unlock()
		auth := r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.Header.Get("X-Amz-Target") != "secretsmanager.GetSecretValue" || r.Header.Get("Content-Type") != "application/x-amz-json-1.1" ||
			!strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auth, "/eu-west-1/secretsmanager/aws4_request") || r.Header.Get("X-Amz-Security-Token") != "session" {
			http.Error(w, `{"__type":"InvalidSignatureException","message":"not signed"}`, http.StatusBadRequest)
			return
		}
		switch {
		case ask["SecretId"] == "prod/db" && ask["VersionStage"] == "AWSPREVIOUS":
			_, _ = io.WriteString(w, `{"Name":"prod/db","SecretString":"{\"password\":\"before\"}"}`)
		case ask["SecretId"] == "prod/db":
			_, _ = io.WriteString(w, `{"Name":"prod/db","SecretString":"{\"username\":\"monitor\",\"password\":\"p4ss\"}"}`)
		case ask["SecretId"] == "prod/token":
			_, _ = io.WriteString(w, `{"Name":"prod/token","SecretBinary":"dG9rLTEyMwo="}`)
		default:
			http.Error(w, `{"__type":"ResourceNotFoundException","Message":"Secrets Manager can't find the specified secret."}`, http.StatusBadRequest)
		}
	}))
	t.Cleanup(aws.Close)
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_SESSION_TOKEN": "session", "AWS_REGION": "",
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null", "AWS_PROFILE": "",
	} {
		t.Setenv(name, value)
	}
	storesInForce(t, model.SecretStore{Name: "aws", Type: "aws", Region: "eu-west-1", Endpoint: aws.URL})
	for ref, want := range map[string]string{
		"secret://aws/prod/db#password":                     "p4ss",
		"secret://aws/prod/db?version=AWSPREVIOUS#password": "before",
		"secret://aws/prod/token":                           "tok-123",
	} {
		if got, err := ReadCredentialFile(ref); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := ReadCredentialFile("secret://aws/prod/none"); err == nil || !strings.Contains(err.Error(), "secrets manager answered 400: ResourceNotFoundException Secrets Manager can't find the specified secret.") {
		t.Errorf("err=%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 4 {
		t.Errorf("%d requests, want one for each secret and version: %v", len(asked), asked)
	}
}
