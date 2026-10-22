//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// pagedCollector is an http collector with the pagination p, validated.
func pagedCollector(t *testing.T, p model.PaginationConfig) *model.Collector {
	t.Helper()
	c := testutil.Collector("paged", "json")
	c.Request.Pagination = &p
	c.Limits.MaxResponseBytes = 1 << 20
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// fetchPaged fetches target with c and fails the test on an error.
func fetchPaged(t *testing.T, c *model.Collector, target string) *HTTPResponse {
	t.Helper()
	response, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// decoded is a merged body as a value, for comparing.
func decoded(t *testing.T, body []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	return v
}

// pageURLs are the URLs of the pages a response says were asked for.
func pageURLs(r *HTTPResponse) []string {
	var urls []string
	for _, p := range r.Pages.Pages {
		urls = append(urls, p.URL)
	}
	return urls
}

// type link follows the Link header's rel="next", relative or absolute, among
// other links and parameters, sends every page the collector's credentials
// and headers, records each page in the trace, and merges a top-level array.
func TestPaginationFollowsTheLinkHeader(t *testing.T) {
	var server *httptest.Server
	var authorizations sync.Map
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		authorizations.Store(page, r.Header.Get("Authorization")+" "+r.Header.Get("X-Tenant"))
		switch page {
		case "", "1":
			w.Header().Add("Link", `<`+server.URL+`/repos?page=3>; rel="last", <`+server.URL+`/repos?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":1},{"id":2}]`))
		case "2":
			w.Header().Add("Link", `</repos?page=1>; rel=prev`)
			w.Header().Add("Link", `</repos?page=3>; title="a, b; c"; rel="Next last"`)
			_, _ = w.Write([]byte(`[{"id":3}]`))
		case "3":
			w.Header().Add("Link", `</repos?page=1>; rel="first"`)
			_, _ = w.Write([]byte(`[{"id":4}]`))
		}
	}))
	t.Cleanup(server.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "link", Items: "."})
	c.Request.BearerToken = "t0ken"
	c.Request.Headers = map[string]string{"X-Tenant": "acme"}
	ctx, trace := WithRequestTrace(context.Background())
	response, err := FetchCollector(ctx, server.URL+"/repos", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decoded(t, response.Body), decoded(t, []byte(`[{"id":1},{"id":2},{"id":3},{"id":4}]`)); !reflect.DeepEqual(got, want) {
		t.Fatalf("merged %s", response.Body)
	}
	if want := []string{server.URL + "/repos", server.URL + "/repos?page=2", server.URL + "/repos?page=3"}; !reflect.DeepEqual(pageURLs(response), want) || response.Pages.MoreLeft || !response.Pages.Merged {
		t.Fatalf("pages %+v", response.Pages)
	}
	for _, page := range []string{"", "2", "3"} {
		if got, _ := authorizations.Load(page); got != "Bearer t0ken acme" {
			t.Errorf("page %q was sent %v", page, got)
		}
	}
	requests := trace.Requests()
	if len(requests) != 3 || requests[0].Page != 1 || requests[2].Page != 3 {
		t.Fatalf("traced %+v", requests)
	}
}

// type next_url reads the next page's URL from the page, relative to it; the
// merged document is the first page with its items array holding every
// page's items, its other keys the first page's, and numbers kept to the
// digit.
func TestPaginationReadsTheNextURLFromThePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hosts":
			_, _ = w.Write([]byte(`{"total":3,"data":{"hosts":[{"id":12345678901234567890}]},"links":{"next":"hosts/2?x=1"}}`))
		case "/api/hosts/2":
			_, _ = w.Write([]byte(`{"total":3,"data":{"hosts":[{"id":2},{"id":1.5e400}]},"links":{"next":"/api/hosts/3"}}`))
		case "/api/hosts/3":
			_, _ = w.Write([]byte(`{"total":3,"data":{"hosts":[]},"links":{"next":null}}`))
		}
	}))
	t.Cleanup(server.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "next_url", Next: ".links.next", Items: ".data.hosts"})
	response := fetchPaged(t, c, server.URL+"/api/hosts")
	if want := `{"data":{"hosts":[{"id":12345678901234567890},{"id":2},{"id":1.5e400}]},"links":{"next":"hosts/2?x=1"},"total":3}`; string(response.Body) != want {
		t.Fatalf("merged\n%s\nwant\n%s", response.Body, want)
	}
	if want := []string{server.URL + "/api/hosts", server.URL + "/api/hosts/2?x=1", server.URL + "/api/hosts/3"}; !reflect.DeepEqual(pageURLs(response), want) {
		t.Fatalf("pages %v", pageURLs(response))
	}
}

