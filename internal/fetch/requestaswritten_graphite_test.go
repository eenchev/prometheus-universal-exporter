//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A graphite target's own query is kept as it was written, with request.query
// and then the type's parameters after it; a pair of the target's that names
// one of the type's own — target, from, until or format — is dropped, so a
// probe's target adds no expression to the collector's and changes neither
// its window nor its format.
func TestAGraphiteTargetsQueryCannotSetTheTypesParameters(t *testing.T) {
	c := graphiteCollector("app.*.count")
	c.Request.Query = map[string]string{"maxDataPoints": "10"}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	u, err := resolveRequestURL("graphite.example:8080/graphite?tenant=a%20b&target=secret.*&debug&Format=csv&from=-1y&x=1;until=-1d", &c, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "tenant=a%20b&debug&maxDataPoints=10&format=json&from=-15min&target=app.%2A.count&until=now"; u.RawQuery != want || u.Path != "/graphite/render" {
		t.Fatalf("url %s, want the query %s", u, want)
	}
}

// A default a Graphite expression cannot take, such as a glob, is refused
// when the configuration loads, as an unusable default of an http request
// is; so is a scheme request.allowed_schemes cannot hold.
func TestAGraphiteCollectorsDefaultsAndSchemesAreCheckedAtLoad(t *testing.T) {
	c := graphiteCollector("app.{{param_env:prod}}.count", "app.{{param_env:prod*}}.errors")
	err := ValidateRequest(&c)
	for _, fragment := range []string{`collector "graphite"`, "request.targets[1]", "param_env", `"prod*"`, "default"} {
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Fatalf("err=%v, want it to say %s", err, fragment)
		}
	}
	good := graphiteCollector("app.{{param_env:prod}}.count", "app.{{param_env}}.errors")
	if err := ValidateRequest(&good); err != nil {
		t.Fatal(err)
	}
	typo := graphiteCollector("app.count")
	typo.Request.AllowedSchemes = []string{"htps"}
	if err := ValidateRequest(&typo); err == nil || !strings.Contains(err.Error(), `collector "graphite" request.allowed_schemes[0] is "htps"`) {
		t.Fatalf("allowed_schemes [htps]: %v", err)
	}
}

// The largest limit that can be written reads a render API's whole answer.
func TestTheLargestResponseLimitReadsAGraphiteAnswer(t *testing.T) {
	const answer = `[{"target":"a.b","datapoints":[[1,1727000000]]}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // no Content-Length
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	c := graphiteCollector("a.b")
	c.Request.MaxResponseBytes = model.ByteSize(math.MaxInt64)
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	resp, err := FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil)
	if err != nil || string(resp.Body) != answer {
		t.Fatalf("resp=%+v err=%v, want the answer whole", resp, err)
	}
}
