// Package kafkatest is a stand-in Kafka cluster for the tests of the kafka
// request type. Each broker is a TCP server, or a TLS one with httptest's
// certificate, which verifies it for 127.0.0.1 and whose file it writes, all answering Kafka's protocol at the versions
// the type sends:
//   - ApiVersions, with the versions a test may narrow to play an old
//     broker;
//   - SaslHandshake and SaslAuthenticate, for PLAIN, SCRAM-SHA-256,
//     SCRAM-SHA-512 and OAUTHBEARER against the users and the token the
//     test gave it, refusing every other request before a login when it
//     has mechanisms;
//   - Metadata, of the brokers, topics and partitions the test gave it;
//   - ListGroups, of the groups each broker coordinates, DescribeGroups
//     and OffsetFetch;
//   - ListOffsets, of the partitions each broker leads, NOT_LEADER for the
//     rest;
//   - DescribeLogDirs, of each broker's one log directory and the replicas
//     it holds, and DescribeConfigs, of the topics' settings.
//
// A test may answer a request itself, with any bytes, to play a broken or
// hostile broker, and stop a broker to play one that is down. It records
// every request each broker was sent. It imports nothing but the standard
// library.
package kafkatest

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"hash"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The API keys the stand-in answers.
const (
	ListOffsets      int16 = 2
	Metadata         int16 = 3
	OffsetFetch      int16 = 9
	DescribeGroups   int16 = 15
	ListGroups       int16 = 16
	SaslHandshake    int16 = 17
	APIVersions      int16 = 18
	DescribeConfigs  int16 = 32
	DescribeLogDirs  int16 = 35
	SaslAuthenticate int16 = 36
)

// Error codes a test may set.
const (
	NotLeader          int16 = 6
	TopicAuthorization int16 = 29
	GroupAuthorization int16 = 30
	StorageError       int16 = 56
	GroupIDNotFound    int16 = 69
)

// Cluster is a stand-in Kafka cluster. Its fields are read under its lock
// by every broker's connections, so a test changes them in Edit once the
// cluster is started.
type Cluster struct {
	mu sync.Mutex
	// ClusterID is the cluster's ID, and Controller the broker that is its
	// controller.
	ClusterID  string
	Controller int32
	Brokers    []*Broker
	Topics     []Topic
	Groups     []Group
	// Mechanisms are the SASL mechanisms the brokers take; with none they
	// take no login. Users are the users and passwords of PLAIN and SCRAM,
	// Token the token of OAUTHBEARER, and Iterations the iterations a SCRAM
	// login is asked to hash the password with, 4096 when 0. BadSignature
	// makes a SCRAM login's last answer a signature of another password.
	Mechanisms []string
	Users      map[string]string
	Token      string
	// CheckToken, when set, judges an OAUTHBEARER token in place of Token,
	// and says why it is refused, "" for one taken.
	CheckToken   func(token string) string
	Iterations   int
	BadSignature bool
	// Versions narrow the versions ApiVersions says of an API, by its key.
	Versions map[int16][2]int16
	// Answer, when it returns bytes, is sent in place of a broker's answer
	// to an API, framed as an answer with the request's correlation ID.
	Answer func(broker int32, api int16) []byte

	asked  []Asked
	conns  []net.Conn
	closed bool
}

// Broker is a broker of the cluster.
type Broker struct {
	ID int32
	// Host is the host the metadata says the broker is at, 127.0.0.1
	// unless a test sets another name for it, and Addr where it listens,
	// on 127.0.0.1.
	Host string
	Addr string
	Rack *string
	// LogDir is the one log directory DescribeLogDirs lists, /var/lib/kafka
	// when empty, and LogDirError its error. HideLogDirs lists none, as
	// Kafka answers a login without the permission to see them.
	LogDir      string
	LogDirError int16
	HideLogDirs bool
	// CAFile verifies a TLS broker's certificate.
	CAFile   string
	listener net.Listener
}

