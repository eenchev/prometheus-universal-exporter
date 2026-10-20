//go:build !select_request_types || request_type_http

package allocator

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func metrics(t *testing.T, a *Allocator) string {
	t.Helper()
	var b bytes.Buffer
	if err := a.WriteMetrics(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// get sends a GET with the headers given as name, value pairs, and returns
// the response with its body read.
func get(t *testing.T, url string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func post(t *testing.T, url, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(out)
}

// serve starts the allocator's API for the test.
func serve(t *testing.T, a *Allocator, token string) string {
	t.Helper()
	server := httptest.NewServer(a.Handler(token))
	t.Cleanup(server.Close)
	return server.URL
}

// The bundle endpoint answers 503 until the member list has formed, then
// the member's bundle with its generation as the ETag, and 304 to a
// request that already has it; a request without its member or with a
// wait that is no duration is refused.
func TestTheBundleEndpoint(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(5), "")
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Settle: 15 * time.Second, Clock: clock, Logger: testutil.QuietLogger(t)})
	url := serve(t, a, "")
	resp, _ := get(t, url+"/v1/bundle?member=r1")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("before the member list formed: got %s, Retry-After %q, want 503 with Retry-After", resp.Status, resp.Header.Get("Retry-After"))
	}
	beat(t, a, "r1")
	keepBeating(t, a, clock, 20*time.Second, "r1")
	resp, body := get(t, url+"/v1/bundle?member=r1")
	var b allocapi.Bundle
	if err := json.Unmarshal([]byte(body), &b); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("got %s %v:\n%s", resp.Status, err, body)
	}
	if b.Generation != a.Bundle("r1").Generation || allocapi.Generation(b.Files) != b.Generation || resp.Header.Get("ETag") != `"`+b.Generation+`"` {
		t.Errorf("got the generation %s and the ETag %s for the bundle %s", b.Generation, resp.Header.Get("ETag"), a.Bundle("r1").Generation)
	}
	for _, tag := range []string{`"` + b.Generation + `"`, `W/"` + b.Generation + `"`, b.Generation, `"` + b.Generation + `", "other"`} {
		resp, _ = get(t, url+"/v1/bundle?member=r1", "If-None-Match", tag)
		if resp.StatusCode != http.StatusNotModified || resp.Header.Get("ETag") != `"`+b.Generation+`"` {
			t.Errorf("If-None-Match %s: got %s with the ETag %q, want 304", tag, resp.Status, resp.Header.Get("ETag"))
		}
	}
	resp, _ = get(t, url+"/v1/bundle?member=r1", "If-None-Match", `"older"`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("an older generation got %s, want the bundle", resp.Status)
	}
	for _, query := range []string{"", "?member=", "?member=r1&wait=soon", "?member=r1&wait=-1s"} {
		if resp, _ = get(t, url+"/v1/bundle"+query); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: got %s, want 400", query, resp.Status)
		}
	}
}

// A request that has its bundle and waits is answered at once when the
// bundle changes, with the new one, before its wait has run out.
func TestALongPollIsAnsweredWhenTheBundleChanges(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(5), "")
	a, clock := settledAllocator(t, mustLoad(t, p), "r1")
	url := serve(t, a, "")
	have := a.Bundle("r1").Generation
	type answer struct {
		resp *http.Response
		body string
	}
	answered := make(chan answer, 1)
	go func() {
		resp, body := get(t, url+"/v1/bundle?member=r1&wait=60s", "If-None-Match", `"`+have+`"`)
		answered <- answer{resp, body}
	}()
	testutil.WaitFor(t, "the request to wait", func() bool { return clock.pending(clock.Now().Add(60 * time.Second)) })
	testutil.WriteIn(t, filepath.Dir(p.Config), filepath.Base(p.StaticTargets), staticTargets(6))
	if !a.Reload(p) {
		t.Fatal("the changed file was not put in force")
	}
	var got answer
	select {
	case got = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("the long poll was not answered when the bundle changed")
	}
	var b allocapi.Bundle
	if err := json.Unmarshal([]byte(got.body), &b); err != nil || got.resp.StatusCode != http.StatusOK || b.Generation == have || b.Generation != a.Bundle("r1").Generation {
		t.Fatalf("got %s %v, the generation %s, want the new bundle %s", got.resp.Status, err, b.Generation, a.Bundle("r1").Generation)
	}
}

