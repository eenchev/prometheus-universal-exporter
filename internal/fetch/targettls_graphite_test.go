//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A graphite static target's own tls block replaces the collector's whole,
// as an http target's does: a render API that takes the target's client
// certificate alone, and refuses the collector's, answers the target. The
// block is checked at load as the collector's is, and refused beside the
// target's insecure_skip_verify.
func TestAGraphiteStaticTargetsOwnTLSBlockIsTheOneItsRequestIsMadeWith(t *testing.T) {
	collectorCert, collectorKey := selfSigned(t, "collector-identity")
	targetCert, targetKey := selfSigned(t, "target-identity")
	server, ca := identityServer(t, func(w http.ResponseWriter, _ *http.Request, presented string) {
		_, _ = io.WriteString(w, `[{"target":"`+presented+`","datapoints":[[1,1700000000]]}]`)
	}, targetCert)
	c := graphiteCollector("app.requests")
	c.Request.TLS = model.TLSConfig{CAFile: ca, CertFile: collectorCert, KeyFile: collectorKey}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	target := &model.StaticTarget{Name: "x", Collector: c.Name, Target: server}
	if _, err := FetchCollector(context.Background(), server, &c, TargetOverrides(target), nil); err == nil {
		t.Fatal("the collector's certificate was taken")
	}
	target.Request.TLS = &model.TLSConfig{CAFile: ca, CertFile: targetCert, KeyFile: targetKey, Set: true}
	if err := CheckTargetRequest(target, &c); err != nil {
		t.Fatal(err)
	}
	response, err := FetchCollector(context.Background(), server, &c, TargetOverrides(target), nil)
	if err != nil || response.StatusCode != http.StatusOK || string(response.Body) != `[{"target":"target-identity","datapoints":[[1,1700000000]]}]` {
		t.Fatalf("with the target's own block: %+v, %v; want the target's certificate presented", response, err)
	}
	skip := true
	checkTargetRequests(t, &c, map[string]struct {
		target  string
		request model.TargetRequestConfig
		want    string
	}{
		"an empty tls block":          {server, model.TargetRequestConfig{TLS: &model.TLSConfig{Set: true}}, ""},
		"half a client certificate":   {server, model.TargetRequestConfig{TLS: &model.TLSConfig{CertFile: targetCert, Set: true}}, "only one of cert_file and key_file"},
		"insecure_skip_verify beside": {server, model.TargetRequestConfig{TLS: &model.TLSConfig{Set: true}, InsecureSkipVerify: &skip}, "beside a request.tls block of its own"},
	})
}

// --dry-run reads a graphite static target's own tls files, as its first
// request would, under the target's name.
func TestAGraphiteStaticTargetsFilesAreReadByTheDryRun(t *testing.T) {
	c := graphiteCollector("app.requests")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	checkTargetFilesAreRead(t, &c, false)
}
