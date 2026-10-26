//go:build request_type_tcp

package fetch

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A tcp collector's placeholders are in its send.
func init() {
	ownPlaceholders[RequestTypeTCP] = func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "get {{param_key:x}}\r\n"} }
}

// tcpCollector is a validated tcp collector, as edit makes it.
func tcpCollector(t *testing.T, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// tcpScrape is a scrape of target through c, with the probe's parameters,
// and the answer's body.
func tcpScrape(t *testing.T, c *model.Collector, target string, params map[string]string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{Params: params}, nil)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// memcachedStats is what memcached answers stats with, cut short.
const memcachedStats = "STAT pid 1\r\nSTAT uptime 3600\r\nSTAT curr_connections 10\r\nSTAT get_hits 90\r\nSTAT get_misses 10\r\nEND\r\n"

// memcached answers stats\r\n as memcached does, and keeps the connection
// open for the next command, as memcached does; it records what it read.
func memcached(sent chan<- string) func(net.Conn) {
	return func(conn net.Conn) {
		line, err := testutil.ReadUntil(conn, "\r\n")
		if sent != nil {
			sent <- line
		}
		if err != nil || line != "stats\r\n" {
			_, _ = io.WriteString(conn, "ERROR\r\n")
		} else {
			_, _ = io.WriteString(conn, memcachedStats)
		}
		// The connection stays open until the client closes it.
		_, _ = io.Copy(io.Discard, conn)
	}
}

// A tcp collector writes send and reads the answer until it ends with the
// terminator, which stays in the body, though the server keeps the
// connection open, as memcached does; the body is the answer as the server
// wrote it, with no Content-Type, and the scrape has no status.
func TestATCPCollectorReadsUntilItsTerminator(t *testing.T) {
	sent := make(chan string, 1)
	server := testutil.NewTCPStub(t, memcached(sent))
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, server.Addr, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != memcachedStats || !resp.NoStatus || resp.Headers.Get("Content-Type") != "" {
		t.Errorf("body %q, no status %t, headers %v", resp.Body, resp.NoStatus, resp.Headers)
	}
	if got := <-sent; got != "stats\r\n" {
		t.Errorf("sent %q", got)
	}
	if label, err := RequestLabelFor(server.Addr, c, RequestOverrides{}); err != nil || label != "tcp://"+server.Addr {
		t.Errorf("label %q, %v", label, err)
	}
	if method := RequestMethodFor(c, RequestOverrides{}); method != "SEND" {
		t.Errorf("method %q", method)
	}
}

// Without a terminator the answer ends when the server closes the
// connection, as ZooKeeper does after a four-letter word; with send left
// out nothing is written, and what the server says first is the answer.
func TestATCPCollectorWithoutATerminatorReadsUntilTheServerCloses(t *testing.T) {
	const mntr = "zk_version\t3.8.4\nzk_avg_latency\t0\nzk_znode_count\t42\nzk_server_state\tstandalone\n"
	zookeeper := testutil.NewTCPStub(t, func(conn net.Conn) {
		if word, err := testutil.ReadUntil(conn, "mntr"); err == nil && word == "mntr" {
			_, _ = io.WriteString(conn, mntr)
		}
	})
	body, err := tcpScrape(t, tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "mntr"} }), zookeeper.Addr, nil)
	if err != nil || body != mntr {
		t.Errorf("body %q, %v", body, err)
	}
	banner := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "OK MPD 0.23.5\n") })
	reader := tcpCollector(t)
	if body, err := tcpScrape(t, reader, banner.Addr, nil); err != nil || body != "OK MPD 0.23.5\n" {
		t.Errorf("a banner: %q, %v", body, err)
	}
	if method := RequestMethodFor(reader, RequestOverrides{}); method != "READ" {
		t.Errorf("method %q", method)
	}
	// A server that closes before the terminator came cut the answer
	// short.
	cut := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "STAT pid 1\r\n") })
	_, err = tcpScrape(t, tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Terminator: "END\r\n"} }), cut.Addr, nil)
	if err == nil || !strings.Contains(err.Error(), `the server closed the connection after 12 bytes, and the answer does not end with request.terminator "END\r\n"`) {
		t.Errorf("an answer cut short: %v", err)
	}
}

// A server that keeps the connection open without the terminator, or
// without closing it when there is none, fails the scrape at the deadline,
// saying what it waited for; so does one that never accepts the write's
// end of the exchange.
func TestATCPServerThatNeverEndsItsAnswerFailsAtTheDeadline(t *testing.T) {
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "STAT pid 1\r\n")
		_, _ = io.Copy(io.Discard, conn)
	})
	for name, tc := range map[string]struct {
		terminator string
		want       string
	}{
		"a terminator": {"END\r\n", `no answer ending with request.terminator "END\r\n" came, 12 bytes read`},
		"none":         {"", "the server kept the connection open, 12 bytes read; without request.terminator the answer ends when the server closes it"},
	} {
		c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "stats\r\n", Terminator: tc.terminator} })
		start := time.Now()
		_, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("%s: took %s", name, elapsed)
		}
	}
}

