//go:build !select_request_types || request_type_http

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// chainCollector is an http collector whose first request asks path and
// reads extract, with the steps then, validated.
func chainCollector(t *testing.T, path string, extract map[string]model.ChainExtract, then []model.ChainStep, edit func(*model.Collector)) *model.Collector {
	t.Helper()
	c := testutil.Collector("chained", "json")
	c.Request.Path = path
	c.Limits.MaxResponseBytes = 1 << 20
	editHTTP(&c, func(r *HTTPConfig) { r.Extract, r.Then = extract, then })
	if edit != nil {
		edit(&c)
	}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// fetchChained fetches target with c.
func fetchChained(c *model.Collector, target string, overrides RequestOverrides) (*HTTPResponse, error) {
	return FetchCollector(context.Background(), target, c, overrides, nil)
}

// jsonValue is a value read with a jq expression.
func jsonValue(expression string) model.ChainExtract { return model.ChainExtract{JSON: expression} }

// A value read from the first answer is sent by the next step, escaped as
// each place needs: as one path segment, as a query value, as a JSON
// string in a json body and as written in a header. The last step's answer
// is the trip's, and the chain's report names each step.
func TestAChainSendsAValueEscapedAsItsPlaceNeeds(t *testing.T) {
	type seen struct{ path, query, body, header, contentType, method string }
	var got seen
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list" {
			_, _ = w.Write([]byte(`{"id": "a b/c\"d"}`))
			return
		}
		{
			body, _ := io.ReadAll(r.Body)
			got = seen{r.URL.EscapedPath(), r.URL.Query().Get("q"), string(body), r.Header.Get("X-Id"), r.Header.Get("Content-Type"), r.Method}
			_, _ = w.Write([]byte(`{"value": 7}`))
		}
	}))
	t.Cleanup(server.Close)
	c := chainCollector(t, "/list", map[string]model.ChainExtract{"id": jsonValue(".id")}, []model.ChainStep{{
		Name:    "detail",
		Path:    "/items/{{chain.id}}",
		Query:   map[string]string{"q": "{{chain.id}}"},
		Headers: map[string]string{"X-Id": "id {{chain.id}}"},
		JSON:    map[string]string{"id": "{{chain.id}}"},
	}}, nil)
	response, err := fetchChained(c, server.URL, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := seen{"/items/a%20b%2Fc%22d", `a b/c"d`, `{"id":"a b/c\"d"}`, `id a b/c"d`, "application/json", http.MethodPost}
	if got != want {
		t.Fatalf("the step was sent\n%+v, want\n%+v", got, want)
	}
	if string(response.Body) != `{"value": 7}` {
		t.Fatalf("the trip's answer is %s, want the last step's", response.Body)
	}
	if response.Chain == nil || len(response.Chain.Steps) != 2 || response.Chain.Steps[0].Name != "first" || response.Chain.Steps[1].State != ChainStepOK || response.Chain.Steps[1].Values != nil {
		t.Fatalf("the report is %+v", response.Chain)
	}
	if v := response.Chain.Steps[0].Values; len(v) != 1 || v[0].Name != "id" || v[0].Text != `a b/c"d` {
		t.Fatalf("the first step read %+v", v)
	}
}

// A link a step reads is followed when it stays on the target's origin, and
// refused, as the target policy refuses a target, when it leads to another
// origin. A redirect to another origin is refused the same way, even for
// the first request.
func TestAChainKeepsToTheTargetsOrigin(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(elsewhere.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/here":
			_, _ = w.Write([]byte(`{"next": "/there?x=1"}`))
		case "/away":
			_, _ = w.Write([]byte(`{"next": "` + elsewhere.URL + `/there"}`))
		case "/bounce":
			http.Redirect(w, r, elsewhere.URL+"/there", http.StatusFound)
		case "/there":
			_, _ = w.Write([]byte(`{"ok": true}`))
		}
	}))
	t.Cleanup(server.Close)
	then := []model.ChainStep{{Name: "detail", URL: "{{chain.next}}"}}
	extract := map[string]model.ChainExtract{"next": jsonValue(".next")}
	follow := func(c *model.Collector) { c.Request.FollowRedirects = true }

	response, err := fetchChained(chainCollector(t, "/here", extract, then, follow), server.URL, RequestOverrides{})
	if err != nil || string(response.Body) != `{"ok": true}` {
		t.Fatalf("a link on the origin: %v, %s", err, response.Body)
	}

	_, err = fetchChained(chainCollector(t, "/away", extract, then, follow), server.URL, RequestOverrides{})
	if !errors.Is(err, ErrTargetRefused) || !strings.HasPrefix(err.Error(), `step "detail": the link "`+elsewhere.URL+`/there" leads to another origin than the target's, `+server.URL+`; a chain asks one target only`) {
		t.Fatalf("a link to another origin: %v", err)
	}
	var chainErr *ChainError
	if !errors.As(err, &chainErr) || chainErr.Report.Steps[1].State != ChainStepFailed || chainErr.Report.FailedStep != "detail" {
		t.Fatalf("the report of the refused link: %+v", chainErr)
	}

	bounced := chainCollector(t, "/bounce", map[string]model.ChainExtract{"next": {JSON: ".next", Optional: true}}, []model.ChainStep{{Name: "detail", Path: "/there?n={{chain.next}}"}}, follow)
	_, err = fetchChained(bounced, server.URL, RequestOverrides{})
	if !errors.Is(err, ErrTargetRefused) || !strings.HasPrefix(err.Error(), `step "first": `) || !strings.Contains(err.Error(), "leads to another origin than the target's") {
		t.Fatalf("a redirect to another origin: %v", err)
	}
}

