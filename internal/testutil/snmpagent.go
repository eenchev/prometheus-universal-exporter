package testutil

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des" //nolint:gosec // G502: the stand-in agent speaks SNMPv3's DES privacy
	"crypto/hmac"
	"crypto/md5"  //nolint:gosec // G501: the stand-in agent speaks SNMPv3's HMAC-MD5-96
	"crypto/sha1" //nolint:gosec // G505: the stand-in agent speaks SNMPv3's HMAC-SHA-96
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"hash"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// SNMPAgent is a stand-in SNMP agent over UDP, for the tests of the snmp
// request type: it answers Get, GetNext and GetBulk of v1 and v2c to its
// community, and of v3 to its users, with discovery, the signatures and
// the encryption of the User-based Security Model (RFC 3414, RFC 3826,
// RFC 7860), from a table of values. It is written apart from the
// exporter's own encoding, from the RFCs, so that a test of the two
// together is a test of either against what the RFCs say. It imports
// nothing but the standard library.
type SNMPAgent struct {
	// Addr is the agent's host:port, on 127.0.0.1.
	Addr string

	conn    *net.UDPConn
	config  SNMPAgentConfig
	values  []SNMPAgentValue
	started time.Time
	mu      sync.Mutex
	got     int
	dropped int
	skewed  bool
	pdus    []string
	wg      sync.WaitGroup
}

// SNMPAgentConfig is what a stand-in agent answers.
type SNMPAgentConfig struct {
	// Community is the community of v1 and v2c; a request of another is
	// dropped unanswered, as agents drop it.
	Community string
	// EngineID is the agent's SNMPv3 engine ID; Boots its boot count.
	EngineID []byte
	Boots    uint32
	// Users are the SNMPv3 users by name.
	Users map[string]SNMPAgentUser
	// Values are the agent's objects, in any order.
	Values []SNMPAgentValue
	// Drop is how many datagrams the agent drops before it answers one.
	Drop int
	// BulkLimit, above 0, answers a GetBulk of more repetitions with
	// tooBig, as an agent whose answer would not fit a datagram does.
	BulkLimit int
	// SkewOnce answers the first authenticated request with a Report of
	// usmStatsNotInTimeWindows, as an agent rebooted since discovery does.
	SkewOnce bool
	// V1 makes the agent an SNMPv1 one, which answers no GetBulk and
	// reports a missing object with noSuchName.
	V1 bool
}

// SNMPAgentUser is an SNMPv3 user: its authentication protocol (MD5,
// SHA, SHA224, SHA256, SHA384 or SHA512) and password, and its privacy
// protocol (DES, AES, AES192, AES256, AES192C or AES256C) and password,
// "" for a user without.
type SNMPAgentUser struct {
	Auth, AuthPassword string
	Priv, PrivPassword string
}

// SNMPAgentValue is an object of the agent: its identifier, the tag of
// its type and the content of its value.
type SNMPAgentValue struct {
	OID     string
	Tag     byte
	Content []byte
	oid     []uint32
}

// SNMPInteger is an INTEGER object.
func SNMPInteger(oid string, v int64) SNMPAgentValue {
	return SNMPAgentValue{OID: oid, Tag: 0x02, Content: agentInt(v)}
}

// SNMPString is an OCTET STRING object.
func SNMPString(oid, v string) SNMPAgentValue {
	return SNMPAgentValue{OID: oid, Tag: 0x04, Content: []byte(v)}
}

// SNMPUnsigned is an object of an unsigned type: Counter32 0x41, Gauge32
// 0x42, TimeTicks 0x43 or Counter64 0x46.
func SNMPUnsigned(oid string, tag byte, v uint64) SNMPAgentValue {
	content := []byte{}
	for x := v; x > 0; x >>= 8 {
		content = append([]byte{byte(x)}, content...)
	}
	if len(content) == 0 || content[0]&0x80 != 0 {
		content = append([]byte{0}, content...)
	}
	return SNMPAgentValue{OID: oid, Tag: tag, Content: content}
}

// SNMPOIDValue is an OBJECT IDENTIFIER object.
func SNMPOIDValue(oid, value string) SNMPAgentValue {
	arcs := agentParseOID(value)
	return SNMPAgentValue{OID: oid, Tag: 0x06, Content: agentOIDContent(arcs)}
}