// A request that has its bundle waits for its wait, 60s at most however
// long it asks for, and is then answered 304; the allocator's shutdown
// answers it at once.
func TestALongPollEndsWith304(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(5), "")
	a, clock := settledAllocator(t, mustLoad(t, p), "r1")
	url := serve(t, a, "")
	have := `"` + a.Bundle("r1").Generation + `"`
	status := make(chan int, 1)
	go func() {
		resp, _ := get(t, url+"/v1/bundle?member=r1&wait=5m", "If-None-Match", have)
		status <- resp.StatusCode
	}()
	testutil.WaitFor(t, "the request to wait 60s", func() bool { return clock.pending(clock.Now().Add(MaxWait)) })
	keepBeating(t, a, clock, MaxWait, "r1")
	if got := <-status; got != http.StatusNotModified {
		t.Errorf("when the wait ran out: got %d, want 304", got)
	}
	go func() {
		resp, _ := get(t, url+"/v1/bundle?member=r1&wait=30s", "If-None-Match", have)
		status <- resp.StatusCode
	}()
	testutil.WaitFor(t, "the request to wait 30s", func() bool { return clock.pending(clock.Now().Add(30 * time.Second)) })
	a.BeginShutdown()
	if got := <-status; got != http.StatusNotModified {
		t.Errorf("at the shutdown: got %d, want 304", got)
	}
	if resp, _ := get(t, url+"/ready"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/ready during the shutdown: got %s, want 503", resp.Status)
	}
}

// The heartbeat endpoint takes a heartbeat as JSON and answers with the
// generation of the member's bundle, "" before the member list has formed;
// what is no heartbeat is refused.
func TestTheHeartbeatEndpoint(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(5), "")
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Clock: clock, Logger: testutil.QuietLogger(t)})
	url := serve(t, a, "")
	resp, body := post(t, url+"/v1/heartbeat", `{"member":"r1","advertise":"10.0.0.1:9115","instance":"i","sequence":1}`)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != `{"generation":""}` {
		t.Errorf("before the member list formed: got %s %s", resp.Status, body)
	}
	clock.Advance(allocapi.HeartbeatInterval)
	resp, body = post(t, url+"/v1/heartbeat", `{"member":"r1","advertise":"10.0.0.1:9115","instance":"i","sequence":2,"generation":"x"}`)
	var reply allocapi.HeartbeatReply
	if err := json.Unmarshal([]byte(body), &reply); err != nil || resp.StatusCode != http.StatusOK || reply.Generation != a.Bundle("r1").Generation {
		t.Errorf("got %s %s, want the bundle's generation %s", resp.Status, body, a.Bundle("r1").Generation)
	}
	for _, bad := range []string{"", "[]", `{"member":"","advertise":"a:1","instance":"i","sequence":3}`, `{"member":"r1","advertise":"nowhere","instance":"i","sequence":3}`,
		`{"member":"r1","advertise":"a:http","instance":"i","sequence":3}`, `{"member":"r1","advertise":"a:0","instance":"i","sequence":3}`,
		`{"member":"r1","advertise":"a:99999","instance":"i","sequence":3}`, `{"member":"r1","advertise":":9115","instance":"i","sequence":3}`,
		`{"member":"r1","advertise":"a:1","sequence":3}`, `{"member":"r1","advertise":"a:1","instance":"i"}`, `{"member":"r1","advertise":"a:1","instance":"i","sequence":-1}`} {
		if resp, _ = post(t, url+"/v1/heartbeat", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: got %s, want 400", bad, resp.Status)
		}
	}
	huge := `{"member":"r1","advertise":"10.0.0.1:9115","generation":"` + strings.Repeat("x", maxHeartbeatBytes) + `"}`
	if resp, body = post(t, url+"/v1/heartbeat", huge); resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(body, "larger than 65536 bytes") {
		t.Errorf("a heartbeat over the bound: got %s %q, want 413 saying it is too large", resp.Status, body)
	}
	if resp, _ = get(t, url+"/v1/heartbeat"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/heartbeat: got %s, want 405", resp.Status)
	}
}

