//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A target that answers in windows-1251 is reported with the Content-Type it
// sent and its body's size as sent; the body, whose bytes a UTF-8 report
// cannot show, is shown as the same text, and the report says what it was
// converted from. The rules read the converted text all the same.
func TestADebugProbeShowsTheResponseAsTheTargetSentIt(t *testing.T) {
	testutil.CaptureLogs(t)
	sent := windows1251(`{"up":1,"name":"Привет"}`)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=windows-1251")
		_, _ = w.Write(sent)
	}))
	t.Cleanup(target.Close)
	c := debugCollector("dbg")
	c.Metrics[0].Labels = []model.LabelRule{{Name: "name", Expression: ".name"}}
	server := modeServer(t, c)
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, body,
		"Content-Type: application/json; charset=windows-1251",
		"Body: 24 bytes in windows-1251, converted to UTF-8 before decoding and shown here as UTF-8",
		`    {"up":1,"name":"Привет"}`,
		`demo_up{name="Привет"} 1`,
	)
	if strings.Contains(body, "charset=utf-8") {
		t.Errorf("the report shows a Content-Type the target did not send:\n%s", body)
	}
}

// A body that is UTF-8 already is not converted, and the report says
// nothing of a conversion and shows the Content-Type as the target wrote it,
// which the decode normalizes for the rules.
func TestADebugProbeSaysNothingOfAConversionThatDidNotHappen(t *testing.T) {
	testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		_, _ = w.Write([]byte(`{"up":1}`))
	}))
	t.Cleanup(target.Close)
	server := modeServer(t, debugCollector("dbg"))
	server.SetProbeDebug(true)
	body := debugProbeGet(t, server, "collector=dbg&debug=true&target="+url.QueryEscape(target.URL)).Body.String()
	assertContains(t, body, "Content-Type: application/json;charset=UTF-8", "Body: 8 bytes\n")
	if strings.Contains(body, "converted") {
		t.Errorf("the report tells of a conversion:\n%s", body)
	}
}
