//go:build request_type_amqp

package fetch

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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

// amqpStandIn is an AMQP 0-9-1 broker of one virtual host, /prod, one
// account, monitor with its password, and the queues of its map, each with
// its ready messages and its consumers; a queue named "secret" is one the
// account may not ask about.
type amqpStandIn struct {
	mu       sync.Mutex
	queues   map[string][2]uint32
	password string
	// mechanisms is what the broker offers, and silent says it closes the
	// connection at a login it refuses, saying nothing.
	mechanisms string
	silent     bool
	// declared are the queues asked about, each with the channel it was
	// asked on, and closed says the client closed as the protocol has it.
	declared []string
	closed   bool
}

func amqpFrame(channel uint16, class, method uint16, arguments []byte) []byte {
	frame := []byte{1}
	frame = binary.BigEndian.AppendUint16(frame, channel)
	frame = binary.BigEndian.AppendUint32(frame, uint32(4+len(arguments)))
	frame = binary.BigEndian.AppendUint16(frame, class)
	frame = binary.BigEndian.AppendUint16(frame, method)
	return append(append(frame, arguments...), 0xCE)
}

// serve speaks the broker's side of one connection.
func (s *amqpStandIn) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil || string(header) != "AMQP\x00\x00\x09\x01" {
		_, _ = conn.Write([]byte("AMQP\x00\x01\x00\x00"))
		return
	}
	read := func() (channel, class, method uint16, arguments []byte, ok bool) {
		head := make([]byte, 7)
		if _, err := io.ReadFull(r, head); err != nil {
			return 0, 0, 0, nil, false
		}
		payload := make([]byte, binary.BigEndian.Uint32(head[3:])+1)
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, 0, 0, nil, false
		}
		return binary.BigEndian.Uint16(head[1:]), binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:]), payload[4 : len(payload)-1], true
	}
	// Start: version 0-9, the properties, the logins, the locales. A
	// heartbeat first, which a client passes over.
	properties := amqpLongString(append(amqpShortString(nil, "product"), 'S'), "RabbitMQ")
	properties = amqpLongString(append(amqpShortString(properties, "capabilities"), 'F'), string(append(amqpShortString(nil, "publisher_confirms"), 't', 1)))
	properties = amqpLongString(append(amqpShortString(properties, "version"), 'S'), "4.0.2")
	properties = append(amqpShortString(properties, "an_int"), 'I', 0, 0, 0, 7)
	properties = amqpLongString(append(amqpShortString(properties, "cluster_name"), 'S'), "rabbit@node1")
	start := amqpLongString([]byte{0, 9}, string(properties))
	start = amqpLongString(amqpLongString(start, s.mechanisms), "en_US")
	_, _ = conn.Write(append([]byte{8, 0, 0, 0, 0, 0, 0, 0xCE}, amqpFrame(0, 10, 10, start)...))
	closeWith := func(code uint16, text string) {
		_, _ = conn.Write(amqpFrame(0, 10, 50, append(amqpShortString(binary.BigEndian.AppendUint16(nil, code), text), 0, 0, 0, 0)))
	}
	_, class, method, arguments, ok := read()
	if !ok || class != 10 || method != 11 {
		return
	}
	startOk := amqpReader{b: arguments}
	client := startOk.table()
	mechanism, response := startOk.shortString(), startOk.longString()
	if mechanism != "PLAIN" || client["product"] != "prometheus-universal-exporter" || response != "\x00monitor\x00"+s.password {
		if !s.silent {
			closeWith(403, "ACCESS_REFUSED - Login was refused using authentication mechanism PLAIN. password="+strings.TrimPrefix(response, "\x00monitor\x00"))
		}
		return
	}
	_, _ = conn.Write(amqpFrame(0, 10, 30, []byte{0, 3, 0, 2, 0, 0, 0, 60}))
	if _, class, method, _, ok = read(); !ok || class != 10 || method != 31 {
		return
	}
	_, class, method, arguments, ok = read()
	open := amqpReader{b: arguments}
	if vhost := open.shortString(); !ok || class != 10 || method != 40 || vhost != "/prod" {
		closeWith(530, "NOT_ALLOWED - vhost "+vhost+" not found")
		return
	}
	_, _ = conn.Write(amqpFrame(0, 10, 41, amqpShortString(nil, "")))
	channels := map[uint16]bool{}
	for {
		channel, class, method, arguments, ok := read()
		if !ok {
			return
		}
		switch {
		case class == 20 && method == 10:
			channels[channel] = true
			_, _ = conn.Write(amqpFrame(channel, 20, 11, amqpLongString(nil, "")))
		case class == 20 && method == 41:
			delete(channels, channel)
		case class == 50 && method == 10 && channels[channel]:
			declare := amqpReader{b: arguments}
			declare.short()
			queue := declare.shortString()
			passive := declare.octet()
			s.mu.Lock()
			s.declared = append(s.declared, queue)
			counts, known := s.queues[queue]
			s.mu.Unlock()
			switch {
			case passive != 1:
				closeWith(540, "NOT_IMPLEMENTED - the stand-in declares nothing")
				return
			case queue == "secret":
				_, _ = conn.Write(amqpFrame(channel, 20, 40, append(amqpShortString(binary.BigEndian.AppendUint16(nil, 403), "ACCESS_REFUSED - access to queue 'secret' refused"), 0, 50, 0, 10)))
			case !known:
				_, _ = conn.Write(amqpFrame(channel, 20, 40, append(amqpShortString(binary.BigEndian.AppendUint16(nil, 404), "NOT_FOUND - no queue '"+queue+"'"), 0, 50, 0, 10)))
			default:
				ok := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(amqpShortString(nil, queue), counts[0]), counts[1])
				_, _ = conn.Write(amqpFrame(channel, 50, 11, ok))
			}
		case class == 10 && method == 50:
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			_, _ = conn.Write(amqpFrame(0, 10, 51, nil))
			return
		default:
			closeWith(503, "COMMAND_INVALID")
			return
		}
	}
}