// The heartbeat of a new member while the allocator has as many live
// members as it takes is answered 429, with Retry-After the member TTL.
func TestAHeartbeatBeyondTheMemberBoundIs429(t *testing.T) {
	p := writeInputs(t, testConfig, "", "")
	a := New(mustLoad(t, p), Options{MaxMembers: 1, Clock: newFakeClock(), Logger: testutil.QuietLogger(t)})
	url := serve(t, a, "")
	if resp, body := post(t, url+"/v1/heartbeat", `{"member":"r1","advertise":"10.0.0.1:9115","instance":"i","sequence":1}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("the first member: got %s %s", resp.Status, body)
	}
	resp, body := post(t, url+"/v1/heartbeat", `{"member":"r2","advertise":"10.0.0.2:9115","instance":"i","sequence":1}`)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "30" || !strings.Contains(body, "--members.max") {
		t.Errorf("a member beyond the bound: got %s, Retry-After %q, %q; want 429 naming --members.max", resp.Status, resp.Header.Get("Retry-After"), body)
	}
}

// The service discovery answer of a probe job is a group per target at the
// address of the member it is routed to, with the probe path, the
// collector, the target and every parameter as __param_ labels, the target
// as the instance and the job's labels, one written "" left out.
func TestServiceDiscoveryAnswersEveryProbeAtItsMember(t *testing.T) {
	p := writeInputs(t, testConfig, "", testProbes)
	clock := newFakeClock()
	a := New(mustLoad(t, p), Options{Clock: clock, Logger: testutil.QuietLogger(t)})
	url := serve(t, a, "")
	if resp, _ := get(t, url+"/v1/sd/websites"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("before the member list formed: got %s, want 503", resp.Status)
	}
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r1", Advertise: "10.0.0.1:9115"})); err != nil {
		t.Fatal(err)
	}
	clock.Advance(allocapi.HeartbeatInterval)
	resp, body := get(t, url+"/v1/sd/websites")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("got %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	var got []SDGroup
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	group := func(target string) SDGroup {
		return SDGroup{Targets: []string{"10.0.0.1:9115"}, Labels: map[string]string{
			"__metrics_path__": "/probe", "__param_collector": "demo", "__param_target": target, "__param_timeout": "10s",
			"instance": target, "team": "web", MemberMetaLabel: "r1",
		}}
	}
	if want := []SDGroup{group("https://shop.example"), group("https://docs.example")}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if resp, _ = get(t, url+"/v1/sd/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown job: got %s, want 404", resp.Status)
	}
}

// A probe goes only to the members whose bundle has its collector, so a
// collector just added draws no "unknown collector" answers from a replica
// that has not loaded it yet; when no member has it, the probe goes to any.
func TestAProbeGoesToTheMembersWhoseBundleHasItsCollector(t *testing.T) {
	p := writeInputs(t, testConfig, "", testProbes)
	a, clock := settledAllocator(t, mustLoad(t, p), "r1", "r2")
	old := a.Bundle("r1").Generation
	added := strings.Replace(testConfig, "collectors:\n", "collectors:\n"+testutil.CollectorYAML("added"), 1)
	testutil.WriteIn(t, filepath.Dir(p.Config), filepath.Base(p.Config), added)
	testutil.WriteIn(t, filepath.Dir(p.Config), filepath.Base(p.ProbeTargets), testProbes+"  - name: fresh\n    collector: added\n    targets: [http://a.example, http://b.example, http://c.example, http://d.example]\n")
	if !a.Reload(p) {
		t.Fatal("the new files were not put in force")
	}
	routed := func(job string) map[string]int {
		t.Helper()
		groups, err := a.SD(job)
		if err != nil {
			t.Fatal(err)
		}
		members := map[string]int{}
		for _, g := range groups {
			members[g.Labels[MemberMetaLabel]]++
		}
		return members
	}
	if got := routed("fresh"); len(got) != 2 {
		t.Errorf("with no member reporting the new bundle, the probes went to %v, want both members", got)
	}
	heartbeat := func(member, generation string) {
		if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: member, Advertise: member + ":9115", Generation: generation})); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat("r1", a.Bundle("r1").Generation)
	heartbeat("r2", old)
	clock.Advance(time.Second)
	if got := routed("fresh"); !reflect.DeepEqual(got, map[string]int{"r1": 4}) {
		t.Errorf("the probes of the new collector went to %v, want r1 alone", got)
	}
}

// While no member of the member list is live, as when every replica
// restarted at once and the new ones have not held for the settle time yet,
// a probe goes to a live member outside it rather than nowhere.
func TestAProbeGoesToALiveMemberWhenNoneOfTheListIs(t *testing.T) {
	a, clock := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, "", testProbes)), "r1")
	if _, err := a.Heartbeat(stamp(allocapi.Heartbeat{Member: "r1", Advertise: "r1:9115", ShuttingDown: true})); err != nil {
		t.Fatal(err)
	}
	beat(t, a, "r2")
	clock.Advance(time.Second)
	groups, err := a.SD("websites")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Targets[0] != "r2:9115" {
			t.Errorf("a probe went to %v, want r2, the one live member", g.Targets)
		}
	}
}

// A probe job with shard_key: address sends every probe of one host and
// port to one member, whatever the path or the scheme's default port.
func TestProbesOfOneAddressShareAMember(t *testing.T) {
	var probes strings.Builder
	probes.WriteString("probe_jobs:\n  - name: j\n    collector: demo\n    shard_key: address\n    targets:\n")
	for _, target := range []string{"http://db.example/a", "http://DB.example:80/b", "http://db.example:80/c?x=1", "https://db.example/d", "https://db.example:443/e"} {
		probes.WriteString("      - ")
		probes.WriteString(target)
		probes.WriteString("\n")
	}
	a, _ := settledAllocator(t, mustLoad(t, writeInputs(t, testConfig, "", probes.String())), "r1", "r2", "r3", "r4", "r5", "r6")
	groups, err := a.SD("j")
	if err != nil {
		t.Fatal(err)
	}
	if groups[0].Labels[MemberMetaLabel] != groups[1].Labels[MemberMetaLabel] || groups[0].Labels[MemberMetaLabel] != groups[2].Labels[MemberMetaLabel] {
		t.Errorf("the probes of db.example:80 went to several members: %+v", groups)
	}
	if groups[3].Labels[MemberMetaLabel] != groups[4].Labels[MemberMetaLabel] {
		t.Errorf("the probes of db.example:443 went to several members: %+v", groups)
	}
}

// With a token, every endpoint but /healthz and /ready answers 401 to a
// request without it or with another, and serves one with it.
func TestTheTokenGuardsEveryEndpointButHealthAndReadiness(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(2), testProbes)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	url := serve(t, a, "s3cret")
	for _, path := range []string{"/v1/bundle?member=r1", "/v1/sd/websites", "/metrics"} {
		for _, auth := range []string{"", "Bearer other", "s3cret", "Basic czNjcmV0"} {
			if resp, _ := get(t, url+path, "Authorization", auth); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s with %q: got %s, want 401 with WWW-Authenticate", path, auth, resp.Status)
			}
		}
		if resp, _ := get(t, url+path, "Authorization", "Bearer s3cret"); resp.StatusCode != http.StatusOK {
			t.Errorf("%s with the token: got %s", path, resp.Status)
		}
	}
	if resp, _ := post(t, url+"/v1/heartbeat", `{"member":"r1","advertise":"a:1","instance":"i","sequence":1}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a heartbeat without the token: got %s, want 401", resp.Status)
	}
	if resp, _ := post(t, url+"/v1/heartbeat", `{"member":"r1","advertise":"a:1","instance":"i","sequence":1}`, "Authorization", "Bearer s3cret"); resp.StatusCode != http.StatusOK {
		t.Errorf("a heartbeat with the token: got %s", resp.Status)
	}
	for _, path := range []string{"/healthz", "/ready"} {
		if resp, _ := get(t, url+path); resp.StatusCode != http.StatusOK {
			t.Errorf("%s without the token: got %s, want 200", path, resp.Status)
		}
	}
}

