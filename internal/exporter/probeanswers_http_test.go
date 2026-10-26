//go:build !select_request_types || request_type_http

package exporter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A probe's caller is not told what the target answered: a failure that
// quotes the answer — a decoder's, a jq program's, a script's, Go's of an
// answer that is no HTTP — is answered with what it quotes withheld, the
// rest of it as it reads, and the log has it whole. The caller may be anyone
// the probe endpoint answers, and may point the collector at a target the
// operator did not mean it to read.
func TestAProbesCallerIsNotToldTheValuesOfTheAnswer(t *testing.T) {
	requirePython(t)
	const secret = "AWS_SECRET_ACCESS_KEY=hunter2"
	const jq = `request: {type: http}, decoder: {type: json}, transform: {type: jq}`
	message := fmt.Sprintf(`{"n": 1, "message": %q}`, secret)
	for name, tc := range map[string]struct {
		collector, contentType, body string
		// raw is set for a target that does not speak HTTP.
		raw bool
		// told is what the answer says in place of the failure's text.
		told string
	}{
		"a Prometheus exposition's value": {
			collector: `{name: c, request: {type: http}, decoder: {type: prometheus}, transform: {type: prometheus}}`,
			body:      "m " + secret + "\n",
			told:      "collector c decode failed: decoding Prometheus exposition: text format parsing error in line 1: expected float as value, got " + model.WithheldMark,
		},
		"a TOML token": {
			collector: `{name: c, request: {type: http}, decoder: {type: toml}, transform: {type: jq}, metrics: [{name: m, expression: .n}]}`,
			body:      "n = " + strings.ReplaceAll(secret, "=", "_") + "\n",
			told:      "collector c decode failed: TOML decode: line 1: " + model.WithheldMark,
		},
		"an XML entity": {
			collector: `{name: c, request: {type: http}, decoder: {type: xml}, transform: {type: xpath}, metrics: [{name: m, expression: //n}]}`,
			body:      "<n>&" + strings.ReplaceAll(secret, "=", "_") + ";</n>",
			told:      "collector c decode failed: XML decode: XML syntax error on line 1: invalid character entity " + model.WithheldMark,
		},
		"a jq program's error": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: '.message[]', error_mode: fail}]}`,
			body:      message,
			told:      `"error":"metric \"m\" expression: the jq program failed on the answer: ` + model.WithheldMark + `"`,
		},
		"a rule's own error()": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: 'error(.message)', error_mode: fail}]}`,
			body:      message,
			told:      `"error":"metric \"m\" expression: the jq program failed on the answer: ` + model.WithheldMark + `"`,
		},
		"a script's exception": {
			collector: `{name: c, request: {type: http}, decoder: {type: json}, transform: {type: python, script: "x = 1\nraise ValueError(data['message'])"}}`,
			body:      message,
			told:      "collector c transform failed: python transform failed: ValueError at line 2 of the script; the rest is in the exporter's log",
		},
		"an answer that is no HTTP": {
			collector: `{name: c, ` + jq + `, metrics: [{name: m, expression: .n}]}`,
			raw:       true, body: "SSH-2.0-" + secret + "\r\n",
			told: "malformed HTTP response " + model.WithheldMark,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, logs := errorLengthServer(t, tc.collector)
			address := ""
			if tc.raw {
				address = rawTarget(t, func() string { return tc.body })
			} else {
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					_, _ = w.Write([]byte(tc.body))
				}))
				t.Cleanup(target.Close)
				address = target.URL
			}
			answer := probeOnce(t, server, probePath("c", address, ""), nil)
			if got := answer.Body.String(); answer.Code != http.StatusBadGateway || strings.Contains(got, "AWS") || !strings.Contains(got, tc.told) {
				t.Errorf("answered %d %q, want 502 with %q and nothing of the answer", answer.Code, got, tc.told)
			}
			if lines := linesOf(t, logs, "probe failed"); len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0]["error"]), "AWS") {
				t.Errorf("logged %.600v, want the failure with what the answer said", lines)
			}
			// The debug report is the operator's, and has it too.
			if report := probeOnce(t, server, probePath("c", address, "&debug=true"), nil).Body.String(); !strings.Contains(report, "AWS") {
				t.Errorf("the debug report does not show what the answer said:\n%.2000s", report)
			}
		})
	}
}

// answeredAs is text, a failure as the log has it, as a probe's caller is
// answered it: each of values, values of the answer the failure quotes as
// the log has them, withheld (model.CallerText).
func answeredAs(text string, values ...string) string {
	for _, value := range values {
		if value == "" {
			continue
		}
		text = strings.Replace(text, value, model.WithheldMark, 1)
	}
	return text
}
