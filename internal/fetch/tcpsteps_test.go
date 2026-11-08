//go:build !select_request_types || request_type_tcp

package fetch

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// smtpServer is an SMTP server's first steps: its greeting, its EHLO reply
// over three lines, its QUIT reply, and then the connection closed; what it
// read is sent on seen, a line at a time.
func smtpServer(seen chan<- string) func(net.Conn) {
	return func(conn net.Conn) {
		_, _ = io.WriteString(conn, "220 mx.example.org ESMTP\r\n")
		line, _ := testutil.ReadUntil(conn, "\n")
		seen <- line
		// The reply in two writes, so that a step reads it in pieces.
		_, _ = io.WriteString(conn, "250-mx.example.org\r\n250-SIZE 35882577\r\n")
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(conn, "250 STARTTLS\r\n")
		line, _ = testutil.ReadUntil(conn, "\n")
		seen <- line
		_, _ = io.WriteString(conn, "221 bye\r\n")
	}
}

// A collector of steps writes each step's send after the greeting, reads
// its answer until it matches the step's until, across reads, and the last
// step's without until until the server closes the connection: the answer
// is their answers one after another, and the document holds each step's
// send, answer and seconds. A probe's parameter fills a step's send, and
// the debug report shows every step's send.
func TestATCPConversationOfSteps(t *testing.T) {
	seen := make(chan string, 4)
	server := testutil.NewTCPStub(t, smtpServer(seen))
	c := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{Greeting: "^220 ", Document: true, Steps: []TCPStep{
			{Send: "EHLO {{param_name:exporter}}\r\n", Until: `(?m)^250 `},
			{Send: "QUIT\r\n"},
		}}
	})
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, server.Addr, c, RequestOverrides{Params: map[string]string{"param_name": "probe.example"}, Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{<-seen, <-seen}; !slices.Equal(got, []string{"EHLO probe.example\r\n", "QUIT\r\n"}) {
		t.Errorf("the server read %q", got)
	}
	var doc tcpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Answer == nil || *doc.Answer != "250-mx.example.org\r\n250-SIZE 35882577\r\n250 STARTTLS\r\n221 bye\r\n" || len(doc.Steps) != 2 ||
		doc.Steps[0].Answer != "250-mx.example.org\r\n250-SIZE 35882577\r\n250 STARTTLS\r\n" || doc.Steps[1].Send != "QUIT\r\n" || doc.Steps[1].Answer != "221 bye\r\n" || doc.Steps[0].Seconds < 0.02 {
		t.Errorf("document %s", resp.Body)
	}
	r := trace.Requests()[0]
	if r.Method != "SEND" || r.Header.Get("Tcp-Steps") != `"EHLO probe.example\r\n" then "QUIT\r\n"` || !strings.HasSuffix(r.Outcome, "; 2 steps, 62 bytes read") {
		t.Errorf("traced %s %v %q", r.Method, r.Header, r.Outcome)
	}
}

// A step whose answer does not come to match its until fails the scrape at
// its deadline, quoting what came, or as soon as the server closes the
// connection; a last step without until on a server that keeps the
// connection open fails at the deadline saying so.
func TestATCPStepThatIsNotAnsweredFailsTheScrape(t *testing.T) {
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = testutil.ReadUntil(conn, "\n")
		_, _ = io.WriteString(conn, "500 what\r\n")
		_, _ = io.Copy(io.Discard, conn)
	})
	for _, tc := range []struct{ until, want string }{
		{`^250\s`, `the answer to request.steps[0], 10 bytes, "500 what\r\n", did not come to match its until "^250\\s"`},
		{"", "the server kept the connection open after the last step, 10 bytes read"},
	} {
		until, want := tc.until, tc.want
		c := tcpCollector(t, func(c *model.Collector) {
			c.Request.Own = TCPConfig{Steps: []TCPStep{{Send: "EHLO x\r\n", Until: until}}}
		})
		_, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("until %q: %v, want %q", until, err, want)
		}
	}
	closing := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = testutil.ReadUntil(conn, "\n")
		_, _ = io.WriteString(conn, "500 what\r\n")
	})
	c := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{Steps: []TCPStep{{Send: "EHLO x\r\n", Until: `^250 `}}}
	})
	if _, err := FetchCollector(context.Background(), closing.Addr, c, RequestOverrides{Timeout: 5 * time.Second}, nil); err == nil || !strings.Contains(err.Error(), "the server closed the connection after 10 bytes of the answer to request.steps[0]") {
		t.Errorf("closed: %v", err)
	}
}

// request.steps is refused beside send, send_hex, terminator, answer_bytes
// and udp; past 16 steps; with a step without send, a step but the last
// without until, an until that is no regular expression or has a
// placeholder, and a send's default a line refuses.
func TestTCPStepsAreCheckedAtLoad(t *testing.T) {
	two := []TCPStep{{Send: "a\r\n", Until: "x"}, {Send: "b\r\n"}}
	many := make([]TCPStep, 17)
	for i := range many {
		many[i] = TCPStep{Send: "a\r\n", Until: "x"}
	}
	for name, tc := range map[string]struct {
		own  TCPConfig
		want string
	}{
		"send":                   {TCPConfig{Send: "x\r\n", Steps: two}, "sets both request.steps and request.send"},
		"send_hex":               {TCPConfig{SendHex: "00", Steps: two}, "sets both request.steps and request.send_hex"},
		"terminator":             {TCPConfig{Terminator: "\n", Steps: two}, "sets both request.steps and request.terminator"},
		"answer_bytes":           {TCPConfig{AnswerBytes: 4, Steps: two}, "sets both request.steps and request.answer_bytes"},
		"seventeen":              {TCPConfig{Steps: many}, "request.steps has 17 steps, more than 16"},
		"no send":                {TCPConfig{Steps: []TCPStep{{Until: "x"}}}, "request.steps[0] has no send"},
		"no until":               {TCPConfig{Steps: []TCPStep{{Send: "a"}, {Send: "b"}}}, "request.steps[0] has no until"},
		"a bad until":            {TCPConfig{Steps: []TCPStep{{Send: "a", Until: "("}}}, `request.steps[0].until "(" is not a regular expression`},
		"a placeholder in until": {TCPConfig{Steps: []TCPStep{{Send: "a", Until: "{{param_x}}"}}}, "request.steps[0].until \"{{param_x}}\" has a placeholder"},
		"a default with a blank": {TCPConfig{Steps: []TCPStep{{Send: "GET {{param_k:a b}}\r\n"}}}, "request.steps[0].send: the default of param_k holds"},
	} {
		x := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP, Own: tc.own}}
		if err := ValidateRequest(x); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	udp := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP, Own: TCPConfig{UDP: true, Steps: two}}}
	if err := ValidateRequest(udp); err == nil || !strings.Contains(err.Error(), "request.udp") {
		t.Errorf("udp: %v", err)
	}
}