// /metrics counts the members, the targets by kind, what each member is
// allocated and the generation of its bundle.
func TestTheMetricsDescribeTheAllocation(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(9), testProbes)
	in := mustLoad(t, p)
	a, _ := settledAllocator(t, in, "r1", "r2")
	url := serve(t, a, "")
	resp, body := get(t, url+"/metrics")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("got %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	members := []string{"r1", "r2"}
	for _, want := range []string{
		"pue_allocator_members 2\n",
		`pue_allocator_targets{kind="static"} 9` + "\n",
		`pue_allocator_targets{kind="probe"} 2` + "\n",
		`pue_allocator_assigned{kind="static",member="r1"} ` + strconv.Itoa(len(owned(in, members, "r1"))) + "\n",
		`pue_allocator_assigned{kind="static",member="r2"} ` + strconv.Itoa(len(owned(in, members, "r2"))) + "\n",
		`pue_allocator_generation_info{generation="` + a.Bundle("r1").Generation + `",member="r1"} 1` + "\n",
		`pue_allocator_reloads_total{result="success"} 0` + "\n",
		"pue_allocator_member_changes_total 0\n",
		"# TYPE pue_allocator_reloads_total counter\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q:\n%s", want, body)
		}
	}
	probes := 0
	for _, m := range members {
		for _, line := range strings.Split(body, "\n") {
			if rest, ok := strings.CutPrefix(line, `pue_allocator_assigned{kind="probe",member="`+m+`"} `); ok {
				n, err := strconv.Atoi(rest)
				if err != nil {
					t.Fatal(err)
				}
				probes += n
			}
		}
	}
	if probes != 2 {
		t.Errorf("the members are allocated %d probe targets, want 2:\n%s", probes, body)
	}
}

// pue_allocator_targets_per_member is the targets allocated to the members,
// by kind, over the number of members: 9 static targets over two members are
// 4.5 each, whichever of them owns more; before the member list has formed it
// is 0.
func TestTheMetricsAverageTheTargetsPerMember(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(9), testProbes)
	in := mustLoad(t, p)
	unformed := New(in, Options{Settle: 15 * time.Second, Clock: newFakeClock(), Logger: testutil.QuietLogger(t)})
	_, body := get(t, serve(t, unformed, "")+"/metrics")
	for _, want := range []string{
		"# TYPE pue_allocator_targets_per_member gauge\n",
		`pue_allocator_targets_per_member{kind="probe"} 0` + "\n",
		`pue_allocator_targets_per_member{kind="static"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics with no member lacks %q:\n%s", want, body)
		}
	}
	a, _ := settledAllocator(t, in, "r1", "r2")
	_, body = get(t, serve(t, a, "")+"/metrics")
	for _, want := range []string{
		`pue_allocator_targets_per_member{kind="probe"} 1` + "\n",
		`pue_allocator_targets_per_member{kind="static"} 4.5` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q:\n%s", want, body)
		}
	}
}