// jobsServer answers /jobs with jobs named job0 to job<n-1>, and each
// /job/<name> with its duration, or 404 for the names in missing. It
// counts the requests sent at once.
type jobsServer struct {
	*httptest.Server
	inFlight, most atomic.Int64
	asked          sync.Map
}

func newJobsServer(t *testing.T, n int, missing ...string) *jobsServer {
	s := &jobsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jobs" {
			var names []string
			for i := range n {
				names = append(names, `{"name":"job`+string(rune('0'+i%10))+string(rune('a'+i/10))+`"}`)
			}
			_, _ = w.Write([]byte(`{"jobs":[` + strings.Join(names, ",") + `]}`))
			return
		}
		now := s.inFlight.Add(1)
		defer s.inFlight.Add(-1)
		for {
			most := s.most.Load()
			if now <= most || s.most.CompareAndSwap(most, now) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		name := strings.TrimPrefix(r.URL.Path, "/job/")
		s.asked.Store(name, true)
		for _, m := range missing {
			if m == name {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		_, _ = w.Write([]byte(`{"duration": 1000}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// fanOutCollector fans out over the jobs of a jobsServer.
func fanOutCollector(t *testing.T, step model.ChainStep) *model.Collector {
	step.Name, step.ForEach, step.Path = "last_build", "jobs", "/job/{{each.name}}"
	return chainCollector(t, "/jobs", map[string]model.ChainExtract{"jobs": jsonValue("[.jobs[]]")}, []model.ChainStep{step}, func(c *model.Collector) { c.Answer = model.ChainAnswerAll })
}

// A fan-out asks for each entry, no more than concurrency at once, and the
// first max_items of them; the cut is counted and the trip succeeds. The
// document of answer: all holds the first answer and each item's.
func TestAFanOutAsksEachEntryWithinItsConcurrencyAndMaxItems(t *testing.T) {
	server := newJobsServer(t, 12)
	c := fanOutCollector(t, model.ChainStep{Concurrency: 3, MaxItems: 10})
	response, err := fetchChained(c, server.URL, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if most := server.most.Load(); most > 3 || most < 2 {
		t.Fatalf("%d requests were sent at once, want at most 3 and more than 1", most)
	}
	step := response.Chain.Steps[1]
	if step.ItemsTotal != 12 || step.ItemsCut != 2 || len(step.Items) != 10 || step.State != ChainStepOK || step.Requests != 10 {
		t.Fatalf("the fan-out's report: total %d, cut %d, items %d, state %s, requests %d", step.ItemsTotal, step.ItemsCut, len(step.Items), step.State, step.Requests)
	}
	var document struct {
		First struct {
			Status int `json:"status"`
		} `json:"first"`
		LastBuild []struct {
			Each struct {
				Name string `json:"name"`
			} `json:"each"`
			Status int `json:"status"`
			Body   struct {
				Duration int `json:"duration"`
			} `json:"body"`
		} `json:"last_build"`
	}
	if err := json.Unmarshal(response.Body, &document); err != nil {
		t.Fatalf("%v: %s", err, response.Body)
	}
	if document.First.Status != 200 || len(document.LastBuild) != 10 || document.LastBuild[9].Each.Name != "job9a" || document.LastBuild[0].Body.Duration != 1000 {
		t.Fatalf("the document is %s", response.Body)
	}
}

// Under on_item_error: fail, an item answered with a status the step does
// not accept fails the trip with that answer, its report naming the item.
// Under skip, the item is an error in the document and the trip goes on.
func TestAFanOutFailsOrSkipsAFailingItem(t *testing.T) {
	server := newJobsServer(t, 4, "job2a")
	response, err := fetchChained(fanOutCollector(t, model.ChainStep{Concurrency: 1}), server.URL, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || response.Chain.Failed != `step "last_build" item 3 of 4 ("{\"name\":\"job2a\"}")` {
		t.Fatalf("fail: status %d, failed %q", response.StatusCode, response.Chain.Failed)
	}
	if _, asked := server.asked.Load("job3a"); asked {
		t.Fatal("fail asked for an item after the one that failed")
	}

	response, err = fetchChained(fanOutCollector(t, model.ChainStep{OnItemError: "skip"}), server.URL, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	step := response.Chain.Steps[1]
	if step.ItemsFailed != 1 || step.State != ChainStepFailed || response.Chain.Failed != "" || !strings.Contains(string(response.Body), `{"each":{"name":"job2a"},"error":"received HTTP status 404"}`) {
		t.Fatalf("skip: failed %d, state %s, %q, %s", step.ItemsFailed, step.State, response.Chain.Failed, response.Body)
	}
}

// The probe's timeout bounds the whole chain, and a fan-out the deadline
// cuts short fails saying how many items were done.
func TestAChainSharesOneBudget(t *testing.T) {
	server := newJobsServer(t, 40)
	c := fanOutCollector(t, model.ChainStep{Concurrency: 1})
	start := time.Now()
	_, err := fetchChained(c, server.URL, RequestOverrides{Timeout: 60 * time.Millisecond})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the chain took %s", elapsed)
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), `step "last_build": the trip's deadline came after `) || !strings.Contains(err.Error(), " of 40 items") {
		t.Fatalf("the error is %v", err)
	}
	var chainErr *ChainError
	if !errors.As(err, &chainErr) || chainErr.Report.Steps[1].State != ChainStepTimedOut {
		t.Fatalf("the report: %+v", chainErr)
	}
}

// The steps of a trip share the cookies the answers set, and the next trip
// starts without them.
func TestAChainSharesCookiesWithinATrip(t *testing.T) {
	var cookies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookies = append(cookies, r.Header.Get("Cookie"))
		if r.URL.Path == "/start" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Path: "/"})
			_, _ = w.Write([]byte(`{"id": 1}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	c := chainCollector(t, "/start", map[string]model.ChainExtract{"id": jsonValue(".id")}, []model.ChainStep{{Name: "next", Path: "/next/{{chain.id}}"}}, nil)
	for range 2 {
		if _, err := fetchChained(c, server.URL, RequestOverrides{}); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"", "session=s1", "", "session=s1"}; strings.Join(cookies, "|") != strings.Join(want, "|") {
		t.Fatalf("the requests carried the cookies %q, want %q", cookies, want)
	}
}

// A value not found fails the step that reads it, naming both; an optional
// one is empty. A status a step does not accept fails the trip with that
// answer, its failure naming the step.
func TestAChainFailsAtTheStepThatFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/first":
			_, _ = w.Write([]byte(`{"id": 1}`))
		case "/gone/1":
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{"page": "` + r.URL.Query().Get("cursor") + `"}`))
		}
	}))
	t.Cleanup(server.Close)
	_, err := fetchChained(chainCollector(t, "/first", map[string]model.ChainExtract{"jobs": jsonValue(".jobs")}, []model.ChainStep{{Name: "list", Path: "/x/{{chain.jobs}}"}}, nil), server.URL, RequestOverrides{})
	if err == nil || err.Error() != `step "first": value "jobs" was not found in the answer` {
		t.Fatalf("a value not found: %v", err)
	}
	response, err := fetchChained(chainCollector(t, "/first", map[string]model.ChainExtract{"cursor": {JSON: ".cursor", Optional: true}}, []model.ChainStep{{Name: "list", Path: "/x", Query: map[string]string{"cursor": "{{chain.cursor}}"}}}, nil), server.URL, RequestOverrides{})
	if err != nil || string(response.Body) != `{"page": ""}` {
		t.Fatalf("an optional value: %v, %s", err, response.Body)
	}
	response, err = fetchChained(chainCollector(t, "/first", map[string]model.ChainExtract{"id": jsonValue(".id")}, []model.ChainStep{{Name: "detail", Path: "/gone/{{chain.id}}"}, {Name: "after", Path: "/after/{{chain.id}}"}}, nil), server.URL, RequestOverrides{})
	if err != nil || response.StatusCode != http.StatusNotFound || response.Chain.Failed != `step "detail"` || response.Chain.Steps[2].State != ChainStepNotReached {
		t.Fatalf("a refused status: %v, %+v", err, response.Chain)
	}
}

