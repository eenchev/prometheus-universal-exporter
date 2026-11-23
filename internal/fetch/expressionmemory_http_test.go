//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The jq expressions that run outside a transform, a pagination's on each
// page and a login's on its answers, are held to the collector's
// limits.max_expression_memory as a transform's are: one that builds a
// list of tens of millions is stopped, and the scrape fails as a limit
// exceeded, naming the key and the limit. With no limit such an
// expression runs to its end.
func TestTheExpressionsOfAPaginationAndALoginAreHeldToTheMemoryLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/step":
			_, _ = io.WriteString(w, `{"access_token":"tok"}`)
		default:
			_, _ = io.WriteString(w, `{"items":[[1]]}`)
		}
	}))
	t.Cleanup(server.Close)
	// The list held to the limit takes seconds to build, so that it is
	// stopped on a machine that is busy too, and the one that is not held
	// to it is a hundredth of that. Under the race detector, where a
	// program runs ten times as long, the first is a sixth of what it is
	// and the limit a quarter.
	held, limit := alloctest.UnlessRaced(30000000, 5000000), model.ByteSize(alloctest.UnlessRaced(8<<20, 2<<20))
	for name, tc := range map[string]struct {
		// collector is a collector one of whose expressions builds a list
		// of count numbers, and stopped is how the failure of that one
		// begins.
		collector func(count int) *model.Collector
		stopped   string
	}{
		"request.pagination.next": {func(count int) *model.Collector {
			return pagedCollector(t, model.PaginationConfig{Type: "cursor", Next: fmt.Sprintf("[range(%d)] | length | null", count), Param: "after", Items: ".items"})
		}, fmt.Sprintf(`request.pagination.next "[range(%d)] | length | null" was stopped: `, held)},
		// The items of a pagination are checked against null when it
		// loads, within five seconds: the expression builds nothing there.
		"request.pagination.items": {func(count int) *model.Collector {
			return pagedCollector(t, model.PaginationConfig{Type: "link", Items: fmt.Sprintf(".items[if . == null then 0 else [range(%d)] | length - %[1]d end]", count)})
		}, fmt.Sprintf(`request.pagination.items ".items[if . == null then 0 else [range(%d)] | length - %[1]d end]" was stopped: `, held)},
		"login.token": {func(count int) *model.Collector {
			return loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: fmt.Sprintf("[range(%d)] | length | tostring", count), FieldFiles: secretFiles(t, map[string]string{"password": "s3cret"})})
		}, fmt.Sprintf(`login.token "[range(%d)] | length | tostring" was stopped: `, held)},
		"a step's extract": {func(count int) *model.Collector {
			return loginCollector(t, model.LoginConfig{Type: "steps", Token: "token",
				Steps: []model.LoginStep{{URL: "/step", Extract: map[string]model.LoginExtract{"token": {JSON: fmt.Sprintf("[range(%d)] | length | tostring", count)}}}}})
		}, `login.steps[0].extract.token was stopped: `},
	} {
		freshLogins(t)
		c := tc.collector(held)
		c.Limits.MaxExpressionMemory = limit
		_, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		want := fmt.Sprintf("%sthe exporter's memory grew by more than limits.max_expression_memory, %d bytes, while it ran; make the expression keep less at once, or raise the limit", tc.stopped, limit)
		if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v\nwant a limit exceeded saying %s", name, err, want)
		}
		logins.reset()
		if _, err := FetchCollector(context.Background(), server.URL+"/data", tc.collector(held/100), RequestOverrides{}, nil); err != nil {
			t.Errorf("%s with no limit: %v", name, err)
		}
	}
}

// The watch of an expression's memory ends with the expression, on every
// way out of it: one that gave two values, or failed, left its watch
// looking at the heap every 20 milliseconds, and asking for collections,
// for as long as the exporter ran.
func TestTheMemoryWatchOfAnExpressionEndsWithIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[1],"more":[2],"access_token":"tok"}`)
	}))
	t.Cleanup(server.Close)
	for name, c := range map[string]*model.Collector{
		"items that name two places":   pagedCollector(t, model.PaginationConfig{Type: "link", Items: "if . == null then .items else (.items, .more) end"}),
		"items that fail":              pagedCollector(t, model.PaginationConfig{Type: "link", Items: `if . == null then .items else error("no") end`}),
		"a next that gives two values": pagedCollector(t, model.PaginationConfig{Type: "cursor", Next: "1, 2", Param: "after", Items: ".items"}),
		"a next that fails":            pagedCollector(t, model.PaginationConfig{Type: "cursor", Next: `error("no")`, Param: "after", Items: ".items"}),
		"a token that gives two values": loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: ".access_token, 2",
			FieldFiles: secretFiles(t, map[string]string{"password": "s3cret"})}),
		"a token that fails": loginCollector(t, model.LoginConfig{Type: "json", URL: "/login", Token: `error("no")`,
			FieldFiles: secretFiles(t, map[string]string{"password": "s3cret"})}),
		"expressions that give their one value": pagedCollector(t, model.PaginationConfig{Type: "cursor", Next: "null", Param: "after", Items: ".items"}),
	} {
		freshLogins(t)
		c.Limits.MaxExpressionMemory = 256 << 20
		_, err := FetchCollector(context.Background(), server.URL+"/data", c, RequestOverrides{}, nil)
		if (err == nil) != (name == "expressions that give their one value") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	// A watch begins 20 milliseconds after its expression did.
	time.Sleep(100 * time.Millisecond)
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	if n := strings.Count(string(stacks), "expr.watchMemory("); n != 0 {
		t.Errorf("%d watches of an expression's memory are still running after their expressions ended", n)
	}
}
