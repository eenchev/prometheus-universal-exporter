//go:build request_type_radius

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // G501: RADIUS (RFC 2865) is defined over MD5
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The radius request type asks a RADIUS server (RFC 2865) to authenticate
// an account, and reads what it answered and how long it took. Its target
// is the server, as host or host:port, with UDP port 1812 when it names
// none. A scrape sends one of:
//   - an Access-Request with request.username and the password of
//     request.password_file, as PAP or CHAP, signed with the shared secret
//     of request.secret_file. The same packet is sent again while no reply
//     comes.
//   - with mode: status-server, a Status-Server (RFC 5997), which asks
//     whether the server is alive and needs no account.
//
//	request:
//	  type: radius
//	  secret_file: /etc/exporter/radius-secret
//	  username: monitor
//	  password_file: /etc/exporter/radius-password
//
// The answer is one JSON document: the reply's code and name, accepted, how
// long the reply took and how many times the request was sent. A reply is
// an answer whatever it says. An Access-Reject comes from a server that is
// up and refused the account, which a rule reads from accepted. So an
// account that should be refused can be watched too.
//
// Every request carries a Message-Authenticator (RFC 3579), which servers
// patched against forged replies (CVE-2024-3596) require. A reply is taken
// only when its Response Authenticator verifies with the shared secret, and
// so does its Message-Authenticator when it has one. Any other datagram is
// ignored, like one from elsewhere, until the deadline. With
// require_message_authenticator, a reply without one fails the scrape.
//
// EAP and MS-CHAP are not spoken, nor RADIUS over TLS.

// RADIUSConfig is a radius collector's own request keys.
type RADIUSConfig struct {
	// Mode is how the request authenticates: pap, the default, chap, or
	// status-server, which sends no account.
	Mode string `yaml:"mode"`
	// SecretFile holds the secret the server shares with this client.
	SecretFile string `yaml:"secret_file"`
	// NASIdentifier is the NAS-Identifier the request names its sender by.
	NASIdentifier string `yaml:"nas_identifier"`
	// Resends is how many more times the request is sent while no reply
	// comes, ResendInterval apart.
	Resends        *int           `yaml:"resends"`
	ResendInterval model.Duration `yaml:"resend_interval"`
	// RequireMessageAuthenticator fails a scrape whose reply has none.
	RequireMessageAuthenticator bool `yaml:"require_message_authenticator"`
}

// radiusRequest is a radius collector's own keys.
func radiusRequest(c *model.Collector) RADIUSConfig {
	own, _ := c.Request.Own.(RADIUSConfig)
	return own
}

const (
	radiusMethod      = "ACCESS-REQUEST"
	radiusDefaultPort = "1812"
	radiusTimeout     = 30 * time.Second

	radiusModePAP    = "pap"
	radiusModeCHAP   = "chap"
	radiusModeStatus = "status-server"

	radiusDefaultNAS            = "prometheus-universal-exporter"
	radiusDefaultResends        = 2
	radiusMaxResends            = 5
	radiusDefaultResendInterval = 2 * time.Second
	radiusMinResendInterval     = 100 * time.Millisecond

	// The codes of the packets (RFC 2865 section 4, RFC 5997).
	radiusAccessRequest      = 1
	radiusAccessAccept       = 2
	radiusAccessReject       = 3
	radiusAccountingResponse = 5
	radiusAccessChallenge    = 11
	radiusStatusServer       = 12

	// The attributes written and read (RFC 2865 section 5, RFC 3579).
	radiusUserName             = 1
	radiusUserPassword         = 2
	radiusCHAPPassword         = 3
	radiusReplyMessage         = 18
	radiusSessionTimeout       = 27
	radiusNASIdentifier        = 32
	radiusMessageAuthenticator = 80

	radiusHeader = 20
	// radiusMaxPacket is the longest packet there is, radiusMaxAttribute
	// the longest value of an attribute, and radiusMaxPassword the longest
	// password PAP carries.
	radiusMaxPacket    = 4096
	radiusMaxAttribute = 253
	radiusMaxPassword  = 128
)

