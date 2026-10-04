//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// movingFailureCollectors is one collector for each transform that names
// where in the response a rule failed, under error_mode log and under fail.
const movingFailureCollectors = `
collectors:
  - name: csv_log
    request: {type: http, path: /csv}
    transform: {type: csv}
    metrics:
      - {name: used, expression: used, error_mode: log, labels: [{name: host, expression: host}]}
  - name: csv_fail
    request: {type: http, path: /csv}
    transform: {type: csv}
    metrics:
      - {name: used, expression: used, error_mode: fail, labels: [{name: host, expression: host}]}
  - name: xpath_log
    request: {type: http, path: /xml}
    decoder: {type: xml}
    transform: {type: xpath}
    metrics:
      - {name: used, expression: //host/used, error_mode: log, labels: [{name: host, expression: ../@name}]}
  - name: xpath_fail
    request: {type: http, path: /xml}
    decoder: {type: xml}
    transform: {type: xpath}
    metrics:
      - {name: used, expression: //host/used, error_mode: fail, labels: [{name: host, expression: ../@name}]}
  - name: css_log
    request: {type: http, path: /html}
    decoder: {type: html}
    transform: {type: css}
    metrics:
      - {name: used, items: tr, expression: td.used, error_mode: log, labels: [{name: host, expression: td.host}]}
  - name: css_fail
    request: {type: http, path: /html}
    decoder: {type: html}
    transform: {type: css}
    metrics:
      - {name: used, items: tr, expression: td.used, error_mode: fail, labels: [{name: host, expression: td.host}]}
  - name: jq_log
    request: {type: http, path: /json}
    transform: {type: jq}
    metrics:
      - {name: used, items: ".hosts[]", expression: .used, error_mode: log, labels: [{name: host, expression: .name}]}
  - name: jq_fail
    request: {type: http, path: /json}
    transform: {type: jq}
    metrics:
      - {name: used, items: ".hosts[]", expression: .used, error_mode: fail, labels: [{name: host, expression: .name}]}
`

// movingFailureTarget answers a table of ten hosts as CSV, XML, HTML and
// JSON by its path, the value of the host numbered *row, from 1, being
// *value rather than a number; row 0 leaves every host its number, and a
// status other than 200 is answered instead of the table.
type movingFailureTarget struct {
	row    atomic.Int64
	value  atomic.Pointer[string]
	status atomic.Int64
}

func (m *movingFailureTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if status := int(m.status.Load()); status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	var body strings.Builder
	cell := func(i int) string {
		if int64(i) == m.row.Load() {
			return *m.value.Load()
		}
		return strconv.Itoa(i * 10)
	}
	switch r.URL.Path {
	case "/csv":
		w.Header().Set("Content-Type", "text/csv")
		body.WriteString("host,used\n")
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(&body, "h%d,%s\n", i, cell(i))
		}
	case "/xml":
		w.Header().Set("Content-Type", "application/xml")
		body.WriteString("<hosts>")
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(&body, `<host name="h%d"><used>%s</used></host>`, i, cell(i))
		}
		body.WriteString("</hosts>")
	case "/html":
		w.Header().Set("Content-Type", "text/html")
		body.WriteString("<html><body><table>")
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(&body, `<tr><td class="host">h%d</td><td class="used">%s</td></tr>`, i, cell(i))
		}
		body.WriteString("</table></body></html>")
	case "/json":
		w.Header().Set("Content-Type", "application/json")
		body.WriteString(`{"hosts":[`)
		for i := 1; i <= 10; i++ {
			if i > 1 {
				body.WriteString(",")
			}
			fmt.Fprintf(&body, `{"name":"h%d","used":%q}`, i, cell(i))
		}
		body.WriteString("]}")
	}
	_, _ = w.Write([]byte(body.String()))
}

// movingFailureServer is a server of movingFailureCollectors that logs at
// debug level into the buffer, its target, and a clock to move the failure
// log's time with.
func movingFailureServer(t *testing.T) (*Server, *bytes.Buffer, *movingFailureTarget, string, *atomic.Int64) {
	t.Helper()
	testutil.CaptureLogs(t)
	table := &movingFailureTarget{}
	empty := ""
	table.value.Store(&empty)
	target := httptest.NewServer(table)
	t.Cleanup(target.Close)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", movingFailureCollectors))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	later := &atomic.Int64{}
	server.failures.now = func() time.Time { return time.Now().Add(time.Duration(later.Load())) }
	return server, logs, table, target.URL, later
}

// answeredError is the error a probe a rule failed was answered with.
func answeredError(t *testing.T, answer *httptest.ResponseRecorder) string {
	t.Helper()
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(answer.Body.Bytes(), &failure); err != nil {
		t.Fatalf("the answer %q is not JSON: %v", answer.Body, err)
	}
	return failure.Error
}

