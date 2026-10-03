//go:build !select_request_types || request_type_graphite

package fetch

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// A graphite collector's request is an HTTP request, held to the same rule
// for its host: a render API named with a character outside ASCII is
// refused before anything is sent, as a target and as the host a redirect
// leads to. "ｌｏｃａｌｈｏｓｔ", in full-width letters, was judged as localhost and
// requested there, with a Host that was another name.
func TestAGraphiteHostOutsideASCIIIsRefused(t *testing.T) {
	var asked atomic.Int32
	var location atomic.Pointer[string]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		if to := location.Load(); to != nil {
			w.Header().Set("Location", *to)
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	c := graphiteCollector("app.count")
	c.Request.FollowRedirects = true
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	elsewhere := strings.Replace(server.URL, "127.0.0.1", "ｌｏｃａｌｈｏｓｔ", 1)
	refused := func(name string, err error, requests int32) {
		t.Helper()
		if !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "target ｌｏｃａｌｈｏｓｔ refused: it has U+FF4C 'ｌ', a character outside ASCII: write an internationalised name in its ASCII form") {
			t.Errorf("%s: err=%v, want the host refused for a character outside ASCII", name, err)
		}
		if got := asked.Swap(0); got != requests {
			t.Errorf("%s: the server was sent %d requests, want %d", name, got, requests)
		}
	}
	_, err := FetchCollector(context.Background(), elsewhere, &c, RequestOverrides{}, nil)
	refused("as the target", err, 0)
	to := elsewhere + "/render"
	location.Store(&to)
	_, err = FetchCollector(context.Background(), server.URL, &c, RequestOverrides{}, nil)
	refused("as a redirect's host", err, 1)
}