func newAMQPStandIn() *amqpStandIn {
	return &amqpStandIn{queues: map[string][2]uint32{"orders": {42, 3}, "emails": {0, 1}}, password: "s3cret-pw", mechanisms: "AMQPLAIN PLAIN"}
}

// amqpCollector is a validated amqp collector of the account monitor,
// changed by edit first.
func amqpCollector(t *testing.T, password string, edit func(*model.Collector, *AMQPConfig)) *model.Collector {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	own := AMQPConfig{VHost: "/prod"}
	c := &model.Collector{Name: "broker", Request: model.RequestConfig{Type: RequestTypeAMQP, Username: "monitor", PasswordFile: path}}
	if edit != nil {
		edit(c, &own)
	}
	c.Request.Own = own
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func amqpDoc(t *testing.T, c *model.Collector, target string) (amqpDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return amqpDocument{}, err
	}
	var doc amqpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v in %s", err, resp.Body)
	}
	return doc, nil
}

// A scrape logs in to the broker, opens the virtual host and asks about
// each queue with a passive declare: the document holds what the broker
// says of itself and each queue's ready messages and consumers. A queue
// the broker does not have exists: false, one the account may not ask
// about says why, and the queues after either are asked on a channel of
// their own. The connection is closed as the protocol has it.
func TestAnAMQPScrapeCountsItsQueues(t *testing.T) {
	s := newAMQPStandIn()
	stub := testutil.NewTCPStub(t, s.serve)
	c := amqpCollector(t, "s3cret-pw", func(_ *model.Collector, r *AMQPConfig) { r.Queues = []string{"orders", "missing", "secret", "emails"} })
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, stub.Addr, c, RequestOverrides{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc amqpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Product != "RabbitMQ" || doc.Version != "4.0.2" || doc.ClusterName != "rabbit@node1" || doc.VHost != "/prod" || doc.TLS || doc.Address != stub.Addr || doc.Host != "127.0.0.1" ||
		strings.Join(doc.Mechanisms, "|") != "AMQPLAIN|PLAIN" || len(doc.ServerProperties) != 3 || doc.ConnectSeconds <= 0 || len(doc.Queues) != 4 {
		t.Fatalf("document %+v", doc)
	}
	got, _ := json.Marshal(doc.Queues)
	want := `[{"name":"orders","exists":true,"messages":42,"consumers":3,"error":""},{"name":"missing","exists":false,"messages":null,"consumers":null,"error":""},` +
		`{"name":"secret","exists":false,"messages":null,"consumers":null,"error":"403 ACCESS_REFUSED - access to queue 'secret' refused"},{"name":"emails","exists":true,"messages":0,"consumers":1,"error":""}]`
	if string(got) != want {
		t.Errorf("queues\n%s\nwant\n%s", got, want)
	}
	s.mu.Lock()
	if strings.Join(s.declared, "|") != "orders|missing|secret|emails" || !s.closed {
		t.Errorf("the broker was asked %v, closed %v", s.declared, s.closed)
	}
	s.mu.Unlock()
	requests := trace.Requests()
	if len(requests) != 1 || requests[0].Method != "DECLARE-PASSIVE" || requests[0].URL != "amqp://"+stub.Addr || requests[0].Outcome != "4 queues asked of RabbitMQ 4.0.2" || requests[0].Header.Get("Amqp-Vhost") != "/prod" {
		t.Errorf("traced %+v", requests)
	}
	if method, stage := RequestMethodFor(c, RequestOverrides{}), FetchStage(c); method != "DECLARE-PASSIVE" || stage != "amqp" {
		t.Errorf("method %q, stage %q", method, stage)
	}
	// Without queues the scrape logs in and asks about none.
	doc, err = amqpDoc(t, amqpCollector(t, "s3cret-pw", nil), stub.Addr)
	if err != nil || doc.Queues == nil || len(doc.Queues) != 0 || doc.Product != "RabbitMQ" {
		t.Errorf("no queues: %+v, %v", doc, err)
	}
}

// Over TLS, amqps://, the broker's certificate is verified and the same is
// spoken; the label is amqps:// and the port 5671's.
func TestAnAMQPScrapeOverTLS(t *testing.T) {
	s := newAMQPStandIn()
	stub := testutil.NewTLSTCPStub(t, s.serve)
	c := amqpCollector(t, "s3cret-pw", func(c *model.Collector, r *AMQPConfig) {
		r.Queues, c.Request.TLS.CAFile = []string{"orders"}, stub.CAFile
	})
	doc, err := amqpDoc(t, c, "amqps://"+stub.Addr)
	if err != nil || !doc.TLS || len(doc.Queues) != 1 || *doc.Queues[0].Messages != 42 {
		t.Fatalf("%+v, %v", doc, err)
	}
	if label, err := RequestLabelFor("amqps://Broker.example.com", c, RequestOverrides{}); err != nil || label != "amqps://broker.example.com:5671" {
		t.Errorf("label %q, %v", label, err)
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	unverified := amqpCollector(t, "s3cret-pw", nil)
	if _, err := amqpDoc(t, unverified, "amqps://"+stub.Addr); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+stub.Addr) {
		t.Errorf("an unverified broker: %v", err)
	}
}

// What a broker refuses fails the scrape with what it said, and never the
// password: a login it does not take, said or with the connection closed,
// a virtual host it does not have, a login other than PLAIN, and what is
// no AMQP 0-9-1 at all.
func TestAnAMQPScrapeFailsOnWhatTheBrokerRefuses(t *testing.T) {
	s := newAMQPStandIn()
	stub := testutil.NewTCPStub(t, s.serve)
	_, err := amqpDoc(t, amqpCollector(t, "wr0ng-pw", nil), stub.Addr)
	if err == nil || !strings.Contains(err.Error(), stub.Addr+" refused the login of monitor: 403 ACCESS_REFUSED") || strings.Contains(err.Error(), "wr0ng-pw") {
		t.Errorf("a refused login: %v", err)
	}
	_, err = amqpDoc(t, amqpCollector(t, "s3cret-pw", func(_ *model.Collector, r *AMQPConfig) { r.VHost = "/dev" }), stub.Addr)
	if err == nil || !strings.Contains(err.Error(), `refused the virtual host "/dev": 530 NOT_ALLOWED - vhost /dev not found`) {
		t.Errorf("a virtual host that is not there: %v", err)
	}
	silent := newAMQPStandIn()
	silent.silent = true
	_, err = amqpDoc(t, amqpCollector(t, "wr0ng-pw", nil), testutil.NewTCPStub(t, silent.serve).Addr)
	if err == nil || !strings.Contains(err.Error(), "closed the connection at the login of monitor, as a broker does to an account or a password it does not take") {
		t.Errorf("a login refused in silence: %v", err)
	}
	external := newAMQPStandIn()
	external.mechanisms = "EXTERNAL"
	_, err = amqpDoc(t, amqpCollector(t, "s3cret-pw", nil), testutil.NewTCPStub(t, external.serve).Addr)
	if err == nil || !strings.Contains(err.Error(), "offers the logins EXTERNAL and not PLAIN") {
		t.Errorf("no PLAIN: %v", err)
	}
	for want, says := range map[string]string{
		"does not speak AMQP 0-9-1: it answered the header of another version": "AMQP\x00\x01\x00\x00",
		"which is not AMQP 0-9-1": "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n",
	} {
		other := testutil.NewTCPStub(t, func(conn net.Conn) {
			_, _ = io.ReadFull(conn, make([]byte, 8))
			_, _ = conn.Write([]byte(says))
			_ = conn.Close()
		})
		if _, err := amqpDoc(t, amqpCollector(t, "s3cret-pw", nil), other.Addr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	// A broker that never answers ends the scrape at its deadline.
	mute := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := FetchCollector(ctx, mute.Addr, amqpCollector(t, "s3cret-pw", nil), RequestOverrides{}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a broker that says nothing: %v", err)
	}
}

// A target is the broker, with amqp:// or amqps:// or neither, and the
// keys are checked when the configuration loads; a host the collector's
// lists refuse is not connected to; and a collector without an account
// logs in as guest.
func TestAnAMQPTargetAndKeysAreChecked(t *testing.T) {
	c := amqpCollector(t, "x", nil)
	for _, target := range []string{"", "http://broker.example.com", "broker.example.com/vhost", "user@broker.example.com", "broker.example.com:0"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("target %q was taken", target)
		}
	}
	for target, label := range map[string]string{"Broker.example.com": "amqp://broker.example.com:5672", "amqp://10.0.0.1:5673": "amqp://10.0.0.1:5673", "amqps://broker.example.com": "amqps://broker.example.com:5671"} {
		if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != label {
			t.Errorf("target %q: label %q, %v; want %q", target, got, err, label)
		}
	}
	for want, edit := range map[string]func(*model.Collector, *AMQPConfig){
		"request.vhost is longer than 255 bytes":         func(_ *model.Collector, r *AMQPConfig) { r.VHost = "a\nb" },
		"request.queues names 101 queues":                func(_ *model.Collector, r *AMQPConfig) { r.Queues = make([]string, 101) },
		"request.queues[1] is empty or longer than 255":  func(_ *model.Collector, r *AMQPConfig) { r.Queues = []string{"a", ""} },
		"request.queues[0] has a control character or a": func(_ *model.Collector, r *AMQPConfig) { r.Queues = []string{"{{param_queue}}"} },
		`request.queues names "a" twice`:                 func(_ *model.Collector, r *AMQPConfig) { r.Queues = []string{"a", "b", "a"} },
		"sets only one of request.username and password": func(c *model.Collector, _ *AMQPConfig) { c.Request.Username, c.Request.PasswordFile = "monitor", "" },
		"request.tls sets only one of cert_file and key": func(c *model.Collector, _ *AMQPConfig) {
			c.Request.Username, c.Request.PasswordFile, c.Request.TLS.CertFile = "", "", "/c"
		},
	} {
		own := AMQPConfig{}
		c := &model.Collector{Name: "broker", Request: model.RequestConfig{Type: RequestTypeAMQP, Username: "monitor", PasswordFile: "/p"}}
		edit(c, &own)
		c.Request.Own = own
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	s := newAMQPStandIn()
	stub := testutil.NewTCPStub(t, s.serve)
	denied := amqpCollector(t, "s3cret-pw", func(c *model.Collector, _ *AMQPConfig) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := amqpDoc(t, denied, stub.Addr); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a refused host: %v", err)
	}
	guest := newAMQPStandIn()
	guest.password = "never"
	asGuest := &model.Collector{Name: "broker", Request: model.RequestConfig{Type: RequestTypeAMQP, Own: AMQPConfig{}}}
	if err := ValidateRequest(asGuest); err != nil {
		t.Fatal(err)
	}
	if _, err := amqpDoc(t, asGuest, testutil.NewTCPStub(t, guest.serve).Addr); err == nil || !strings.Contains(err.Error(), "refused the login of guest") {
		t.Errorf("without an account: %v", err)
	}
	missing := amqpCollector(t, "x", func(c *model.Collector, _ *AMQPConfig) { c.Request.PasswordFile = filepath.Join(t.TempDir(), "none") })
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.password_file") {
		t.Errorf("warnings %v", warnings)
	}
}

// The keys the opt-in amqp type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheAMQPTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeAMQP)
}
