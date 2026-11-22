//go:build request_type_pop3

package fetch

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: APOP (RFC 1939) is MD5, which a server may speak alone
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The pop3 request type logs in to a mail server as a mail client that
// fetches over POP3 does and asks how much mail the mailbox holds: the
// greeting, CAPA, STLS, a login, then STAT, and LIST when the collector
// asks for the largest message. No message is fetched or deleted, and
// QUIT ends the session with the mailbox as it was.
//
//	request:
//	  type: pop3
//	  username: monitor@example.com
//	  password_file: /etc/exporter/pop3-password
//	  auth: apop                               # optional: the mechanism
//
// What the server said is handed to the transforms as one JSON document:
// the greeting, whether the session is over TLS, the capabilities and the
// SASL mechanisms the server names, the mechanism the login used, and the
// mailbox's messages and their size together. A greeting that is no +OK,
// TLS that cannot be begun, a login refused and a STAT refused fail the
// scrape.
//
// The login is the mechanism request.auth names: user (USER and PASS),
// apop, or one of the SASL mechanisms of AUTH, plain, login, cram-md5,
// xoauth2 and oauthbearer. Left out, it is user where the server takes it, else plain,
// else login, and xoauth2 where the collector has the block.
//
// The target is host, host:port, pop3://host:port or pop3s://host:port:
// port 110, and 995 for pop3s://, which is TLS from the first byte. On
// pop3:// TLS is begun with STLS where the server offers it. A credential
// is never sent over a connection that is not TLS, unless allow_plaintext
// says the server has none.
//
// The type imports nothing outside the standard library.

// POP3Config is a pop3 collector's own request keys; its account and its
// login are of the common part, as the other mail types take them.
type POP3Config struct {
	// List also asks LIST, each message's size, for the largest of them.
	List bool `yaml:"list,omitempty"`
}

func pop3Request(c *model.Collector) POP3Config {
	own, _ := c.Request.Own.(POP3Config)
	return own
}

const (
	// pop3Timeout bounds a scrape with no deadline of its own.
	pop3Timeout = 30 * time.Second
	// pop3LineLimit is the most one line of a response may be: RFC 2449
	// allows 512 bytes, and an XOAUTH2 refusal is a few hundred more.
	pop3LineLimit = 4096
	// pop3MaxCapabilities bounds the lines read of a CAPA response, and
	// pop3CapabilityBytes how long a capability or a mechanism kept of it
	// may be: a server names a dozen, each a word.
	pop3MaxCapabilities = 200
	pop3CapabilityBytes = 100

	// The mechanisms a pop3 login may be made with beside those every
	// mail type speaks (netxoauth2.go).
	pop3AuthUser = "user"
	pop3AuthAPOP = "apop"
)

// pop3Ports are the schemes of a pop3 target and their default ports.
var pop3Ports = map[string]int{"": 110, "pop3": 110, "pop3s": 995}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypePOP3,
		Fields:       []string{"username", "password_file", "auth", "xoauth2", "allow_plaintext", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       POP3Config{},
		KeyRules:     pop3KeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "username", "password_file", "tls"},
		Requester:    pop3Requester{},
		JSONAnswer:   true,
		Stage:        "pop3",
		TargetHint: func(*model.Collector) (string, string) {
			return "Mail server", "host, host:port, pop3://host:110 or pop3s://host:995"
		},
		AnswerHint: "Paste the JSON document the exporter makes of the server's answers, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		CheckTargetRequest: checkNetTargetLogin,
		CheckFiles: func(c *model.Collector, t *model.StaticTarget) []string {
			return append(accountFileWarnings(c, t), xoauth2FileWarnings(c, t)...)
		},
	})
}

// pop3Requester is the pop3 request type's Requester.
type pop3Requester struct{}

