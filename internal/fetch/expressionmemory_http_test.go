//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
