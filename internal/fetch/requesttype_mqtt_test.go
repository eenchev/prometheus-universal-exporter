//go:build request_type_mqtt

package fetch

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// An mqtt collector's placeholders are in its topics.
func init() {
	ownPlaceholders[RequestTypeMQTT] = func(c *model.Collector) {
		c.Request.Own = MQTTConfig{Topics: []string{"sites/{{param_site:x}}/status"}}
	}
}

// mqttSent is a message the broker of the tests sends.
type mqttSent struct {
	topic, payload string
	qos            byte
	retained       bool
}

// mqttBroker is an MQTT 3.1.1 broker as the tests need one: it takes a
// CONNECT, answering with refuse when that is set and with a session when
// session is, sends queued to a client whose session it kept, answers a
// SUBSCRIBE and sends retained after it, answers pings, and publishes what
// publish hands it to every client connected. It keeps what it was sent.
type mqttBroker struct {
	*testutil.TCPStub
	mu sync.Mutex
	// What it answers with.
	refuse       byte
	session      bool
	subackRefuse bool
	queued       []mqttSent
	retained     []mqttSent
	// What it was sent.
	clientIDs, usernames, passwords []string
	cleanSessions                   []bool
	keepAlives                      []uint16
	subscribed                      [][]string
	subscribedQoS                   []byte
	acks, pings, disconnects        int
	clients                         map[net.Conn]bool
}

func newMQTTBroker(t *testing.T, secure bool) *mqttBroker {
	t.Helper()
	b := &mqttBroker{clients: map[net.Conn]bool{}}
	if secure {
		b.TCPStub = testutil.NewTLSTCPStub(t, b.serve)
	} else {
		b.TCPStub = testutil.NewTCPStub(t, b.serve)
	}
	return b
}

func (b *mqttBroker) send(conn net.Conn, id uint16, m mqttSent) {
	first := byte(mqttPublish<<4) | m.qos<<1
	if m.retained {
		first |= 1
	}
	rest := mqttString(nil, m.topic)
	if m.qos > 0 {
		rest = binary.BigEndian.AppendUint16(rest, id)
	}
	_, _ = conn.Write(mqttPacket(first, append(rest, m.payload...)))
}

// publish sends m to every client connected.
func (b *mqttBroker) publish(m mqttSent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for conn := range b.clients {
		b.send(conn, 77, m)
	}
}

