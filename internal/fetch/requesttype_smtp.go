//go:build request_type_smtp

package fetch

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The smtp request type talks to a mail server as a client about to send a
// message would, and stops before the message: the greeting, EHLO and the
// extensions it lists, STARTTLS, AUTH when the collector has an account,
// and, when it names a sender and recipients, MAIL FROM and RCPT TO, each
// answered by the server and none followed by DATA. Nothing is ever sent.
//
//	request:
//	  type: smtp
//	  starttls_mode: auto                           # auto, required or never
//	  username: monitor@example.com            # optional: AUTH
//	  password_file: /etc/exporter/smtp-pass   # PLAIN or LOGIN
//	  auth: plain                              # optional: plain, login or xoauth2
//	  mail_from: monitor@example.com           # optional: MAIL FROM
//	  rcpt_to: [postmaster@example.com]        # optional: RCPT TO, each
//
// What the server said is handed to the transforms as one JSON document:
// the greeting, whether the session is over TLS and whether STARTTLS began
// it, the extensions, the mechanisms AUTH offers, how AUTH ended, and the
// code and text of the answer to MAIL FROM and to each RCPT TO. A server
// that refuses a recipient has answered: the refusal is in the document,
// and the scrape succeeds. A greeting that is no 220, an EHLO refused, TLS
// that cannot be begun where it must and an AUTH refused fail the scrape.
//
// The target is host, host:port, smtp://host:port or smtps://host:port:
// port 25, and 465 for smtps://, which is TLS from the first byte. A
// password is never sent over a connection that is not TLS, unless
// starttls_mode: never says the server has none.
//
// The type imports nothing outside the standard library.

// SMTPConfig is an smtp collector's own request keys. How it logs in, auth
// and xoauth2, is of the common part, which imap takes too.
type SMTPConfig struct {
	StartTLS string   `yaml:"starttls_mode,omitempty"`
	Hello    string   `yaml:"hello,omitempty"`
	MailFrom string   `yaml:"mail_from,omitempty"`
	RcptTo   []string `yaml:"rcpt_to,omitempty"`
}

func smtpRequest(c *model.Collector) SMTPConfig {
	own, _ := c.Request.Own.(SMTPConfig)
	return own
}

const (
	smtpStartTLSAuto     = "auto"
	smtpStartTLSRequired = "required"
	smtpStartTLSNever    = "never"

	// smtpTimeout bounds a scrape with no deadline of its own.
	smtpTimeout = 30 * time.Second
	// smtpReplyLimit is the most one reply may be: an EHLO answer is a few
	// hundred bytes.
	smtpReplyLimit = 64 << 10
	// smtpMaxRecipients bounds request.rcpt_to: a check, not a mailing.
	smtpMaxRecipients = 20
	// smtpAddressKind is the kind of templated field an address is.
	smtpAddressKind = "smtp_address"
)

// smtpPorts are the schemes of an smtp target and their default ports.
var smtpPorts = map[string]int{"": 25, "smtp": 25, "smtps": 465}

// smtpAddress is what an address of mail_from or rcpt_to may be once its
// placeholders are filled: a local part and a domain, with no blank, angle
// bracket or control character, which would end the command it is written
// in or begin another.
var smtpAddress = regexp.MustCompile(`^[^\s<>@\x00-\x1f\x7f]+@[^\s<>@\x00-\x1f\x7f]+$`)

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeSMTP,
		Fields:       []string{"username", "password_file", "auth", "xoauth2", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       SMTPConfig{},
		KeyRules:     smtpKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "username", "password_file", "tls"},
		Requester:    smtpRequester{},
		JSONAnswer:   true,
		Stage:        "smtp",
		TargetHint: func(*model.Collector) (string, string) {
			return "Mail server", "host, host:port, smtp://host:587 or smtps://host:465"
		},
		AnswerHint: "Paste the JSON document the exporter makes of the server's answers, as the Response of a run shows it",
		Templates:  smtpTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its request.mail_from, request.rcpt_to or label values")
		},
		CheckTargetRequest: func(_ *model.Collector, t *model.StaticTarget) error { return checkNetTargetAccount(t) },
		CheckFiles:         checkSMTPFiles,
	})
	templateEscapers[smtpAddressKind] = func(where, name, value string) (string, error) {
		if strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == '<' || r == '>' || r == 0x7f }) {
			return "", fmt.Errorf("%s: the value of %s holds a blank, an angle bracket or a control character, which an address written into a command may not", where, name)
		}
		return value, nil
	}
}

