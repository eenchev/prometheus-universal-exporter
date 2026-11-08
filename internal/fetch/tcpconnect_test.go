//go:build !select_request_types || request_type_tcp

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// tcpConnectAnswer is the document of a scrape of connect_only, as a test
// reads it.
type tcpConnectAnswer struct {
	Open            bool     `json:"open"`
	Reason          string   `json:"reason"`
	Error           string   `json:"error"`
	Address         string   `json:"address"`
	ConnectSeconds  *float64 `json:"connect_seconds"`
	TLSSeconds      *float64 `json:"tls_seconds"`
	TLSVersion      string   `json:"tls_version"`
	OpenPorts       *int     `json:"open_ports"`
	Ports           map[string]tcpConnectAnswer
	DurationSeconds *float64 `json:"duration_seconds"`
}

// connectScrape is a scrape of c of target, its document read.
func connectScrape(t *testing.T, ctx context.Context, c *model.Collector, target string) (tcpConnectAnswer, *HTTPResponse) {
	t.Helper()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc tcpConnectAnswer
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v: %s", err, resp.Body)
	}
	return doc, resp
}

// silent is a server that records how many bytes it read before the
// client closed the connection, of the first connection read alone, and
// writes nothing.
func silent(read chan<- int) func(net.Conn) {
	return func(conn net.Conn) {
		buf := make([]byte, 64)
		total := 0
		for {
			n, err := conn.Read(buf)
			total += n
			if err != nil {
				select {
				case read <- total:
				default:
				}
				return
			}
		}
	}
}

// A collector of connect_only connects, writes and reads nothing, and
// answers a JSON document: open, the address reached and how long the
// connection took; a port that refuses the connection is open: false with
// the reason, not a failed scrape; its method is CONNECT.
func TestATCPConnectOnlyScrapeAnswersWhetherThePortTakesAConnection(t *testing.T) {
	read := make(chan int, 1)
	server := testutil.NewTCPStub(t, silent(read))
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{ConnectOnly: true} })
	ctx, trace := WithRequestTrace(context.Background())
	doc, resp := connectScrape(t, ctx, c, server.Addr)
	if !doc.Open || doc.Address != server.Addr || doc.ConnectSeconds == nil || doc.TLSSeconds != nil || doc.Reason != "" || doc.DurationSeconds == nil {
		t.Errorf("open port: %+v", doc)
	}
	if got := resp.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q", got)
	}
	select {
	case n := <-read:
		if n != 0 {
			t.Errorf("the server read %d bytes", n)
		}
	case <-time.After(2 * time.Second):
		t.Error("the connection was not closed")
	}
	if r := trace.Requests(); len(r) != 1 || r[0].Method != tcpMethodConnect || r[0].URL != "tcp://"+server.Addr || r[0].Outcome != "open, connected to "+server.Addr || r[0].Header.Get("Tcp-Connect-Timeout") != "5s" {
		t.Errorf("traced %+v", r)
	}
	if f := trace.Requests()[0].Facts; len(f) != 1 || f[0].Key != "tcp.open" || !f[0].Bool {
		t.Errorf("facts %+v", f)
	}
	if method := RequestTypes[RequestTypeTCP].Method(c, RequestOverrides{}); method != tcpMethodConnect {
		t.Errorf("method %q", method)
	}

	refusing := testutil.RefusingAddress(t)
	ctx, trace = WithRequestTrace(context.Background())
	doc, _ = connectScrape(t, ctx, c, refusing)
	if doc.Open || doc.Reason != "refused" || !strings.Contains(doc.Error, "refused") || doc.Address != "" || doc.ConnectSeconds != nil {
		t.Errorf("closed port: %+v", doc)
	}
	if r := trace.Requests(); len(r) != 1 || r[0].Failed || !strings.HasPrefix(r[0].Outcome, "not open, refused: ") {
		t.Errorf("traced %+v", r)
	}
	if f := trace.Requests()[0].Facts; len(f) != 2 || f[0].Bool || f[1].Key != "tcp.closed.reason" || f[1].Name != "refused" {
		t.Errorf("facts %+v", f)
	}
}