// Topic is a topic and its partitions, numbered from 0 in their order.
type Topic struct {
	Name       string
	Internal   bool
	Error      int16
	Partitions []Partition
	// Configs are its settings, nil for one shown as null; Sensitive names
	// those DescribeConfigs marks sensitive, and ConfigError fails it for the
	// topic.
	Configs     map[string]*string
	Sensitive   []string
	ConfigError int16
}

// Partition is a partition: its leader, -1 for none, its replicas, in sync
// and offline, its newest and oldest offsets, the error the metadata gives
// it, and the bytes each replica takes, by broker, which DescribeLogDirs
// lists; Future, by broker, is a copy a broker is moving to another
// directory.
type Partition struct {
	Leader                 int32
	Replicas, ISR, Offline []int32
	Newest, Oldest         int64
	Error                  int16
	Sizes, Future          map[int32]int64
}

// Group is a consumer group, coordinated by Coordinator, which lists it,
// with the offsets it committed by topic and partition. DescribeError fails
// DescribeGroups for it.
type Group struct {
	Name, Protocol, State string
	Members               int
	Coordinator           int32
	Offsets               map[string]map[int32]int64
	DescribeError         int16
}

// Asked is a request a broker was sent.
type Asked struct {
	Broker int32
	API    int16
}

// New starts a cluster of n brokers, numbered from 1, over TLS when
// secure. The test's cleanup stops them. Broker 1 is the controller.
func New(t testing.TB, n int, secure bool) *Cluster {
	t.Helper()
	c := &Cluster{ClusterID: "stand-in-cluster", Controller: 1}
	var config *tls.Config
	caFile := ""
	if secure {
		// httptest's own certificate, for 127.0.0.1 and example.com.
		issuer := httptest.NewUnstartedServer(nil)
		issuer.StartTLS()
		certificate, ca := issuer.TLS.Certificates[0], issuer.Certificate()
		issuer.Close()
		caFile = filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
		config = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	}
	var wg sync.WaitGroup
	for i := range n {
		id := int32(i + 1) //nolint:gosec // a few brokers
		var listen net.ListenConfig
		listener, err := listen.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if config != nil {
			listener = tls.NewListener(listener, config)
		}
		c.Brokers = append(c.Brokers, &Broker{ID: id, Host: "127.0.0.1", Addr: listener.Addr().String(), CAFile: caFile, listener: listener})
		wg.Go(func() {
			var conns sync.WaitGroup
			for {
				conn, err := listener.Accept()
				if err != nil {
					conns.Wait()
					return
				}
				// Kept before it is served, so that the cleanup, once it has
				// closed the connections, has none left to wait for.
				if !c.track(conn) {
					_ = conn.Close()
					continue
				}
				conns.Go(func() {
					defer conn.Close()
					c.serve(id, conn)
				})
			}
		})
	}
	t.Cleanup(func() {
		for _, b := range c.Brokers {
			_ = b.listener.Close()
		}
		c.closeAll()
		wg.Wait()
	})
	return c
}

// track keeps conn, to close it when the test ends, and says false once
// the connections have been closed, when conn is to be closed at once.
func (c *Cluster) track(conn net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.conns = append(c.conns, conn)
	return true
}

// closeAll closes every connection still open, and every one accepted
// after.
func (c *Cluster) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, conn := range c.conns {
		_ = conn.Close()
	}
}

// Edit changes the cluster under its lock.
func (c *Cluster) Edit(edit func(*Cluster)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	edit(c)
}

// Stop stops broker id, which then refuses connections.
func (c *Cluster) Stop(id int32) {
	c.mu.Lock()
	b := c.broker(id)
	c.mu.Unlock()
	_ = b.listener.Close()
}