// smtpRequester is the smtp request type's Requester.
type smtpRequester struct{}

// Validate checks an smtp collector's request and fills in its defaults.
func (smtpRequester) Validate(x *model.Collector) error {
	if err := checkNetAccount(x); err != nil {
		return err
	}
	r := &x.Request
	own := smtpRequest(x)
	switch own.StartTLS {
	case "":
		own.StartTLS = smtpStartTLSAuto
	case smtpStartTLSAuto, smtpStartTLSRequired, smtpStartTLSNever:
	default:
		return fmt.Errorf("collector %q request.starttls_mode %q is not auto, required or never", x.Name, own.StartTLS)
	}
	if own.Hello != "" && (strings.ContainsFunc(own.Hello, func(c rune) bool { return c <= ' ' || c == 0x7f }) || HasPathParams(own.Hello)) {
		return fmt.Errorf("collector %q request.hello %q is not a name a client gives in EHLO: a host name or an address literal such as [192.0.2.7], with no blank and no placeholder", x.Name, own.Hello)
	}
	if err := checkNetLogin(x, "AUTH sends both"); err != nil {
		return err
	}
	if len(own.RcptTo) > 0 && own.MailFrom == "" {
		return fmt.Errorf("collector %q sets request.rcpt_to without request.mail_from; a server takes a recipient after a sender, so name one, or <> for the null sender", x.Name)
	}
	if len(own.RcptTo) > smtpMaxRecipients {
		return fmt.Errorf("collector %q request.rcpt_to has %d addresses, more than the %d a check asks a server about", x.Name, len(own.RcptTo), smtpMaxRecipients)
	}
	for _, field := range smtpTemplates(&model.Collector{Request: model.RequestConfig{Own: own}}, RequestOverrides{}) {
		placeholders, err := field.parse()
		if err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
		if len(placeholders) == 0 && field.text != "<>" && !smtpAddress.MatchString(field.text) {
			return fmt.Errorf("collector %q %s %q is not an address, local@domain, with no blank or angle bracket", x.Name, field.where, field.text)
		}
		if field.text == "<>" && field.where != "request.mail_from" {
			return fmt.Errorf("collector %q %s is <>, the null sender, which is no recipient", x.Name, field.where)
		}
	}
	r.Own = own
	return nil
}

// smtpTemplates are the addresses of an smtp collector, which take
// placeholders.
func smtpTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	own := smtpRequest(c)
	var out []templateField
	if own.MailFrom != "" {
		out = append(out, templateField{"request.mail_from", own.MailFrom, smtpAddressKind})
	}
	for i, address := range own.RcptTo {
		out = append(out, templateField{fmt.Sprintf("request.rcpt_to[%d]", i), address, smtpAddressKind})
	}
	return out
}

// CheckTarget checks an smtp collector's target.
func (smtpRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, _, err := smtpTarget(c, target)
	return err
}

// smtpTarget reads a target, and whether its session is TLS from the first
// byte: smtps://, which a collector that says starttls_mode: required or never
// contradicts.
func smtpTarget(c *model.Collector, target string) (netAddress, bool, error) {
	a, err := parseNetTarget("smtp", target, smtpPorts, false)
	if err != nil {
		return a, false, err
	}
	implicit := a.scheme == "smtps"
	if mode := smtpRequest(c).StartTLS; implicit && mode != "" && mode != smtpStartTLSAuto {
		return a, false, fmt.Errorf("target %q is smtps://, TLS from the first byte, but the collector sets request.starttls_mode: %s; use smtp://, or leave starttls out", target, mode)
	}
	return a, implicit, nil
}