// radiusModes are the values of request.mode.
var radiusModes = []string{radiusModePAP, radiusModeCHAP, radiusModeStatus}

// radiusReplies names the codes a server answers with.
var radiusReplies = map[int]string{
	radiusAccessAccept:       "access-accept",
	radiusAccessReject:       "access-reject",
	radiusAccountingResponse: "accounting-response",
	radiusAccessChallenge:    "access-challenge",
}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeRADIUS,
		Fields:       []string{"username", "password_file", "allowed_targets", "denied_targets"},
		Config:       RADIUSConfig{},
		KeyRules:     radiusKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    radiusRequester{},
		JSONAnswer:   true,
		TargetHint:   func(*model.Collector) (string, string) { return "RADIUS server", "host or host:" + radiusDefaultPort },
		AnswerHint:   "Paste the reply as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "radius",
		CheckFiles: func(c *model.Collector, t *model.StaticTarget) []string {
			if t != nil {
				return nil
			}
			var warnings []string
			if _, _, err := radiusCredentials(c); err != nil {
				warnings = append(warnings, fmt.Sprintf("collector %q: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", c.Name, err))
			}
			return warnings
		},
	})
}

// radiusRequester is the radius request type's Requester.
type radiusRequester struct{}

// Validate checks a radius collector's request and fills in its defaults:
// the mode, the shared secret's file, the account the mode needs, and how
// the request is sent again.
func (radiusRequester) Validate(c *model.Collector) error {
	r := radiusRequest(c)
	if r.Mode == "" {
		r.Mode = radiusModePAP
	}
	if r.NASIdentifier == "" {
		r.NASIdentifier = radiusDefaultNAS
	}
	if r.Resends == nil {
		resends := radiusDefaultResends
		r.Resends = &resends
	}
	if r.ResendInterval == 0 {
		r.ResendInterval = model.Duration(radiusDefaultResendInterval)
	}
	account := c.Request.Username != "" || c.Request.PasswordFile != ""
	switch {
	case !slices.Contains(radiusModes, r.Mode):
		return fmt.Errorf("collector %q request.mode is %q; want pap, chap or status-server, in lower case", c.Name, r.Mode)
	case strings.TrimSpace(r.SecretFile) == "" && !c.RequestNotMade:
		return fmt.Errorf("collector %q has no request.secret_file; a radius request is signed with the secret the server shares with this client, so name the file that holds it", c.Name)
	case HasPathParams(r.SecretFile) || HasPathParams(c.Request.Username) || HasPathParams(c.Request.PasswordFile):
		return fmt.Errorf("collector %q request.secret_file, username or password_file has a placeholder; the account and the secret are used as they are written", c.Name)
	case r.Mode == radiusModeStatus && account:
		return fmt.Errorf("collector %q sets request.username or password_file with mode: status-server, which asks whether the server is alive and sends no account; take them out, or set mode to pap or chap", c.Name)
	case r.Mode != radiusModeStatus && c.Request.Username == "":
		return fmt.Errorf("collector %q has no request.username; a radius collector asks the server to authenticate an account, so name it, or set mode: status-server to ask only whether the server is alive", c.Name)
	case r.Mode != radiusModeStatus && strings.TrimSpace(c.Request.PasswordFile) == "" && !c.RequestNotMade:
		return fmt.Errorf("collector %q has no request.password_file; name the file that holds the password of %s", c.Name, c.Request.Username)
	case len(c.Request.Username) > radiusMaxAttribute:
		return fmt.Errorf("collector %q request.username is %d bytes; a RADIUS attribute holds at most %d", c.Name, len(c.Request.Username), radiusMaxAttribute)
	case len(r.NASIdentifier) > radiusMaxAttribute || strings.ContainsFunc(r.NASIdentifier, func(c rune) bool { return c < 0x20 || c == 0x7f }):
		return fmt.Errorf("collector %q request.nas_identifier is longer than %d bytes or has a control character; it is the name the request gives its sender", c.Name, radiusMaxAttribute)
	case *r.Resends < 0 || *r.Resends > radiusMaxResends:
		return fmt.Errorf("collector %q request.resends is %d; it is how many more times the request is sent while no reply comes, from 0 to %d", c.Name, *r.Resends, radiusMaxResends)
	case time.Duration(r.ResendInterval) < radiusMinResendInterval:
		return fmt.Errorf("collector %q request.resend_interval is %s; it is how long a reply is waited for before the request is sent again, at least %s", c.Name, time.Duration(r.ResendInterval), radiusMinResendInterval)
	}
	c.Request.Own = r
	return nil
}

