//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A graphite collector's request is made as an http collector's is, so its
// answer carries the timings of the request too.
func TestAGraphiteAnswerHasTheTimingsOfItsRequest(t *testing.T) {
	render := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	defer render.Close()
	c := graphiteCollector("app.cpu")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	response, err := FetchCollector(context.Background(), render.URL, &c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if trip := response.Trip; trip == nil || trip.Connect == nil || trip.FirstByte == nil || trip.TLS != nil || trip.Total <= 0 {
		t.Errorf("trip %+v", trip)
	}
}