// With a tls block the handshake is made too, and timed: one the server
// fails, or that does not end within connect_timeout, is open: false, with
// the reason tls or timeout, and the connection's time and address.
func TestATCPConnectOnlyScrapeMakesTheTLSHandshake(t *testing.T) {
	read := make(chan int, 1)
	server := testutil.NewTLSTCPStub(t, silent(read))
	c := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.TLS.CAFile = TCPConfig{ConnectOnly: true}, server.CAFile
	})
	doc, _ := connectScrape(t, context.Background(), c, server.Addr)
	if !doc.Open || doc.TLSSeconds == nil || !strings.HasPrefix(doc.TLSVersion, "TLS 1.") || doc.Address != server.Addr {
		t.Errorf("handshake: %+v", doc)
	}
	if label, err := RequestLabelFor(server.Addr, c, RequestOverrides{}); err != nil || label != "tls://"+server.Addr {
		t.Errorf("label %q, %v", label, err)
	}
	unknown := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.TLS.ServerName = TCPConfig{ConnectOnly: true}, "127.0.0.1"
	})
	doc, _ = connectScrape(t, context.Background(), unknown, server.Addr)
	if doc.Open || doc.Reason != "tls" || !strings.Contains(doc.Error, "TLS handshake with "+server.Addr) || doc.ConnectSeconds == nil || doc.Address != server.Addr {
		t.Errorf("an unknown authority: %+v", doc)
	}

	done := make(chan struct{})
	mute := testutil.NewTCPStub(t, func(net.Conn) { <-done })
	t.Cleanup(func() { close(done) })
	waiting := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{ConnectOnly: true, ConnectTimeout: model.Duration(100 * time.Millisecond)}
		c.Request.TLS.CAFile = server.CAFile
	})
	began := time.Now()
	doc, _ = connectScrape(t, context.Background(), waiting, mute.Addr)
	if doc.Open || doc.Reason != "timeout" || doc.ConnectSeconds == nil {
		t.Errorf("a handshake never answered: %+v", doc)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("connect_timeout of 100ms took %s", took)
	}
}

// What fails a scrape of connect_only is being unable to ask: a target
// the target lists refuse, which is never connected to, and a name that
// does not resolve.
func TestATCPConnectOnlyScrapeFailsWhenItCannotAsk(t *testing.T) {
	server := testutil.NewTCPStub(t, silent(make(chan int, 1)))
	denied := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.DeniedTargets = TCPConfig{ConnectOnly: true}, []string{"127.0.0.0/8"}
	})
	if _, err := FetchCollector(context.Background(), server.Addr, denied, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("denied: %v", err)
	}
	ports := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.DeniedTargets = TCPConfig{ConnectOnly: true, Ports: []int{1, 2}}, []string{"127.0.0.0/8"}
	})
	if _, err := FetchCollector(context.Background(), "127.0.0.1", ports, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("denied ports: %v", err)
	}
	if n := server.Accepted(); n != 0 {
		t.Errorf("a refused target was connected to %d times", n)
	}
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{ConnectOnly: true} })
	if _, err := FetchCollector(context.Background(), "nonexistent.invalid:443", c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "connecting to nonexistent.invalid:443") {
		t.Errorf("a name that does not resolve: %v", err)
	}
}

// With request.ports, the ports of the target's host are dialed at once,
// each its own request of the trace: the document has each in ports, open
// says whether every one was and open_ports how many; the target is the
// host alone.
func TestATCPConnectOnlyScrapeDialsEachOfItsPorts(t *testing.T) {
	first := testutil.NewTCPStub(t, silent(make(chan int, 1)))
	second := testutil.NewTCPStub(t, silent(make(chan int, 1)))
	refusing := testutil.RefusingAddress(t)
	var ports []int
	for _, addr := range []string{first.Addr, second.Addr, refusing} {
		_, port, _ := net.SplitHostPort(addr)
		n, _ := strconv.Atoi(port)
		ports = append(ports, n)
	}
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{ConnectOnly: true, Ports: ports} })
	ctx, trace := WithRequestTrace(context.Background())
	doc, _ := connectScrape(t, ctx, c, "127.0.0.1")
	if doc.Open || doc.OpenPorts == nil || *doc.OpenPorts != 2 || len(doc.Ports) != 3 {
		t.Fatalf("ports: %+v", doc)
	}
	for i, addr := range []string{first.Addr, second.Addr} {
		if p := doc.Ports[strconv.Itoa(ports[i])]; !p.Open || p.Address != addr || p.ConnectSeconds == nil {
			t.Errorf("port %d: %+v", ports[i], p)
		}
	}
	if p := doc.Ports[strconv.Itoa(ports[2])]; p.Open || p.Reason != "refused" {
		t.Errorf("refusing port: %+v", p)
	}
	traced := map[string]string{}
	for _, r := range trace.Requests() {
		if r.Method != tcpMethodConnect {
			t.Errorf("method %q", r.Method)
		}
		traced[r.URL] = r.Outcome
	}
	if len(traced) != 3 || traced["tcp://"+first.Addr] != "open, connected to "+first.Addr || !strings.HasPrefix(traced["tcp://"+refusing], "not open, refused") {
		t.Errorf("traced %v", traced)
	}
	if label, err := RequestLabelFor("127.0.0.1", c, RequestOverrides{}); err != nil || label != "tcp://127.0.0.1" {
		t.Errorf("label %q, %v", label, err)
	}
	if label, err := RequestLabelFor("[2001:db8::1]", c, RequestOverrides{}); err != nil || label != "tcp://[2001:db8::1]" {
		t.Errorf("label %q, %v", label, err)
	}
	all := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{ConnectOnly: true, Ports: ports[:2]} })
	if doc, _ := connectScrape(t, context.Background(), all, "127.0.0.1"); !doc.Open || *doc.OpenPorts != 2 {
		t.Errorf("every port open: %+v", doc)
	}
	if display := RequestTypes[RequestTypeTCP].Display("2001:db8::1"); display != "2001:db8::1" {
		t.Errorf("display %q", display)
	}
	for target, want := range map[string]string{
		"db.example":         "",
		"[2001:db8::1]":      "",
		"db.example:5432":    "names a port; the collector's request.ports are the ports dialed",
		"[2001:db8::1]:5432": "names a port",
		"2001:db8::1":        "",
		"db.example/x":       "has a path, query or user",
	} {
		err := CheckTarget(c, target, false)
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", target, err, want)
		}
	}
}