// radiusKeyRules is what the configuration schema says of a radius
// collector's own keys.
func radiusKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"mode":                          {"enum": SchemaOptionalEnum(radiusModes), "description": "radius: how the request authenticates: pap, the default, which sends the password hidden with the shared secret; chap, which sends a digest of it; or status-server, which asks whether the server is alive and sends no account. See docs/RADIUS.md#the-mode."},
		"secret_file":                   {"description": "radius: the file that holds the secret the server shares with this client, read at each scrape. Required."},
		"nas_identifier":                {"maxLength": radiusMaxAttribute, "pattern": `^[^\x00-\x1f\x7f]*$`, "description": "radius: the NAS-Identifier the request names its sender by, which a server may choose its policy by; prometheus-universal-exporter when left out."},
		"resends":                       {"minimum": 0, "maximum": radiusMaxResends, "description": "radius: how many more times the request is sent while no reply comes, from 0 to 5; 2 when left out."},
		"resend_interval":               {"not": SchemaLeastDuration(radiusMinResendInterval, true), "description": "radius: how long a reply is waited for before the request is sent again, at least 100ms; 2s when left out."},
		"require_message_authenticator": {"description": "radius: fail the scrape when the reply carries no Message-Authenticator, as a server patched against CVE-2024-3596 always sends one; false when left out. One that is there is verified either way."},
	}
}

// Fetch performs one request of a radius collector.
func (radiusRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchRADIUS(ctx, target, c, overrides)
}

// CheckTarget checks a radius collector's target.
func (radiusRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseRADIUSTarget(target)
	return err
}

// Label is the url label of a radius request: radius:// and the server.
func (radiusRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseRADIUSTarget(target)
	if err != nil {
		return "", err
	}
	return "radius://" + address.hostPort, nil
}

// Method is the http_method label of a radius request: the packet sent.
func (radiusRequester) Method(c *model.Collector, _ RequestOverrides) string {
	if radiusRequest(c).Mode == radiusModeStatus {
		return "STATUS-SERVER"
	}
	return radiusMethod
}

// Display renders a radius target for logs and labels: as written, since
// it holds no credentials, when it is one at all.
func (radiusRequester) Display(target string) string {
	if _, err := parseRADIUSTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// radiusAddress is a target: the server's host and port.
type radiusAddress struct {
	host     string
	port     int
	hostPort string
}

// parseRADIUSTarget reads a target: host, host:port or [v6]:port, port
// 1812 when it names none. Nothing else may come with it.
func parseRADIUSTarget(raw string) (radiusAddress, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return radiusAddress{}, ErrMissingTarget
	}
	if strings.Contains(target, "://") {
		return radiusAddress{}, fmt.Errorf("target %q has a scheme; a radius target is host or host:port, such as radius.example.com:1812", raw)
	}
	if strings.ContainsAny(target, "/?#@ \t") {
		return radiusAddress{}, fmt.Errorf("target %q has a path, query or user in it; a radius target is only the host and its port, and the account is request.username", raw)
	}
	host, port := target, radiusDefaultPort
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
	} else if strings.Contains(target, ":") {
		return radiusAddress{}, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:1812", raw)
	}
	if host == "" {
		return radiusAddress{}, fmt.Errorf("target %q names no host; a radius target is host or host:port", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return radiusAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return radiusAddress{}, err
	}
	return radiusAddress{host: canonical, port: n, hostPort: net.JoinHostPort(canonical, port)}, nil
}