// NewSNMPAgent starts an agent, which the test's cleanup stops.
func NewSNMPAgent(t testing.TB, config SNMPAgentConfig) *SNMPAgent {
	t.Helper()
	var listen net.ListenConfig
	packet, err := listen.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &SNMPAgent{Addr: packet.LocalAddr().String(), conn: packet.(*net.UDPConn), config: config, started: time.Now()}
	for _, v := range config.Values {
		v.oid = agentParseOID(v.OID)
		a.values = append(a.values, v)
	}
	slices.SortFunc(a.values, func(x, y SNMPAgentValue) int { return slices.Compare(x.oid, y.oid) })
	a.wg.Add(1)
	go a.serve()
	t.Cleanup(func() {
		_ = a.conn.Close()
		a.wg.Wait()
	})
	return a
}

// Requests is how many datagrams the agent read, those it dropped
// included.
func (a *SNMPAgent) Requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.got
}

// PDUs are the PDUs the agent answered, each its kind and, for GetBulk,
// its max-repetitions: "get", "getnext", "getbulk 25", "discovery".
func (a *SNMPAgent) PDUs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.pdus...)
}

func (a *SNMPAgent) serve() {
	defer a.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		a.mu.Lock()
		a.got++
		drop := a.dropped < a.config.Drop
		if drop {
			a.dropped++
		}
		a.mu.Unlock()
		if drop {
			continue
		}
		if reply := a.answer(append([]byte{}, buf[:n]...)); reply != nil {
			_, _ = a.conn.WriteToUDP(reply, from)
		}
	}
}

// agentNode is a decoded value: its tag, its content and, constructed,
// what it holds.
type agentNode struct {
	tag     byte
	content []byte
	// hdr is where the value begins, at its tag, and start and end where
	// its content is, in the message decoded.
	hdr, start, end int
}

// agentParse reads the values of buf[pos:end] at its top level.
func agentParse(buf []byte, pos, end int) ([]agentNode, error) {
	var out []agentNode
	for pos < end {
		if pos+2 > end {
			return nil, errors.New("short")
		}
		hdr := pos
		tag, n := buf[pos], int(buf[pos+1])
		pos += 2
		if n&0x80 != 0 {
			k := n & 0x7f
			if k == 0 || k > 4 || pos+k > end {
				return nil, errors.New("bad length")
			}
			n = 0
			for _, d := range buf[pos : pos+k] {
				n = n<<8 | int(d)
			}
			pos += k
		}
		if pos+n > end {
			return nil, errors.New("short")
		}
		out = append(out, agentNode{tag: tag, content: buf[pos : pos+n], hdr: hdr, start: pos, end: pos + n})
		pos += n
	}
	return out, nil
}

func agentChildren(buf []byte, n agentNode) []agentNode {
	children, _ := agentParse(buf, n.start, n.end)
	return children
}

func agentTLV(tag byte, content []byte) []byte {
	n := len(content)
	var length []byte
	switch {
	case n < 128:
		length = []byte{byte(n)}
	case n < 256:
		length = []byte{0x81, byte(n)}
	default:
		length = []byte{0x82, byte(n >> 8), byte(n)} //nolint:gosec // G115: a datagram's length, below 65536
	}
	return append(append([]byte{tag}, length...), content...)
}

func agentSeq(tag byte, parts ...[]byte) []byte {
	return agentTLV(tag, bytes.Join(parts, nil))
}

func agentInt(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v)) //nolint:gosec // G115: two's complement
	for len(b) > 1 && ((b[0] == 0 && b[1] < 0x80) || (b[0] == 0xff && b[1] >= 0x80)) {
		b = b[1:]
	}
	return b
}

func agentIntOf(content []byte) int64 {
	if len(content) == 0 {
		return 0
	}
	v := int64(int8(content[0])) //nolint:gosec // G115: the first byte's sign, as two's complement reads it
	for _, b := range content[1:] {
		v = v<<8 | int64(b)
	}
	return v
}

func agentParseOID(text string) []uint32 {
	var arcs []uint32
	for _, part := range strings.Split(strings.TrimPrefix(text, "."), ".") {
		n, _ := strconv.ParseUint(part, 10, 32)
		arcs = append(arcs, uint32(n))
	}
	return arcs
}