func (b *mqttBroker) serve(conn net.Conn) {
	defer func() {
		b.mu.Lock()
		delete(b.clients, conn)
		b.mu.Unlock()
	}()
	link := &mqttLink{conn: &netConn{Conn: conn, r: bufio.NewReader(conn)}, most: 1 << 20}
	text := func(rest []byte) (string, []byte) {
		n := int(binary.BigEndian.Uint16(rest))
		return string(rest[2 : 2+n]), rest[2+n:]
	}
	for {
		first, rest, err := link.read()
		if err != nil {
			return
		}
		b.mu.Lock()
		switch first >> 4 {
		case mqttConnect:
			_, rest = text(rest)
			flags, alive := rest[1], binary.BigEndian.Uint16(rest[2:])
			id, rest := text(rest[4:])
			user, pass := "", ""
			if flags&0x80 != 0 {
				user, rest = text(rest)
			}
			if flags&0x40 != 0 {
				pass, _ = text(rest)
			}
			b.clientIDs, b.usernames, b.passwords = append(b.clientIDs, id), append(b.usernames, user), append(b.passwords, pass)
			b.cleanSessions, b.keepAlives = append(b.cleanSessions, flags&2 != 0), append(b.keepAlives, alive)
			present := byte(0)
			if b.session && flags&2 == 0 {
				present = 1
			}
			_, _ = conn.Write(mqttPacket(mqttConnack<<4, []byte{present, b.refuse}))
			if b.refuse != 0 {
				b.mu.Unlock()
				return
			}
			if present == 1 {
				for i, m := range b.queued {
					b.send(conn, uint16(i+1), m)
				}
				b.clients[conn] = true
			}
		case mqttSubscribe:
			var topics []string
			codes := rest[:2:2]
			for rest = rest[2:]; len(rest) > 0; rest = rest[1:] {
				var topic string
				topic, rest = text(rest)
				topics = append(topics, topic)
				b.subscribedQoS = append(b.subscribedQoS, rest[0])
				if b.subackRefuse {
					codes = append(codes, 0x80)
				} else {
					codes = append(codes, rest[0])
				}
			}
			b.subscribed = append(b.subscribed, topics)
			// A retained message may come before the SUBACK does.
			for i, m := range b.retained {
				if i == 0 {
					b.send(conn, 50, m)
				}
			}
			_, _ = conn.Write(mqttPacket(mqttSuback<<4, codes))
			for i, m := range b.retained {
				if i > 0 {
					b.send(conn, uint16(50+i), m)
				}
			}
			b.clients[conn] = true
		case mqttPuback:
			b.acks++
		case mqttPingreq:
			b.pings++
			_, _ = conn.Write(mqttPacket(mqttPingresp<<4, nil))
		case mqttDisconnect:
			b.disconnects++
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
	}
}

// seen reads what the broker was sent, and changes what it answers with,
// under its lock.
func (b *mqttBroker) seen(read func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	read()
}

func mqttCollector(t *testing.T, own MQTTConfig, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	if own.Window == 0 {
		own.Window = model.Duration(150 * time.Millisecond)
	}
	c := &model.Collector{Name: "broker", Request: model.RequestConfig{Type: RequestTypeMQTT, Own: own}}
	for _, fn := range change {
		fn(&c.Request)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mqttConns.closeAll)
	return c
}

func mqttDoc(t *testing.T, c *model.Collector, target string, params map[string]string) mqttDocument {
	t.Helper()
	resp, err := FetchCollector(context.Background(), target, c, RequestOverrides{Params: params}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc mqttDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v: %s", err, resp.Body)
	}
	return doc
}

// A scrape connects with a client identifier of its own and a clean
// session, logs in, subscribes to the collector's topics, a placeholder
// filled from the probe, reads the retained messages, the one that came
// before the SUBACK included, and disconnects: the document has each
// message's topic, its payload as text, as JSON when it is, and in base64
// when it is no text.
func TestAnMQTTScrapeReadsTheRetainedMessages(t *testing.T) {
	b := newMQTTBroker(t, false)
	b.seen(func() {
		b.retained = []mqttSent{
			{topic: "sites/berlin/status", payload: `{"up":true,"load":0.5}`, retained: true},
			{topic: "sites/berlin/temperature", payload: "21.5", retained: true},
			{topic: "sites/berlin/blob", payload: "\xff\x00", retained: true},
		}
	})
	password := testutil.WriteFile(t, "password", "s3cret\n")
	c := mqttCollector(t, MQTTConfig{Topics: []string{"sites/{{param_site}}/#", "plant/status"}}, withAccount("exporter", password))
	doc := mqttDoc(t, c, b.Addr, map[string]string{"param_site": "berlin"})
	if len(doc.Messages) != 3 || doc.Connection.Kept || !doc.Connection.New || doc.Connection.Received != 3 {
		t.Fatalf("the document is %+v", doc)
	}
	status, temperature, blob := doc.Messages[0], doc.Messages[1], doc.Messages[2]
	if status.Topic != "sites/berlin/status" || *status.Payload != `{"up":true,"load":0.5}` || string(status.JSON) != `{"up":true,"load":0.5}` || !status.Retained || status.QoS != 0 {
		t.Errorf("the first message is %+v", status)
	}
	if string(temperature.JSON) != "21.5" || blob.Payload != nil || blob.PayloadBase64 != "/wA=" || blob.JSON != nil {
		t.Errorf("the messages are %+v and %+v", temperature, blob)
	}
	b.seen(func() {
		if len(b.clientIDs) != 1 || !strings.HasPrefix(b.clientIDs[0], "pue-") || len(b.clientIDs[0]) != 20 || !b.cleanSessions[0] || b.usernames[0] != "exporter" || b.passwords[0] != "s3cret" || b.keepAlives[0] != 30 {
			t.Errorf("the CONNECT was of %q, clean %v, %q %q, keep alive %v", b.clientIDs, b.cleanSessions, b.usernames, b.passwords, b.keepAlives)
		}
		if fmt.Sprint(b.subscribed) != "[[sites/berlin/# plant/status]]" {
			t.Errorf("subscribed to %v", b.subscribed)
		}
	})
	testutil.WaitFor(t, "the scrape's DISCONNECT", func() bool {
		done := false
		b.seen(func() { done = b.disconnects == 1 })
		return done
	})
	// A value that would widen the subscription is refused before the
	// broker is connected to.
	_, err := FetchCollector(context.Background(), b.Addr, c, RequestOverrides{Params: map[string]string{"param_site": "+"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "request.topics[0]: the value of param_site is empty or holds a /, a wildcard or a control character") || b.Accepted() != 1 {
		t.Errorf("err=%v, %d connections", err, b.Accepted())
	}
}

// Reading ends once max_messages have come, long before the window has
// passed; and messages past the response limit fail the scrape as a limit
// exceeded.
func TestAnMQTTScrapeEndsAtMaxMessagesAndHoldsTheResponseLimit(t *testing.T) {
	b := newMQTTBroker(t, false)
	b.seen(func() {
		b.retained = []mqttSent{{topic: "a", payload: "1"}, {topic: "b", payload: "2"}, {topic: "c", payload: strings.Repeat("x", 4096)}}
	})
	start := time.Now()
	// The keep alive is told in whole seconds, a part of one a whole one.
	doc := mqttDoc(t, mqttCollector(t, MQTTConfig{Topics: []string{"#"}, MaxMessages: 2, Window: model.Duration(20 * time.Second), KeepAlive: model.Duration(1500 * time.Millisecond)}), b.Addr, nil)
	b.seen(func() {
		if b.keepAlives[0] != 2 {
			t.Errorf("the keep alive told is %d seconds, want 2 for 1.5s", b.keepAlives[0])
		}
	})
	if len(doc.Messages) != 2 || time.Since(start) > 5*time.Second {
		t.Errorf("%d messages after %s, want 2 at once", len(doc.Messages), time.Since(start))
	}
	small := mqttCollector(t, MQTTConfig{Topics: []string{"#"}}, func(r *model.RequestConfig) { r.MaxResponseBytes = 1024 })
	_, err := FetchCollector(context.Background(), b.Addr, small, RequestOverrides{}, nil)
	if !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("err=%v, want the response limit", err)
	}
}

// persistent_session connects with the collector's client identifier and
// no clean session; a broker that kept the session sends what it queued at
// once, which is acknowledged, and is not subscribed to again, so that it
// does not send every retained message again.
func TestAnMQTTPersistentSessionReadsWhatTheBrokerQueued(t *testing.T) {
	b := newMQTTBroker(t, false)
	b.seen(func() { b.retained = []mqttSent{{topic: "jobs/done", payload: "old", retained: true}} })
	c := mqttCollector(t, MQTTConfig{Topics: []string{"jobs/done"}, QoS: 1, ClientID: "exporter-1", PersistentSession: true})
	first := mqttDoc(t, c, b.Addr, nil)
	if first.Connection.SessionPresent || len(first.Messages) != 1 {
		t.Fatalf("the first scrape, with no session kept: %+v", first)
	}
	b.seen(func() {
		b.session, b.queued = true, []mqttSent{{topic: "jobs/done", payload: "a", qos: 1}, {topic: "jobs/done", payload: "b", qos: 1}}
	})
	second := mqttDoc(t, c, b.Addr, nil)
	if !second.Connection.SessionPresent || len(second.Messages) != 2 || *second.Messages[1].Payload != "b" || second.Messages[0].QoS != 1 {
		t.Errorf("the second scrape: %+v", second)
	}
	b.seen(func() {
		if fmt.Sprint(b.clientIDs) != "[exporter-1 exporter-1]" || b.cleanSessions[0] || len(b.subscribed) != 1 || b.subscribedQoS[0] != 1 || b.acks != 2 {
			t.Errorf("client ids %v, clean %v, %d subscriptions at QoS %v, %d acknowledgements", b.clientIDs, b.cleanSessions, len(b.subscribed), b.subscribedQoS, b.acks)
		}
	})
}

// keep_connection makes one connection and keeps it: the next scrape
// answers at once from what came on it, the last message of each topic and
// how many came; the connection is pinged, counted for the collector's
// self-metrics, closed with a DISCONNECT when no scrape used it for
// idle_timeout, and made again by the scrape after.
func TestAnMQTTKeptConnectionAnswersFromWhatCame(t *testing.T) {
	b := newMQTTBroker(t, false)
	b.seen(func() { b.retained = []mqttSent{{topic: "line/speed", payload: "10", retained: true}} })
	idle := model.Duration(1200 * time.Millisecond)
	c := mqttCollector(t, MQTTConfig{Topics: []string{"line/#"}, KeepConnection: true, KeepAlive: model.Duration(time.Second)}, func(r *model.RequestConfig) { r.IdleTimeout = &idle })
	first := mqttDoc(t, c, b.Addr, nil)
	if !first.Connection.Kept || !first.Connection.New || len(first.Messages) != 1 || !first.Messages[0].Retained {
		t.Fatalf("the first scrape: %+v", first)
	}
	b.publish(mqttSent{topic: "line/speed", payload: "11"})
	b.publish(mqttSent{topic: "line/speed", payload: "12"})
	b.publish(mqttSent{topic: "line/state", payload: `"run"`})
	var second mqttDocument
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		start := time.Now()
		second = mqttDoc(t, c, b.Addr, nil)
		if time.Since(start) > 100*time.Millisecond {
			t.Fatalf("a scrape of a kept connection took %s, and waits for nothing", time.Since(start))
		}
		if second.Connection.Received == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if second.Connection.New || len(second.Messages) != 2 || *second.Messages[0].Payload != "12" || second.Messages[0].Count != 3 || second.Messages[0].Retained || second.Messages[1].Topic != "line/state" {
		t.Fatalf("the second scrape: %+v", second)
	}
	if open, made := CollectorConnections(RequestTypeMQTT, c.Name); open != 1 || made != 1 || b.Accepted() != 1 {
		t.Errorf("%d open, %d made, %d accepted; want one connection for every scrape", open, made, b.Accepted())
	}
	// Left alone past idle_timeout, the connection is pinged, then closed.
	testutil.WaitFor(t, "the idle connection to close with a DISCONNECT", func() bool {
		open, _ := CollectorConnections(RequestTypeMQTT, c.Name)
		closed := false
		b.seen(func() { closed = b.disconnects == 1 && b.pings > 0 })
		return open == 0 && closed
	})
	if third := mqttDoc(t, c, b.Addr, nil); !third.Connection.New || b.Accepted() != 2 {
		t.Errorf("the scrape after the connection closed: %+v on %d connections", third.Connection, b.Accepted())
	}
	// A configuration without the collector closes its connection.
	FollowConfiguration(nil, nil)
	if open, made := CollectorConnections(RequestTypeMQTT, c.Name); open != 0 || made != 0 {
		t.Errorf("%d open and %d made after the collector went", open, made)
	}
}

// The connections kept are at most mqttMaxKept: a scrape that would make
// one more fails, saying so, until one of them has ended, whose place the
// next is given; and a connection that could not be made takes no place.
func TestTheMQTTConnectionsKeptAreBounded(t *testing.T) {
	saved := mqttMaxKept
	mqttMaxKept = 1
	t.Cleanup(func() { mqttMaxKept = saved })
	b := newMQTTBroker(t, false)
	refusing := newMQTTBroker(t, false)
	refusing.seen(func() { refusing.refuse = 3 })
	c := mqttCollector(t, MQTTConfig{Topics: []string{"line/{{param_line}}"}, KeepConnection: true, Window: model.Duration(10 * time.Millisecond)})
	line := func(name string) RequestOverrides {
		return RequestOverrides{Params: map[string]string{"param_line": name}}
	}
	if _, err := FetchCollector(context.Background(), refusing.Addr, c, line("a"), nil); err == nil || !strings.Contains(err.Error(), "server unavailable") {
		t.Fatalf("err=%v, want the broker's refusal", err)
	}
	if _, err := FetchCollector(context.Background(), b.Addr, c, line("a"), nil); err != nil {
		t.Fatalf("after a connection that was not made: %v", err)
	}
	if _, err := FetchCollector(context.Background(), b.Addr, c, line("b"), nil); err == nil || !strings.Contains(err.Error(), "the exporter keeps 1 MQTT connections already") {
		t.Errorf("err=%v, want the bound named", err)
	}
	// The broker ends the first connection, and its place is free.
	b.Close()
	other := newMQTTBroker(t, false)
	testutil.WaitFor(t, "the kept connection to end", func() bool {
		open, _ := CollectorConnections(RequestTypeMQTT, c.Name)
		return open == 0
	})
	if _, err := FetchCollector(context.Background(), other.Addr, c, line("b"), nil); err != nil {
		t.Errorf("after the first connection ended: %v", err)
	}
}

// A scrape that waits for a kept connection another scrape is making
// gives up at its own deadline, and not at the other's.
func TestAnMQTTScrapeWaitingForAKeptConnectionHoldsItsDeadline(t *testing.T) {
	silent := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	c := mqttCollector(t, MQTTConfig{Topics: []string{"a"}, KeepConnection: true})
	making := make(chan struct{})
	go func() {
		defer close(making)
		_, _ = FetchCollector(context.Background(), silent.Addr, c, RequestOverrides{Timeout: 3 * time.Second}, nil)
	}()
	testutil.WaitFor(t, "the first scrape to connect", func() bool { return silent.Accepted() == 1 })
	start := time.Now()
	_, err := FetchCollector(context.Background(), silent.Addr, c, RequestOverrides{Timeout: 100 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("err=%v after %s, want the scrape's own deadline", err, time.Since(start))
	}
	<-making
}

// What a kept connection remembers is counted as the document counts it,
// a JSON payload twice: messages that would not fit the response limit
// together are dropped and counted, and the scrape is answered.
func TestAnMQTTKeptConnectionKeepsWhatFitsTheDocument(t *testing.T) {
	b := newMQTTBroker(t, false)
	payload := `{"text":"` + strings.Repeat("x", 380) + `"}`
	b.seen(func() {
		b.retained = []mqttSent{{topic: "a", payload: payload}, {topic: "b", payload: payload}, {topic: "c", payload: payload}}
	})
	c := mqttCollector(t, MQTTConfig{Topics: []string{"#"}, KeepConnection: true}, func(r *model.RequestConfig) { r.MaxResponseBytes = 1024 })
	doc := mqttDoc(t, c, b.Addr, nil)
	if len(doc.Messages) != 1 || doc.Connection.Dropped != 2 || doc.Connection.Received != 3 {
		t.Errorf("%d messages kept, %d dropped of %d; want one that fits and two dropped", len(doc.Messages), doc.Connection.Dropped, doc.Connection.Received)
	}
}

// A window longer than the keep alive is pinged through, so that the
// broker does not give the connection up in the middle of it.
func TestAnMQTTScrapePingsThroughALongWindow(t *testing.T) {
	b := newMQTTBroker(t, false)
	c := mqttCollector(t, MQTTConfig{Topics: []string{"a"}, KeepAlive: model.Duration(time.Second), Window: model.Duration(1300 * time.Millisecond)})
	mqttDoc(t, c, b.Addr, nil)
	pings := 0
	b.seen(func() { pings = b.pings })
	if pings == 0 {
		t.Error("no PINGREQ was sent in a window longer than the keep alive")
	}
}

// A broker that refuses the connection, or a subscription, fails the
// scrape, saying which and why, and never with the password.
func TestAnMQTTScrapeFailsOnARefusal(t *testing.T) {
	password := testutil.WriteFile(t, "password", "hunter2\n")
	c := mqttCollector(t, MQTTConfig{Topics: []string{"secret/#"}}, withAccount("exporter", password))
	b := newMQTTBroker(t, false)
	b.seen(func() { b.refuse = 5 })
	ctx, trace := WithRequestTrace(context.Background())
	_, err := FetchCollector(ctx, b.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "CONNECT of mqtt://"+b.Addr+": the broker refused the connection: not authorized") {
		t.Errorf("err=%v, want the refusal", err)
	}
	if shown := fmt.Sprint(err, trace.Requests()); strings.Contains(shown, "hunter2") {
		t.Errorf("the error or the trace holds the password: %s", shown)
	}
	// A return code MQTT does not define is a value of the answer, not
	// told to whoever called the probe.
	b = newMQTTBroker(t, false)
	b.seen(func() { b.refuse = 77 })
	_, err = FetchCollector(context.Background(), b.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "return code 77, which MQTT 3.1.1 does not define") || strings.Contains(model.CallerText(err), "return code 77") {
		t.Errorf("err=%v, told %q", err, model.CallerText(err))
	}
	b = newMQTTBroker(t, false)
	b.seen(func() { b.subackRefuse = true })
	_, err = FetchCollector(context.Background(), b.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), `the broker refused the subscription to "secret/#"`) {
		t.Errorf("err=%v, want the subscription's refusal", err)
	}
	// A text MQTT cannot say the length of is not sent cut short.
	long := mqttCollector(t, MQTTConfig{Topics: []string{"a"}}, withAccount(strings.Repeat("u", 70000), password))
	b = newMQTTBroker(t, false)
	_, err = FetchCollector(context.Background(), b.Addr, long, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "longer than 65535 bytes") {
		t.Errorf("err=%v; want the length refused", err)
	}
	stub := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\n\r\n")
	})
	_, err = FetchCollector(context.Background(), stub.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "is the target an MQTT broker?") {
		t.Errorf("err=%v, want what is no broker named", err)
	}
}