// type cursor sends the cursor next reads, text or a number, in param,
// replacing a parameter of that name in the target's query, and next sees
// the page's headers as $headers; a page without a cursor is the last.
func TestPaginationSendsTheCursor(t *testing.T) {
	var asked []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RawQuery)
		mu.Unlock()
		switch r.URL.Query().Get("after") {
		case "stale":
			w.Header().Set("X-Next", "abc")
			_, _ = w.Write([]byte(`{"items":[1]}`))
		case "abc":
			w.Header().Set("X-Next", "")
			_, _ = w.Write([]byte(`{"items":[2],"n":42}`))
		case "42":
			_, _ = w.Write([]byte(`{"items":[3]}`))
		}
	}))
	t.Cleanup(server.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "cursor", Next: `if ($headers["x-next"] // "") != "" then $headers["x-next"] else .n end`, Param: "after", Items: ".items"})
	response := fetchPaged(t, c, server.URL+"/list?after=stale&view=all")
	if string(response.Body) != `{"items":[1,2,3]}` {
		t.Fatalf("merged %s", response.Body)
	}
	if want := []string{"after=stale&view=all", "view=all&after=abc", "view=all&after=42"}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("asked %q, want %q", asked, want)
	}
}

// type page counts pages in param from start, 1 by default, and ends at a
// page whose items are empty or missing.
func TestPaginationCountsPages(t *testing.T) {
	for name, tc := range map[string]struct {
		start *int
		want  []string
		body  string
	}{
		"from 1": {nil, []string{"1", "2", "3"}, `{"items":["a1","a2"]}`},
		"from 0": {ptr(0), []string{"0", "1", "2", "3"}, `{"items":["a0","a1","a2"]}`},
	} {
		t.Run(name, func(t *testing.T) {
			var asked []string
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("p")
				mu.Lock()
				asked = append(asked, page)
				mu.Unlock()
				if n, _ := strconv.Atoi(page); n >= 3 {
					// The last page leaves its items out altogether.
					_, _ = w.Write([]byte(`{}`))
					return
				}
				fmt.Fprintf(w, `{"items":["a%s"]}`, page)
			}))
			t.Cleanup(server.Close)
			c := pagedCollector(t, model.PaginationConfig{Type: "page", Param: "p", Start: tc.start, Items: ".items"})
			response := fetchPaged(t, c, server.URL)
			if string(response.Body) != tc.body || !reflect.DeepEqual(asked, tc.want) {
				t.Fatalf("asked %v, merged %s", asked, response.Body)
			}
		})
	}
}

// type offset moves the offset on by the items each page held, or by size.
func TestPaginationCountsOffsets(t *testing.T) {
	rows := []int{0, 1, 2, 3, 4}
	for name, tc := range map[string]struct {
		size int
		want []string
	}{
		"by the items": {0, []string{"0", "2", "4", "5"}},
		"by size":      {3, []string{"0", "3", "6"}},
	} {
		t.Run(name, func(t *testing.T) {
			var asked []string
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				mu.Lock()
				asked = append(asked, strconv.Itoa(offset))
				mu.Unlock()
				page := rows[min(offset, len(rows)):min(offset+2, len(rows))]
				_ = json.NewEncoder(w).Encode(map[string]any{"rows": page})
			}))
			t.Cleanup(server.Close)
			c := pagedCollector(t, model.PaginationConfig{Type: "offset", Param: "offset", Size: tc.size, Items: ".rows"})
			response := fetchPaged(t, c, server.URL)
			if !reflect.DeepEqual(asked, tc.want) {
				t.Fatalf("asked %v, want %v", asked, tc.want)
			}
			if tc.size == 0 && string(response.Body) != `{"rows":[0,1,2,3,4]}` {
				t.Fatalf("merged %s", response.Body)
			}
		})
	}
}

