//go:build !select_request_types || request_type_http

package allocator

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A heartbeat a browser marks as made by another site's page, or another
// origin of the same site, is refused with 403, with a token and without:
// a form posted from such a page would otherwise add a member, and name
// where Prometheus sends its probes. A GET so marked is answered, since it
// changes nothing and the exporter's status page links to the allocator's
// view of its member; and a client that is no browser passes.
func TestTheAllocatorRefusesRequestsOfOtherSitesPages(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(5), "")
	a := New(mustLoad(t, p), Options{Clock: newFakeClock(), Logger: testutil.QuietLogger(t)})
	url := serve(t, a, "")
	heartbeat := `{"member":"evil","advertise":"203.0.113.9:80","instance":"i","sequence":1}`
	for _, site := range []string{"cross-site", "same-site", "Cross-Site"} {
		resp, body := post(t, url+"/v1/heartbeat", heartbeat, "Sec-Fetch-Site", site, "Content-Type", "text/plain")
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "page of another site") {
			t.Errorf("a heartbeat marked %s: got %s %s, want 403", site, resp.Status, body)
		}
		for _, path := range []string{"/", "/?member=r1", "/metrics", "/v1/status"} {
			if resp, _ := get(t, url+path, "Sec-Fetch-Site", site); resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s marked %s: got %s, want 200", path, site, resp.Status)
			}
		}
	}
	if live := a.Status().Members.Live; live != 0 {
		t.Errorf("the refused heartbeats made %d live members", live)
	}
	for _, site := range []string{"same-origin", "none"} {
		if resp, _ := post(t, url+"/v1/heartbeat", `{"member":"own","advertise":"10.0.0.2:9115","instance":"i","sequence":1}`, "Sec-Fetch-Site", site); resp.StatusCode != http.StatusOK {
			t.Errorf("a heartbeat marked %q: got %s, want 200", site, resp.Status)
		}
	}
	if resp, _ := post(t, url+"/v1/heartbeat", heartbeat); resp.StatusCode != http.StatusOK {
		t.Errorf("a heartbeat of a client that is no browser: got %s", resp.Status)
	}
	// With a token the refusal comes before it is asked for.
	if resp, _ := post(t, serve(t, a, "secret")+"/v1/heartbeat", heartbeat, "Sec-Fetch-Site", "cross-site"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("with a token: got %s, want 403", resp.Status)
	}
}