// Validate checks a pop3 collector's request.
func (pop3Requester) Validate(x *model.Collector) error {
	if err := checkNetAccount(x); err != nil {
		return err
	}
	if err := checkNetLogin(x, "a login sends both", pop3AuthUser, pop3AuthAPOP); err != nil {
		return err
	}
	if pop3Request(x).List && x.Request.Username == "" {
		return fmt.Errorf("collector %q sets request.list without an account; a server lists a mailbox after a login, so set request.username and request.password_file", x.Name)
	}
	if r := &x.Request; r.Auth == pop3AuthAPOP && strings.ContainsFunc(r.Username, func(r rune) bool { return r == ' ' }) {
		return fmt.Errorf("collector %q request.username holds a blank, which the name of an APOP command may not; use another request.auth", x.Name)
	}
	return nil
}

// CheckTarget checks a pop3 collector's target.
func (pop3Requester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseNetTarget("pop3", target, pop3Ports, false)
	return err
}

// Label is the url label of a pop3 request: pop3://host:port, or pop3s://.
func (pop3Requester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	a, err := parseNetTarget("pop3", target, pop3Ports, false)
	if err != nil {
		return "", err
	}
	return pop3URL(a), nil
}

func pop3URL(a netAddress) string {
	if a.scheme == "pop3s" {
		return "pop3s://" + a.hostPort
	}
	return "pop3://" + a.hostPort
}

// Method is the http_method label of a pop3 request: how far the session
// goes.
func (pop3Requester) Method(c *model.Collector, _ RequestOverrides) string {
	if c.Request.Username != "" {
		return "STAT"
	}
	return "CAPA"
}

// Display renders a pop3 target for logs and labels.
func (pop3Requester) Display(target string) string {
	return netDisplay("pop3", target, pop3Ports, false)
}

// pop3Document is what a scrape hands the transforms. auth, messages and
// size_bytes are null of a session that did not log in, and largest_bytes
// unless the collector asks for the list, and of a mailbox without a
// message.
type pop3Document struct {
	Greeting      string   `json:"greeting"`
	TLS           bool     `json:"tls"`
	StartTLS      bool     `json:"starttls"`
	Capabilities  []string `json:"capabilities"`
	SASL          []string `json:"sasl"`
	APOP          bool     `json:"apop"`
	Authenticated bool     `json:"authenticated"`
	Auth          *string  `json:"auth"`
	Messages      *int64   `json:"messages"`
	SizeBytes     *int64   `json:"size_bytes"`
	LargestBytes  *int64   `json:"largest_bytes"`
}

// pop3Session is one conversation with a server.
type pop3Session struct {
	ctx   context.Context
	conn  *netConn
	label string
	// left is what the responses may still take of the response limit.
	left int64
}

// pop3Answer is the server's answer to a command: +OK, -ERR, or, within an
// AUTH, a continuation request, "+ " and a challenge, with its text.
type pop3Answer struct {
	ok        bool
	continued bool
	text      string
}

// status is the answer's indicator, as a failure names it.
func (a pop3Answer) status() string {
	switch {
	case a.continued:
		return "+"
	case a.ok:
		return "+OK"
	}
	return "-ERR"
}

func pop3TooLarge() error {
	return model.MarkError(model.Errorf("the server's responses are larger than the response limit; raise request.max_response_bytes or limits.max_response_bytes"), model.ErrLimitExceeded)
}