// A trip stops at max_pages, and says so when another page was to come; one
// that ends at max_pages with no page to come does not.
func TestPaginationStopsAtMaxPages(t *testing.T) {
	var hits atomic.Int32
	endless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		fmt.Fprintf(w, `{"items":[%d]}`, n)
	}))
	t.Cleanup(endless.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "page", Param: "page", Items: ".items", MaxPages: 3})
	response := fetchPaged(t, c, endless.URL)
	if string(response.Body) != `{"items":[1,2,3]}` || hits.Load() != 3 || !response.Pages.MoreLeft || len(response.Pages.Pages) != 3 {
		t.Fatalf("%d requests, merged %s, pages %+v", hits.Load(), response.Body, response.Pages)
	}
	// Left unset, max_pages is 10.
	hits.Store(0)
	c = pagedCollector(t, model.PaginationConfig{Type: "page", Param: "page", Items: ".items"})
	if response := fetchPaged(t, c, endless.URL); hits.Load() != model.DefaultMaxPages || !response.Pages.MoreLeft {
		t.Fatalf("%d requests", hits.Load())
	}
	last := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"items":[2],"next":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[1],"next":"?page=2"}`))
	}))
	t.Cleanup(last.Close)
	c = pagedCollector(t, model.PaginationConfig{Type: "next_url", Next: ".next", Items: ".items", MaxPages: 2})
	if response := fetchPaged(t, c, last.URL); response.Pages.MoreLeft || string(response.Body) != `{"items":[1,2],"next":"?page=2"}` {
		t.Fatalf("pages %+v, merged %s", response.Pages, response.Body)
	}
}

// A next page on another origin than the target's — another host, port or
// scheme — is refused before it is asked for: it would be sent the target's
// credentials. So is a next page that was asked for already.
func TestPaginationStaysOnTheTargetsOrigin(t *testing.T) {
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignHits.Add(1)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(foreign.Close)
	foreignURL, _ := url.Parse(foreign.URL)
	for name, next := range map[string]string{
		"another port":   foreign.URL + "/steal",
		"another host":   "http://other.invalid" + ":" + foreignURL.Port() + "/x",
		"another scheme": "https://" + "HOST" + "/x",
	} {
		t.Run(name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Link", "<"+strings.Replace(next, "HOST", strings.TrimPrefix(server.URL, "http://"), 1)+`>; rel="next"`)
				_, _ = w.Write([]byte(`{"items":[1]}`))
			}))
			t.Cleanup(server.Close)
			c := pagedCollector(t, model.PaginationConfig{Type: "link", Items: ".items"})
			c.Request.BearerToken = "t0ken"
			_, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil)
			if err == nil || !strings.Contains(err.Error(), "page 1 of request.pagination names the next page") || !strings.Contains(err.Error(), "is not on the target's origin "+server.URL+"; a next page is sent the target's credentials") {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if foreignHits.Load() != 0 {
		t.Fatalf("the foreign server was asked %d times", foreignHits.Load())
	}
	circle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := "/b"
		if r.URL.Path == "/b" {
			next = "/a"
		}
		fmt.Fprintf(w, `{"items":[1],"next":%q}`, next)
	}))
	t.Cleanup(circle.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "next_url", Next: ".next", Items: ".items"})
	_, err := FetchCollector(context.Background(), circle.URL+"/a", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "page 2 of request.pagination names the next page "+circle.URL+"/a, which page 1 was") {
		t.Fatalf("err=%v", err)
	}
}

// The response limit holds each page, the pages together and the merged
// document: pages each within it that come to more fail the trip as a
// limit, after the page that took them past it.
func TestPaginationHoldsThePagesToTheResponseLimit(t *testing.T) {
	var hits atomic.Int32
	page := `{"items":["` + strings.Repeat("x", 400) + `"]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(server.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "page", Param: "page", Items: ".items"})
	c.Limits.MaxResponseBytes = 1000
	_, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the pages of request.pagination come to more than the response limit 1000 by page 3") || hits.Load() != 3 {
		t.Fatalf("%d requests, err=%v", hits.Load(), err)
	}
	// A page over the limit by itself fails as one response does.
	c.Limits.MaxResponseBytes = 300
	if _, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{}, nil); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "exceeds limit 300") {
		t.Fatalf("err=%v", err)
	}
	// The merged document is held to it too: written again, each byte of
	// a page that is no UTF-8 is the three of U+FFFD, so pages of 34, 34
	// and 12 bytes merge into 137.
	wide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "3" {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":["` + strings.Repeat("\xff", 20) + `"]}`))
	}))
	t.Cleanup(wide.Close)
	c.Limits.MaxResponseBytes = 100
	if _, err := FetchCollector(context.Background(), wide.URL, c, RequestOverrides{}, nil); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "the merged pages of request.pagination come to 137 bytes, which exceeds the response limit 100") {
		t.Fatalf("err=%v", err)
	}
}

