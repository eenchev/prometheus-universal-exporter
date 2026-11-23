package transform

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// log(message, level) writes a line of the exporter's log at the level it
// names, info when it names none, with the collector and the target, as
// the logs show one, without its credentials, in the order the
// script logged them and apart from what it printed, which stays the one
// debug line it was. A message is whatever it is as text, on as many lines
// as it has. A level that is none fails the script, naming the four.
func TestAPythonScriptLogsAtALevel(t *testing.T) {
	requirePython(t)
	run := func(t *testing.T, script string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(`{"depth": 7}`), Headers: http.Header{}, Target: "https://monitor:hunter2@queue.example.com"}
		_, err := executePython(WithRuleLogger(context.Background(), logger), "python3", script, &decode.Decoded{Kind: "json", Data: map[string]any{"depth": 7}, Raw: r.Body}, r, workerCollector("queue", script))
		return out.String(), err
	}
	got, err := run(t, `
print("before")
log("depth is %d" % data["depth"])
log("stalled\nfor a while", level="warning")
log({"depth": 7}, "error")
log("fine", level="debug")
print("after")
metric(name="queue_depth", value=data["depth"])
`)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`level=DEBUG msg="python transform printed" collector=queue output="before\nafter\n"`,
		`level=INFO msg="python transform logged" collector=queue target=https://redacted:redacted@queue.example.com message="depth is 7"`,
		`level=WARN msg="python transform logged" collector=queue target=https://redacted:redacted@queue.example.com message="stalled\nfor a while"`,
		`level=ERROR msg="python transform logged" collector=queue target=https://redacted:redacted@queue.example.com message="{'depth': 7}"`,
		`level=DEBUG msg="python transform logged" collector=queue target=https://redacted:redacted@queue.example.com message=fine`,
	}, "\n") + "\n"
	if strings.Contains(got, "hunter2") {
		t.Errorf("a logged line holds the target's password:\n%s", got)
	}
	if got != want {
		t.Errorf("logged\n got %s\nwant %s", got, want)
	}
	// Only logged: no line of what was printed, since nothing was.
	if got, err := run(t, `log("only")`); err != nil || got != `level=INFO msg="python transform logged" collector=queue target=https://redacted:redacted@queue.example.com message=only`+"\n" {
		t.Errorf("only a log line: %q, %v", got, err)
	}
	if _, err := run(t, `log("x", level="fatal")`); err == nil || !strings.Contains(err.Error(), `log level 'fatal' is not one of "debug", "info", "warning" and "error"`) {
		t.Errorf("an unknown level: %v", err)
	}
}