// An mqtts:// target is TLS, verified with the collector's tls block, and
// its certificate is what $tls describes; a target without a scheme is TLS
// too when the collector has a tls block.
func TestAnMQTTSTargetIsTLS(t *testing.T) {
	b := newMQTTBroker(t, true)
	b.seen(func() { b.retained = []mqttSent{{topic: "a", payload: "1"}} })
	c := mqttCollector(t, MQTTConfig{Topics: []string{"a"}, MaxMessages: 1}, func(r *model.RequestConfig) { r.TLS = model.TLSConfig{CAFile: b.CAFile} })
	for _, target := range []string{"mqtts://" + b.Addr, b.Addr} {
		resp, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil)
		if err != nil || resp.Trip == nil || resp.Trip.TLSConnection == nil {
			t.Errorf("%s: %v, %+v", target, err, resp)
		}
	}
	if label, err := RequestLabelFor(b.Addr, c, RequestOverrides{}); err != nil || label != "mqtts://"+b.Addr {
		t.Errorf("the label is %q, %v", label, err)
	}
	if _, err := FetchCollector(context.Background(), "mqtt://"+b.Addr, c, RequestOverrides{}, nil); err == nil {
		t.Error("a plaintext connection to a TLS broker was answered")
	}
}

// A collector is refused at load for what no subscription can be made of.
func TestMQTTConfigurationRules(t *testing.T) {
	second := model.Duration(time.Second)
	negative := model.Duration(-time.Second)
	// 0 keeps a connection as long as its collector is configured.
	never := model.Duration(0)
	mqttCollector(t, MQTTConfig{Topics: []string{"a"}, KeepConnection: true}, func(r *model.RequestConfig) { r.IdleTimeout = &never })
	for name, tc := range map[string]struct {
		own    MQTTConfig
		change func(*model.RequestConfig)
		want   string
	}{
		"no topics":      {MQTTConfig{}, nil, "request.topics is required"},
		"hash":           {MQTTConfig{Topics: []string{"a/#/b"}}, nil, "has a # that is not the whole last level"},
		"plus":           {MQTTConfig{Topics: []string{"a/b+"}}, nil, "has a + that is not a whole level"},
		"twice":          {MQTTConfig{Topics: []string{"a", "a"}}, nil, `request.topics[1] "a" is written twice`},
		"qos":            {MQTTConfig{Topics: []string{"a"}, QoS: 2}, nil, "request.qos 2 is not 0 or 1"},
		"keep alive":     {MQTTConfig{Topics: []string{"a"}, KeepAlive: model.Duration(-time.Second)}, nil, "request.keep_alive is negative"},
		"session no id":  {MQTTConfig{Topics: []string{"a"}, PersistentSession: true}, nil, "request.persistent_session without request.client_id"},
		"both":           {MQTTConfig{Topics: []string{"a"}, PersistentSession: true, ClientID: "x", KeepConnection: true}, nil, "both request.persistent_session and request.keep_connection"},
		"idle alone":     {MQTTConfig{Topics: []string{"a"}}, func(r *model.RequestConfig) { r.IdleTimeout = &second }, "request.idle_timeout without request.keep_connection"},
		"idle negative":  {MQTTConfig{Topics: []string{"a"}, KeepConnection: true}, func(r *model.RequestConfig) { r.IdleTimeout = &negative }, "request.idle_timeout is negative"},
		"password alone": {MQTTConfig{Topics: []string{"a"}}, func(r *model.RequestConfig) { r.PasswordFile = "/p" }, "request.password_file without request.username"},
	} {
		c := &model.Collector{Name: "broker", Request: model.RequestConfig{Type: RequestTypeMQTT, Own: tc.own}}
		if tc.change != nil {
			tc.change(&c.Request)
		}
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
}

// A broker among the collector's denied targets is refused before it is
// connected to.
func TestAnMQTTCollectorHoldsTheTargetPolicy(t *testing.T) {
	b := newMQTTBroker(t, false)
	c := mqttCollector(t, MQTTConfig{Topics: []string{"a"}}, func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := FetchCollector(context.Background(), b.Addr, c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) || b.Accepted() != 0 {
		t.Errorf("err=%v, %d connections; want the target refused", err, b.Accepted())
	}
}

// The keys the opt-in mqtt type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheMQTTTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeMQTT)
}