// The probe's timeout covers every page together, not each page by itself.
func TestPaginationTimeoutCoversEveryPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(150 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, `{"items":[%q]}`, r.URL.Query().Get("page"))
	}))
	t.Cleanup(server.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "page", Param: "page", Items: ".items", MaxPages: 4})
	start := time.Now()
	_, err := FetchCollector(context.Background(), server.URL, c, RequestOverrides{Timeout: 400 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "page 3 of request.pagination") {
		t.Fatalf("err=%v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s", took)
	}
}

// A page after the first answered with a status the collector does not
// accept is the trip's answer, with that page's status and body, and the
// pages before it; the first page's is the answer as without pagination.
// A first page that is no JSON is handed to the decoder as it came; a later
// one fails the trip.
func TestPaginationAnswersWithAFailingPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"items":[1]}`))
		case "2":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`busy`))
		}
	}))
	t.Cleanup(server.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "page", Param: "page", Items: ".items"})
	response := fetchPaged(t, c, server.URL)
	if response.StatusCode != http.StatusServiceUnavailable || string(response.Body) != "busy" || len(response.Pages.Pages) != 2 || response.Pages.Merged {
		t.Fatalf("status %d, body %q, pages %+v", response.StatusCode, response.Body, response.Pages)
	}
	text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`<html>`))
			return
		}
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(text.Close)
	if response := fetchPaged(t, c, text.URL); string(response.Body) != "not json" || len(response.Pages.Pages) != 1 {
		t.Fatalf("body %q, pages %+v", response.Body, response.Pages)
	}
	c = pagedCollector(t, model.PaginationConfig{Type: "page", Param: "page", Start: ptr(2), Items: ".items"})
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "3" {
			_, _ = w.Write([]byte(`{"items":[2]} {"items":[3]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[1]}`))
	}))
	t.Cleanup(broken.Close)
	if _, err := FetchCollector(context.Background(), broken.URL, c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "page 2 of request.pagination, "+broken.URL+"?page=3, is not JSON") {
		t.Fatalf("err=%v", err)
	}
}

// One page, the only one, is the answer byte for byte, as without
// pagination; items that are not an array, and a next of the wrong kind,
// fail the trip naming the page.
func TestPaginationOfOnePageAndOfWrongValues(t *testing.T) {
	body := `{ "items" : [ 1 ], "a":"<&>" }`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/one":
			_, _ = w.Write([]byte(body))
		case "/object":
			_, _ = w.Write([]byte(`{"items":{"a":1}}`))
		case "/next":
			_, _ = w.Write([]byte(`{"items":[1],"next":["/x"]}`))
		}
	}))
	t.Cleanup(server.Close)
	c := pagedCollector(t, model.PaginationConfig{Type: "next_url", Next: ".next", Items: ".items"})
	if response := fetchPaged(t, c, server.URL+"/one"); string(response.Body) != body || len(response.Pages.Pages) != 1 || !response.Pages.Merged {
		t.Fatalf("body %q", response.Body)
	}
	if _, err := FetchCollector(context.Background(), server.URL+"/object", c, RequestOverrides{}, nil); err == nil || err.Error() != `page 1 of request.pagination: request.pagination.items ".items" is an object, not an array` {
		t.Fatalf("err=%v", err)
	}
	if _, err := FetchCollector(context.Background(), server.URL+"/next", c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), `page 1 of request.pagination: request.pagination.next ".next" gave an array; it must give text, a number, or null, false or "" when there is no next page`) {
		t.Fatalf("err=%v", err)
	}
}

// The Link header is read as RFC 8288 writes it: links separated by commas,
// a comma or semicolon in a quoted value no separator, rel compared without
// regard to case among the relation types it lists, only the first rel of
// a link counting, and a malformed link skipped without hiding the rest.
func TestTheNextLinkOfALinkHeader(t *testing.T) {
	for header, want := range map[string]string{
		`<https://a/2>; rel="next"`:                             "https://a/2",
		`<https://a/1>; rel="prev", <https://a/3>; rel=next`:    "https://a/3",
		`<https://a/x>; title="rel=next, <https://a/y>"; rel=x`: "",
		`<https://a/4>; rel="first NEXT"`:                       "https://a/4",
		`<https://a/5>; rel="last"; rel="next"`:                 "",
		`garbage; rel="next", <https://a/6>;rel="next"`:         "https://a/6",
		`<https://a/7>; title="a \"q\""; rel="next"`:            "https://a/7",
		`<https://a/8>`: "",
		`<https://a/9`:  "",
	} {
		if got := nextLink(http.Header{"Link": {header}}); got != want {
			t.Errorf("%s: got %q, want %q", header, got, want)
		}
	}
}

