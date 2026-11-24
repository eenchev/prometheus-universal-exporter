//go:build request_type_nats

package fetch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// natsStandIn is a NATS server: it sends its INFO, takes a CONNECT whose
// account check takes, answers PING, and answers the JetStream API's
// subjects with what its map holds for each, with a 503 of no responders
// for the rest when JetStream is off.
type natsStandIn struct {
	mu        sync.Mutex
	info      string
	check     func(connect map[string]any) bool
	jetstream map[string]string
	// connects are the CONNECT options the clients sent, and subjects the
	// subjects published to.
	connects []map[string]any
	subjects []string
}

func (s *natsStandIn) serve(conn net.Conn, upgrade func() (net.Conn, error)) {
	_, _ = io.WriteString(conn, "INFO "+s.info+"\r\n")
	if strings.Contains(s.info, `"tls_required":true`) && upgrade != nil {
		secured, err := upgrade()
		if err != nil {
			return
		}
		conn = secured
	}
	r := bufio.NewReader(conn)
	inbox := ""
	// The server asks whether the client is there, as a real one does.
	_, _ = io.WriteString(conn, "PING\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch verb {
		case "CONNECT":
			var options map[string]any
			_ = json.Unmarshal([]byte(rest), &options)
			s.mu.Lock()
			s.connects = append(s.connects, options)
			s.mu.Unlock()
			if s.check != nil && !s.check(options) {
				_, _ = io.WriteString(conn, "-ERR 'Authorization Violation'\r\n")
				_ = conn.Close()
				return
			}
		case "PING":
			_, _ = io.WriteString(conn, "PONG\r\n")
		case "PONG":
		case "SUB":
			inbox = strings.Fields(rest)[0]
		case "PUB":
			fields := strings.Fields(rest)
			_, _ = r.ReadString('\n')
			s.mu.Lock()
			s.subjects = append(s.subjects, fields[0])
			answer, known := s.jetstream[fields[0]]
			s.mu.Unlock()
			if !strings.HasPrefix(fields[1], strings.TrimSuffix(inbox, "*")) {
				_, _ = io.WriteString(conn, "-ERR 'Permissions Violation'\r\n")
				continue
			}
			if !known {
				header := "NATS/1.0 503\r\n\r\n"
				_, _ = fmt.Fprintf(conn, "HMSG %s 1 %d %d\r\n%s\r\n", fields[1], len(header), len(header), header)
				continue
			}
			_, _ = fmt.Fprintf(conn, "MSG %s 1 %d\r\n%s\r\n", fields[1], len(answer), answer)
		}
	}
}

const natsInfo = `{"server_id":"NA1","server_name":"n1","version":"2.10.18","proto":1,"headers":true,"max_payload":1048576,"jetstream":true,"auth_required":true,"nonce":"n0nce"}`

func newNATSStandIn() *natsStandIn {
	return &natsStandIn{info: natsInfo, jetstream: map[string]string{
		"$JS.API.INFO":                         `{"type":"io.nats.jetstream.api.v1.account_info_response","memory":0,"storage":1024,"streams":2,"consumers":3}`,
		"$JS.API.STREAM.INFO.ORDERS":           `{"config":{"name":"ORDERS"},"state":{"messages":120,"bytes":4096,"consumer_count":1}}`,
		"$JS.API.STREAM.INFO.GONE":             `{"error":{"code":404,"err_code":10059,"description":"stream not found"}}`,
		"$JS.API.CONSUMER.INFO.ORDERS.billing": `{"stream_name":"ORDERS","name":"billing","num_pending":7,"num_ack_pending":2}`,
		"$JS.API.CONSUMER.INFO.ORDERS.gone":    `{"error":{"code":404,"err_code":10014,"description":"consumer not found"}}`,
		"$JS.API.STREAM.INFO.LOCKED":           `{"error":{"code":403,"description":"not allowed, token=s3cret-token"}}`,
	}}
}

