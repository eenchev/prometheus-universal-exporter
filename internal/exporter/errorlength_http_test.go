//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// errorLengthServer is an exporter of one collector, named c, written as
// YAML, with its log in a buffer at debug level and debug probes enabled.
func errorLengthServer(t *testing.T, collector string) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", "collectors:\n  - "+collector+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	server.SetProbeDebug(true)
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return server, logs
}

// refusingAddress is an address nothing listens on: that of a listener that
// has been closed.
func refusingAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// rawTarget answers every connection with what answer gives, written as it
// is, and closes it: a target that does not speak HTTP.
func rawTarget(t *testing.T, answer func() string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				request := make([]byte, 4096)
				if _, err := conn.Read(request); err != nil {
					return
				}
				_, _ = conn.Write([]byte(answer()))
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// errorLengthCase is a probe that fails at one stage with an error made of
// something as large as its target, or its scraper, makes it.
type errorLengthCase struct {
	// collector is the collector c in YAML, and answer the target's answer
	// when what the failure is made of is size bytes; a target that is no
	// HTTP server is raw.
	collector string
	answer    func(w http.ResponseWriter, size int)
	raw       func(size int) string
	// target is the probe's target, when it is not the test server.
	target func(base string, size int) string
	// stage and code are where the probe fails and what it answers, and msg
	// the log line of the failure; a rule that carries on fails at no
	// stage.
	stage string
	code  int
	msg   string
	// starts is how the error starts, and ends how it ends when a site cut
	// the value it quotes and what the error says after it is whole; an
	// error cut by the bound of every failure ends with its length.
	starts, ends string
	// most is the largest size the failure can be made of, when that is
	// under the test's.
	most int
}

// The largest failure each stage of a probe can be made to end with is
// answered, logged and remembered in no more than the 2,000 bytes of any
// failure: with a response, a redirect, a script's exception or a rule's
// error of ten megabytes, or as large as the stage lets one be, the answer
// to the scraper is the error after the collector and the stage, the log
// line's error is the error, and what the failure log remembers it by is no
// longer. They were as long as what the error quoted, megabytes each, the
// remembered text kept for as long as the failure repeated. Where the error
// says why after the value it quotes — a URL, a metric's name, a script's
// exception — the reason is whole, the value shown by its start; any other
// error starts as it did and ends with its length. Half as large, the same
// failure is a repeat to the log, at debug level, recognised by the same
// text; and a debug probe's report names the failure in a line no longer.
// Under the race detector the failures are made of 256 kB, a hundred times
// the bound.
func TestTheLargestFailureOfEveryStageIsAnsweredLoggedAndRememberedWithinTheBound(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	refusing := refusingAddress(t)
	redirectTo := func(location string) func(http.ResponseWriter, int) {
		return func(w http.ResponseWriter, size int) {
			w.Header().Set("Location", location+strings.Repeat("L", size))
			w.WriteHeader(http.StatusFound)
		}
	}
	body := func(contentType string, made func(size int) string) func(http.ResponseWriter, int) {
		return func(w http.ResponseWriter, size int) {
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write([]byte(made(size)))
		}
	}
	message := func(size int) string { return `{"n": 1, "message": "` + strings.Repeat("a", size) + `"}` }
	const jq = `request: {type: http}, decoder: {type: json}, transform: {type: jq}`
	const failed = "probe failed"
	cases := map[string]errorLengthCase{
		"http, a scraper's long target": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}]}`,
			target:    func(_ string, size int) string { return "http://" + refusing + "/" + strings.Repeat("p", size) },
			stage:     "http", code: http.StatusBadGateway, msg: failed, most: MaxProbeParameterBytes - 100,
			starts: `HTTP request failed: Get "http://` + refusing + `/ppp`, ends: ` bytes): dial tcp ` + refusing + `: connect: connection refused`,
		},
		"http, a redirect's long Location": {
			collector: `{name: c, request: {type: http, follow_redirects: true}, decoder: {type: json}, transform: {type: jq}, metrics: [{name: m, expression: .n}]}`,
			answer:    redirectTo("http://" + refusing + "/"),
			stage:     "http", code: http.StatusBadGateway, msg: failed, most: 900 << 10,
			starts: `HTTP request failed: Get "http://` + refusing + `/LLL`, ends: ` bytes): dial tcp ` + refusing + `: connect: connection refused`,
		},
		"http, an answer that is no HTTP": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}]}`,
			raw:       func(size int) string { return strings.Repeat("\x01", size) + "\r\n\r\n" },
			stage:     "http", code: http.StatusBadGateway, msg: failed, most: 200 << 10,
			starts: `HTTP request failed: Get "http://127.0.0.1:`,
		},
		"target_policy, a redirect's long Location": {
			collector: `{name: c, request: {type: http, follow_redirects: true, denied_targets: ["10.0.0.0/8"]}, decoder: {type: json}, transform: {type: jq}, metrics: [{name: m, expression: .n}]}`,
			answer:    redirectTo("http://10.1.2.3/"),
			stage:     "target_policy", code: http.StatusForbidden, msg: failed, most: 900 << 10,
			starts: `HTTP request failed: Get "http://10.1.2.3/LLL`, ends: ` bytes): target 10.1.2.3 refused: its address 10.1.2.3 is in 10.0.0.0/8 in request.denied_targets`,
		},
		"http_status, an error page": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}]}`,
			answer: func(w http.ResponseWriter, size int) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(strings.Repeat("e", size)))
			},
			stage: "http_status", code: http.StatusBadGateway, msg: failed, starts: "received HTTP status 500", ends: "received HTTP status 500",
		},
		"decode, a label value never closed": {
			collector: `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}}`,
			answer:    body("text/plain", func(size int) string { return `m{a="` + strings.Repeat("a", size) + "\n" }),
			stage:     "decode", code: http.StatusBadGateway, msg: failed,
			starts: `decoding Prometheus exposition: text format parsing error in line 1: label value "aaa`, ends: ` bytes) contains unescaped new-line`,
		},
		"validation, a metric's long name": {
			collector: `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}}`,
			answer:    body("text/plain", func(size int) string { return "h" + strings.Repeat("a", size) + " 1\n" }),
			stage:     "validation", code: http.StatusBadGateway, msg: failed,
			starts: `invalid metric name "haaa`, ends: ` bytes): longer than limits.max_metric_name_length 200`,
		},
		"transform, a script's exception": {
			collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "raise Exception(data['message'])"}, limits: {max_output_bytes: 64MiB}}`,
			answer:    body("application/json", message),
			stage:     "transform", code: http.StatusBadGateway, msg: failed,
			starts: "python transform failed: Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    raise Exception(data['message'])\nException: aaa", ends: " bytes)",
		},
		"transform, a pre-script's exception": {
			collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: jq, pre_script: "raise Exception(data['message'])"}, metrics: [{name: m, expression: .n}], limits: {max_output_bytes: 64MiB}}`,
			answer:    body("application/json", message),
			stage:     "transform", code: http.StatusBadGateway, msg: failed, most: 1 << 20,
			starts: "python pre-script failed: Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    raise Exception(data['message'])\nException: aaa", ends: " bytes)",
		},
		"transform, labels that escape alike": {
			collector: `{name: c, name_escaping: underscores, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}}`,
			answer: body("text/plain", func(size int) string {
				label := strings.Repeat("l", size)
				return `{"m", "` + label + `.x"="1", "` + label + `-x"="2"} 1` + "\n"
			}),
			stage: "transform", code: http.StatusBadGateway, msg: failed, most: 3 << 20,
			starts: `series m: labels "lll`,
		},
		"metric, a rule that raises under fail": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: 'error(.message)', error_mode: fail}]}`,
			answer:    body("application/json", message),
			stage:     "metric", code: http.StatusBadGateway, msg: failed,
			starts: `metric "m" expression: error: aaa`,
		},
		"a rule that raises under log": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: 'error(.message)', error_mode: log}, {name: n, expression: .n}]}`,
			answer:    body("application/json", message),
			code:      http.StatusOK, msg: "metric extraction failed",
			starts: `error: aaa`,
		},
		"transform, carried on under error_handling log": {
			collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "raise Exception(data['message'])"}, error_handling: {on_transform_error: log}, limits: {max_output_bytes: 64MiB}}`,
			answer:    body("application/json", message),
			stage:     "transform", code: http.StatusOK, msg: "probe stage failed; continuing", most: 1 << 20,
			starts: "python transform failed: Traceback (most recent call last):\n", ends: " bytes)",
		},
	}
	largest := alloctest.UnlessRaced(10<<20, 1<<18) - 200
	for name, tc := range cases {
		server, logs := errorLengthServer(t, tc.collector)
		var size atomic.Int64
		base := ""
		switch {
		case tc.raw != nil:
			base = rawTarget(t, func() string { return tc.raw(int(size.Load())) })
		case tc.answer != nil:
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tc.answer(w, int(size.Load())) }))
			t.Cleanup(target.Close)
			base = target.URL
		}
		whole := largest
		if tc.most > 0 {
			whole = min(whole, tc.most)
		}
		// probe fails the probe with a failure made of so many bytes, and
		// returns its error as the log line has it and the line.
		probe := func(of int) (string, map[string]any) {
			t.Helper()
			size.Store(int64(of))
			target := base
			if tc.target != nil {
				target = tc.target(base, of)
			}
			answer := probeOnce(t, server, probePath("c", target, ""), nil)
			lines := linesOf(t, logs, tc.msg)
			if len(lines) != 1 {
				t.Fatalf("%s: %d lines say %q", name, len(lines), tc.msg)
			}
			text, _ := lines[0]["error"].(string)
			if answer.Code != tc.code || tc.stage != "" && lines[0]["stage"] != tc.stage {
				t.Fatalf("%s: answered %d at stage %v, %.300q, want %d at %s", name, answer.Code, lines[0]["stage"], answer.Body, tc.code, tc.stage)
			}
			if len(text) > model.MaxFailureBytes || !strings.HasPrefix(text, tc.starts) {
				t.Fatalf("%s: a failure made of %d bytes is logged in %d, %.200q ... %q, want at most %d that start %q", name, of, len(text), text, text[max(0, len(text)-80):], model.MaxFailureBytes, tc.starts)
			}
			// The answer is the error and what the probe always wrote
			// around it: the collector and the stage, or, for a rule
			// under fail, the JSON object that names them.
			switch {
			case tc.code == http.StatusOK:
			case tc.stage == "metric":
				var said probeError
				if err := json.Unmarshal(answer.Body.Bytes(), &said); err != nil || said.Error != text || said.Stage != "metric" || said.Metric != "m" {
					t.Fatalf("%s: answered %.300q, want the error in a JSON object: %v", name, answer.Body, err)
				}
			case tc.stage == "target_policy":
				if got := answer.Body.String(); got != "collector c refused the target: "+text+"\n" {
					t.Fatalf("%s: answered %d bytes, %.300q", name, len(got), got)
				}
			default:
				if got := answer.Body.String(); got != "collector c "+tc.stage+" failed: "+text+"\n" {
					t.Fatalf("%s: answered %d bytes, %.300q", name, len(got), got)
				}
			}
			for _, kept := range rememberedTexts(server) {
				if len(kept) > model.MaxFailureBytes {
					t.Fatalf("%s: the failure log remembers the failure by %d bytes, %.200q", name, len(kept), kept)
				}
			}
			return text, lines[0]
		}
		text, line := probe(whole)
		if line["repeat"] != nil || line["level"] == "DEBUG" {
			t.Errorf("%s: the first failure is logged as %v at %v", name, line["repeat"], line["level"])
		}
		switch {
		case tc.ends != "":
			if !strings.HasSuffix(text, tc.ends) {
				t.Errorf("%s: the error ends %q, want what it says after the value, %q", name, text[max(0, len(text)-120):], tc.ends)
			}
		case len(text) != model.MaxFailureBytes || !strings.HasSuffix(text, " bytes)") || !strings.Contains(text[len(text)-30:], "... ("):
			t.Errorf("%s: the error is %d bytes and ends %q, want %d ending with its length", name, len(text), text[max(0, len(text)-60):], model.MaxFailureBytes)
		}
		// A debug probe says the same in a line no longer.
		target := base
		if tc.target != nil {
			target = tc.target(base, whole)
		}
		report := debugProbeGet(t, server, strings.TrimPrefix(probePath("c", target, "&debug=true"), "/probe?")).Body.String()
		start, _, _ := strings.Cut(text, "\n")
		start = start[:min(len(start), 60)]
		named := 0
		for _, said := range strings.Split(report, "\n") {
			if strings.Contains(said, start) {
				if named++; len(said) > model.MaxFailureBytes+300 {
					t.Errorf("%s: the debug report names the failure in a line of %d bytes, %.200q", name, len(said), said)
				}
			}
		}
		if named == 0 {
			t.Errorf("%s: the debug report does not name the failure, %q:\n%.2000s", name, start, report)
		}
		logs.Reset()
		// Half as large, the failure is the one the log remembers.
		if tc.starts == tc.ends {
			continue
		}
		again, line := probe(whole / 2)
		if tc.target != nil {
			// Another target is another probe to the log, with a failure
			// of its own: the two are remembered by one text.
			if kept := rememberedTexts(server); len(kept) != 2 || kept[0] != kept[1] || again == text {
				t.Errorf("%s: half as large, the failure is remembered by %.300q, in the same text %t; want the two targets' failures remembered by one text, each logged with its own length", name, kept, again == text)
			}
			continue
		}
		if line["repeat"] != true || line["level"] != "DEBUG" || again == text {
			t.Errorf("%s: half as large, the failure is logged as a repeat %v at %v, in the same text %t; want a repeat at debug level with its own length", name, line["repeat"], line["level"], again == text)
		}
	}
}

// shortFailure is a probe's failure as the stages make it, with what the
// probe answered, logged and remembered of it before any error was bounded:
// the stages are run here as collect runs them, and the answer is worded as
// probeTrip words it.
func shortFailure(ctx context.Context, t *testing.T, c *model.Collector, target string) (stage, answer, logged, remembered string) {
	t.Helper()
	fail := func(stage string, err error) (string, string, string, string) {
		return stage, fmt.Sprintf("collector %s %s failed: %v\n", c.Name, stage, err), err.Error(), model.SameFailureText(err)
	}
	response, err := fetch.FetchCollector(ctx, target, c, fetch.RequestOverrides{}, nil)
	if err != nil {
		return fail(fetch.FetchErrorStage(c, err), err)
	}
	if !fetch.AcceptedStatus(c, fetch.RequestOverrides{}, response.StatusCode) {
		return fail("http_status", fmt.Errorf("received HTTP status %d", response.StatusCode))
	}
	decoded, err := decode.Decode(response, c)
	if err != nil {
		return fail("decode", err)
	}
	set, err := transform.Transform(transform.LeaveRuleLoggingToCaller(ctx), decoded, response, c, "python3")
	var rule *transform.MetricFailure
	switch {
	case errors.As(err, &rule):
		said, encodeErr := json.Marshal(probeError{Status: "error", Stage: "metric", Collector: c.Name, Metric: rule.Metric, Target: target, Error: err.Error()})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		return "metric", string(said) + "\n", err.Error(), model.SameFailureText(err)
	case err != nil:
		return fail("transform", err)
	}
	if err := set.Validate(c.Limits); err != nil {
		return fail("validation", err)
	}
	t.Fatalf("the probe of %s does not fail", target)
	return "", "", "", ""
}

// A failure within the bound is answered, logged and remembered as it was,
// to the letter, at whichever stage the probe fails: for generated failures
// of the fetch, of an unaccepted status, of each decoder's kind, of a rule
// under fail, of a script, of a transform and of the validation, each made
// of a value of up to 150 bytes — a path, a label, a message, a name — the
// answer to the scraper, the log line's error and the text the failure log
// remembers are what the stage's own error gave before any was bounded
// (shortFailure).
func TestAShortFailureOfAnyStageIsAnsweredLoggedAndRememberedAsItWas(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	refusing := refusingAddress(t)
	var body atomic.Pointer[string]
	var status atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(target.Close)
	const jq = `request: {type: http}, decoder: {type: json}, transform: {type: jq}`
	kinds := []struct {
		collector string
		body      func(value string) string
		refused   bool
		status    int
		// says is what every failure of the kind says, where the kind is
		// of one failure that must not be another's.
		says string
	}{
		{collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}]}`, refused: true},
		{collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}]}`, body: func(value string) string { return value }, status: http.StatusServiceUnavailable},
		{collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}]}`, body: func(value string) string { return `{"n": ` + value }},
		{collector: `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}}`, body: func(value string) string { return "ok 1\nm{a=\"" + value + "\n" }},
		{collector: `{name: c, request: {type: http}, decoder: {type: yaml}, transform: {type: yq}, metrics: [{name: m, expression: .n}]}`, body: func(value string) string { return "n: 1\n? [" + value + "]\n: 2\n" }},
		{collector: `{name: c, request: {type: http}, decoder: {type: csv}, transform: {type: csv}, metrics: [{name: m, expression: n}]}`, body: func(value string) string { return "n," + value + "," + value + "\n1,2,3\n" }},
		{collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: 'error(.message)', error_mode: fail}]}`, body: func(value string) string { return fmt.Sprintf(`{"message": %q}`, value) }},
		{collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .message, error_mode: fail}]}`, body: func(value string) string { return fmt.Sprintf(`{"message": %q}`, value) }},
		{collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "raise ValueError(data['message'])"}}`, body: func(value string) string { return fmt.Sprintf(`{"message": %q}`, value) }},
		{collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: jq, pre_script: "data = data['message'] + 1"}, metrics: [{name: m, expression: .n}]}`, body: func(value string) string { return fmt.Sprintf(`{"message": %q}`, value) }},
		{collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "metric(data['message'], 1.5)"}}`, body: func(value string) string { return fmt.Sprintf(`{"message": %q}`, "bad name "+value) }},
		// The value is named: metric's second argument is its type.
		{collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "metric('m', value=1, labels={data['message']: 'v'})"}}`, body: func(value string) string { return fmt.Sprintf(`{"message": %q}`, "bad label "+value) }, says: "which is not a classic Prometheus label name"},
		{collector: `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}, limits: {max_metric_name_length: 5}}`, body: func(value string) string { return "metric_" + strings.Repeat("n", len(value)) + " 1\n" }},
		{collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n, labels: [{name: l, expression: .message}]}], limits: {max_label_value_length: 3}}`, body: func(value string) string { return fmt.Sprintf(`{"n": 1, "message": %q}`, "long"+value) }},
		{collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}, {name: m, expression: '.n + 0'}]}`, body: func(value string) string { return fmt.Sprintf(`{"n": 1, "message": %q}`, value) }},
		{collector: `{name: c, name_escaping: underscores, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}}`, body: func(value string) string {
			return `{"m", "l` + strings.ReplaceAll(value, `"`, "") + `.x"="1", "l` + strings.ReplaceAll(value, `"`, "") + `-x"="2"} 1` + "\n"
		}},
	}
	random := rand.New(rand.NewPCG(36, 6))
	alphabet := []string{"a", "b", "é", "日", " ", `"`, "7", "x/y", "%41"}
	stages := map[string]int{}
	for i, kind := range kinds {
		server, logs := errorLengthServer(t, kind.collector)
		collector := server.manager.Get().Collectors[0]
		for round := range alloctest.UnlessRaced(40, 8) {
			var value strings.Builder
			for size := random.IntN(150); value.Len() < size; {
				value.WriteString(alphabet[random.IntN(len(alphabet))])
			}
			address := target.URL + "/" + strconv.Itoa(round)
			status.Store(http.StatusOK)
			switch {
			case kind.refused:
				address = "http://" + refusing + "/" + strings.ReplaceAll(strings.ReplaceAll(value.String(), " ", "+"), `"`, "q")
			default:
				if kind.status != 0 {
					status.Store(int64(kind.status))
				}
				answer := kind.body(value.String())
				body.Store(&answer)
			}
			stage, answer, logged, remembered := shortFailure(t.Context(), t, &collector, address)
			if !strings.Contains(logged, kind.says) {
				t.Fatalf("kind %d, %q: the probe fails at the %s stage with %q, not with %q", i, value.String(), stage, logged, kind.says)
			}
			if len(logged) > model.MaxFailureBytes {
				t.Fatalf("kind %d: a value of %d bytes makes an error of %d", i, value.Len(), len(logged))
			}
			stages[stage]++
			// The log forgets, so that this failure is logged as a new
			// one whatever the one before it was.
			server.failures.forgetCollectors(map[string]bool{"c": true})
			got := probeOnce(t, server, probePath("c", address, ""), nil)
			lines := linesOf(t, logs, "probe failed")
			if len(lines) != 1 || lines[0]["stage"] != stage || lines[0]["error"] != logged {
				t.Fatalf("kind %d, %q: the failure is logged as %.600v, want the %s stage's %q", i, value.String(), lines, stage, logged)
			}
			if got.Code != http.StatusBadGateway || got.Body.String() != answer {
				t.Fatalf("kind %d, %q: answered %d %q, and was %q", i, value.String(), got.Code, got.Body, answer)
			}
			if kept := rememberedTexts(server); len(kept) != 1 || kept[0] != remembered {
				t.Fatalf("kind %d, %q: remembered by %q, and was by %q", i, value.String(), kept, remembered)
			}
		}
	}
	for _, stage := range []string{"http", "http_status", "decode", "metric", "transform", "validation"} {
		if stages[stage] < alloctest.UnlessRaced(40, 8) {
			t.Errorf("%d failures were of the %s stage: %v", stages[stage], stage, stages)
		}
	}
}