// request.pagination is checked when the configuration loads: the keys each
// type needs, those that do not apply to it, the expressions, the ranges,
// and a parameter request.query sets too; the defaults are filled in.
func TestPaginationValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		pagination model.PaginationConfig
		change     func(*model.Collector)
		want       string
	}{
		"no type":          {model.PaginationConfig{Items: "."}, nil, `collector "paged" request.pagination.type is required: one of link, next_url, cursor, page, offset`},
		"unknown type":     {model.PaginationConfig{Type: "pages", Items: "."}, nil, `collector "paged" request.pagination.type is "pages"; want one of link, next_url, cursor, page, offset`},
		"no items":         {model.PaginationConfig{Type: "link", Items: " "}, nil, `collector "paged" request.pagination.items is required`},
		"items no path":    {model.PaginationConfig{Type: "link", Items: ".items | length"}, nil, `collector "paged" request.pagination.items ".items | length" is not a path to the array of a page's items`},
		"items two":        {model.PaginationConfig{Type: "link", Items: ".a, .b"}, nil, `request.pagination.items ".a, .b" names 2 places; it must name one`},
		"items bad":        {model.PaginationConfig{Type: "link", Items: ".items["}, nil, `request.pagination.items ".items[" does not compile`},
		"next missing":     {model.PaginationConfig{Type: "next_url", Items: "."}, nil, `collector "paged" request.pagination.next is required with type next_url: the jq expression of the next page's URL`},
		"cursor no next":   {model.PaginationConfig{Type: "cursor", Param: "c", Items: "."}, nil, `request.pagination.next is required with type cursor: the jq expression of the next page's cursor`},
		"next not applied": {model.PaginationConfig{Type: "page", Param: "p", Next: ".next", Items: "."}, nil, `collector "paged" sets request.pagination.next, which applies to type next_url and cursor; type page finds the next page by counting pages in request.pagination.param`},
		"next bad":         {model.PaginationConfig{Type: "next_url", Next: ".next |", Items: "."}, nil, `request.pagination.next ".next |" does not compile`},
		"no param":         {model.PaginationConfig{Type: "offset", Items: "."}, nil, `collector "paged" request.pagination.param is required with type offset: the query parameter the offset is sent in`},
		"param link":       {model.PaginationConfig{Type: "link", Param: "p", Items: "."}, nil, `sets request.pagination.param, which applies to type cursor, page and offset; type link finds the next page by the Link header's rel="next"`},
		"param control":    {model.PaginationConfig{Type: "page", Param: "p\n", Items: "."}, nil, `request.pagination.param "p\n" has the control character '\n' in it`},
		"param in query":   {model.PaginationConfig{Type: "page", Param: "page", Items: "."}, func(c *model.Collector) { c.Request.Query = map[string]string{"Page": "1"} }, `collector "paged" sets request.query "Page", the parameter request.pagination.param sets on every page`},
		"start cursor":     {model.PaginationConfig{Type: "cursor", Param: "c", Next: ".n", Start: ptr(1), Items: "."}, nil, `sets request.pagination.start, which applies to type page and offset`},
		"start negative":   {model.PaginationConfig{Type: "page", Param: "p", Start: ptr(-1), Items: "."}, nil, `request.pagination.start must not be negative`},
		"size page":        {model.PaginationConfig{Type: "page", Param: "p", Size: 5, Items: "."}, nil, `sets request.pagination.size, which applies to type offset`},
		"size negative":    {model.PaginationConfig{Type: "offset", Param: "p", Size: -5, Items: "."}, nil, `request.pagination.size must be at least 1`},
		"max pages":        {model.PaginationConfig{Type: "link", MaxPages: 1001, Items: "."}, nil, `collector "paged" request.pagination.max_pages must be from 1 to 1000`},
		"max pages low":    {model.PaginationConfig{Type: "link", MaxPages: -1, Items: "."}, nil, `request.pagination.max_pages must be from 1 to 1000`},
		"head":             {model.PaginationConfig{Type: "link", Items: "."}, func(c *model.Collector) { c.Request.Method = "head" }, `collector "paged" sets request.pagination with method HEAD`},
	} {
		t.Run(name, func(t *testing.T) {
			c := testutil.Collector("paged", "json")
			p := tc.pagination
			c.Request.Pagination = &p
			if tc.change != nil {
				tc.change(&c)
			}
			if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v\nwant %s", err, tc.want)
			}
		})
	}
	c := testutil.Collector("paged", "json")
	c.Request.Pagination = &model.PaginationConfig{Type: " Offset ", Param: "o", Items: "."}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if p := c.Request.Pagination; p.Type != "offset" || p.MaxPages != model.DefaultMaxPages || p.Start == nil || *p.Start != 0 {
		t.Fatalf("defaults %+v", p)
	}
	c.Request.Pagination = &model.PaginationConfig{Type: "page", Param: "p", Items: ".data[0].items # the list"}
	if err := ValidateRequest(&c); err != nil || *c.Request.Pagination.Start != 1 {
		t.Fatalf("err=%v", err)
	}
}

