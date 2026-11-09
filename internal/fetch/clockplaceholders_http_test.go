//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A clock placeholder's format is checked where it stands when the
// configuration loads, as a default is: a date is no number.
func TestAClockPlaceholderItsPlaceCannotWriteIsRefusedAtLoad(t *testing.T) {
	f := templateField{where: "request.body", text: `{"day": {{date|number}}}`, kind: "body"}
	if err := f.check(); err == nil || !strings.Contains(err.Error(), "the clock placeholder writes values such as that one") {
		t.Fatalf("got %v", err)
	}
	if err := (templateField{where: "request.body", text: `{"at": {{unix|number}}}`, kind: "body"}).check(); err != nil {
		t.Fatal(err)
	}
}

// A collector's clock placeholders are filled at every scrape from the
// clock of that scrape, in the path, a header, a query value and the body,
// each written as its place writes a value; and no probe parameter is asked
// for, listed or taken for them.
func TestAClockPlaceholderIsFilledAtEveryScrape(t *testing.T) {
	type seen struct{ uri, header, body string }
	requests := make(chan seen, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- seen{r.RequestURI, r.Header.Get("X-Day"), string(body)}
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	c := httpCollector(t, func(c *model.Collector) {
		c.Request.Path = "/daily/{{date_us}}/{{param_user:me}}"
		c.Request.Headers = map[string]string{"X-Day": "{{date_eu:-1d}}"}
		c.Request.Query = map[string]string{"from": "{{datetime:-1d/d@Europe/Sofia}}"}
		c.Request.Own = HTTPConfig{Method: http.MethodPost, Body: `{"at": {{unix_ms|number}}, "day": {{date|json}}}`}
	})
	if params := RequestParams(c); len(params) != 1 || params[0].Name != "param_user" {
		t.Fatalf("the probe parameters are %+v", params)
	}
	if unused, err := CheckRequestParams(c, RequestOverrides{}); err != nil || len(unused) != 0 {
		t.Fatalf("a probe with no parameters: %v, %v", unused, err)
	}
	if err := CheckPathParams(c, RequestOverrides{Params: map[string]string{"date": "2020-01-01"}}); err != nil {
		t.Fatalf("a probe parameter that is no param_ one is no business of the request: %v", err)
	}
	for _, now := range []time.Time{
		time.Date(2026, time.November, 9, 22, 30, 0, 0, time.UTC),
		time.Date(2026, time.November, 10, 8, 0, 0, 0, time.UTC),
	} {
		atClock(t, now)
		if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		got := <-requests
		day := now.Format("2006-01-02")
		from := time.Date(now.In(sofia(t)).Year(), now.In(sofia(t)).Month(), now.In(sofia(t)).Day()-1, 0, 0, 0, 0, sofia(t)).Format(time.RFC3339)
		want := seen{
			uri:    "/daily/" + strings.ReplaceAll(now.Format("01/02/2006"), "/", "%2F") + "/me?from=" + strings.ReplaceAll(strings.ReplaceAll(from, ":", "%3A"), "+", "%2B"),
			header: now.AddDate(0, 0, -1).Format("02.01.2006"),
			body:   `{"at": ` + itoa(now.UnixMilli()) + `, "day": "` + day + `"}`,
		}
		if got != want {
			t.Fatalf("at %s the target was sent\n%+v\nwant\n%+v", now, got, want)
		}
	}
}

// A clock placeholder stands in a request, not in a label value.
func TestAClockPlaceholderInALabelValueIsRefused(t *testing.T) {
	c := &model.Collector{Name: "web", Transform: model.TransformConfig{Labels: map[string]string{"day": "{{date}}"}}}
	_, err := ParseLabelParams(c, func(int) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "not in a label value") {
		t.Fatalf("got %v", err)
	}
}

func sofia(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Sofia")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