func agentOIDContent(arcs []uint32) []byte {
	var out []byte
	put := func(v uint32) {
		var digits []byte
		digits = append(digits, byte(v&0x7f))
		for v >>= 7; v > 0; v >>= 7 {
			digits = append([]byte{byte(v&0x7f) | 0x80}, digits...)
		}
		out = append(out, digits...)
	}
	put(arcs[0]*40 + arcs[1])
	for _, a := range arcs[2:] {
		put(a)
	}
	return out
}

func agentOIDOf(content []byte) []uint32 {
	var arcs []uint32
	var v uint32
	for _, b := range content {
		v = v<<7 | uint32(b&0x7f)
		if b&0x80 == 0 {
			arcs = append(arcs, v)
			v = 0
		}
	}
	if len(arcs) == 0 {
		return nil
	}
	first := arcs[0]
	head := []uint32{min(first/40, 2), first - min(first/40, 2)*40}
	return append(head, arcs[1:]...)
}

// answer is the agent's reply to a message, nil for none.
func (a *SNMPAgent) answer(msg []byte) []byte {
	top, err := agentParse(msg, 0, len(msg))
	if err != nil || len(top) != 1 || top[0].tag != 0x30 {
		return nil
	}
	parts := agentChildren(msg, top[0])
	if len(parts) < 3 || parts[0].tag != 0x02 {
		return nil
	}
	switch version := agentIntOf(parts[0].content); version {
	case 0, 1:
		if a.config.V1 != (version == 0) || string(parts[1].content) != a.config.Community || a.config.Community == "" {
			return nil
		}
		pdu, ok := a.respond(msg, parts[2])
		if !ok {
			return nil
		}
		return agentSeq(0x30, agentTLV(0x02, agentInt(version)), agentTLV(0x04, parts[1].content), pdu)
	case 3:
		return a.answerV3(msg, parts)
	}
	return nil
}

// respond is the response PDU to a request PDU.
func (a *SNMPAgent) respond(msg []byte, pdu agentNode) ([]byte, bool) {
	fields := agentChildren(msg, pdu)
	if len(fields) != 4 {
		return nil, false
	}
	id := fields[0].content
	var oids [][]uint32
	for _, vb := range agentChildren(msg, fields[3]) {
		pair := agentChildren(msg, vb)
		if len(pair) != 2 {
			return nil, false
		}
		oids = append(oids, agentOIDOf(pair[0].content))
	}
	reply := func(status, index int64, varbinds [][]byte) []byte {
		return agentSeq(0xa2, agentTLV(0x02, id), agentTLV(0x02, agentInt(status)), agentTLV(0x02, agentInt(index)), agentSeq(0x30, varbinds...))
	}
	varbind := func(oid []uint32, tag byte, content []byte) []byte {
		return agentSeq(0x30, agentTLV(0x06, agentOIDContent(oid)), agentTLV(tag, content))
	}
	echo := func() [][]byte {
		var out [][]byte
		for _, oid := range oids {
			out = append(out, varbind(oid, 0x05, nil))
		}
		return out
	}
	next := func(oid []uint32) (SNMPAgentValue, bool) {
		for _, v := range a.values {
			if slices.Compare(v.oid, oid) > 0 {
				return v, true
			}
		}
		return SNMPAgentValue{}, false
	}
	a.mu.Lock()
	switch pdu.tag {
	case 0xa0:
		a.pdus = append(a.pdus, "get")
	case 0xa1:
		a.pdus = append(a.pdus, "getnext")
	case 0xa5:
		a.pdus = append(a.pdus, "getbulk "+strconv.FormatInt(agentIntOf(fields[2].content), 10))
	}
	a.mu.Unlock()
	switch pdu.tag {
	case 0xa0:
		var out [][]byte
		for i, oid := range oids {
			j := slices.IndexFunc(a.values, func(v SNMPAgentValue) bool { return slices.Equal(v.oid, oid) })
			switch {
			case j >= 0:
				out = append(out, varbind(oid, a.values[j].Tag, a.values[j].Content))
			case a.config.V1:
				return reply(2, int64(i+1), echo()), true
			default:
				out = append(out, varbind(oid, 0x80, nil))
			}
		}
		return reply(0, 0, out), true
	case 0xa1:
		var out [][]byte
		for i, oid := range oids {
			v, ok := next(oid)
			switch {
			case ok:
				out = append(out, varbind(v.oid, v.Tag, v.Content))
			case a.config.V1:
				return reply(2, int64(i+1), echo()), true
			default:
				out = append(out, varbind(oid, 0x82, nil))
			}
		}
		return reply(0, 0, out), true
	case 0xa5:
		if a.config.V1 {
			return nil, false
		}
		nonRepeaters, repetitions := int(agentIntOf(fields[1].content)), int(agentIntOf(fields[2].content))
		if a.config.BulkLimit > 0 && repetitions > a.config.BulkLimit {
			return reply(1, 0, nil), true
		}
		var out [][]byte
		for i, oid := range oids {
			if i < nonRepeaters {
				if v, ok := next(oid); ok {
					out = append(out, varbind(v.oid, v.Tag, v.Content))
				} else {
					out = append(out, varbind(oid, 0x82, nil))
				}
			}
		}
		last := oids[min(nonRepeaters, len(oids)):]
		for range repetitions {
			for k, oid := range last {
				v, ok := next(oid)
				if !ok {
					out = append(out, varbind(oid, 0x82, nil))
					continue
				}
				out = append(out, varbind(v.oid, v.Tag, v.Content))
				last[k] = v.oid
			}
		}
		return reply(0, 0, out), true
	}
	return nil, false
}