// A target reached by its address with a Host override builds its links from
// that Host: a next page on the Host's virtual host, with the first page's
// scheme and the Host's port (the scheme's, when it names none), is fetched
// from the target's address with the same Host, never by looking the
// virtual host up, and with the target's credentials; a static target's Host
// counts as the collector's does. A next page on another port or scheme of
// the virtual host, or on another host, is still refused.
func TestPaginationFollowsTheHostOverridesVirtualHost(t *testing.T) {
	type sent struct{ host, uri, authorization string }
	var mu sync.Mutex
	var requests []sent
	next := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, sent{r.Host, r.URL.RequestURI(), r.Header.Get("Authorization")})
		mu.Unlock()
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"items":[2]}`))
			return
		}
		w.Header().Set("Link", "<"+strings.ReplaceAll(next, "HOST", r.Host)+`>; rel="next"`)
		_, _ = w.Write([]byte(`{"items":[1]}`))
	}))
	t.Cleanup(server.Close)
	for name, tc := range map[string]struct {
		collectorHost, targetHost, next string
		refused                         bool
	}{
		"the collector's Host":                {collectorHost: "api.example", next: "http://HOST/items?page=2"},
		"the Host in another case":            {collectorHost: "api.example", next: "http://API.Example:80/items?page=2"},
		"a static target's Host":              {collectorHost: "other.example", targetHost: "api.example", next: "http://HOST/items?page=2"},
		"a Host with a port":                  {collectorHost: "api.example:8080", next: "http://HOST/items?page=2"},
		"the Host's virtual host on a port":   {collectorHost: "api.example", next: "http://api.example:8080/items?page=2", refused: true},
		"the Host's virtual host over https":  {collectorHost: "api.example", next: "https://api.example/items?page=2", refused: true},
		"another virtual host":                {collectorHost: "api.example", next: "http://www.api.example/items?page=2", refused: true},
		"a static target's Host replaces one": {collectorHost: "api.example", targetHost: "other.example", next: "http://api.example/items?page=2", refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			requests, next = nil, tc.next
			mu.Unlock()
			c := pagedCollector(t, model.PaginationConfig{Type: "link", Items: ".items"})
			c.Request.BearerToken = "t0ken"
			c.Request.Headers = map[string]string{"Host": tc.collectorHost}
			var forwarded http.Header
			if tc.targetHost != "" {
				forwarded = http.Header{"Host": {tc.targetHost}}
			}
			response, err := FetchCollector(context.Background(), server.URL+"/items", c, RequestOverrides{}, forwarded)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "is not on the target's origin "+server.URL+"; a next page is sent the target's credentials") {
					t.Fatalf("err=%v", err)
				}
				if len(requests) != 1 {
					t.Fatalf("sent %+v", requests)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, want := decoded(t, response.Body), decoded(t, []byte(`{"items":[1,2]}`)); !reflect.DeepEqual(got, want) {
				t.Fatalf("merged %s", response.Body)
			}
			host := tc.collectorHost
			if tc.targetHost != "" {
				host = tc.targetHost
			}
			want := []sent{{host, "/items", "Bearer t0ken"}, {host, "/items?page=2", "Bearer t0ken"}}
			if !reflect.DeepEqual(requests, want) {
				t.Fatalf("sent %+v, want %+v", requests, want)
			}
			if want := []string{server.URL + "/items", server.URL + "/items?page=2"}; !reflect.DeepEqual(pageURLs(response), want) {
				t.Fatalf("pages %v", pageURLs(response))
			}
		})
	}
}
