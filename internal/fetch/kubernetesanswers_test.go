//go:build !select_request_types || request_type_kubernetes

package fetch

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What the API server said, and where it is, are the operator's: a probe's
// caller is told the listing, the status and what to do about it, and that
// the rest is in the exporter's log, of a refusal; that the answer is not a
// list, without the server's URL; and that the request failed, without the
// URL and the address the exporter connected from, of a server it could
// not reach. The error itself, which the log and a debug report show, has
// it all.
func TestAKubernetesListingsCallerIsNotToldWhatTheAPIServerSaid(t *testing.T) {
	s := kubeAPI(t)
	s.Forbid("/apis/apps/v1/namespaces/monitoring/deployments")
	_, err := listDoc(t, kubernetesCollector(t, "apps", "deployments"))
	if err == nil || !strings.Contains(err.Error(), `User "system:serviceaccount:monitoring:exporter" cannot list`) {
		t.Fatalf("forbidden: %v, want the API server's message in the error", err)
	}
	if told, want := model.CallerText(err), `listing deployments (apps/v1) in the namespace monitoring answered 403 Forbidden; what it said is in the exporter's log; the exporter's service account may not list them: grant it list on deployments in group "apps"`; !strings.HasPrefix(told, want) || strings.Contains(told, "system:serviceaccount") {
		t.Errorf("forbidden: the caller is told %q, want it to start %q", told, want)
	}

	s.Answer("/api/v1/namespaces/monitoring/pods", `{"kind": "PodList", "items": "none"}`)
	_, err = listDoc(t, kubernetesCollector(t, "", "pods"))
	if err == nil || !strings.Contains(err.Error(), s.URL) {
		t.Fatalf("not a list: %v, want the server's URL in the error", err)
	}
	if told := model.CallerText(err); !strings.HasPrefix(told, "listing pods: the answer is not a list: json: cannot unmarshal") || strings.Contains(told, "127.0.0.1") {
		t.Errorf("not a list: the caller is told %q", told)
	}

	refusing := testutil.RefusingAddress(t)
	c := kubernetesCollector(t, "", "pods", func(c *model.Collector) {
		editKubernetes(c, func(r *KubernetesConfig) {
			r.APIServer = "https://" + refusing
			r.Namespaces = []string{"shop"}
		})
		c.Request.TLS.CAFile = filepath.Join(s.Dir, "ca.crt")
		c.Request.BearerTokenFile = filepath.Join(s.Dir, "token")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = FetchCollector(ctx, "", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), refusing) {
		t.Fatalf("unreachable: %v, want the server's address in the error", err)
	}
	if told := model.CallerText(err); told != "listing pods: the request to the API server failed; why is in the exporter's log" {
		t.Errorf("unreachable: the caller is told %q", told)
	}
}
