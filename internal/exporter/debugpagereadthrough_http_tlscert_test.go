//go:build !select_request_types || (request_type_http && request_type_tlscert)

package exporter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A collector run on what another fetches reads it as its own request type
// reads an answer, so it is given what a collector of its type fetched
// alone: a copy of a tlscert collector reads what that collector's static
// target answers, and an http playground is refused it, before anything is
// fetched, as a tlscert one is refused what an http collector fetches.
func TestTheDebugPageReadsWhatACollectorOfItsOwnTypeFetches(t *testing.T) {
	certs := model.Collector{
		Name: "certs", Request: model.RequestConfig{Type: "tlscert"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "tls_verified", Type: model.GaugeMetricType, Expression: "if .verified then 1 else 0 end"}},
	}
	file := &model.StaticTargetFile{Interval: model.Duration(2 * time.Second), Targets: []model.StaticTarget{
		{Name: "closed", Collector: "certs", Target: "127.0.0.1:1"},
		{Name: "rooms_eu", Collector: "rooms", Target: "http://127.0.0.1:1"},
	}}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms"), certs}}, file)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	copied := "name: certs\nrequest:\n  type: tlscert\ntransform:\n  type: jq\nmetrics:\n  - name: tls_verified\n    expression: 'if .verified then 1 else 0 end'\n"
	// Nothing listens on port 1: the handshake fails, as the target's own
	// scrape would, and is answered as a scrape's failure.
	answer := postRun(t, server, map[string]any{"collector": copied, "static_target": "closed"}, 200)
	if answer.OK || !strings.HasPrefix(answer.Verdict, "target up 0") || !strings.Contains(answer.Report, `scraped through collector "certs"`) {
		t.Errorf("the copy on certs' static target: %+v", answer)
	}
	answer = postRun(t, server, map[string]any{"collector": copied, "request_from": "certs", "target": "127.0.0.1:1"}, 200)
	if answer.OK || !strings.Contains(answer.Report, `fetched through collector "certs"`) {
		t.Errorf("the copy through certs: %+v", answer)
	}
	for run, want := range map[string]map[string]any{
		`static target "closed" is scraped through collector "certs": collector "example" reads what a collector of its request type, http, fetches only, and collector "certs" is of request type tlscert`: {"collector": debugExample, "static_target": "closed"},
		`request_from: collector "example" reads what a collector of its request type, http, fetches only, and collector "certs" is of request type tlscert`:                                                {"collector": debugExample, "request_from": "certs", "target": "127.0.0.1:1"},
		`request_from: collector "certs" reads what a collector of its request type, tlscert, fetches only, and collector "rooms" is of request type http`:                                                  {"collector": copied, "request_from": "rooms", "target": "http://127.0.0.1:1"},
		`static target "rooms_eu" is scraped through collector "rooms": collector "certs" reads what a collector of its request type, tlscert, fetches only`:                                                {"collector": copied, "static_target": "rooms_eu"},
	} {
		if answer := postRun(t, server, want, 400); !strings.Contains(answer.Error, run) {
			t.Errorf("%v answered %q, want %q", want, answer.Error, run)
		}
	}
}

// The page holds the form of a playground of each request type the build
// has, as the type's registration says it, and follows the request.type the
// editor holds with them: a tlscert playground is asked for the handshake's
// JSON document alone, with no Content-Type, status or headers, which an
// http one is asked for.
func TestTheDebugPageHoldsTheFormOfAPlaygroundOfEachType(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	page := getPage(t, server, "/debug")
	start := strings.Index(page, `<script type="application/json" id="forms">`)
	end := strings.Index(page[max(start, 0):], "</script>")
	if start < 0 || end < 0 {
		t.Fatalf("the page holds no forms")
	}
	var held debugForms
	if err := json.Unmarshal([]byte(page[start+len(`<script type="application/json" id="forms">`):start+end]), &held); err != nil {
		t.Fatal(err)
	}
	for _, name := range fetch.BuiltRequestTypes() {
		if want := debugFormOf(&model.Collector{Request: model.RequestConfig{Type: name}}); !reflect.DeepEqual(held.Types[name], want) {
			t.Errorf("the form of a %s playground is %+v, want %+v", name, held.Types[name], want)
		}
	}
	if len(held.Types) != len(fetch.BuiltRequestTypes()) {
		t.Errorf("the page holds the forms of %d types, want %d", len(held.Types), len(fetch.BuiltRequestTypes()))
	}
	if tls := held.Types["tlscert"]; tls.ContentType || tls.HTTPAnswer || tls.Answer != "Paste the handshake as the JSON document the exporter makes of it, as the Response of a run shows it" || tls.TargetLabel != "Server" {
		t.Errorf("the form of a tlscert playground is %+v", tls)
	}
	if web := held.Types["http"]; !web.ContentType || !web.HTTPAnswer || !reflect.DeepEqual(held.Playground, web) {
		t.Errorf("the form of an http playground is %+v, the example's %+v", web, held.Playground)
	}
	requireContains(t, debugPageScript(page),
		`const playgroundForm = () => forms.types[writtenType] || forms.playground;`,
		`const readerForm = () => $("configured").value !== "" ? formOf($("configured").value) : playgroundForm();`,
		`typeFrame = requestAnimationFrame(() => {`,
		`if (now !== writtenType) { writtenType = now; showForm(); }`,
		`$("status-part").hidden = !reads.http_answer;`,
		`$("headers-part").hidden = !reads.http_answer;`,
		`$("body").placeholder = reads.answer;`,
		`request.status = reads.http_answer ? Number($("status").value) || 0 : 0;`,
		`request.headers = reads.http_answer ? filled(headers, "header", (name) => name.toLowerCase()) : {};`)
}