// natsCollector is a validated nats collector, changed by edit first.
func natsCollector(t *testing.T, edit func(*model.Collector, *NATSConfig)) *model.Collector {
	t.Helper()
	own := NATSConfig{}
	c := &model.Collector{Name: "bus", Request: model.RequestConfig{Type: RequestTypeNATS}}
	if edit != nil {
		edit(c, &own)
	}
	c.Request.Own = own
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func natsFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func natsDoc(t *testing.T, c *model.Collector, target string) (natsDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return natsDocument{}, err
	}
	var doc natsDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v in %s", err, resp.Body)
	}
	return doc, nil
}

// A scrape reads the server's INFO, logs in with the collector's account
// and times a PING: the document is the INFO, without its nonce, and the
// round trip. The server's own PING is answered on the way. The request is
// traced and labelled by the server.
func TestANATSScrapeTimesAPing(t *testing.T) {
	s := newNATSStandIn()
	s.check = func(o map[string]any) bool { return o["user"] == "monitor" && o["pass"] == "s3cret-pw" }
	stub := testutil.NewStartTLSStub(t, s.serve)
	c := natsCollector(t, func(c *model.Collector, _ *NATSConfig) {
		c.Request.Username, c.Request.PasswordFile = "monitor", natsFile(t, "s3cret-pw")
	})
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, stub.Addr, c, RequestOverrides{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc natsDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info["version"] != "2.10.18" || doc.Info["server_name"] != "n1" || doc.Info["nonce"] != nil || doc.TLS || doc.Address != stub.Addr || doc.Host != "127.0.0.1" ||
		doc.RTTSeconds <= 0 || doc.ConnectSeconds <= 0 || string(doc.JetStream) != "null" || doc.Streams == nil || len(doc.Streams) != 0 || len(doc.Consumers) != 0 {
		t.Errorf("document %s", resp.Body)
	}
	s.mu.Lock()
	connect := s.connects[0]
	s.mu.Unlock()
	if connect["verbose"] != false || connect["headers"] != true || connect["no_responders"] != true || connect["name"] != "prometheus-universal-exporter" || connect["auth_token"] != nil {
		t.Errorf("CONNECT %v", connect)
	}
	requests := trace.Requests()
	if len(requests) != 1 || requests[0].Method != "PING" || requests[0].URL != "nats://"+stub.Addr || !strings.HasPrefix(requests[0].Outcome, "PONG after ") {
		t.Errorf("traced %+v", requests)
	}
	if method, stage := RequestMethodFor(c, RequestOverrides{}), FetchStage(c); method != "PING" || stage != "nats" {
		t.Errorf("method %q, stage %q", method, stage)
	}
}

// With jetstream the scrape asks the account's JetStream state, and each
// stream and consumer named, with the JetStream API: the document holds
// each answer as the API writes it. A stream or a consumer the server does
// not have is found: false, one it refuses says why without a credential
// the refusal holds, and an account without JetStream says so and is no
// failure. A token logs in as one.
func TestANATSScrapeAsksJetStream(t *testing.T) {
	s := newNATSStandIn()
	s.check = func(o map[string]any) bool { return o["auth_token"] == "s3cret-token" && o["user"] == nil }
	stub := testutil.NewStartTLSStub(t, s.serve)
	c := natsCollector(t, func(c *model.Collector, r *NATSConfig) {
		c.Request.BearerTokenFile = natsFile(t, "s3cret-token")
		r.JetStream, r.Streams, r.Consumers = true, []string{"ORDERS", "GONE", "LOCKED"}, []string{"ORDERS/billing", "ORDERS/gone"}
	})
	doc, err := natsDoc(t, c, stub.Addr)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(struct {
		J json.RawMessage
		S []natsAnswer
		C []natsAnswer
	}{doc.JetStream, doc.Streams, doc.Consumers})
	want := `{"J":{"type":"io.nats.jetstream.api.v1.account_info_response","memory":0,"storage":1024,"streams":2,"consumers":3},` +
		`"S":[{"name":"ORDERS","found":true,"error":"","info":{"config":{"name":"ORDERS"},"state":{"messages":120,"bytes":4096,"consumer_count":1}}},{"name":"GONE","found":false,"error":"","info":null},{"name":"LOCKED","found":false,"error":"403 not allowed, token=<redacted>","info":null}],` +
		`"C":[{"stream":"ORDERS","name":"billing","found":true,"error":"","info":{"stream_name":"ORDERS","name":"billing","num_pending":7,"num_ack_pending":2}},{"stream":"ORDERS","name":"gone","found":false,"error":"","info":null}]}`
	// JSON writes an angle bracket as its escape.
	lt, gt := string([]byte{'\\', 'u', '0', '0', '3', 'c'}), string([]byte{'\\', 'u', '0', '0', '3', 'e'})
	if written := strings.NewReplacer(lt, "<", gt, ">").Replace(string(got)); written != want {
		t.Errorf("JetStream\n%s\nwant\n%s", written, want)
	}
	s.mu.Lock()
	if subjects := strings.Join(s.subjects, " "); subjects != "$JS.API.INFO $JS.API.STREAM.INFO.ORDERS $JS.API.STREAM.INFO.GONE $JS.API.STREAM.INFO.LOCKED $JS.API.CONSUMER.INFO.ORDERS.billing $JS.API.CONSUMER.INFO.ORDERS.gone" {
		t.Errorf("asked %s", subjects)
	}
	s.mu.Unlock()

	off := newNATSStandIn()
	off.jetstream = nil
	doc, err = natsDoc(t, natsCollector(t, func(_ *model.Collector, r *NATSConfig) { r.JetStream, r.Streams = true, []string{"ORDERS"} }), testutil.NewStartTLSStub(t, off.serve).Addr)
	if err != nil || string(doc.JetStream) != "null" || !strings.Contains(doc.JetStreamError, "JetStream is not enabled") || len(doc.Streams) != 0 {
		t.Errorf("without JetStream: %+v, %v", doc, err)
	}
	// An answer past the response limit fails the scrape as the limit.
	small := natsCollector(t, func(c *model.Collector, r *NATSConfig) { r.JetStream, c.Request.MaxResponseBytes = true, 16 })
	if _, err := natsDoc(t, small, testutil.NewStartTLSStub(t, newNATSStandIn().serve).Addr); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("an answer past the limit: %v", err)
	}
}