// A 401 at a later step drops the token, logs in again and sends that
// request once more, once per trip: a second step that refuses the new
// token fails with its 401.
func TestAChainLogsInAgainOncePerTrip(t *testing.T) {
	freshLogins(t)
	var issued, logins atomic.Int64
	refuse := map[string]bool{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			logins.Add(1)
			_, _ = w.Write([]byte(`{"token": "t` + string(rune('0'+issued.Add(1))) + `"}`))
			return
		}
		mu.Lock()
		refused := refuse[r.URL.Path+" "+r.Header.Get("Authorization")]
		mu.Unlock()
		if refused {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	t.Cleanup(server.Close)
	files := secretFiles(t, map[string]string{"password": "p"})
	login := func(c *model.Collector) {
		c.Request.Login = &model.LoginConfig{Type: "json", URL: "/login", Token: ".token", FieldFiles: files}
	}
	c := chainCollector(t, "/first", map[string]model.ChainExtract{"id": jsonValue(".id")}, []model.ChainStep{{Name: "a", Path: "/a/{{chain.id}}"}, {Name: "b", Path: "/b/{{chain.id}}"}}, login)
	mu.Lock()
	refuse["/a/1 Bearer t1"] = true
	mu.Unlock()
	if _, err := fetchChained(c, server.URL, RequestOverrides{}); err != nil {
		t.Fatalf("a 401 at step a: %v", err)
	}
	if n := logins.Load(); n != 2 {
		t.Fatalf("%d logins, want 2: the first and one after the 401", n)
	}
	mu.Lock()
	refuse["/first Bearer t2"], refuse["/b/1 Bearer t3"] = true, true
	mu.Unlock()
	response, err := fetchChained(c, server.URL, RequestOverrides{})
	if err != nil || response.StatusCode != http.StatusUnauthorized || response.Chain.Failed != `step "b"` {
		t.Fatalf("a second 401 in a trip: %v, %+v", err, response)
	}
	if n := logins.Load(); n != 3 {
		t.Fatalf("%d logins, want 3: one more for the trip", n)
	}
}

// A probe parameter that only a step's placeholder names is one the
// collector takes, and is not refused as unused; the parameters a chain
// fills in itself are not among those a probe can give.
func TestAStepsPlaceholdersTakeProbeParameters(t *testing.T) {
	c := chainCollector(t, "/first", map[string]model.ChainExtract{"id": jsonValue(".id")}, []model.ChainStep{{Name: "detail", Path: "/x/{{chain.id}}", Query: map[string]string{"tenant": "{{param_tenant}}"}}}, nil)
	unused, err := CheckRequestParams(c, RequestOverrides{Params: map[string]string{"param_tenant": "acme"}})
	if err != nil || len(unused) != 0 {
		t.Fatalf("unused %v, error %v", unused, err)
	}
	params := RequestParams(c)
	if len(params) != 1 || params[0].Name != "param_tenant" || !params[0].Required {
		t.Fatalf("the collector takes %+v, want param_tenant alone, required", params)
	}
}

// A value marked secret is never shown: not in the URL of a step's report
// or its trace, not in an error, not in the start of an answer or a header
// the report shows. A probe parameter named as a chain's own is refused as
// unused, not taken.
func TestAChainNeverShowsASecretValue(t *testing.T) {
	const secret = "s3cret-session"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("X-Session", secret)
			_, _ = w.Write([]byte(`{"session": "` + secret + `"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	c := chainCollector(t, "/start", map[string]model.ChainExtract{"session": {Header: "X-Session", Secret: true}}, []model.ChainStep{{Name: "detail", Path: "/x/{{chain.session}}", Query: map[string]string{"s": "{{chain.session}}"}}}, nil)
	ctx, trace := WithRequestTrace(context.Background())
	response, err := FetchCollector(ctx, server.URL, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	shown := fmt.Sprintf("%+v %+v", response.Chain, trace.Requests())
	if strings.Contains(shown, secret) || !strings.Contains(response.Chain.Steps[1].URL, "/x/***") {
		t.Fatalf("the secret is shown:\n%s", shown)
	}
	c.Request.FollowRedirects = true
	unused, err := CheckRequestParams(c, RequestOverrides{Params: map[string]string{"param__chain_session": "x"}})
	if err != nil || len(unused) != 1 {
		t.Fatalf("a probe parameter of the chain's own: unused %v, error %v", unused, err)
	}
}