// A static target's scrape that fails is logged and remembered in no more
// than the 2,000 bytes of any failure, at a stage of its trip and at one
// before it: a script that raises its response of megabytes fails the
// transform in an error that is whole, the exception by its start, and a
// credential file whose path is three kilobytes that cannot be read fails
// the credentials stage, which the trip never sees, in an error of 2,000
// bytes that starts as it did and ends with its length. They were logged
// and remembered whole, ten megabytes and three kilobytes.
func TestAStaticTargetsFailureIsLoggedAndRememberedWithinTheBound(t *testing.T) {
	requirePython(t)
	testutil.CaptureLogs(t)
	message := `{"message": "` + strings.Repeat("a", alloctest.UnlessRaced(10<<20, 1<<20)-200) + `"}`
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(message))
	}))
	t.Cleanup(target.Close)
	raising := testutil.Collector("raising", "json")
	raising.Transform = model.TransformConfig{Type: "python", Script: "raise Exception(data['message'])"}
	raising.Metrics = nil
	raising.Limits = model.Limits{MaxResponseBytes: 16 << 20, MaxOutputBytes: 64 << 20}
	plain := testutil.Collector("plain", "text")
	// A path of components a file system takes, none of which is there.
	missing := "/nonexistent" + strings.Repeat("/"+strings.Repeat("d", 200), 15) + "/token"
	scripted := model.StaticTarget{Name: "scripted", Collector: "raising", Target: target.URL}
	keyed := model.StaticTarget{Name: "keyed", Collector: "plain", Target: target.URL, Request: model.TargetRequestConfig{BearerTokenFile: missing}}
	server := newStaticServer(t, &model.Config{Collectors: []model.Collector{raising, plain}}, &model.StaticTargetFile{Interval: model.Duration(60e9), Targets: []model.StaticTarget{scripted, keyed}})
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, tc := range []struct {
		static              model.StaticTarget
		stage, starts, ends string
		whole               int
	}{
		{scripted, "transform", "python transform failed: Traceback (most recent call last):\n  File \"<collector-python>\", line 1, in <module>\n    raise Exception(data['message'])\nException: aaa", " bytes)", 400},
		{keyed, "credentials", "reading bearer token file: open /nonexistent/ddd", fmt.Sprintf("ddd... (%d bytes)", len("reading bearer token file: open "+missing+": no such file or directory")), model.MaxFailureBytes},
	} {
		server.scrapeTarget(t.Context(), server.manager.Get(), tc.static)
		lines := linesOf(t, logs, "static target scrape failed")
		if len(lines) != 1 || lines[0]["stage"] != tc.stage {
			t.Fatalf("static target %s: its failure is logged as %.600v, want one line of the %s stage", tc.static.Name, lines, tc.stage)
		}
		text, _ := lines[0]["error"].(string)
		if len(text) > tc.whole || !strings.HasPrefix(text, tc.starts) || !strings.HasSuffix(text, tc.ends) {
			t.Errorf("static target %s: its failure is logged in %d bytes, %.200q ... %q, want at most %d that start %q and end %q", tc.static.Name, len(text), text, text[max(0, len(text)-80):], tc.whole, tc.starts, tc.ends)
		}
	}
	kept := rememberedTexts(server)
	if len(kept) != 2 {
		t.Fatalf("the failure log remembers %d failures: %.300q", len(kept), kept)
	}
	for _, text := range kept {
		if len(text) > model.MaxFailureBytes || !strings.Contains(text, "... (# bytes)") {
			t.Errorf("the failure log remembers a failure by %d bytes, %.200q ... %q", len(text), text, text[max(0, len(text)-60):])
		}
	}
}