// connect_only goes with no key of what is sent, read or spoken, nor with
// udp or unix_socket; connect_timeout and ports go with connect_only alone,
// connect_timeout at least 10ms, ports at most 64 distinct ports.
func TestATCPConnectOnlyCollectorsKeysAreChecked(t *testing.T) {
	only := func(r TCPConfig) func(*model.Collector) {
		return func(c *model.Collector) { r.ConnectOnly = true; c.Request.Own = r }
	}
	many := make([]int, 65)
	for i := range many {
		many[i] = i + 1
	}
	for name, tc := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"send":         {only(TCPConfig{Send: "x\n"}), "sets request.connect_only and request.send;"},
		"send_hex":     {only(TCPConfig{SendHex: "00"}), "request.connect_only and request.send_hex;"},
		"steps":        {only(TCPConfig{Steps: []TCPStep{{Send: "x\n"}}}), "request.connect_only and request.steps;"},
		"greeting":     {only(TCPConfig{Greeting: "^220"}), "request.connect_only and request.greeting;"},
		"terminator":   {only(TCPConfig{Terminator: "\n"}), "request.connect_only and request.terminator;"},
		"answer_bytes": {only(TCPConfig{AnswerBytes: 4}), "request.connect_only and request.answer_bytes;"},
		"starttls": {func(c *model.Collector) {
			c.Request.Own, c.Request.StartTLS = TCPConfig{ConnectOnly: true}, "smtp"
		}, "request.connect_only and request.starttls;"},
		"udp": {only(TCPConfig{UDP: true}), "request.connect_only and request.udp; UDP has no connection"},
		"unix_socket": {func(c *model.Collector) {
			c.Request.Own, c.Request.UnixSocket = TCPConfig{ConnectOnly: true}, "/run/x.sock"
		}, "request.connect_only and request.unix_socket"},
		"a short connect_timeout": {only(TCPConfig{ConnectTimeout: model.Duration(time.Millisecond)}), "request.connect_timeout is 1ms; it is how long a connection may take before the port counts as not open, at least 10ms"},
		"connect_timeout alone":   {func(c *model.Collector) { c.Request.Own = TCPConfig{ConnectTimeout: model.Duration(time.Second)} }, "sets request.connect_timeout without request.connect_only"},
		"ports alone":             {func(c *model.Collector) { c.Request.Own = TCPConfig{Ports: []int{22}} }, "sets request.ports without request.connect_only"},
		"port 0":                  {only(TCPConfig{Ports: []int{22, 0}}), "request.ports[1] is 0; want a port from 1 to 65535"},
		"port 65536":              {only(TCPConfig{Ports: []int{65536}}), "request.ports[0] is 65536"},
		"a port twice":            {only(TCPConfig{Ports: []int{22, 443, 22}}), "request.ports lists 22 twice"},
		"more ports than at once": {only(TCPConfig{Ports: many}), "request.ports lists 65 ports, more than the 64"},
	} {
		c := &model.Collector{Name: "ports", Request: model.RequestConfig{Type: RequestTypeTCP}}
		tc.edit(c)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	tcpCollector(t, only(TCPConfig{Ports: many[:64], ConnectTimeout: model.Duration(10 * time.Millisecond), Document: true}))
}

// The connections end at connect_timeout, 5s when left out, and within
// the scrape's deadline at nine tenths of the time left to it, so that the
// document is answered before the deadline.
func TestATCPConnectOnlyScrapeEndsItsConnectionsBeforeItsDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		budget  time.Duration
		timeout time.Duration
		want    time.Duration
	}{
		"the default":               {0, 0, 5 * time.Second},
		"connect_timeout":           {0, 2 * time.Second, 2 * time.Second},
		"within a longer deadline":  {time.Minute, 2 * time.Second, 2 * time.Second},
		"within a shorter deadline": {time.Second, 0, 900 * time.Millisecond},
	} {
		ctx := context.Background()
		if tc.budget > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, tc.budget)
			defer cancel()
		}
		got := time.Until(tcpConnectDeadline(ctx, TCPConfig{ConnectTimeout: model.Duration(tc.timeout)}))
		if got > tc.want || got < tc.want-50*time.Millisecond {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}