// linesOf takes the lines with the message out of the log, and empties it.
func linesOf(t *testing.T, logs *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, record := range logRecords(t, logs) {
		if record["msg"] == msg {
			lines = append(lines, record)
		}
	}
	logs.Reset()
	return lines
}

// A rule that fails on every scrape is one failure to the log wherever in
// the response it fails: with the empty cell in row 3, then 7, then 3, then 5
// of a table, a csv, an xpath, a css and a jq rule under error_mode log are
// each logged in full once, as a warning, and then as repeats at debug
// level; five minutes on the line comes again with every repeat counted and
// when the failure began; and the recovery counts every failing scrape. The
// same holds for the probe's failure under error_mode fail. Each line, and
// each answer of a failed probe, names the row, node or item it was in on
// that scrape.
func TestAFailureThatMovesInTheResponseIsOneFailureToTheLog(t *testing.T) {
	// where is how each transform names host number row: the csv transform
	// counts rows from 1, the others nodes and items from 0.
	where := map[string]func(row int) string{
		"csv":   func(row int) string { return fmt.Sprintf(`CSV column "used" is empty in row %d`, row) },
		"xpath": func(row int) string { return fmt.Sprintf(`metric "used" value is missing for node %d: `, row-1) },
		"css":   func(row int) string { return fmt.Sprintf(`metric "used" value is missing for item %d: `, row-1) },
		"jq":    func(row int) string { return fmt.Sprintf(`metric "used" value is missing for item %d`, row-1) },
	}
	for _, kind := range []string{"csv", "xpath", "css", "jq"} {
		for _, mode := range []string{model.ErrorModeLog, model.ErrorModeFail} {
			t.Run(kind+"_"+mode, func(t *testing.T) {
				server, logs, table, target, later := movingFailureServer(t)
				failed, recovered, level, status := "metric extraction failed", "metric extraction recovered", "WARN", http.StatusOK
				if mode == model.ErrorModeFail {
					failed, recovered, level, status = "probe failed", "probe recovered", "ERROR", http.StatusBadGateway
				}
				scrape := func(row int) map[string]any {
					t.Helper()
					table.row.Store(int64(row))
					answer := probeOnce(t, server, probePath(kind+"_"+mode, target, ""), nil)
					if answer.Code != status {
						t.Fatalf("row %d: answered %d, want %d: %s", row, answer.Code, status, answer.Body)
					}
					if mode == model.ErrorModeFail && !strings.Contains(answeredError(t, answer), where[kind](row)) {
						t.Errorf("row %d: the probe is answered %q, which does not say %q", row, answer.Body, where[kind](row))
					}
					lines := linesOf(t, logs, failed)
					if len(lines) != 1 {
						t.Fatalf("row %d: %d lines of %q, want 1: %v", row, len(lines), failed, lines)
					}
					if text, _ := lines[0]["error"].(string); !strings.Contains(text, where[kind](row)) {
						t.Errorf("row %d: the line's error is %q, which does not say %q", row, text, where[kind](row))
					}
					return lines[0]
				}
				for i, row := range []int{3, 3, 7, 7, 3, 5} {
					line := scrape(row)
					switch {
					case i == 0 && (line["level"] != level || line["repeat"] != nil || line["repeated"] != nil):
						t.Errorf("the first failure, in row %d, is logged as %v, want in full at %s", row, line, level)
					case i > 0 && (line["level"] != "DEBUG" || line["repeat"] != true):
						t.Errorf("scrape %d, failing in row %d, is logged as %v, want as a repeat at debug level", i+1, row, line)
					}
				}
				later.Store(int64(failureRepeatInterval + time.Second))
				if line := scrape(9); line["level"] != level || line["repeated"] != float64(6) || line["failing_since"] == nil {
					t.Errorf("five minutes on, failing in row 9, the line is %v, want at %s with repeated 6 and failing_since", line, level)
				}
				table.row.Store(0)
				if answer := probeOnce(t, server, probePath(kind+"_"+mode, target, ""), nil); answer.Code != http.StatusOK {
					t.Fatalf("with every value there: answered %d: %s", answer.Code, answer.Body)
				}
				if lines := linesOf(t, logs, recovered); len(lines) != 1 || lines[0]["failures"] != float64(7) {
					t.Errorf("the recovery is logged as %v, want one line with failures 7", lines)
				}
			})
		}
	}
}

