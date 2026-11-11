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
)

// log(message, level) writes a line of the exporter's log at the level it
// names, info when it names none, with the collector, in the order the
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
		r := &fetch.HTTPResponse{StatusCode: 200, Body: []byte(`{"depth": 7}`), Headers: http.Header{}}
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
		`level=INFO msg="python transform logged" collector=queue message="depth is 7"`,
		`level=WARN msg="python transform logged" collector=queue message="stalled\nfor a while"`,
		`level=ERROR msg="python transform logged" collector=queue message="{'depth': 7}"`,
		`level=DEBUG msg="python transform logged" collector=queue message=fine`,
	}, "\n") + "\n"
	if got != want {
		t.Errorf("logged\n got %s\nwant %s", got, want)
	}
	// Only logged: no line of what was printed, since nothing was.
	if got, err := run(t, `log("only")`); err != nil || got != `level=INFO msg="python transform logged" collector=queue message=only`+"\n" {
		t.Errorf("only a log line: %q, %v", got, err)
	}
	if _, err := run(t, `log("x", level="fatal")`); err == nil || !strings.Contains(err.Error(), `log level 'fatal' is not one of "debug", "info", "warning" and "error"`) {
		t.Errorf("an unknown level: %v", err)
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