// agentAuth is the hash and truncation of an authentication protocol.
func agentAuth(name string) (func() hash.Hash, int) {
	switch name {
	case "MD5":
		return md5.New, 12
	case "SHA":
		return sha1.New, 12
	case "SHA224":
		return sha256.New224, 16
	case "SHA256":
		return sha256.New, 24
	case "SHA384":
		return sha512.New384, 32
	case "SHA512":
		return sha512.New, 48
	}
	return nil, 0
}

// agentKey is RFC 3414 § A.2.1's password to key, localized to engine.
func agentKey(h func() hash.Hash, password, engine []byte) []byte {
	expanded := bytes.Repeat(password, 1<<20/len(password)+1)[:1<<20]
	ku := h()
	ku.Write(expanded)
	k := ku.Sum(nil)
	kul := h()
	kul.Write(k)
	kul.Write(engine)
	kul.Write(k)
	return kul.Sum(nil)
}

// agentPrivKey is the key a user's privacy protocol encrypts with.
func agentPrivKey(user SNMPAgentUser, engine []byte) []byte {
	h, _ := agentAuth(user.Auth)
	key := agentKey(h, []byte(user.PrivPassword), engine)
	want := map[string]int{"DES": 16, "AES": 16, "AES192": 24, "AES192C": 24, "AES256": 32, "AES256C": 32}[user.Priv]
	for len(key) < want {
		if strings.HasSuffix(user.Priv, "C") {
			// The Reeder extension: the key localized from the key's
			// first part as a password.
			key = append(key, agentKey(h, key[len(key)-h().Size():], engine)...)
		} else {
			sum := h()
			sum.Write(key)
			key = append(key, sum.Sum(nil)...)
		}
	}
	return key[:want]
}

func agentCFB(key, iv, text []byte, encrypt bool) []byte {
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(text))
	prev := append([]byte{}, iv...)
	ks := make([]byte, 16)
	for i := 0; i < len(text); i += 16 {
		block.Encrypt(ks, prev)
		end := min(i+16, len(text))
		for j := i; j < end; j++ {
			out[j] = text[j] ^ ks[j-i]
		}
		if encrypt {
			prev = append(out[i:end:end], prev[end-i:]...)
		} else {
			prev = append(text[i:end:end], prev[end-i:]...)
		}
	}
	return out
}

func agentIVs(user SNMPAgentUser, key []byte, boots, engineTime uint32, salt []byte) []byte {
	if user.Priv == "DES" {
		iv := make([]byte, 8)
		for i := range iv {
			iv[i] = key[8+i] ^ salt[i]
		}
		return iv
	}
	iv := make([]byte, 16)
	binary.BigEndian.PutUint32(iv, boots)
	binary.BigEndian.PutUint32(iv[4:], engineTime)
	copy(iv[8:], salt)
	return iv
}