// radiusCredentials reads the shared secret and, except for a status-server
// collector, the password. An empty file is a failure: a collector that
// names a file never means a request signed with no secret or a login with
// no password.
func radiusCredentials(c *model.Collector) (secret, password string, err error) {
	r := radiusRequest(c)
	if secret, err = ReadCredentialFile(r.SecretFile); err != nil {
		return "", "", fmt.Errorf("reading request.secret_file: %w", err)
	}
	if secret == "" {
		return "", "", fmt.Errorf("request.secret_file %s is empty", r.SecretFile)
	}
	if r.Mode == radiusModeStatus {
		return secret, "", nil
	}
	if password, err = ReadCredentialFile(c.Request.PasswordFile); err != nil {
		return "", "", fmt.Errorf("reading request.password_file: %w", err)
	}
	switch {
	case password == "":
		return "", "", fmt.Errorf("request.password_file %s is empty", c.Request.PasswordFile)
	case r.Mode == radiusModePAP && len(password) > radiusMaxPassword:
		return "", "", fmt.Errorf("the password of request.password_file is %d bytes, and PAP carries at most %d", len(password), radiusMaxPassword)
	}
	return secret, password, nil
}

// radiusDocument is a server's reply as the transforms read it.
type radiusDocument struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Address is the address the request was sent to, host:port.
	Address string `json:"address"`
	Mode    string `json:"mode"`
	// Code is the reply's code and Reply its name: access-accept,
	// access-reject, access-challenge or accounting-response.
	Code  int    `json:"code"`
	Reply string `json:"reply"`
	// Accepted says the reply is an Access-Accept; to a status-server
	// request, that the server answered as one that is alive does, an
	// Accounting-Response of an accounting port too.
	Accepted bool `json:"accepted"`
	// Attempts is how many times the request was sent, and
	// ResponseSeconds how long after the first the reply came.
	Attempts        int     `json:"attempts"`
	ResponseSeconds float64 `json:"response_seconds"`
	// ReplyMessages is the text the server sent to be shown to the user.
	ReplyMessages []string `json:"reply_messages"`
	// MessageAuthenticator says the reply carried one, which verified.
	MessageAuthenticator bool `json:"message_authenticator"`
	// SessionTimeout is the seconds of session the server grants, null
	// when it names none.
	SessionTimeout *int `json:"session_timeout_seconds"`
	// AttributeTypes is the types of the reply's attributes, in order.
	AttributeTypes  []int   `json:"attribute_types"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// fetchRADIUS sends a scrape's request and answers the reply as JSON.
func fetchRADIUS(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseRADIUSTarget(target)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, radiusTimeout)
		defer cancel()
	}
	// The host's name now; the address the datagrams go to when the socket
	// is connected, as an http request's connections are checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "radius", Host: address.hostPort}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	secret, password, err := radiusCredentials(c)
	if err != nil {
		return nil, err
	}
	r := radiusRequest(c)
	method := radiusRequester{}.Method(c, overrides)
	at := traceRequestAt(ctx, method, "radius://"+address.hostPort, http.Header{"Radius-Mode": {r.Mode}}, "", false)
	doc, err := radiusExchange(ctx, address, c.Request.Username, password, secret, r)
	if err != nil {
		at.outcome("error: "+err.Error(), true)
		return nil, err
	}
	at.outcome(fmt.Sprintf("%s after %d of %d sent", doc.Reply, doc.Attempts, *r.Resends+1), false)
	doc.DurationSeconds = time.Since(start).Seconds()
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}, nil
}

// radiusPacket writes the request: an Access-Request with the account (PAP
// or CHAP), or a Status-Server. Each carries the NAS-Identifier and a
// Message-Authenticator, which comes first, per RFC 3579 since
// CVE-2024-3596. id and authenticator are the request's own.
func radiusPacket(id byte, authenticator [16]byte, username, password, secret string, r RADIUSConfig) []byte {
	code := byte(radiusAccessRequest)
	if r.Mode == radiusModeStatus {
		code = radiusStatusServer
	}
	packet := make([]byte, radiusHeader, 512)
	packet[0], packet[1] = code, id
	copy(packet[4:], authenticator[:])
	attribute := func(kind byte, value []byte) {
		packet = append(packet, kind, byte(len(value)+2)) //nolint:gosec // G115: a value is at most 253 bytes, as Validate and the credentials hold each
		packet = append(packet, value...)
	}
	signature := len(packet) + 2
	attribute(radiusMessageAuthenticator, make([]byte, md5.Size))
	switch r.Mode {
	case radiusModePAP:
		attribute(radiusUserName, []byte(username))
		attribute(radiusUserPassword, radiusHidePassword(password, secret, authenticator))
	case radiusModeCHAP:
		attribute(radiusUserName, []byte(username))
		// The CHAP ident, and MD5 over it, the password and the challenge,
		// which is the Request Authenticator when no CHAP-Challenge is sent
		// (RFC 2865 section 5.3).
		ident := authenticator[0]
		digest := md5.New() //nolint:gosec // G401: CHAP is this digest (RFC 2865 § 5.3)
		digest.Write([]byte{ident})
		digest.Write([]byte(password))
		digest.Write(authenticator[:])
		attribute(radiusCHAPPassword, append([]byte{ident}, digest.Sum(nil)...))
	}
	attribute(radiusNASIdentifier, []byte(r.NASIdentifier))
	binary.BigEndian.PutUint16(packet[2:], uint16(len(packet))) //nolint:gosec // G115: a request is far under 4096 bytes
	mac := hmac.New(md5.New, []byte(secret))
	mac.Write(packet)
	copy(packet[signature:], mac.Sum(nil))
	return packet
}

// radiusHidePassword hides a PAP password (RFC 2865 section 5.2). The
// password is padded with zeros to a multiple of 16 bytes. Each block is
// XORed with the MD5 of the secret and the previous block as sent. The
// first block uses the Request Authenticator instead.
func radiusHidePassword(password, secret string, authenticator [16]byte) []byte {
	hidden := make([]byte, (len(password)+15)/16*16)
	copy(hidden, password)
	previous := authenticator[:]
	for i := 0; i < len(hidden); i += 16 {
		digest := md5.New() //nolint:gosec // G401: User-Password is hidden with this digest (RFC 2865 § 5.2)
		digest.Write([]byte(secret))
		digest.Write(previous)
		for j, b := range digest.Sum(nil) {
			hidden[i+j] ^= b
		}
		previous = hidden[i : i+16]
	}
	return hidden
}

// radiusReply is a verified reply: its code and its attributes in order.
type radiusReply struct {
	code       int
	attributes []radiusAttribute
	signed     bool
}

type radiusAttribute struct {
	kind  int
	value []byte
}

// radiusCheckReply reads a datagram as the reply to the request with id and
// authenticator. When it is not that reply, it says why: another id, a
// length that does not fit, attributes that do not parse, or an
// authenticator that the shared secret does not produce.
func radiusCheckReply(b []byte, id byte, authenticator [16]byte, secret string) (radiusReply, string) {
	if len(b) < radiusHeader {
		return radiusReply{}, fmt.Sprintf("it is %d bytes, shorter than a RADIUS packet", len(b))
	}
	length := int(binary.BigEndian.Uint16(b[2:]))
	if length < radiusHeader || length > len(b) {
		return radiusReply{}, fmt.Sprintf("its length says %d bytes and it is %d", length, len(b))
	}
	b = b[:length]
	if b[1] != id {
		return radiusReply{}, fmt.Sprintf("it answers request %d, not %d", b[1], id)
	}
	reply := radiusReply{code: int(b[0])}
	signature := -1
	for rest, at := b[radiusHeader:], radiusHeader; len(rest) > 0; {
		if len(rest) < 2 {
			return radiusReply{}, "its attributes do not fit its length"
		}
		size := int(rest[1])
		if size < 2 || size > len(rest) {
			return radiusReply{}, "its attributes do not fit its length"
		}
		kind, value := int(rest[0]), rest[2:size]
		if kind == radiusMessageAuthenticator {
			if len(value) != md5.Size || signature >= 0 {
				return radiusReply{}, "its Message-Authenticator is not one of 16 bytes"
			}
			signature = at + 2
		}
		reply.attributes = append(reply.attributes, radiusAttribute{kind: kind, value: slices.Clone(value)})
		rest, at = rest[size:], at+size
	}
	// The Response Authenticator: MD5 over the reply with the request's
	// authenticator in its place, and the secret (RFC 2865 section 3).
	digest := md5.New() //nolint:gosec // G401: the Response Authenticator is this digest (RFC 2865 § 3)
	digest.Write(b[:4])
	digest.Write(authenticator[:])
	digest.Write(b[radiusHeader:])
	digest.Write([]byte(secret))
	if subtle.ConstantTimeCompare(digest.Sum(nil), b[4:radiusHeader]) != 1 {
		return radiusReply{}, "its Response Authenticator is not what the shared secret makes, so it is not this server's reply, or request.secret_file is not the secret the server has for this client"
	}
	if signature >= 0 {
		// HMAC-MD5 over the same, the Message-Authenticator as zeros.
		mac := hmac.New(md5.New, []byte(secret))
		mac.Write(b[:4])
		mac.Write(authenticator[:])
		mac.Write(b[radiusHeader:signature])
		mac.Write(make([]byte, md5.Size))
		mac.Write(b[signature+md5.Size:])
		if !hmac.Equal(mac.Sum(nil), b[signature:signature+md5.Size]) {
			return radiusReply{}, "its Message-Authenticator is not what the shared secret makes"
		}
		reply.signed = true
	}
	if _, known := radiusReplies[reply.code]; !known {
		return radiusReply{}, fmt.Sprintf("its code is %d, which is no reply to a request", reply.code)
	}
	return reply, ""
}

// radiusExchange sends the request to address. It sends it again every
// resend_interval while no reply comes and resends are left, and reads
// datagrams until the reply comes or ctx ends. A resend is the same packet,
// so a server that answered one whose reply was lost answers the same.
func radiusExchange(ctx context.Context, address radiusAddress, username, password, secret string, r RADIUSConfig) (radiusDocument, error) {
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "udp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = fmt.Errorf("connecting to %s: %w", address.hostPort, err)
		}
		return radiusDocument{}, err
	}
	defer func() { _ = conn.Close() }()
	// A scrape cancelled before its deadline, as when every probe waiting
	// for it went away, ends the read too.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	var nonce [17]byte
	_, _ = rand.Read(nonce[:])
	id, authenticator := nonce[16], [16]byte(nonce[:16])
	packet := radiusPacket(id, authenticator, username, password, secret, r)

	deadline, _ := ctx.Deadline()
	first := time.Now()
	reply := make([]byte, radiusMaxPacket)
	ignored, lastIgnored := 0, ""
	sends := *r.Resends + 1
	for attempt := 1; ; attempt++ {
		if _, err := conn.Write(packet); err != nil {
			return radiusDocument{}, fmt.Errorf("sending the request to %s: %w", address.hostPort, err)
		}
		// The last request sent waits out the scrape's deadline.
		until := deadline
		if next := time.Now().Add(time.Duration(r.ResendInterval)); attempt < sends && next.Before(deadline) {
			until = next
		}
		if ctx.Err() == nil {
			if err := conn.SetReadDeadline(until); err != nil {
				return radiusDocument{}, err
			}
		}
		for {
			n, err := conn.Read(reply)
			elapsed := time.Since(first)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil && until != deadline {
					break
				}
				return radiusDocument{}, radiusReadFailure(ctx, address, err, attempt, ignored, lastIgnored)
			}
			checked, why := radiusCheckReply(reply[:n], id, authenticator, secret)
			if why != "" {
				ignored, lastIgnored = ignored+1, why
				continue
			}
			if r.RequireMessageAuthenticator && !checked.signed {
				return radiusDocument{}, fmt.Errorf("the %s of %s has no Message-Authenticator, which request.require_message_authenticator asks of a reply; a server patched against CVE-2024-3596 sends one", radiusReplies[checked.code], address.hostPort)
			}
			doc := radiusDescribe(checked, r)
			doc.Host, doc.Port, doc.Address = address.host, address.port, conn.RemoteAddr().String()
			doc.Attempts, doc.ResponseSeconds = attempt, elapsed.Seconds()
			return doc, nil
		}
	}
}

// radiusDescribe makes the document of a verified reply.
func radiusDescribe(reply radiusReply, r RADIUSConfig) radiusDocument {
	doc := radiusDocument{
		Mode:  r.Mode,
		Code:  reply.code,
		Reply: radiusReplies[reply.code],
		// A Status-Server is answered as the port answers a request: an
		// Access-Accept on the authentication port, an
		// Accounting-Response on the accounting port.
		Accepted:             reply.code == radiusAccessAccept || r.Mode == radiusModeStatus && reply.code == radiusAccountingResponse,
		MessageAuthenticator: reply.signed,
		ReplyMessages:        []string{},
		AttributeTypes:       []int{},
	}
	for _, a := range reply.attributes {
		doc.AttributeTypes = append(doc.AttributeTypes, a.kind)
		switch {
		case a.kind == radiusReplyMessage:
			doc.ReplyMessages = append(doc.ReplyMessages, strings.ToValidUTF8(string(a.value), "�"))
		case a.kind == radiusSessionTimeout && len(a.value) == 4:
			seconds := int(binary.BigEndian.Uint32(a.value))
			doc.SessionTimeout = &seconds
		}
	}
	return doc
}

// radiusReadFailure describes a read that ended without the reply. It is
// either:
//   - the deadline (which wraps context.DeadlineExceeded) or the scrape's
//     cancellation, saying how many times the request was sent, how many
//     datagrams were ignored, and why the last one was;
//   - or a socket error, such as the refusal an ICMP port unreachable brings.
func radiusReadFailure(ctx context.Context, address radiusAddress, err error, sent, ignored int, lastIgnored string) error {
	if ctx.Err() == nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("reading the reply from %s: %w", address.hostPort, err)
	}
	cause, until := ctx.Err(), "the deadline"
	if errors.Is(cause, context.Canceled) {
		until = "the scrape was cancelled"
	} else {
		cause = context.DeadlineExceeded
	}
	times := "once"
	if sent > 1 {
		times = strconv.Itoa(sent) + " times"
	}
	switch ignored {
	case 0:
		return fmt.Errorf("no reply from %s before %s, the request sent %s: %w", address.hostPort, until, times, cause)
	case 1:
		return model.Errorf("no reply from %s before %s, the request sent %s; %d datagram was ignored, as %s: %w", address.hostPort, until, times, model.Size(ignored), lastIgnored, cause)
	}
	return model.Errorf("no reply from %s before %s, the request sent %s; %d datagrams were ignored, the last as %s: %w", address.hostPort, until, times, model.Size(ignored), lastIgnored, cause)
}