// The answer is held to the response limit as it arrives, whether reading
// waits for the terminator or the close: an answer of the limit is taken,
// one byte more fails the scrape as a limit exceeded, before the server has
// sent the rest.
func TestATCPAnswerIsHeldToTheResponseLimit(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	big := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, strings.Repeat("x", 2048))
		// More would follow; the client must stop at the limit, not wait.
		<-release
	})
	exact := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.WriteString(conn, strings.Repeat("x", 1020)+"END\n") })
	for _, terminator := range []string{"", "END\n"} {
		c := tcpCollector(t, func(c *model.Collector) {
			c.Request.Own, c.Request.MaxResponseBytes = TCPConfig{Terminator: terminator}, 1024
		})
		start := time.Now()
		if _, err := tcpScrape(t, c, big.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "larger than the response limit") {
			t.Errorf("terminator %q: %v", terminator, err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("terminator %q: took %s", terminator, elapsed)
		}
		if body, err := tcpScrape(t, c, exact.Addr, nil); err != nil || len(body) != 1024 {
			t.Errorf("terminator %q, an answer of the limit: %d bytes, %v", terminator, len(body), err)
		}
	}
	limited := tcpCollector(t, func(c *model.Collector) { c.Limits.MaxResponseBytes = 1023 })
	if _, err := tcpScrape(t, limited, exact.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("limits.max_response_bytes: %v", err)
	}
}

// A tls block makes the scrape speak TLS, verifying the server's
// certificate against ca_file; one that does not verify fails the scrape,
// and the label is tls://host:port.
func TestATCPCollectorSpeaksTLS(t *testing.T) {
	server := testutil.NewTLSTCPStub(t, memcached(nil))
	verified := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.TLS.CAFile = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"}, server.CAFile
	})
	if body, err := tcpScrape(t, verified, server.Addr, nil); err != nil || body != memcachedStats {
		t.Errorf("body %q, %v", body, err)
	}
	if label, err := RequestLabelFor(server.Addr, verified, RequestOverrides{}); err != nil || label != "tls://"+server.Addr {
		t.Errorf("label %q, %v", label, err)
	}
	unknown := tcpCollector(t, func(c *model.Collector) { c.Request.TLS.ServerName = "127.0.0.1" })
	if _, err := tcpScrape(t, unknown, server.Addr, nil); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+server.Addr) {
		t.Errorf("an unknown authority: %v", err)
	}
	plain := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"} })
	if body, err := FetchCollector(context.Background(), server.Addr, plain, RequestOverrides{Timeout: 500 * time.Millisecond}, nil); err == nil {
		t.Errorf("plaintext to a TLS server was taken: %q", body.Body)
	}
}

// allowed_targets and denied_targets are kept to, and the cloud metadata
// service is refused without them: a refused target is never connected to.
func TestATCPCollectorKeepsToItsTargetLists(t *testing.T) {
	if _, err := tcpScrape(t, tcpCollector(t), "169.254.169.254:11211", nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("the metadata service: %v", err)
	}
	server := testutil.NewTCPStub(t, memcached(nil))
	for name, edit := range map[string]func(*model.Collector){
		"denied":      func(c *model.Collector) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} },
		"not allowed": func(c *model.Collector) { c.Request.AllowedTargets = []string{"10.0.0.0/8"} },
	} {
		if _, err := tcpScrape(t, tcpCollector(t, edit), server.Addr, nil); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := server.Accepted(); n != 0 {
		t.Errorf("a refused target was connected to %d times", n)
	}
	allowed := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own, c.Request.AllowedTargets = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"}, []string{"127.0.0.0/8"}
	})
	if _, err := tcpScrape(t, allowed, server.Addr, nil); err != nil {
		t.Errorf("allowed: %v", err)
	}
}