// Label is the url label of an smtp request: smtp://host:port, or smtps://.
func (smtpRequester) Label(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	a, implicit, err := smtpTarget(c, target)
	if err != nil {
		return "", err
	}
	return smtpURL(a, implicit), nil
}

func smtpURL(a netAddress, implicit bool) string {
	if implicit {
		return "smtps://" + a.hostPort
	}
	return "smtp://" + a.hostPort
}

// Method is the http_method label of an smtp request: how far the session
// goes.
func (smtpRequester) Method(c *model.Collector, _ RequestOverrides) string {
	switch own := smtpRequest(c); {
	case len(own.RcptTo) > 0:
		return "RCPT"
	case own.MailFrom != "":
		return "MAIL"
	case c.Request.Username != "":
		return "AUTH"
	}
	return "EHLO"
}

// Display renders an smtp target for logs and labels.
func (smtpRequester) Display(target string) string {
	return netDisplay("smtp", target, smtpPorts, false)
}

// smtpReply is a server's answer to one command: its code, and its text,
// the lines after the code joined.
type smtpReply struct {
	Code int    `json:"code"`
	Text string `json:"text"`
	// lines are the text of each line, without its code.
	lines []string
}

// smtpAnswered is the answer to MAIL FROM or to one RCPT TO.
type smtpAnswered struct {
	Address  string `json:"address"`
	Code     int    `json:"code"`
	Accepted bool   `json:"accepted"`
	Text     string `json:"text"`
}

// smtpAuthDone is how AUTH ended.
type smtpAuthDone struct {
	Mechanism string `json:"mechanism"`
	Code      int    `json:"code"`
}

// smtpDocument is what a scrape hands the transforms.
type smtpDocument struct {
	Greeting       smtpReply      `json:"greeting"`
	TLS            bool           `json:"tls"`
	StartTLS       bool           `json:"starttls"`
	Extensions     map[string]any `json:"extensions"`
	AuthMechanisms []string       `json:"auth_mechanisms"`
	Size           *int64         `json:"size"`
	Auth           *smtpAuthDone  `json:"auth"`
	MailFrom       *smtpAnswered  `json:"mail_from"`
	RcptTo         []smtpAnswered `json:"rcpt_to"`
}

// smtpSession is one conversation with a server.
type smtpSession struct {
	ctx   context.Context
	conn  *netConn
	label string
	// left is what the replies may still take of the response limit.
	left int64
}

