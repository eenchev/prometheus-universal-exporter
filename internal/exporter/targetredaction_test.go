package exporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A target is shown with the values of its credential-named query parameters
// withheld — in the log, the static targets endpoint's target label and the
// OTLP target attribute — and the rest kept, so targets that differ in
// anything else stay apart.

func TestAShownTargetWithholdsItsQueryCredentials(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	up := textTarget(t, "value=1\n")
	otlp := otlpConfig("http://collector.invalid/v1/metrics")
	otlp.ProbeAttributes = true
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlp}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "t", Collector: "text", Target: up.URL + "/?api_key=s3cret-static&tenant=a"},
	}}
	server := newStaticServer(t, cfg, file)

	// Two probes whose targets differ in tenant stay two OTLP series; the
	// token is in neither.
	for _, tenant := range []string{"a", "b"} {
		if r := probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape(up.URL+"/?token=s3cret-probe&tenant="+tenant), nil); r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
	}
	targets := map[string]bool{}
	for _, p := range pendingPoints(server) {
		targets[p.Labels["target"]] = true
	}
	for _, tenant := range []string{"a", "b"} {
		if want := up.URL + "/?token=<redacted>&tenant=" + tenant; !targets[want] {
			t.Errorf("no OTLP point for %s in %v", want, targets)
		}
	}

	server.scrapeStaticTargets(context.Background(), 10*time.Second)
	body := getStaticTargets(t, server, "/static-targets")
	if !strings.Contains(body, `target="`+up.URL+`/?api_key=<redacted>&tenant=a"`) {
		t.Errorf("the target label is not withheld:\n%s", body)
	}

	// A failing probe's log line.
	probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape("http://127.0.0.1:1/?token=s3cret-down&tenant=a"), nil)
	for what, text := range map[string]string{"log": logs.String(), "endpoint": body} {
		if strings.Contains(text, "s3cret") {
			t.Errorf("the %s holds a credential:\n%s", what, text)
		}
	}
	if !strings.Contains(logs.String(), `token=<redacted>&tenant=a`) {
		t.Errorf("the failure is not logged with its target:\n%s", logs)
	}
}

// Two targets that differ only in a credential's value are shown alike, but
// the failure log still tells them apart: each one's first failure is
// logged, rather than the second read as a repeat of the first.
func TestTargetsShownAlikeFailApart(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}, nil)
	for _, token := range []string{"one", "two"} {
		probeOnce(t, server, "/probe?collector=text&target="+url.QueryEscape("http://127.0.0.1:1/?token="+token), nil)
	}
	if n := strings.Count(logs.String(), `"msg":"probe failed"`); n != 2 {
		t.Fatalf("%d first failures logged, want 2:\n%s", n, logs)
	}
}

// Probes of one target that differ in their parameters fail apart: a broken
// path does not make a healthy one read as recovering on every scrape, and
// the log line says which request failed.
func TestProbesDifferingInParametersFailApart(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("value=1\n"))
	}))
	t.Cleanup(target.Close)
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}, nil)
	for range 5 {
		for _, path := range []string{"/good", "/bad"} {
			probeOnce(t, server, "/probe?collector=text&path="+path+"&target="+url.QueryEscape(target.URL), nil)
		}
	}
	if failed, recovered := strings.Count(logs.String(), `"msg":"probe failed"`), strings.Count(logs.String(), `"msg":"probe recovered"`); failed != 1 || recovered != 0 {
		t.Fatalf("%d failures and %d recoveries logged, want 1 and 0:\n%s", failed, recovered, logs)
	}
	if !strings.Contains(logs.String(), `"url":"`+target.URL+`/bad"`) {
		t.Fatalf("the failure does not say which request failed:\n%s", logs)
	}
}