// answerV3 answers an SNMPv3 message.
func (a *SNMPAgent) answerV3(msg []byte, parts []agentNode) []byte {
	if len(parts) != 4 {
		return nil
	}
	global := agentChildren(msg, parts[1])
	if len(global) != 4 || len(global[2].content) != 1 {
		return nil
	}
	msgID, flags := global[0].content, global[2].content[0]
	usmNodes, err := agentParse(msg, parts[2].start, parts[2].end)
	if err != nil || len(usmNodes) != 1 {
		return nil
	}
	usm := agentChildren(msg, usmNodes[0])
	if len(usm) != 6 {
		return nil
	}
	engineTime := uint32(time.Since(a.started)/time.Second) + 1000 //nolint:gosec // G115: a test's seconds
	userName := string(usm[3].content)
	// send writes a message of flags, its scoped PDU encrypted with priv
	// and signed with auth of user when the flags say so.
	send := func(flags byte, user SNMPAgentUser, pdu []byte) []byte {
		scoped := agentSeq(0x30, agentTLV(0x04, a.config.EngineID), agentTLV(0x04, nil), pdu)
		var authParams, privParams []byte
		var data []byte
		h, trunc := agentAuth(user.Auth)
		if flags&1 != 0 {
			authParams = make([]byte, trunc)
		}
		data = scoped
		if flags&2 != 0 {
			key := agentPrivKey(user, a.config.EngineID)
			privParams = []byte{1, 2, 3, 4, 5, 6, 7, 8}
			iv := agentIVs(user, key, a.config.Boots, engineTime, privParams)
			if user.Priv == "DES" {
				block, _ := des.NewCipher(key[:8]) //nolint:gosec // G405: SNMPv3's DES privacy
				padded := append(append([]byte{}, scoped...), make([]byte, (8-len(scoped)%8)%8)...)
				sealed := make([]byte, len(padded))
				cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, padded)
				data = agentTLV(0x04, sealed)
			} else {
				data = agentTLV(0x04, agentCFB(key, iv, scoped, true))
			}
		}
		usmOut := agentSeq(0x30, agentTLV(0x04, a.config.EngineID), agentTLV(0x02, agentInt(int64(a.config.Boots))), agentTLV(0x02, agentInt(int64(engineTime))),
			agentTLV(0x04, []byte(userName)), agentTLV(0x04, authParams), agentTLV(0x04, privParams))
		out := agentSeq(0x30, agentTLV(0x02, agentInt(3)), agentSeq(0x30, agentTLV(0x02, msgID), agentTLV(0x02, agentInt(65507)), agentTLV(0x04, []byte{flags}), agentTLV(0x02, agentInt(3))),
			agentTLV(0x04, usmOut), data)
		if flags&1 != 0 {
			mac := hmac.New(h, agentKey(h, []byte(user.AuthPassword), a.config.EngineID))
			mac.Write(out)
			at := bytes.Index(out, append(agentTLV(0x04, make([]byte, trunc)), agentTLV(0x04, privParams)...))
			copy(out[at+2:], mac.Sum(nil)[:trunc])
		}
		return out
	}
	report := func(flags byte, user SNMPAgentUser, id []byte, oid string) []byte {
		if id == nil {
			id = agentInt(0)
		}
		pdu := agentSeq(0xa8, agentTLV(0x02, id), agentTLV(0x02, agentInt(0)), agentTLV(0x02, agentInt(0)),
			agentSeq(0x30, agentSeq(0x30, agentTLV(0x06, agentOIDContent(agentParseOID(oid))), agentTLV(0x41, []byte{1}))))
		return send(flags, user, pdu)
	}
	requestID := func(scoped []byte) []byte {
		nodes, err := agentParse(scoped, 0, len(scoped))
		if err != nil || len(nodes) != 1 {
			return nil
		}
		inner := agentChildren(scoped, nodes[0])
		if len(inner) != 3 {
			return nil
		}
		fields := agentChildren(scoped, inner[2])
		if len(fields) != 4 {
			return nil
		}
		return fields[0].content
	}
	if len(usm[0].content) == 0 {
		a.mu.Lock()
		a.pdus = append(a.pdus, "discovery")
		a.mu.Unlock()
		scoped := msg[parts[3].hdr:parts[3].end]
		return report(0, SNMPAgentUser{}, requestID(scoped), "1.3.6.1.6.3.15.1.1.4.0")
	}
	user, known := a.config.Users[userName]
	if !known {
		scoped := msg[parts[3].hdr:parts[3].end]
		return report(0, SNMPAgentUser{}, requestID(scoped), "1.3.6.1.6.3.15.1.1.3.0")
	}
	if (flags&1 != 0) != (user.Auth != "") || (flags&2 != 0) != (user.Priv != "") {
		return report(0, SNMPAgentUser{}, nil, "1.3.6.1.6.3.15.1.1.1.0")
	}
	h, trunc := agentAuth(user.Auth)
	if flags&1 != 0 {
		zeroed := append([]byte{}, msg...)
		got := append([]byte{}, msg[usm[4].start:usm[4].end]...)
		clear(zeroed[usm[4].start:usm[4].end])
		mac := hmac.New(h, agentKey(h, []byte(user.AuthPassword), a.config.EngineID))
		mac.Write(zeroed)
		if len(got) != trunc || !hmac.Equal(got, mac.Sum(nil)[:trunc]) {
			return report(0, SNMPAgentUser{}, nil, "1.3.6.1.6.3.15.1.1.5.0")
		}
		a.mu.Lock()
		skew := a.config.SkewOnce && !a.skewed
		a.skewed = a.skewed || skew
		a.mu.Unlock()
		if skew {
			return report(1, user, nil, "1.3.6.1.6.3.15.1.1.2.0")
		}
		sentTime := uint32(agentIntOf(usm[2].content)) //nolint:gosec // G115: a test's seconds
		if sentTime+150 < engineTime || sentTime > engineTime+150 {
			return report(1, user, nil, "1.3.6.1.6.3.15.1.1.2.0")
		}
	}
	scoped := msg[parts[3].hdr:parts[3].end]
	if flags&2 != 0 {
		key := agentPrivKey(user, a.config.EngineID)
		iv := agentIVs(user, key, uint32(agentIntOf(usm[1].content)), uint32(agentIntOf(usm[2].content)), usm[5].content) //nolint:gosec // G115: a test's counters
		sealed := parts[3].content
		if user.Priv == "DES" {
			block, _ := des.NewCipher(key[:8]) //nolint:gosec // G405: SNMPv3's DES privacy
			if len(sealed)%8 != 0 {
				return report(0, SNMPAgentUser{}, nil, "1.3.6.1.6.3.15.1.1.6.0")
			}
			plain := make([]byte, len(sealed))
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, sealed)
			scoped = plain
		} else {
			scoped = agentCFB(key, iv, sealed, false)
		}
		// The scoped PDU is the first value of what was decrypted: DES's
		// padding comes after it.
		end := agentFirstEnd(scoped)
		if end < 0 {
			return report(0, SNMPAgentUser{}, nil, "1.3.6.1.6.3.15.1.1.6.0")
		}
		scoped = scoped[:end]
	}
	nodes, err := agentParse(scoped, 0, len(scoped))
	if err != nil || len(nodes) != 1 {
		return nil
	}
	inner := agentChildren(scoped, nodes[0])
	if len(inner) != 3 || !bytes.Equal(inner[0].content, a.config.EngineID) {
		return nil
	}
	pdu, ok := a.respond(scoped, inner[2])
	if !ok {
		return nil
	}
	return send(flags&3, user, pdu)
}

// agentFirstEnd is where the first value of buf ends, -1 when it does not
// within buf.
func agentFirstEnd(buf []byte) int {
	if len(buf) < 2 {
		return -1
	}
	n, pos := int(buf[1]), 2
	if n&0x80 != 0 {
		k := n & 0x7f
		if k == 0 || k > 4 || pos+k > len(buf) {
			return -1
		}
		n = 0
		for _, d := range buf[pos : pos+k] {
			n = n<<8 | int(d)
		}
		pos += k
	}
	if pos+n > len(buf) {
		return -1
	}
	return pos + n
}