// A placeholder in send is filled from the probe; a value holding a
// carriage return or a line feed is refused before anything is sent, so a
// probe cannot add a command of its own; and a parameter send does not use
// is refused naming request.send.
func TestATCPCollectorFillsSendsPlaceholders(t *testing.T) {
	sent := make(chan string, 4)
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		line, _ := testutil.ReadUntil(conn, "\n")
		sent <- line
		_, _ = io.WriteString(conn, "VAR ups battery.charge \"100\"\nEND LIST VAR ups\n")
	})
	c := tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{Send: "LIST VAR {{param_ups:ups}}\n", Terminator: "END LIST VAR ups\n"}
	})
	if _, err := tcpScrape(t, c, server.Addr, map[string]string{"param_ups": "rack1"}); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != "LIST VAR rack1\n" {
		t.Errorf("sent %q", got)
	}
	for value, character := range map[string]string{"ups\nFSD ups": "a line break (U+000A)", "ups\rFSD": "a line break (U+000D)"} {
		_, err := tcpScrape(t, c, server.Addr, map[string]string{"param_ups": value})
		if err == nil || !strings.Contains(err.Error(), "request.send: the value of param_ups holds "+character) {
			t.Errorf("%q: %v", value, err)
		}
	}
	if n := server.Accepted(); n != 1 {
		t.Errorf("a refused value was sent: %d connections", n)
	}
	if err := CheckPathParams(c, RequestOverrides{Params: map[string]string{"param_ups": "a", "param_other": "b"}}); err == nil || !strings.Contains(err.Error(), "param_other") || !strings.Contains(err.Error(), "request.send") {
		t.Errorf("an unused parameter: %v", err)
	}
}

// A placeholder's value in send may hold only letters, digits and
// . _ - : / @ + = , %: HAProxy's runtime API runs each ';'-separated command
// of a line, so docs/TCP.md's own example with param_proxy=-1;disable
// server app/app1 would disable a server, and a blank or a quote would add
// an argument. Each is refused before the connection is made, naming the
// parameter and the character; every allowed character is sent. A default
// is held to the same characters when the collector loads.
func TestATCPCollectorHoldsSendsValuesToOneArgument(t *testing.T) {
	sent := make(chan string, 32)
	server := testutil.NewTCPStub(t, func(conn net.Conn) {
		line, _ := testutil.ReadUntil(conn, "\n")
		sent <- line
	})
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "show stat {{param_proxy:-1}} -1 -1\n"} })
	for value, character := range map[string]string{
		"-1;disable server app/app1": `';' (U+003B)`,
		"web 2":                      "a blank (U+0020)",
		`web"`:                       `'"' (U+0022)`,
		"web'":                       `'\'' (U+0027)`,
		"web|x":                      `'|' (U+007C)`,
		"web&x":                      `'&' (U+0026)`,
		"web#x":                      `'#' (U+0023)`,
		"web\tx":                     "a control character (U+0009)",
		"web\x00":                    "a control character (U+0000)",
		"wéb":                        `'é' (U+00E9)`,
		"web\xff":                    "a byte that is not UTF-8",
	} {
		_, err := tcpScrape(t, c, server.Addr, map[string]string{"param_proxy": value})
		if err == nil || !strings.Contains(err.Error(), "request.send: the value of param_proxy holds "+character+"; a value written into request.send may hold only letters, digits and . _ - : / @ + = , %") {
			t.Errorf("%q: %v", value, err)
		}
		if err := CheckPathParams(c, RequestOverrides{Params: map[string]string{"param_proxy": value}}); err == nil {
			t.Errorf("%q: the probe's check took it", value)
		}
	}
	if n := server.Accepted(); n != 0 {
		t.Errorf("a refused value was sent: %d connections", n)
	}
	allowed := "Web-1.app_2:3/x@y+z=w,v%20"
	if _, err := tcpScrape(t, c, server.Addr, map[string]string{"param_proxy": allowed}); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != "show stat "+allowed+" -1 -1\n" {
		t.Errorf("sent %q", got)
	}
	bad := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP, Own: TCPConfig{Send: "show stat {{param_proxy:-1;x}}\n"}}}
	if err := ValidateRequest(bad); err == nil || !strings.Contains(err.Error(), `collector "stats" request.send: the default of param_proxy holds ';' (U+003B)`) {
		t.Errorf("a default with ';': %v", err)
	}
	// sshexec's rules for an argument, no leading - and no .., are not
	// send's: a line of text is no program's argv.
	lead := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "{{param_proxy:-1}}\n"} })
	for _, value := range []string{"-1", "../..", "a/../b"} {
		if _, err := tcpScrape(t, lead, server.Addr, map[string]string{"param_proxy": value}); err != nil {
			t.Errorf("%q: %v", value, err)
		}
		if got := <-sent; got != value+"\n" {
			t.Errorf("sent %q", got)
		}
	}
}