// A server that requires TLS is spoken to over it after its INFO, its
// certificate verified by the tls block, and so is one the target names
// with tls://; a target that says tls:// of a server that offers none
// fails, and nothing is logged in with.
func TestANATSScrapeTurnsToTLS(t *testing.T) {
	s := newNATSStandIn()
	s.info = strings.Replace(natsInfo, `"auth_required":true`, `"tls_required":true`, 1)
	stub := testutil.NewStartTLSStub(t, s.serve)
	c := natsCollector(t, func(c *model.Collector, _ *NATSConfig) { c.Request.TLS.CAFile = stub.CAFile })
	for _, target := range []string{stub.Addr, "tls://" + stub.Addr} {
		doc, err := natsDoc(t, c, target)
		if err != nil || !doc.TLS || doc.Info["tls_required"] != true {
			t.Fatalf("%s: %+v, %v", target, doc, err)
		}
	}
	if label, err := RequestLabelFor("tls://NATS.example.com", c, RequestOverrides{}); err != nil || label != "tls://nats.example.com:4222" {
		t.Errorf("label %q, %v", label, err)
	}
	if _, err := natsDoc(t, natsCollector(t, nil), stub.Addr); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+stub.Addr) {
		t.Errorf("an unverified server: %v", err)
	}
	plain := newNATSStandIn()
	plainStub := testutil.NewStartTLSStub(t, plain.serve)
	if _, err := natsDoc(t, c, "tls://"+plainStub.Addr); err == nil || !strings.Contains(err.Error(), "offers no TLS, and the target says tls://") || len(plain.connects) != 0 {
		t.Errorf("tls:// of a server without TLS: %v, %d logins", err, len(plain.connects))
	}
}