// read reads one reply: its lines, each begun by the same three digits,
// the last followed by a blank.
func (s *smtpSession) read() (smtpReply, error) {
	var reply smtpReply
	size := 0
	for {
		line, err := netLine(s.conn.r, smtpReplyLimit-size)
		size += len(line)
		if s.left -= int64(len(line)); s.left < 0 {
			return reply, model.MarkError(model.Errorf("the server's replies are larger than the response limit; raise request.max_response_bytes or limits.max_response_bytes"), model.ErrLimitExceeded)
		}
		if errors.Is(err, errNetLineTooLong) {
			return reply, fmt.Errorf("a reply is longer than %d bytes, which no SMTP reply is", smtpReplyLimit)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("the server closed the connection")
			}
			return reply, netFailure(s.ctx, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return reply, model.Errorf("the server answered %s, which is no SMTP reply; is the target a mail server?", model.Quoted(netShown(line)))
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil || code < 100 || code > 599 || (len(line) > 3 && line[3] != ' ' && line[3] != '-') {
			return reply, model.Errorf("the server answered %s, which is no SMTP reply; is the target a mail server?", model.Quoted(netShown(line)))
		}
		reply.Code = code
		text := ""
		if len(line) > 4 {
			text = line[4:]
		}
		reply.lines = append(reply.lines, text)
		if len(line) == 3 || line[3] == ' ' {
			reply.Text = netShown(strings.Join(reply.lines, " "))
			return reply, nil
		}
	}
}

// command sends one line and reads its reply. shown is the line as a debug
// report shows it, the line itself unless it holds a credential.
func (s *smtpSession) command(line, shown string) (smtpReply, error) {
	if shown == "" {
		shown = line
	}
	verb, _, _ := strings.Cut(shown, " ")
	traceRequest(s.ctx, verb, s.label, http.Header{"Smtp-Command": {shown}}, "", false)
	if _, err := io.WriteString(s.conn, line+"\r\n"); err != nil {
		err = netFailure(s.ctx, fmt.Errorf("%s: %w", shown, err))
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return smtpReply{}, err
	}
	reply, err := s.read()
	if err != nil {
		err = fmt.Errorf("%s: %w", shown, err)
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return reply, err
	}
	traceOutcome(s.ctx, strconv.Itoa(reply.Code)+" "+reply.Text)
	traceFacts(s.ctx, intFact("smtp.reply.code", reply.Code))
	return reply, nil
}

// refused is the failure of a command the session cannot go on without.
func smtpRefused(what, hostPort string, reply smtpReply) error {
	return model.Errorf("the server %s answered %s with %d %s", hostPort, what, reply.Code, model.Bare(reply.Text))
}

// Fetch performs one conversation of an smtp collector.
func (smtpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	c = withTargetAccount(c, overrides)
	own := smtpRequest(c)
	address, implicit, err := smtpTarget(c, target)
	if err != nil {
		return nil, err
	}
	var addresses []string
	for _, field := range smtpTemplates(c, overrides) {
		filled, err := field.render(overrides.Params)
		if err != nil {
			return nil, err
		}
		if filled != "<>" && !smtpAddress.MatchString(filled) {
			return nil, fmt.Errorf("%s: %q is not an address once its placeholders are filled", field.where, filled)
		}
		addresses = append(addresses, filled)
	}
	ctx, cancel := netBudget(ctx, overrides, smtpTimeout)
	defer cancel()
	password, err := netPassword(c)
	if err != nil {
		return nil, err
	}
	cfg, err := netTLSConfig(c, true)
	if err != nil {
		return nil, err
	}
	if ctx, err = netGuard(ctx, c, "smtp", address.hostPort); err != nil {
		return nil, err
	}
	label := smtpURL(address, implicit)
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
	s := &smtpSession{ctx: ctx, conn: conn, label: label, left: responseLimit(c)}
	doc := smtpDocument{TLS: implicit, RcptTo: []smtpAnswered{}}
	greeting, err := s.read()
	if err != nil {
		err = fmt.Errorf("reading the greeting of %s: %w", address.hostPort, err)
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	traceOutcome(ctx, strconv.Itoa(greeting.Code)+" "+greeting.Text)
	if greeting.Code != 220 {
		return nil, model.Errorf("the server %s greeted with %d %s, not 220: it does not serve this client", address.hostPort, greeting.Code, model.Bare(greeting.Text))
	}
	doc.Greeting = greeting
	name := own.Hello
	if name == "" {
		name = smtpHelloName(conn)
	}
	hello := func() error {
		reply, err := s.command("EHLO "+name, "")
		if err != nil {
			return err
		}
		if reply.Code != 250 {
			return smtpRefused("EHLO", address.hostPort, reply)
		}
		doc.Extensions, doc.AuthMechanisms, doc.Size = smtpExtensions(reply.lines)
		return nil
	}
	if err := hello(); err != nil {
		return nil, err
	}
	_, offered := doc.Extensions["STARTTLS"]
	switch {
	case implicit || own.StartTLS == smtpStartTLSNever:
	case offered:
		reply, err := s.command("STARTTLS", "")
		if err != nil {
			return nil, err
		}
		if reply.Code != 220 {
			return nil, smtpRefused("STARTTLS", address.hostPort, reply)
		}
		if err := conn.secure(ctx, cfg, address.host, address.hostPort); err != nil {
			return nil, err
		}
		doc.TLS, doc.StartTLS = true, true
		// What the server offered before TLS is not what it offers within.
		if err := hello(); err != nil {
			return nil, err
		}
	case own.StartTLS == smtpStartTLSRequired:
		return nil, fmt.Errorf("the server %s does not offer STARTTLS, which request.starttls_mode: required asks for", address.hostPort)
	}
	if c.Request.Username != "" {
		if !doc.TLS && own.StartTLS != smtpStartTLSNever {
			return nil, fmt.Errorf("the server %s offers no STARTTLS, and the account's credential is not sent over a connection that is not TLS; use smtps://, or request.starttls_mode: never for a server that has none", address.hostPort)
		}
		done, err := s.auth(c, own, password, doc.AuthMechanisms, address.hostPort)
		if err != nil {
			return nil, err
		}
		doc.Auth = done
	}
	if len(addresses) > 0 {
		sender := addresses[0]
		path := "<" + strings.Trim(sender, "<>") + ">"
		reply, err := s.command("MAIL FROM:"+path, "")
		if err != nil {
			return nil, err
		}
		doc.MailFrom = &smtpAnswered{Address: strings.Trim(sender, "<>"), Code: reply.Code, Accepted: reply.Code/100 == 2, Text: reply.Text}
		if doc.MailFrom.Accepted {
			for _, recipient := range addresses[1:] {
				reply, err := s.command("RCPT TO:<"+recipient+">", "")
				if err != nil {
					return nil, err
				}
				doc.RcptTo = append(doc.RcptTo, smtpAnswered{Address: recipient, Code: reply.Code, Accepted: reply.Code/100 == 2, Text: reply.Text})
			}
			// The transaction is given up: no DATA follows.
			_, _ = s.command("RSET", "")
		}
	}
	_, _ = s.command("QUIT", "")
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

// smtpHelloName is the name a client gives in EHLO when the collector
// names none: its address as an address literal (RFC 5321 § 4.1.3), since
// the exporter's host name is a pod's or a machine's that the server
// cannot look up.
func smtpHelloName(conn *netConn) string {
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "[127.0.0.1]"
	}
	if strings.Contains(host, ":") {
		zone := strings.IndexByte(host, '%')
		if zone >= 0 {
			host = host[:zone]
		}
		return "[IPv6:" + host + "]"
	}
	return "[" + host + "]"
}

// smtpExtensions reads the lines of an EHLO answer after its first, which
// names the server: each an extension's keyword, in upper case, with its
// parameters as text or true when it has none; the mechanisms of AUTH; and
// SIZE's limit, nil when the server says none.
func smtpExtensions(lines []string) (map[string]any, []string, *int64) {
	extensions := map[string]any{}
	mechanisms := []string{}
	var size *int64
	for _, line := range lines[min(1, len(lines)):] {
		keyword, params, _ := strings.Cut(strings.TrimSpace(line), " ")
		// "AUTH=LOGIN PLAIN" is how some old servers write it.
		if alt, rest, found := strings.Cut(keyword, "="); found && strings.EqualFold(alt, "AUTH") {
			keyword, params = alt, strings.TrimSpace(rest+" "+params)
		}
		keyword = strings.ToUpper(keyword)
		if keyword == "" {
			continue
		}
		params = netShown(strings.TrimSpace(params))
		if params == "" {
			extensions[keyword] = true
		} else {
			extensions[keyword] = params
		}
		switch keyword {
		case "AUTH":
			for _, m := range strings.Fields(strings.ToUpper(params)) {
				if !slices.Contains(mechanisms, m) {
					mechanisms = append(mechanisms, m)
				}
			}
		case "SIZE":
			if n, err := strconv.ParseInt(params, 10, 64); err == nil && n >= 0 {
				size = &n
			}
		}
	}
	slices.Sort(mechanisms)
	return extensions, mechanisms, size
}

// auth logs in: with the mechanism the collector names, or else PLAIN where
// the server offers it and LOGIN where it offers that. A refusal fails the
// scrape, saying the code and what the server said, never what was sent.
func (s *smtpSession) auth(c *model.Collector, own SMTPConfig, password string, offered []string, hostPort string) (*smtpAuthDone, error) {
	mechanism := c.Request.Auth
	if mechanism == "" {
		switch {
		case slices.Contains(offered, "PLAIN"):
			mechanism = netAuthPlain
		case slices.Contains(offered, "LOGIN"):
			mechanism = netAuthLogin
		default:
			return nil, model.Errorf("the server %s offers AUTH %s, neither PLAIN nor LOGIN; name the mechanism in request.auth", hostPort, model.Bare(netShown(strings.Join(offered, " "))))
		}
	}
	name := strings.ToUpper(mechanism)
	if !slices.Contains(offered, name) {
		return nil, model.Errorf("the server %s does not offer AUTH %s; it offers %s", hostPort, name, model.Bare(netShown(strings.Join(offered, " "))))
	}
	user := c.Request.Username
	b64 := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	var reply smtpReply
	var err error
	switch mechanism {
	case netAuthPlain:
		reply, err = s.command("AUTH PLAIN "+b64("\x00"+user+"\x00"+password), "AUTH PLAIN ***")
	case netAuthLogin:
		if reply, err = s.command("AUTH LOGIN", ""); err == nil && reply.Code == 334 {
			if reply, err = s.command(b64(user), "*** (the username)"); err == nil && reply.Code == 334 {
				reply, err = s.command(b64(password), "*** (the password)")
			}
		}
	case netAuthXOAuth2:
		var token string
		if token, err = netAccessToken(s.ctx, c.Name, c.Request.XOAuth2); err != nil {
			return nil, err
		}
		reply, err = s.command("AUTH XOAUTH2 "+b64("user="+user+"\x01auth=Bearer "+token+"\x01\x01"), "AUTH XOAUTH2 ***")
		// A refusal comes as a 334 whose text is the error, base64, which
		// an empty line answers; the 535 follows.
		if err == nil && reply.Code == 334 {
			netTokens.forget(c.Name, c.Request.XOAuth2)
			reply, err = s.command("", "(an empty line, to end the refused XOAUTH2)")
		}
	}
	if err != nil {
		return nil, err
	}
	if reply.Code != 235 {
		if mechanism == netAuthXOAuth2 {
			netTokens.forget(c.Name, c.Request.XOAuth2)
		}
		return nil, model.Errorf("the server %s refused AUTH %s for %s with %d %s", hostPort, name, user, reply.Code, model.Bare(reply.Text))
	}
	return &smtpAuthDone{Mechanism: name, Code: reply.Code}, nil
}

// checkSMTPFiles reads, for --dry-run, the files a scrape reads: the
// password file and the tls block's, and those of request.xoauth2.
func checkSMTPFiles(c *model.Collector, t *model.StaticTarget) []string {
	return append(accountFileWarnings(c, t), xoauth2FileWarnings(c, t)...)
}

// smtpKeyRules is what the configuration schema says of an smtp collector's
// own keys.
func smtpKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"starttls_mode": {"enum": SchemaOptionalEnum([]string{smtpStartTLSAuto, smtpStartTLSRequired, smtpStartTLSNever}), "description": "smtp: auto, the default, begins TLS with STARTTLS where the server offers it; required fails the scrape where it does not; never leaves the session in plaintext, and lets the account's credential be sent so. Not for an smtps:// target, which is TLS from the first byte. See docs/SMTP.md."},
		"hello":         {"description": "smtp: the name given in EHLO. Left out, the exporter's address as an address literal, such as [192.0.2.7]."},
		"mail_from":     {"description": "smtp: the sender MAIL FROM names, an address or <> for the null sender; may contain {{param_name}} placeholders. Left out, the session ends after EHLO, or AUTH. Nothing is sent: no DATA follows."},
		"rcpt_to":       {"maxItems": smtpMaxRecipients, "description": "smtp: the recipients RCPT TO asks the server about, one command each, after request.mail_from; each may contain {{param_name}} placeholders. The server's answer to each is in the document, and a refusal fails nothing."},
	}
}