// Asked are the requests the brokers were sent, in order.
func (c *Cluster) Asked() []Asked {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

func (c *Cluster) broker(id int32) *Broker {
	for _, b := range c.Brokers {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// session is a connection's login: whether it is done, and SCRAM's state
// between its two messages.
type session struct {
	authed     bool
	challenged bool
	mechanism  string
	user       string
	firstBare  string
	serverPart string
	salt       []byte
	iterations int
}

// serve answers the requests of a connection to broker id until it ends.
func (c *Cluster) serve(id int32, conn net.Conn) {
	c.mu.Lock()
	s := &session{authed: len(c.Mechanisms) == 0}
	c.mu.Unlock()
	for {
		var size [4]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		frame := make([]byte, binary.BigEndian.Uint32(size[:]))
		if _, err := io.ReadFull(conn, frame); err != nil {
			return
		}
		r := &reader{b: frame}
		api, version, correlation := r.i16(), r.i16(), r.i32()
		r.str() // client_id
		c.mu.Lock()
		c.asked = append(c.asked, Asked{Broker: id, API: api})
		var body []byte
		if c.Answer != nil {
			body = c.Answer(id, api)
		}
		if body == nil {
			if !s.authed && api != APIVersions && api != SaslHandshake && api != SaslAuthenticate {
				c.mu.Unlock()
				return // as a broker closes an unauthenticated connection
			}
			body = c.answer(id, api, version, r, s)
		}
		c.mu.Unlock()
		if body == nil {
			return
		}
		out := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)) //nolint:gosec // a small frame
		out = binary.BigEndian.AppendUint32(out, uint32(correlation))  //nolint:gosec // the field's bits
		if _, err := conn.Write(append(out, body...)); err != nil {
			return
		}
	}
}

// answer is broker id's answer to a request, nil for one it closes the
// connection on.
func (c *Cluster) answer(id int32, api, version int16, r *reader, s *session) []byte {
	w := &writer{}
	switch api {
	case APIVersions:
		spoken := map[int16][2]int16{Metadata: {0, 12}, ListOffsets: {1, 8}, OffsetFetch: {1, 9}, DescribeGroups: {0, 5}, ListGroups: {0, 5}, SaslHandshake: {0, 1}, APIVersions: {0, 3}, DescribeConfigs: {1, 4}, DescribeLogDirs: {1, 4}, SaslAuthenticate: {0, 2}}
		for key, versions := range c.Versions {
			spoken[key] = versions
		}
		w.i16(0)
		keys := make([]int16, 0, len(spoken))
		for key := range spoken {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		w.count(len(keys))
		for _, key := range keys {
			w.i16(key)
			w.i16(spoken[key][0])
			w.i16(spoken[key][1])
		}
		w.i32(0)
	case SaslHandshake:
		s.mechanism = r.str()
		switch {
		case len(c.Mechanisms) == 0:
			w.i16(34) // ILLEGAL_SASL_STATE, as a listener without SASL answers
		case slices.Contains(c.Mechanisms, s.mechanism):
			w.i16(0)
		default:
			w.i16(33)
		}
		w.count(len(c.Mechanisms))
		for _, m := range c.Mechanisms {
			w.str(m)
		}
	case SaslAuthenticate:
		answer, failure := c.authenticate(s, r.bytes())
		if failure != "" {
			w.i16(58)
			w.str(failure)
		} else {
			w.i16(0)
			w.i16(-1)
		}
		w.bytes(answer)
		if version >= 1 {
			w.i64(0)
		}
	case Metadata:
		w.i32(0)
		w.count(len(c.Brokers))
		for _, b := range c.Brokers {
			_, port, _ := net.SplitHostPort(b.Addr)
			w.i32(b.ID)
			w.str(b.Host)
			w.i32(atoi(port))
			w.nstr(b.Rack)
		}
		w.str(c.ClusterID)
		w.i32(c.Controller)
		w.count(len(c.Topics))
		for _, t := range c.Topics {
			w.i16(t.Error)
			w.str(t.Name)
			w.bool(t.Internal)
			w.count(len(t.Partitions))
			for i, p := range t.Partitions {
				w.i16(p.Error)
				w.count(i)
				w.i32(p.Leader)
				w.i32(0)
				w.i32s(p.Replicas)
				w.i32s(p.ISR)
				w.i32s(p.Offline)
			}
		}
	case ListGroups:
		w.i32(0)
		w.i16(0)
		var mine []Group
		for _, g := range c.Groups {
			if g.Coordinator == id {
				mine = append(mine, g)
			}
		}
		w.count(len(mine))
		for _, g := range mine {
			w.str(g.Name)
			w.str(g.Protocol)
		}
	case DescribeGroups:
		n := r.i32()
		w.i32(0)
		w.i32(n)
		for range n {
			name := r.str()
			g := c.group(name)
			code := int16(0)
			switch {
			case g == nil:
				code = GroupIDNotFound
			case g.Coordinator != id:
				code = 16 // NOT_COORDINATOR
			case g.DescribeError != 0:
				code = g.DescribeError
			}
			w.i16(code)
			w.str(name)
			if code != 0 {
				w.str("")
				w.str("")
				w.str("")
				w.i32(0)
				continue
			}
			w.str(g.State)
			w.str(g.Protocol)
			w.str("range")
			w.count(g.Members)
			for m := range g.Members {
				w.str("member-" + string(rune('a'+m)))
				w.str("client")
				w.str("/10.0.0.1")
				w.bytes([]byte{0, 1})
				w.bytes([]byte{0, 1})
			}
		}
	case OffsetFetch:
		name := r.str()
		w.i32(0)
		g := c.group(name)
		var topics []string
		if g != nil {
			for topic := range g.Offsets {
				topics = append(topics, topic)
			}
		}
		slices.Sort(topics)
		w.count(len(topics))
		for _, topic := range topics {
			w.str(topic)
			partitions := g.Offsets[topic]
			keys := make([]int32, 0, len(partitions))
			for p := range partitions {
				keys = append(keys, p)
			}
			slices.Sort(keys)
			w.count(len(keys))
			for _, p := range keys {
				w.i32(p)
				w.i64(partitions[p])
				w.i32(-1)
				w.str("")
				w.i16(0)
			}
		}
		w.i16(0)
	case ListOffsets:
		r.i32() // replica_id
		r.i8()  // isolation_level
		w.i32(0)
		n := r.i32()
		w.i32(n)
		for range n {
			name := r.str()
			w.str(name)
			m := r.i32()
			w.i32(m)
			for range m {
				index := r.i32()
				r.i32() // current_leader_epoch
				timestamp := r.i64()
				w.i32(index)
				p := c.partition(name, index)
				if p == nil || p.Leader != id {
					w.i16(NotLeader)
					w.i64(-1)
					w.i64(-1)
					w.i32(-1)
					continue
				}
				offset := p.Newest
				if timestamp == -2 {
					offset = p.Oldest
				}
				w.i16(0)
				w.i64(-1)
				w.i64(offset)
				w.i32(0)
			}
		}
	case DescribeLogDirs:
		w.i32(0)
		b := c.broker(id)
		if b.HideLogDirs {
			w.count(0)
			break
		}
		w.count(1)
		w.i16(b.LogDirError)
		w.str(cmp.Or(b.LogDir, "/var/lib/kafka"))
		type held struct {
			index  int
			size   int64
			future bool
		}
		var topics []string
		mine := map[string][]held{}
		for _, t := range c.Topics {
			for i, p := range t.Partitions {
				if size, ok := p.Sizes[id]; ok {
					mine[t.Name] = append(mine[t.Name], held{i, size, false})
				}
				if size, ok := p.Future[id]; ok {
					mine[t.Name] = append(mine[t.Name], held{i, size, true})
				}
			}
			if len(mine[t.Name]) > 0 {
				topics = append(topics, t.Name)
			}
		}
		w.count(len(topics))
		for _, topic := range topics {
			w.str(topic)
			w.count(len(mine[topic]))
			for _, h := range mine[topic] {
				w.count(h.index)
				w.i64(h.size)
				w.i64(0)
				w.bool(h.future)
			}
		}
	case DescribeConfigs:
		n := r.i32()
		w.i32(0)
		w.i32(n)
		for range n {
			r.i8() // resource_type
			name := r.str()
			var keys []string
			for m := r.i32(); m > 0; m-- {
				keys = append(keys, r.str())
			}
			var t *Topic
			for i := range c.Topics {
				if c.Topics[i].Name == name {
					t = &c.Topics[i]
				}
			}
			code := int16(3) // UNKNOWN_TOPIC_OR_PARTITION
			if t != nil {
				code = t.ConfigError
			}
			w.i16(code)
			if code != 0 {
				w.str("refused by the stand-in")
			} else {
				w.i16(-1)
			}
			w.i8(2)
			w.str(name)
			if code != 0 {
				w.count(0)
				continue
			}
			var shown []string
			for _, key := range keys {
				if _, ok := t.Configs[key]; ok {
					shown = append(shown, key)
				}
			}
			w.count(len(shown))
			for _, key := range shown {
				w.str(key)
				w.nstr(t.Configs[key])
				w.bool(false)
				w.i8(1) // DYNAMIC_TOPIC_CONFIG
				w.bool(slices.Contains(t.Sensitive, key))
				w.count(0)
			}
		}
	default:
		return nil
	}
	return w.b
}

func (c *Cluster) group(name string) *Group {
	for i := range c.Groups {
		if c.Groups[i].Name == name {
			return &c.Groups[i]
		}
	}
	return nil
}

func (c *Cluster) partition(topic string, index int32) *Partition {
	for _, t := range c.Topics {
		if t.Name == topic && index >= 0 && int(index) < len(t.Partitions) {
			return &t.Partitions[index]
		}
	}
	return nil
}

// authenticate takes one SASL message of s's mechanism and answers it, or
// says why it fails.
func (c *Cluster) authenticate(s *session, message []byte) ([]byte, string) {
	refused := "Authentication failed: Invalid username or password"
	switch s.mechanism {
	case "PLAIN":
		parts := strings.Split(string(message), "\x00")
		if len(parts) != 3 || c.Users[parts[1]] == "" || c.Users[parts[1]] != parts[2] {
			return nil, refused
		}
		s.authed = true
		return nil, ""
	case "OAUTHBEARER":
		// A token not taken is answered as Kafka answers it: an error
		// challenge, as JSON, with no error code, and the login failed at
		// the client's %x01 after it.
		token, framed := strings.CutPrefix(string(message), "n,,\x01auth=Bearer ")
		token, ends := strings.CutSuffix(token, "\x01\x01")
		taken := framed && ends && token == c.Token
		if c.CheckToken != nil {
			taken = framed && ends && c.CheckToken(token) == ""
		}
		switch {
		case string(message) == "\x01" && s.challenged:
			return nil, "Authentication failed: invalid token"
		case !taken:
			s.challenged = true
			return []byte(`{"status":"invalid_token"}`), ""
		}
		s.authed = true
		return nil, ""
	}
	newHash := sha256.New
	if s.mechanism == "SCRAM-SHA-512" {
		newHash = sha512.New
	}
	text := string(message)
	if s.firstBare == "" {
		bare, ok := strings.CutPrefix(text, "n,,")
		fields := scramFields(bare)
		if !ok || fields["n"] == "" || fields["r"] == "" {
			return nil, "Authentication failed: invalid SCRAM client-first message"
		}
		s.user, s.firstBare = strings.NewReplacer("=2C", ",", "=3D", "=").Replace(fields["n"]), bare
		s.salt, s.iterations = []byte("stand-in salt"), c.Iterations
		if s.iterations == 0 {
			s.iterations = 4096
		}
		s.serverPart = "r=" + fields["r"] + "server-nonce,s=" + base64.StdEncoding.EncodeToString(s.salt) + ",i=" + strconv.Itoa(s.iterations)
		return []byte(s.serverPart), ""
	}
	fields := scramFields(text)
	password, known := c.Users[s.user]
	withoutProof, _, _ := strings.Cut(text, ",p=")
	salted, _ := pbkdf2.Key(newHash, password, s.salt, s.iterations, newHash().Size())
	clientKey := mac(newHash, salted, "Client Key")
	stored := newHash()
	stored.Write(clientKey)
	authMessage := s.firstBare + "," + s.serverPart + "," + withoutProof
	signature := mac(newHash, stored.Sum(nil), authMessage)
	proof, _ := base64.StdEncoding.DecodeString(fields["p"])
	if !known || len(proof) != len(signature) {
		return nil, refused
	}
	for i := range proof {
		proof[i] ^= signature[i]
	}
	check := newHash()
	check.Write(proof)
	if !bytes.Equal(check.Sum(nil), stored.Sum(nil)) {
		return nil, refused
	}
	serverKey := mac(newHash, salted, "Server Key")
	if c.BadSignature {
		serverKey = mac(newHash, []byte("another password"), "Server Key")
	}
	s.authed = true
	return []byte("v=" + base64.StdEncoding.EncodeToString(mac(newHash, serverKey, authMessage))), ""
}

func mac(newHash func() hash.Hash, key []byte, text string) []byte {
	h := hmac.New(newHash, key)
	h.Write([]byte(text))
	return h.Sum(nil)
}

func scramFields(message string) map[string]string {
	fields := map[string]string{}
	for part := range strings.SplitSeq(message, ",") {
		if key, value, ok := strings.Cut(part, "="); ok && len(key) == 1 {
			fields[key] = value
		}
	}
	return fields
}

// writer builds an answer.
type writer struct{ b []byte }

func (w *writer) i8(v int8)      { w.b = append(w.b, byte(v)) }                          //nolint:gosec // the bits
func (w *writer) i16(v int16)    { w.b = binary.BigEndian.AppendUint16(w.b, uint16(v)) } //nolint:gosec // the bits
func (w *writer) i32(v int32)    { w.b = binary.BigEndian.AppendUint32(w.b, uint32(v)) } //nolint:gosec // the bits
func (w *writer) i64(v int64)    { w.b = binary.BigEndian.AppendUint64(w.b, uint64(v)) } //nolint:gosec // the bits
func (w *writer) str(s string)   { w.i16(int16(len(s))); w.b = append(w.b, s...) }       //nolint:gosec // short
func (w *writer) bytes(b []byte) { w.count(len(b)); w.b = append(w.b, b...) }            //nolint:gosec // short
func (w *writer) bool(v bool) {
	if v {
		w.i8(1)
	} else {
		w.i8(0)
	}
}

// count writes the count of an array, or an index, as an int32.
func (w *writer) count(n int) { w.i32(int32(n)) } //nolint:gosec // a stand-in's few items

func (w *writer) nstr(s *string) {
	if s == nil {
		w.i16(-1)
		return
	}
	w.str(*s)
}

func (w *writer) i32s(list []int32) {
	w.count(len(list)) //nolint:gosec // short
	for _, v := range list {
		w.i32(v)
	}
}

// reader reads a request; a short one reads as zeroes.
type reader struct{ b []byte }

func (r *reader) take(n int) []byte {
	if n < 0 || n > len(r.b) {
		r.b = nil
		return make([]byte, max(n, 0))
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) i8() int8   { return int8(r.take(1)[0]) }                        //nolint:gosec // the bits
func (r *reader) i16() int16 { return int16(binary.BigEndian.Uint16(r.take(2))) } //nolint:gosec // the bits
func (r *reader) i32() int32 { return int32(binary.BigEndian.Uint32(r.take(4))) } //nolint:gosec // the bits
func (r *reader) i64() int64 { return int64(binary.BigEndian.Uint64(r.take(8))) } //nolint:gosec // the bits

func (r *reader) str() string {
	n := r.i16()
	if n < 0 {
		return ""
	}
	return string(r.take(int(n)))
}

func (r *reader) bytes() []byte { return r.take(int(r.i32())) }

func atoi(s string) int32 {
	n, _ := strconv.Atoi(s)
	return int32(n) //nolint:gosec // a port
}