// What a failure says other than where it happened still tells one failure
// from another: text that is no number in node 2 and then in node 6 is one
// failure, and other text in node 6 a new one, logged in full; and a probe
// that fails for a rule and then for the target's status fails in another
// stage, logged in full.
func TestAnotherValueOrStageIsStillAnotherFailureToTheLog(t *testing.T) {
	server, logs, table, target, _ := movingFailureServer(t)
	text := func(value string) { table.value.Store(&value) }
	scrape := func(collector, msg string, row int) map[string]any {
		t.Helper()
		table.row.Store(int64(row))
		probeOnce(t, server, probePath(collector, target, ""), nil)
		lines := linesOf(t, logs, msg)
		if len(lines) != 1 {
			t.Fatalf("%s, row %d: %d lines of %q, want 1: %v", collector, row, len(lines), msg, lines)
		}
		return lines[0]
	}
	notANumber := func(value string) string {
		return fmt.Sprintf("value %q is not a number; map text to numbers with value_map", value)
	}
	text("n/a")
	if line := scrape("xpath_log", "metric extraction failed", 3); line["level"] != "WARN" || line["error"] != `metric "used" node 2: `+notANumber("n/a") {
		t.Errorf("the first failure is logged as %v", line)
	}
	if line := scrape("xpath_log", "metric extraction failed", 7); line["level"] != "DEBUG" || line["repeat"] != true || line["error"] != `metric "used" node 6: `+notANumber("n/a") {
		t.Errorf("the same text in another node is logged as %v, want as a repeat at debug level", line)
	}
	text("N/A")
	if line := scrape("xpath_log", "metric extraction failed", 7); line["level"] != "WARN" || line["repeat"] != nil || line["error"] != `metric "used" node 6: `+notANumber("N/A") {
		t.Errorf("other text in the same node is logged as %v, want in full as a warning", line)
	}

	if line := scrape("xpath_fail", "probe failed", 3); line["level"] != "ERROR" || line["stage"] != "metric" {
		t.Errorf("the rule's failure is logged as %v", line)
	}
	if line := scrape("xpath_fail", "probe failed", 4); line["level"] != "DEBUG" || line["stage"] != "metric" {
		t.Errorf("the rule's failure in another node is logged as %v, want as a repeat at debug level", line)
	}
	table.status.Store(http.StatusInternalServerError)
	if line := scrape("xpath_fail", "probe failed", 4); line["level"] != "ERROR" || line["repeat"] != nil || line["stage"] != "http_status" {
		t.Errorf("the failure of another stage is logged as %v, want in full as an error", line)
	}
}

// A label value over limits.max_label_value_length whose length differs
// from scrape to scrape is one failure to the log, as one of the same length
// on every scrape is: logged in full once, and each answer names the length
// the value had on that scrape.
func TestALimitFailureOfAnotherSizeIsOneFailureToTheLog(t *testing.T) {
	for _, growing := range []bool{false, true} {
		t.Run(fmt.Sprintf("growing=%v", growing), func(t *testing.T) {
			testutil.CaptureLogs(t)
			var scrapes atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				length := 600
				if n := int(scrapes.Add(1)); growing {
					length += n
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = fmt.Fprintf(w, "value=42 note=%s\n", strings.Repeat("x", length))
			}))
			t.Cleanup(target.Close)
			c := testutil.Collector("noted", "text")
			c.Limits.MaxResponseBytes = 4096
			c.Metrics[0].Expression = `value=(?P<value>\d+) note=(?P<note>\S+)`
			c.Metrics[0].Labels = []model.LabelRule{{Name: "note", Expression: "note"}}
			server, _ := newCacheTestServer(t, c)
			logs := &bytes.Buffer{}
			server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			full := 0
			for i := 1; i <= 6; i++ {
				length := 600
				if growing {
					length += i
				}
				answer := probeOnce(t, server, probePath("noted", target.URL, ""), nil)
				want := fmt.Sprintf(`metric "demo_value" label "note" value is %d bytes, longer than limits.max_label_value_length 500; `, length)
				if answer.Code != http.StatusBadGateway || !strings.Contains(answer.Body.String(), want) {
					t.Fatalf("scrape %d: answered %d %q, want 502 saying %q", i, answer.Code, answer.Body, want)
				}
				lines := linesOf(t, logs, "probe failed")
				if len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0]["error"]), want) || lines[0]["stage"] != "validation" {
					t.Fatalf("scrape %d: the failure is logged as %v, want one line saying %q", i, lines, want)
				}
				if lines[0]["level"] == "ERROR" && lines[0]["repeat"] == nil {
					full++
				} else if lines[0]["level"] != "DEBUG" || lines[0]["repeat"] != true {
					t.Errorf("scrape %d: the failure is logged as %v", i, lines[0])
				}
			}
			if full != 1 {
				t.Errorf("six scrapes failing for one label over its limit were logged in full %d times, want once", full)
			}
		})
	}
}