// A login the server does not take fails the scrape with what the server
// said, without the password; what is no NATS server fails it saying so;
// and a server that says nothing ends it at its deadline.
func TestANATSScrapeFailsOnWhatTheServerRefuses(t *testing.T) {
	s := newNATSStandIn()
	s.check = func(map[string]any) bool { return false }
	stub := testutil.NewStartTLSStub(t, s.serve)
	c := natsCollector(t, func(c *model.Collector, _ *NATSConfig) {
		c.Request.Username, c.Request.PasswordFile = "monitor", natsFile(t, "wr0ng-pw")
	})
	if _, err := natsDoc(t, c, stub.Addr); err == nil || !strings.Contains(err.Error(), stub.Addr+" refused: Authorization Violation") || strings.Contains(err.Error(), "wr0ng-pw") {
		t.Errorf("a refused login: %v", err)
	}
	other := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "220 mail.example ESMTP\r\n") })
	if _, err := natsDoc(t, natsCollector(t, nil), other.Addr); err == nil || !strings.Contains(err.Error(), "does not speak the NATS protocol: it did not begin with INFO") {
		t.Errorf("what is no NATS: %v", err)
	}
	mute := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := FetchCollector(ctx, mute.Addr, natsCollector(t, nil), RequestOverrides{}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a server that says nothing: %v", err)
	}
}

// A target is the server, with nats:// or tls:// or neither, and the keys
// are checked when the configuration loads; a host the collector's lists
// refuse is not connected to.
func TestANATSTargetAndKeysAreChecked(t *testing.T) {
	c := natsCollector(t, nil)
	for _, target := range []string{"", "http://nats.example.com", "nats.example.com/path", "user@nats.example.com", "nats.example.com:0"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("target %q was taken", target)
		}
	}
	for target, label := range map[string]string{"NATS.example.com": "nats://nats.example.com:4222", "nats://10.0.0.1:4333": "nats://10.0.0.1:4333"} {
		if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != label {
			t.Errorf("target %q: label %q, %v; want %q", target, got, err, label)
		}
	}
	for want, edit := range map[string]func(*model.Collector, *NATSConfig){
		"sets request.streams or request.consumers without":   func(_ *model.Collector, r *NATSConfig) { r.Streams = []string{"ORDERS"} },
		`request.streams[0] "A.B" is no name of a stream`:     func(_ *model.Collector, r *NATSConfig) { r.JetStream, r.Streams = true, []string{"A.B"} },
		`request.streams names "A" twice`:                     func(_ *model.Collector, r *NATSConfig) { r.JetStream, r.Streams = true, []string{"A", "A"} },
		`request.consumers[0] "billing" is not written strea`: func(_ *model.Collector, r *NATSConfig) { r.JetStream, r.Consumers = true, []string{"billing"} },
		`request.consumers[0] "A/b c" is not written stream/`: func(_ *model.Collector, r *NATSConfig) { r.JetStream, r.Consumers = true, []string{"A/b c"} },
		"request.streams or request.consumers names more tha": func(_ *model.Collector, r *NATSConfig) { r.JetStream, r.Streams = true, make([]string, 101) },
		"sets request.bearer_token_file beside request.usern": func(c *model.Collector, _ *NATSConfig) {
			c.Request.BearerTokenFile, c.Request.Username, c.Request.PasswordFile = "/t", "u", "/p"
		},
		"sets only one of request.username and password_file": func(c *model.Collector, _ *NATSConfig) { c.Request.Username = "monitor" },
	} {
		own := NATSConfig{}
		c := &model.Collector{Name: "bus", Request: model.RequestConfig{Type: RequestTypeNATS}}
		edit(c, &own)
		c.Request.Own = own
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	stub := testutil.NewStartTLSStub(t, newNATSStandIn().serve)
	denied := natsCollector(t, func(c *model.Collector, _ *NATSConfig) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := natsDoc(t, denied, stub.Addr); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a refused host: %v", err)
	}
	missing := natsCollector(t, func(c *model.Collector, _ *NATSConfig) {
		c.Request.BearerTokenFile = filepath.Join(t.TempDir(), "none")
	})
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.bearer_token_file") {
		t.Errorf("warnings %v", warnings)
	}
}

// The keys the opt-in nats type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheNATSTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeNATS)
}
