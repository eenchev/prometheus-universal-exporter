package repository

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ciRun is a run of ci.yml as the GitHub API lists it, with the fields
// tools/ci-passed.py reads.
type ciRun struct {
	ID         int    `json:"id"`
	Event      string `json:"event"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	UpdatedAt  string `json:"updated_at"`
	// Suite is the conclusion of the run's step that runs the suite, as its
	// jobs list it: success, or skipped for a run whose changed paths ran
	// none of the Go steps.
	Suite string `json:"-"`
}

// ciSuiteStep is the name tools/ci-passed.py looks for among a run's steps.
const ciSuiteStep = "Run the repository's checks"

const ciPassedSHA = "0123456789abcdef0123456789abcdef01234567"

// pushRun is a run a push of ciPassedSHA started.
func pushRun(id int, status, conclusion, updated string) ciRun {
	return ciRun{ID: id, Event: "push", HeadSHA: ciPassedSHA, Status: status, Conclusion: conclusion, UpdatedAt: updated, Suite: "success"}
}

// skippedRun is a run a push of ciPassedSHA started that concluded success
// with every step after the setup skipped.
func skippedRun(id int, updated string) ciRun {
	run := pushRun(id, "completed", "success", updated)
	run.Suite = "skipped"
	return run
}

// fakeRunsAPI answers the runs of ci.yml for ciPassedSHA, one answer per
// request in turn and the last one from then on; an answer of nil is a 500.
// It answers the jobs of a run of the answer last given, their steps those
// of ci.yml's job test with the suite's concluded as the run says. It
// refuses a request that is not one the script is meant to make.
type fakeRunsAPI struct {
	t        *testing.T
	answers  [][]ciRun
	mu       sync.Mutex
	requests int
	last     []ciRun
}

var jobsPath = regexp.MustCompile(`^/repos/owner/name/actions/runs/([0-9]+)/jobs$`)

func (api *fakeRunsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if match := jobsPath.FindStringSubmatch(r.URL.Path); match != nil {
		api.serveJobs(w, r, match[1])
		return
	}
	api.mu.Lock()
	index := min(api.requests, len(api.answers)-1)
	api.requests++
	api.last = api.answers[index]
	api.mu.Unlock()
	query := r.URL.Query()
	if r.URL.Path != "/repos/owner/name/actions/workflows/ci.yml/runs" || query.Get("head_sha") != ciPassedSHA || query.Get("event") != "push" {
		api.t.Errorf("tools/ci-passed.py asked for %s", r.URL)
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		api.t.Errorf("tools/ci-passed.py sent the Authorization %q", r.Header.Get("Authorization"))
	}
	answer := api.answers[index]
	if answer == nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(answer), "workflow_runs": answer})
}

func (api *fakeRunsAPI) serveJobs(w http.ResponseWriter, r *http.Request, id string) {
	api.mu.Lock()
	runs := api.last
	api.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		api.t.Errorf("tools/ci-passed.py sent the Authorization %q", r.Header.Get("Authorization"))
	}
	for _, run := range runs {
		if strconv.Itoa(run.ID) != id {
			continue
		}
		if run.Status != "completed" || run.Conclusion != "success" {
			api.t.Errorf("tools/ci-passed.py asked for the jobs of run %d, which did not succeed and counts whatever it ran", run.ID)
		}
		step := func(name, conclusion string) map[string]any {
			return map[string]any{"name": name, "status": "completed", "conclusion": conclusion}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []map[string]any{{
			"name": "test", "status": "completed", "conclusion": "success",
			"steps": []map[string]any{
				step("Set up job", "success"),
				step("Run actions/checkout", "success"),
				step("Run dorny/paths-filter", "success"),
				step("Run every step on a tag push", "success"),
				step("Run actions/setup-go", run.Suite),
				step(ciSuiteStep, run.Suite),
				step("Complete job", "success"),
			},
		}}})
		return
	}
	api.t.Errorf("tools/ci-passed.py asked for the jobs of run %s, which the runs it was given do not hold", id)
	http.NotFound(w, r)
}

func (api *fakeRunsAPI) count() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.requests
}

// runCIPassed runs tools/ci-passed.py against handler with the timeout given
// in seconds and a short interval, and returns its output and whether it
// succeeded.
func runCIPassed(t *testing.T, handler http.Handler, timeout string) (string, bool) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	cmd := exec.Command(python, "tools/ci-passed.py", "--repository", "owner/name", "--sha", ciPassedSHA, "--timeout", timeout, "--interval", "0.01") // #nosec G204 -- the test's own arguments
	// A proxy of the machine's must not stand between the script and the
	// stand-in API.
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.EqualFold(name, "http_proxy") && !strings.EqualFold(name, "https_proxy") && !strings.EqualFold(name, "all_proxy") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GITHUB_API_URL="+server.URL, "GITHUB_TOKEN=test-token")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatalf("tools/ci-passed.py did not run: %v", err)
		}
	}
	return string(out), err == nil
}

// The release workflows publish only once tools/ci-passed.py has found CI
// passed on the tagged commit: the newest finished ci.yml run a push of the
// commit started, and that ran the suite, concluded success. A run that
// succeeded with every step skipped, as a push whose changed paths ran none
// of the Go steps does, takes back no failure before it. It fails at once when every run has
// finished and the newest did not succeed, waits while a run is queued or
// running or none has started, gives up with the same advice when the wait
// runs out, and counts no run a pull request started, which tested a merge
// rather than the commit.
func TestTheCIGateOfTheReleasesPassesOnlyWhenCIPassedOnTheCommit(t *testing.T) {
	const advice = "CI has not passed on " + ciPassedSHA + ": run CI or wait for it, then push the tag again"
	pullRequest := ciRun{ID: 9, Event: "pull_request", HeadSHA: ciPassedSHA, Status: "completed", Conclusion: "success", UpdatedAt: "2026-10-14T10:09:00Z"}
	for _, test := range []struct {
		name     string
		answers  [][]ciRun
		timeout  string
		passes   bool
		want     string
		requests int // 0 for any number
	}{
		{
			name:    "a run concluded success",
			answers: [][]ciRun{{pushRun(1, "completed", "success", "2026-10-14T10:00:00Z")}},
			timeout: "60", passes: true, want: "CI has passed on " + ciPassedSHA, requests: 1,
		},
		{
			name:    "the only run failed",
			answers: [][]ciRun{{pushRun(1, "completed", "failure", "2026-10-14T10:00:00Z")}},
			timeout: "60", want: advice, requests: 1,
		},
		{
			name:    "a run was cancelled",
			answers: [][]ciRun{{pushRun(1, "completed", "cancelled", "2026-10-14T10:00:00Z")}},
			timeout: "60", want: "concluded cancelled", requests: 1,
		},
		{
			name: "running, then success",
			answers: [][]ciRun{
				{pushRun(1, "queued", "", "2026-10-14T10:00:00Z")},
				{pushRun(1, "in_progress", "", "2026-10-14T10:01:00Z")},
				{pushRun(1, "completed", "success", "2026-10-14T10:20:00Z")},
			},
			timeout: "60", passes: true, want: "waiting for CI on " + ciPassedSHA, requests: 3,
		},
		{
			name: "the branch's run passed while the tag's own still runs",
			answers: [][]ciRun{{
				pushRun(2, "in_progress", "", "2026-10-14T10:05:00Z"),
				pushRun(1, "completed", "success", "2026-10-14T10:00:00Z"),
			}},
			timeout: "60", passes: true, requests: 1,
		},
		{
			name: "a run started again after a failure passed",
			answers: [][]ciRun{{
				pushRun(1, "completed", "failure", "2026-10-14T10:00:00Z"),
				pushRun(2, "completed", "success", "2026-10-14T10:30:00Z"),
			}},
			timeout: "60", passes: true, requests: 1,
		},
		{
			name: "the newest run failed after one passed",
			answers: [][]ciRun{{
				pushRun(1, "completed", "success", "2026-10-14T10:00:00Z"),
				pushRun(2, "completed", "failure", "2026-10-14T10:30:00Z"),
			}},
			timeout: "60", want: "concluded failure", requests: 1,
		},
		{
			name: "a failure is waited past while another run goes on",
			answers: [][]ciRun{
				{pushRun(1, "completed", "failure", "2026-10-14T10:00:00Z"), pushRun(2, "in_progress", "", "2026-10-14T10:01:00Z")},
				{pushRun(1, "completed", "failure", "2026-10-14T10:00:00Z"), pushRun(2, "completed", "success", "2026-10-14T10:30:00Z")},
			},
			timeout: "60", passes: true, requests: 2,
		},
		{
			name: "a run that skipped the suite takes back no failure before it",
			answers: [][]ciRun{{
				pushRun(1, "completed", "failure", "2026-10-14T10:00:00Z"),
				skippedRun(2, "2026-10-14T10:30:00Z"),
			}},
			timeout: "60", want: "concluded failure", requests: 1,
		},
		{
			name: "a run that ran the suite takes back a failure past one that skipped it",
			answers: [][]ciRun{{
				pushRun(1, "completed", "failure", "2026-10-14T10:00:00Z"),
				skippedRun(2, "2026-10-14T10:30:00Z"),
				pushRun(3, "completed", "success", "2026-10-14T10:40:00Z"),
			}},
			timeout: "60", passes: true, want: "concluded success", requests: 1,
		},
		{
			name: "a run that skipped the suite passes nothing while the tag's own runs",
			answers: [][]ciRun{
				{skippedRun(1, "2026-10-14T10:00:00Z"), pushRun(2, "in_progress", "", "2026-10-14T10:01:00Z")},
				{skippedRun(1, "2026-10-14T10:00:00Z"), pushRun(2, "completed", "failure", "2026-10-14T10:30:00Z")},
			},
			timeout: "60", want: "concluded failure", requests: 2,
		},
		{
			name:    "every run skipped the suite",
			answers: [][]ciRun{{skippedRun(1, "2026-10-14T10:00:00Z"), skippedRun(2, "2026-10-14T10:10:00Z")}},
			timeout: "60", want: "no ci.yml run of a push of this commit ran the suite", requests: 1,
		},
		{
			name:    "no run found",
			answers: [][]ciRun{{}},
			timeout: "0.2", want: "still waiting after 0.2 seconds: no ci.yml run of a push of this commit yet",
		},
		{
			name:    "only a pull request's run passed",
			answers: [][]ciRun{{pullRequest}},
			timeout: "0.2", want: "no ci.yml run of a push of this commit yet",
		},
		{
			name:    "still running when the wait runs out",
			answers: [][]ciRun{{pushRun(1, "in_progress", "", "2026-10-14T10:00:00Z")}},
			timeout: "0.2", want: advice + " or re-run this release (still waiting after 0.2 seconds: 1 ci.yml run(s) not finished (in_progress))",
		},
		{
			name:    "the API fails for a while",
			answers: [][]ciRun{nil, nil, {pushRun(1, "completed", "success", "2026-10-14T10:00:00Z")}},
			timeout: "60", passes: true, want: "the GitHub API could not be read", requests: 3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeRunsAPI{t: t, answers: test.answers}
			out, passed := runCIPassed(t, api, test.timeout)
			if passed != test.passes {
				t.Errorf("tools/ci-passed.py passed=%t, want %t:\n%s", passed, test.passes, out)
			}
			if !strings.Contains(out, test.want) {
				t.Errorf("tools/ci-passed.py does not say %q:\n%s", test.want, out)
			}
			if test.requests != 0 && api.count() != test.requests {
				t.Errorf("tools/ci-passed.py asked %d times, want %d:\n%s", api.count(), test.requests, out)
			}
		})
	}
}

// An API that refuses the request, as it does a token without actions: read
// or a repository that does not exist, fails the gate at once, saying what to
// give the job, rather than being asked again until the wait runs out.
func TestTheCIGateOfTheReleasesFailsAtOnceWhenTheAPIRefusesIt(t *testing.T) {
	var requests int
	var mu sync.Mutex
	refuse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	})
	out, passed := runCIPassed(t, refuse, "60")
	if passed {
		t.Fatalf("tools/ci-passed.py passed on a refusal:\n%s", out)
	}
	if !strings.Contains(out, "answered 403") || !strings.Contains(out, "`actions: read`") {
		t.Errorf("tools/ci-passed.py does not say the API refused it and what to give the job:\n%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Errorf("tools/ci-passed.py asked %d times after a refusal, want 1", requests)
	}
}

// tools/ci-passed.py finds by its name the step of ci.yml that runs whenever
// the Go suite does, so the step has to be there under that name, in the job
// test, running the repository's checks under the go output: renamed, the
// gate would find no run that ran the suite and fail every release; moved
// under another output, it would take a run that ran only part of CI.
func TestTheCIGateFindsTheSuiteStepOfCI(t *testing.T) {
	script := read(t, "tools/ci-passed.py")
	if !strings.Contains(script, "\nSUITE_STEP = "+strconv.Quote(ciSuiteStep)+"\n") {
		t.Errorf("tools/ci-passed.py does not look for the step %q", ciSuiteStep)
	}
	index := slices.IndexFunc(workflowSteps(t, ".github/workflows/ci.yml")["test"], func(step map[string]any) bool {
		return step["name"] == ciSuiteStep
	})
	if index < 0 {
		t.Fatalf("ci.yml's job test has no step %q", ciSuiteStep)
	}
	step := workflowSteps(t, ".github/workflows/ci.yml")["test"][index]
	if step["run"] != "go test ./test/repository/" || step["if"] != sideCondition("go") {
		t.Errorf("ci.yml's step %q runs %v under %v, not the repository's checks under the go output", ciSuiteStep, step["run"], step["if"])
	}
}