// A collector's keys are checked when it loads: send and terminator are
// bounded, a terminator holds no placeholder and send no broken one, a key
// of another type is refused, and so is half a client certificate. A
// target is host:port, its port required, with no scheme, path or user.
func TestATCPCollectorsKeysAndTargetsAreChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"a send too long":       {func(c *model.Collector) { c.Request.Own = TCPConfig{Send: strings.Repeat("x", 64<<10+1)} }, "request.send is 65537 bytes, more than the 65536"},
		"a terminator too long": {func(c *model.Collector) { c.Request.Own = TCPConfig{Terminator: strings.Repeat("x", 64<<10+1)} }, "request.terminator is 65537 bytes"},
		"a placeholder ending":  {func(c *model.Collector) { c.Request.Own = TCPConfig{Terminator: "END {{param_x}}\n"} }, "request.terminator \"END {{param_x}}\\n\" has a placeholder"},
		"a broken placeholder":  {func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "get {{param_\r\n"} }, "request.send has an unclosed placeholder"},
		"a filter":              {func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "get {{param_k|json}}\r\n"} }, "has a filter"},
		"a key of another type": {func(c *model.Collector) {
			c.Request.Foreign = []string{"commands"}
		}, "sets request.commands, which does not apply"},
		"a header":                  {func(c *model.Collector) { c.Request.Headers = map[string]string{"X": "y"} }, "sets request.headers, which does not apply"},
		"half a client certificate": {func(c *model.Collector) { c.Request.TLS.CertFile = "/c.pem" }, "only one of cert_file and key_file"},
	} {
		c := &model.Collector{Name: "stats", Request: model.RequestConfig{Type: RequestTypeTCP}}
		tc.edit(c)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	tcpCollector(t, func(c *model.Collector) {
		c.Request.Own = TCPConfig{Send: strings.Repeat("x", 64<<10), Terminator: strings.Repeat("x", 64<<10)}
	})
	c := tcpCollector(t)
	for target, want := range map[string]string{
		"cache.internal:11211": "",
		"[2001:db8::1]:2181":   "",
		"cache.internal":       "is not host:port; a tcp target names its port",
		"2001:db8::1":          "is not host:port",
		"tcp://cache:11211":    "has the scheme tcp://",
		"tls://cache:11211":    "has the scheme tls://",
		"cache:11211/stats":    "has a path, query or user",
		"user@cache:11211":     "has a path, query or user",
		"cache:0":              `has the port "0"`,
		"cache:memcache":       `has the port "memcache"`,
		":11211":               "names no host",
		"":                     "target is required",
	} {
		err := CheckTarget(c, target, false)
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", target, err, want)
		}
	}
	if display := RequestTypes[RequestTypeTCP].Display("cache:11211/x"); display != "<invalid target>" {
		t.Errorf("display %q", display)
	}
}

// A debug probe's request section shows the bytes sent, quoted so that
// \r\n is seen, the url and method, and how reading stopped; a refused
// connection names the address; --dry-run reads the tls block's files.
func TestATCPScrapeIsTraced(t *testing.T) {
	server := testutil.NewTCPStub(t, memcached(nil))
	c := tcpCollector(t, func(c *model.Collector) { c.Request.Own = TCPConfig{Send: "stats\r\n", Terminator: "END\r\n"} })
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range trace.Requests() {
		got = append(got, r.Method+" "+r.URL+" "+r.Header.Get("Tcp-Send")+" -> "+r.Outcome)
	}
	want := `SEND tcp://` + server.Addr + ` "stats\r\n" -> read 99 bytes, the answer ended with the terminator "END\r\n"`
	if strings.Join(got, "\n") != want {
		t.Errorf("traced\n%s\nwant\n%s", strings.Join(got, "\n"), want)
	}
	closing := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "hello\n") })
	ctx, trace = WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, closing.Addr, tcpCollector(t), RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if r := trace.Requests(); len(r) != 1 || r[0].Method != "READ" || r[0].Header.Get("Tcp-Send") != `""` || r[0].Outcome != "read 6 bytes, the server closed the connection" {
		t.Errorf("traced %+v", r)
	}
	addr := testutil.RefusingAddress(t)
	ctx, trace = WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, addr, c, RequestOverrides{}, nil); err == nil || !strings.Contains(err.Error(), "connecting to "+addr) {
		t.Errorf("a refused connection: %v", err)
	}
	if r := trace.Requests(); len(r) != 1 || !strings.HasPrefix(r[0].Outcome, "error: connecting to "+addr) {
		t.Errorf("traced %+v", r)
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	missing := tcpCollector(t, func(c *model.Collector) { c.Request.TLS.CAFile = "/nonexistent/ca.pem" })
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.tls") {
		t.Errorf("warnings %v", warnings)
	}
}

// optInRequestKeys lists the keys tcp accepts that no type of a default
// build does: send and terminator (checkOptInRequestKeys).
func TestTheOptInRequestKeysAreTCPsOwn(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeTCP)
}