// What a script logged before it failed is written with its failure, in
// the order it logged it, more than twenty lines said as for a run that
// succeeds: they are often what explains the failure. What it printed is
// not, as before. The failure itself is what it was without them, and a
// failure whose line has no room for them under limits.max_output_bytes
// is written without them and whole.
func TestWhatAPythonScriptLoggedBeforeItFailedIsWritten(t *testing.T) {
	requirePython(t)
	run := func(script string, change func(*model.Collector)) (string, error) {
		var out bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
		c := workerCollector("queue", script)
		if change != nil {
			change(c)
		}
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(`{}`), Headers: http.Header{}}
		_, err := executePython(WithRuleLogger(context.Background(), logger), "python3", script, &decode.Decoded{Kind: "text", Data: "{}", Raw: r.Body}, r, c)
		return out.String(), err
	}
	failing := `print("printed first")
log("the queue answered 3 of 5 shards", "warning")
log("shard 4 timed out", level="error")
fail("not every shard answered")
`
	got, err := run(failing, nil)
	if err == nil || !strings.Contains(err.Error(), "RuntimeError: not every shard answered") {
		t.Fatalf("err=%v", err)
	}
	// What it printed goes with the failure, for whoever logs the failure
	// to write beside it, and is no line of its own.
	if printed := PrintedBeforeFailure(err); printed != "printed first\n" {
		t.Errorf("printed before the failure: %q", printed)
	}
	want := `level=WARN msg="python transform logged" collector=queue message="the queue answered 3 of 5 shards"` + "\n" +
		`level=ERROR msg="python transform logged" collector=queue message="shard 4 timed out"` + "\n"
	if got != want {
		t.Errorf("logged\n got %s\nwant %s", got, want)
	}
	// The failure is the same with the lines and without them.
	_, plain := run(`fail("not every shard answered")`, nil)
	if plain == nil || strings.ReplaceAll(err.Error(), "line 4", "line 1") != plain.Error() || PrintedBeforeFailure(plain) != "" {
		t.Errorf("the failure with lines logged before it\n%v\nis not the one without\n%v", err, plain)
	}
	// Another kind of failure, and more lines than a run writes.
	got, err = run("for i in range(25):\n    log('line %d' % i)\n1/0\n", nil)
	if err == nil || !strings.Contains(err.Error(), "ZeroDivisionError") || strings.Count(got, "\n") != 21 || !strings.Contains(got, `message="5 more lines the script logged are not written; a run writes 20"`) {
		t.Errorf("err=%v, logged\n%s", err, got)
	}
	// A run after a failure starts with none of its lines, and none of
	// what it printed.
	if got, err := run(`metric(name="v", value=1)`, nil); err != nil || got != "" {
		t.Errorf("the next run: %q, %v", got, err)
	}
	// What was printed alone, to both of the script's streams, and its
	// last 1,024 characters of more.
	got, err = run("import sys\nprint('shard 4 answered', 503)\nprint('to stderr', file=sys.stderr)\n1/0\n", nil)
	if printed := PrintedBeforeFailure(err); err == nil || !strings.Contains(err.Error(), "ZeroDivisionError") || printed != "shard 4 answered 503\nto stderr\n" || got != "" {
		t.Errorf("err=%v, printed %q, logged %q", err, printed, got)
	}
	got, err = run("print('x' * 5000 + 'the last line')\nlog('after it')\n1/0\n", nil)
	if printed := PrintedBeforeFailure(err); err == nil || printed != "(3990 more characters before) ..."+strings.Repeat("x", 1010)+"the last line\n" || !strings.Contains(got, `message="after it"`) {
		t.Errorf("err=%v, printed %.80q, logged\n%s", err, printed, got)
	}
	// What reads as a credential in it is masked: it is written to a log
	// nobody asked a script's output of.
	_, err = run("print('asking with token=s3cr3t-token and', 5, 'items')\n1/0\n", nil)
	if printed := PrintedBeforeFailure(err); printed != "asking with token=<redacted> and 5 items\n" {
		t.Errorf("err=%v, printed %q", err, printed)
	}
	// Where what was printed does not fit beside the failure, what was
	// logged still goes with it.
	got, err = run("print('x' * 3000)\nlog('the line')\nfail('boom')\n", func(c *model.Collector) { c.Limits.MaxOutputBytes = 1200 })
	if want := `level=INFO msg="python transform logged" collector=queue message="the line"` + "\n"; err == nil || !strings.Contains(err.Error(), "RuntimeError: boom") || got != want || PrintedBeforeFailure(err) != "" {
		t.Errorf("under an output limit with room for the lines alone: err=%v, logged\n got %s\nwant %s", err, got, want)
	}
	// Under an output limit the lines do not fit, the failure is whole.
	got, err = run("log('x' * 900)\nfail('boom')\n", func(c *model.Collector) { c.Limits.MaxOutputBytes = 400 })
	if err == nil || !strings.Contains(err.Error(), "RuntimeError: boom") || got != "" {
		t.Errorf("under a small output limit: err=%v, logged %q", err, got)
	}
}

// What a script prints takes no room of what it logs: a line logged after
// more was printed than is kept is written all the same. A run writes its
// first twenty lines and says how many more there were, and a line by its
// first 1,024 characters and how many more it had.
func TestWhatAPythonScriptLogsHasBoundsOfItsOwn(t *testing.T) {
	requirePython(t)
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	script := `print("p" * 5000)
log("x" * 1500, "error")
for i in range(24):
    log("line %d" % i)
`
	r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(`{}`), Headers: http.Header{}}
	if _, err := executePython(WithRuleLogger(context.Background(), logger), "python3", script, &decode.Decoded{Kind: "text", Data: "{}", Raw: r.Body}, r, workerCollector("queue", script)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 21 {
		t.Fatalf("%d lines at info and above, want the 20 and the one that says more were logged:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "level=ERROR") || !strings.Contains(lines[0], `message="`+strings.Repeat("x", 1024)+`... (476 more characters)"`) {
		t.Errorf("the long line: %s", lines[0])
	}
	if !strings.HasSuffix(lines[19], `message="line 18"`) || !strings.Contains(lines[20], "level=WARN") || !strings.HasSuffix(lines[20], `message="5 more lines the script logged are not written; a run writes 20"`) {
		t.Errorf("the last lines:\n%s\n%s", lines[19], lines[20])
	}
}

// response.ok is whether the status is a success: an HTTP status of 2xx,
// a grpc call's 0 and no other of its codes, and None for an answer that
// has no status, which is neither.
func TestAPythonScriptReadsWhetherTheStatusIsASuccess(t *testing.T) {
	requirePython(t)
	five := 5
	zero := 0
	for want, r := range map[string]*fetch.HTTPResponse{
		"True 200":  {StatusCode: 200},
		"True 204":  {StatusCode: 204},
		"False 199": {StatusCode: 199},
		"False 300": {StatusCode: 300},
		"False 503": {StatusCode: 503},
		"True 0":    {GRPCCode: &zero},
		"False 5":   {GRPCCode: &five},
		"None None": {NoStatus: true},
	} {
		r.Body, r.Headers = []byte(`{}`), http.Header{}
		c := workerCollector("c", `metric(name="m", value=1, labels={"ok": "%s %s" % (response.ok, response.status_code)})`)
		set, err := executePython(context.Background(), "python3", c.Transform.Script, &decode.Decoded{Kind: "text", Data: "{}", Raw: r.Body}, r, c)
		if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Labels["ok"] != want {
			t.Errorf("want %q, got %+v, %v", want, set, err)
		}
	}
}