// line reads one line of a response, without its line ending.
func (s *pop3Session) line() (string, error) {
	line, err := netLine(s.conn.r, pop3LineLimit)
	if s.left -= int64(len(line)); s.left < 0 {
		return "", pop3TooLarge()
	}
	if errors.Is(err, errNetLineTooLong) {
		return "", fmt.Errorf("a line of the response is longer than %d bytes, which no answer to these commands is", pop3LineLimit)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("the server closed the connection")
		}
		return "", netFailure(s.ctx, err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// read reads the answer to what was sent last.
func (s *pop3Session) read() (pop3Answer, error) {
	line, err := s.line()
	if err != nil {
		return pop3Answer{}, err
	}
	indicator, text, _ := strings.Cut(line, " ")
	switch {
	case strings.EqualFold(indicator, "+OK"):
		return pop3Answer{ok: true, text: pop3Text(text)}, nil
	case strings.EqualFold(indicator, "-ERR"):
		return pop3Answer{text: pop3Text(text)}, nil
	case indicator == "+":
		// A challenge is base64, read as it came; it is shown nowhere.
		return pop3Answer{continued: true, text: strings.TrimSpace(text)}, nil
	}
	return pop3Answer{}, model.Errorf("the server answered %s, which is no POP3 response; is the target a POP3 server?", model.Quoted(netShown(line)))
}

// send writes one command and reads the answer to it. shown is the command
// as a debug report shows it, the command itself unless it holds a
// credential.
func (s *pop3Session) send(command, shown string) (pop3Answer, error) {
	if shown == "" {
		shown = command
	}
	verb, _, _ := strings.Cut(shown, " ")
	return s.exchange(verb, command, shown)
}

// answer writes the answer to a challenge, shown as shown, and reads what
// the server says to it.
func (s *pop3Session) answer(line, shown string) (pop3Answer, error) {
	return s.exchange("+", line, shown)
}

// exchange writes one line and reads the answer to it.
func (s *pop3Session) exchange(verb, line, shown string) (pop3Answer, error) {
	traceRequest(s.ctx, verb, s.label, http.Header{"Pop3-Command": {shown}}, "", false)
	failed := func(err error) (pop3Answer, error) {
		err = fmt.Errorf("%s: %w", verb, err)
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return pop3Answer{}, err
	}
	if _, err := io.WriteString(s.conn, line+"\r\n"); err != nil {
		return failed(netFailure(s.ctx, err))
	}
	answer, err := s.read()
	if err != nil {
		return failed(err)
	}
	if answer.continued {
		traceOutcome(s.ctx, "+ (a challenge)")
	} else {
		traceOutcome(s.ctx, answer.status()+" "+answer.text)
	}
	return answer, nil
}

// pop3Text is a server's text as the document, an error and a debug report
// have it: its control characters written as ?, and cut where any
// failure's text is. It is not looked through for credentials as netShown
// looks: an answer holds nothing the scrape sent.
func pop3Text(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, text)
	return model.CutTo(text, model.MaxFailureBytes, false)
}

// pop3Refused is the failure of a command the session cannot go on without.
func pop3Refused(what, hostPort string, answer pop3Answer) error {
	return model.Errorf("the server %s answered %s with %s %s", hostPort, what, answer.status(), model.Bare(answer.text))
}

// capabilities asks CAPA and reads its lines to the one that is a dot: the
// capabilities, each by its first word, in upper case and sorted, each
// once, and the mechanisms of its SASL line. A server that refuses CAPA,
// as one older than RFC 2449 does, names none, and known is false.
func (s *pop3Session) capabilities() (names, sasl []string, known bool, err error) {
	names, sasl = []string{}, []string{}
	answer, err := s.send("CAPA", "")
	if err != nil || !answer.ok {
		return names, sasl, false, err
	}
	word := func(text string) (string, bool) {
		text = strings.ToUpper(text)
		return text, len(text) <= pop3CapabilityBytes && !strings.ContainsFunc(text, func(r rune) bool { return r <= ' ' || r >= 0x7f })
	}
	for n := 0; ; n++ {
		line, err := s.line()
		if err != nil {
			return nil, nil, false, fmt.Errorf("CAPA: %w", err)
		}
		if line == "." {
			break
		}
		if n >= pop3MaxCapabilities {
			return nil, nil, false, fmt.Errorf("the answer to CAPA has more than %d lines, which no list of capabilities has", pop3MaxCapabilities)
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name, ok := word(fields[0])
		if !ok {
			continue
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
		if name != "SASL" {
			continue
		}
		for _, field := range fields[1:] {
			if mechanism, ok := word(field); ok && !slices.Contains(sasl, mechanism) {
				sasl = append(sasl, mechanism)
			}
		}
	}
	slices.Sort(names)
	slices.Sort(sasl)
	return names, sasl, true, nil
}

// largest asks LIST and reads its lines to the one that is a dot, each a
// message's number and its size: the size of the largest message, nil of
// a mailbox without one. The lines are as many as the mailbox has
// messages, and are held to the response limit as every answer is.
func (s *pop3Session) largest(hostPort string) (*int64, error) {
	answer, err := s.send("LIST", "")
	if err != nil {
		return nil, err
	}
	if !answer.ok {
		return nil, pop3Refused("LIST", hostPort, answer)
	}
	var largest *int64
	for {
		line, err := s.line()
		if err != nil {
			return nil, fmt.Errorf("LIST: %w", err)
		}
		if line == "." {
			return largest, nil
		}
		fields := strings.Fields(line)
		var size int64 = -1
		if len(fields) >= 2 {
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				size = n
			}
		}
		if size < 0 {
			return nil, model.Errorf("the server %s answered LIST with the line %s, which is no message's number and size", hostPort, model.Quoted(pop3Text(line)))
		}
		if largest == nil || size > *largest {
			largest = &size
		}
	}
}

// pop3Timestamp is the timestamp of a greeting, which a server that takes
// APOP ends its greeting's text with or holds within it: the last
// <...> of it, as RFC 1939 has it, or "".
func pop3Timestamp(greeting string) string {
	open := strings.LastIndexByte(greeting, '<')
	if open < 0 {
		return ""
	}
	end := strings.IndexByte(greeting[open:], '>')
	if end < 2 || strings.ContainsAny(greeting[open:open+end], " \t") {
		return ""
	}
	return greeting[open : open+end+1]
}

// Fetch performs one conversation of a pop3 collector.
func (pop3Requester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	c = withTargetAccount(c, overrides)
	address, err := parseNetTarget("pop3", target, pop3Ports, false)
	if err != nil {
		return nil, err
	}
	implicit := address.scheme == "pop3s"
	ctx, cancel := netBudget(ctx, overrides, pop3Timeout)
	defer cancel()
	password, err := netPassword(c)
	if err != nil {
		return nil, err
	}
	cfg, err := netTLSConfig(c, true)
	if err != nil {
		return nil, err
	}
	if ctx, err = netGuard(ctx, c, "pop3", address.hostPort); err != nil {
		return nil, err
	}
	label := pop3URL(address)
	var first *tls.Config
	if implicit {
		first = cfg
	}
	traceRequest(ctx, "CONNECT", label, nil, "", false)
	conn, err := netDial(ctx, address, first)
	if err != nil {
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	defer conn.close()
	s := &pop3Session{ctx: ctx, conn: conn, label: label, left: responseLimit(c)}
	doc := pop3Document{TLS: implicit}
	greeting, err := s.line()
	if err != nil {
		err = fmt.Errorf("reading the greeting of %s: %w", address.hostPort, err)
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	traceOutcome(ctx, pop3Text(greeting))
	indicator, text, _ := strings.Cut(greeting, " ")
	if !strings.EqualFold(indicator, "+OK") {
		return nil, model.Errorf("the server %s greeted with %s, not +OK: it is no POP3 server, or does not serve this client", address.hostPort, model.Quoted(netShown(greeting)))
	}
	doc.Greeting = pop3Text(text)
	timestamp := pop3Timestamp(text)
	doc.APOP = timestamp != ""
	var known bool
	if doc.Capabilities, doc.SASL, known, err = s.capabilities(); err != nil {
		return nil, err
	}
	if !implicit && slices.Contains(doc.Capabilities, "STLS") {
		answer, err := s.send("STLS", "")
		if err != nil {
			return nil, err
		}
		if !answer.ok {
			return nil, pop3Refused("STLS", address.hostPort, answer)
		}
		if err := conn.secure(ctx, cfg, address.host, address.hostPort); err != nil {
			return nil, err
		}
		doc.TLS, doc.StartTLS = true, true
		// What the server offered before TLS is not what it offers within.
		if doc.Capabilities, doc.SASL, known, err = s.capabilities(); err != nil {
			return nil, err
		}
	}
	if c.Request.Username != "" {
		if !doc.TLS && !c.Request.AllowPlaintext {
			return nil, fmt.Errorf("the server %s offers no STLS, and the account's credential is not sent over a connection that is not TLS; use pop3s://, or request.allow_plaintext for a server that has none", address.hostPort)
		}
		mechanism, err := s.login(c, password, timestamp, doc.Capabilities, doc.SASL, known, address.hostPort)
		if err != nil {
			return nil, err
		}
		doc.Authenticated, doc.Auth = true, &mechanism
		answer, err := s.send("STAT", "")
		if err != nil {
			return nil, err
		}
		if !answer.ok {
			return nil, pop3Refused("STAT", address.hostPort, answer)
		}
		fields := strings.Fields(answer.text)
		if len(fields) < 2 {
			return nil, model.Errorf("the server %s answered STAT with %s, which is no count of messages and their size", address.hostPort, model.Quoted(answer.text))
		}
		messages, errMessages := strconv.ParseInt(fields[0], 10, 64)
		size, errSize := strconv.ParseInt(fields[1], 10, 64)
		if errMessages != nil || errSize != nil || messages < 0 || size < 0 {
			return nil, model.Errorf("the server %s answered STAT with %s, which is no count of messages and their size", address.hostPort, model.Quoted(answer.text))
		}
		doc.Messages, doc.SizeBytes = &messages, &size
		if pop3Request(c).List {
			if doc.LargestBytes, err = s.largest(address.hostPort); err != nil {
				return nil, err
			}
		}
	}
	_, _ = s.send("QUIT", "")
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	resp := &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}
	if state := conn.tlsState(); state != nil {
		resp.Trip = &HTTPTrip{Total: resp.Duration, TLSConnection: tlsConnectionOf(state)}
	}
	return resp, nil
}

// login logs in with the mechanism request.auth names and returns it. Left
// out, the mechanism is USER and PASS where the server names USER or
// answered no CAPA, which every server older than SASL takes; else AUTH
// PLAIN, else AUTH LOGIN, of those it names. A SASL mechanism the
// collector names is one the server must name, where it answered CAPA.
//
// A SASL credential is sent in answer to the server's challenge, never in
// the AUTH command, which RFC 5034 allows and servers older than it, and
// some that are not, do not take.
func (s *pop3Session) login(c *model.Collector, password, timestamp string, capabilities, sasl []string, known bool, hostPort string) (string, error) {
	user, mechanism := c.Request.Username, c.Request.Auth
	if mechanism == "" {
		switch {
		case !known || slices.Contains(capabilities, "USER"):
			mechanism = pop3AuthUser
		case slices.Contains(sasl, "PLAIN"):
			mechanism = netAuthPlain
		case slices.Contains(sasl, "LOGIN"):
			mechanism = netAuthLogin
		default:
			mechanism = pop3AuthUser
		}
	}
	// A line ends the command it is in: a name or a password with one
	// would be two commands.
	if strings.ContainsFunc(user+password, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", errors.New("the account's name or password holds a control character, which no POP3 login carries")
	}
	b64 := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	const hidden = "*** (the credential)"
	offered := "none"
	if len(sasl) > 0 {
		offered = strings.Join(sasl, ", ")
	}
	// begin sends AUTH with a mechanism's name and reads the challenge.
	begin := func(name string) (pop3Answer, error) {
		if known && !slices.Contains(sasl, name) {
			return pop3Answer{}, fmt.Errorf("the server %s does not offer the SASL mechanism %s, which request.auth asks for; it offers %s", hostPort, name, offered)
		}
		return s.send("AUTH "+name, "")
	}
	// end reads on to the server's verdict: a challenge when the exchange
	// is over is given up with *, as RFC 5034 ends one.
	end := func(answer pop3Answer, err error) (pop3Answer, error) {
		if err == nil && answer.continued {
			return s.answer("*", "* (to end the login)")
		}
		return answer, err
	}
	var answer pop3Answer
	var err error
	how := "AUTH " + strings.ToUpper(mechanism)
	switch mechanism {
	case pop3AuthUser:
		how = "USER"
		if answer, err = s.send("USER "+user, ""); err == nil && answer.ok {
			how = "PASS"
			answer, err = s.send("PASS "+password, "PASS ***")
		}
	case pop3AuthAPOP:
		how = "APOP"
		if timestamp == "" {
			return "", fmt.Errorf("the greeting of %s holds no timestamp, so the server takes no APOP; use another request.auth", hostPort)
		}
		digest := md5.Sum([]byte(timestamp + password)) //nolint:gosec // G401: APOP is this digest (RFC 1939 § 7)
		answer, err = s.send("APOP "+user+" "+hex.EncodeToString(digest[:]), "APOP "+user+" ***")
	case netAuthPlain:
		if answer, err = begin("PLAIN"); err == nil && answer.continued {
			answer, err = end(s.answer(b64("\x00"+user+"\x00"+password), hidden))
		}
	case netAuthLogin:
		if answer, err = begin("LOGIN"); err == nil && answer.continued {
			if answer, err = s.answer(b64(user), b64(user)+" (the name)"); err == nil && answer.continued {
				answer, err = end(s.answer(b64(password), hidden))
			}
		}
	case netAuthCRAMMD5:
		if answer, err = begin("CRAM-MD5"); err == nil && answer.continued {
			response, bad := netCRAMMD5Response(user, password, answer.text)
			if bad != nil {
				_, _ = s.answer("*", "* (to end the login)")
				return "", fmt.Errorf("the server %s answered AUTH CRAM-MD5 with a challenge that is not base64", hostPort)
			}
			answer, err = end(s.answer(response, hidden))
		}
	case netAuthXOAuth2, netAuthOAuthBearer:
		var token string
		if token, err = netAccessToken(s.ctx, c.Name, c.Request.XOAuth2); err != nil {
			return "", err
		}
		// A refusal comes as a challenge whose text is the error, which
		// an empty line answers for XOAUTH2 and one byte for OAUTHBEARER.
		sent := netXOAuth2Credential(user, token)
		name, last, ended := "XOAUTH2", "", "(an empty line, to end the refused XOAUTH2)"
		if mechanism == netAuthOAuthBearer {
			sent = netOAuthBearerCredential(user, hostPort, token)
			name, last, ended = "OAUTHBEARER", netOAuthBearerEnd, netOAuthBearerEnd+" (to end the refused OAUTHBEARER)"
		}
		if answer, err = begin(name); err == nil && answer.continued {
			if answer, err = s.answer(b64(sent), hidden); err == nil && answer.continued {
				answer, err = s.answer(last, ended)
			}
		}
		if err == nil && !answer.ok {
			netTokens.forget(c.Name, c.Request.XOAuth2)
		}
	}
	if err != nil {
		return "", err
	}
	if !answer.ok {
		return "", model.Errorf("the server %s refused %s for %s with %s %s", hostPort, how, user, answer.status(), model.Bare(answer.text))
	}
	return mechanism, nil
}

// pop3KeyRules is what the configuration schema says of a pop3 collector's
// own keys.
func pop3KeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"list": {"description": "pop3: also ask LIST, each message's size, for the size of the largest: largest_bytes in the document. The answer has a line for each message of the mailbox, held to the response limit; no message is fetched."},
	}
}
