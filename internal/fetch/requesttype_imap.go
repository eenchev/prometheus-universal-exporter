//go:build request_type_imap

package fetch

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The imap request type logs in to a mail server as a mail client does and
// asks how much mail each mailbox holds, without opening one: the greeting,
// CAPABILITY, STARTTLS, LOGIN or AUTHENTICATE PLAIN, then STATUS for each
// mailbox the collector names, LIST for each it names by a pattern, and
// GETQUOTAROOT when it asks for the quota. No mailbox is selected, so no
// message is read, marked or moved.
//
//	request:
//	  type: imap
//	  username: monitor@example.com
//	  password_file: /etc/exporter/imap-password
//	  mailboxes: [INBOX, "Archive/*"]          # INBOX when left out
//	  quota: true                              # optional: GETQUOTAROOT
//
// What the server said is handed to the transforms as one JSON document:
// the greeting, whether the session is over TLS, the capabilities, and for
// each mailbox its messages, unseen, recent, the next UID and, where the
// server counts it, its size. A mailbox the server refuses STATUS for has
// answered: it is in the document with what the server said, and the scrape
// succeeds. A greeting that is no OK, TLS that cannot be begun and a login
// refused fail the scrape.
//
// The target is host, host:port, imap://host:port or imaps://host:port:
// port 143, and 993 for imaps://, which is TLS from the first byte. On
// imap:// TLS is begun with STARTTLS where the server offers it. A password
// is never sent over a connection that is not TLS, unless allow_plaintext
// says the server has none.
//
// The type imports nothing outside the standard library.

// IMAPConfig is an imap collector's own request keys.
type IMAPConfig struct {
	Mailboxes      []string `yaml:"mailboxes,omitempty"`
	Quota          bool     `yaml:"quota,omitempty"`
	AllowPlaintext bool     `yaml:"allow_plaintext,omitempty"`
}

func imapRequest(c *model.Collector) IMAPConfig {
	own, _ := c.Request.Own.(IMAPConfig)
	return own
}

const (
	// imapTimeout bounds a scrape with no deadline of its own.
	imapTimeout = 30 * time.Second
	// imapLineLimit is the most one line of a response may be, the
	// literals within it counted: a LIST line is a mailbox's name and a
	// few flags.
	imapLineLimit = 64 << 10
	// imapMaxNamed bounds request.mailboxes, and imapMaxMailboxes the
	// mailboxes a scrape asks STATUS for, those its patterns list among
	// them: each is a command and an answer.
	imapMaxNamed     = 50
	imapMaxMailboxes = 500
	// imapMailboxKind is the kind of templated field a mailbox's name is.
	imapMailboxKind = "imap_mailbox"
)

// imapPorts are the schemes of an imap target and their default ports.
var imapPorts = map[string]int{"": 143, "imap": 143, "imaps": 993}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeIMAP,
		Fields:       []string{"username", "password_file", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       IMAPConfig{},
		KeyRules:     imapKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "username", "password_file", "tls"},
		Requester:    imapRequester{},
		JSONAnswer:   true,
		Stage:        "imap",
		TargetHint: func(*model.Collector) (string, string) {
			return "Mail server", "host, host:port, imap://host:143 or imaps://host:993"
		},
		AnswerHint: "Paste the JSON document the exporter makes of the server's answers, as the Response of a run shows it",
		Templates:  imapTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its request.mailboxes or label values")
		},
		CheckTargetRequest: func(_ *model.Collector, t *model.StaticTarget) error { return checkNetTargetAccount(t) },
		CheckFiles:         accountFileWarnings,
	})
	templateEscapers[imapMailboxKind] = func(where, name, value string) (string, error) {
		if strings.ContainsFunc(value, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return "", fmt.Errorf("%s: the value of %s holds a control character, which a mailbox's name written into a command may not", where, name)
		}
		return value, nil
	}
}

// imapRequester is the imap request type's Requester.
type imapRequester struct{}

// Validate checks an imap collector's request.
func (imapRequester) Validate(x *model.Collector) error {
	if err := checkNetAccount(x); err != nil {
		return err
	}
	r := &x.Request
	own := imapRequest(x)
	switch {
	case r.PasswordFile != "" && r.Username == "":
		return fmt.Errorf("collector %q sets request.password_file without request.username; name the account, for most servers the mailbox's address", x.Name)
	case r.Username != "" && r.PasswordFile == "" && !x.RequestNotMade:
		return fmt.Errorf("collector %q sets request.username without request.password_file; a login sends both, so name the file of the password", x.Name)
	case r.Username == "" && (len(own.Mailboxes) > 0 || own.Quota):
		return fmt.Errorf("collector %q asks about mailboxes without an account; a server answers STATUS after a login, so set request.username and request.password_file, or leave mailboxes and quota out to read the greeting and the capabilities alone", x.Name)
	case len(own.Mailboxes) > imapMaxNamed:
		return fmt.Errorf("collector %q request.mailboxes has %d entries, more than %d; name the rest by a pattern, such as \"Archive/*\"", x.Name, len(own.Mailboxes), imapMaxNamed)
	}
	for i, name := range own.Mailboxes {
		switch {
		case strings.TrimSpace(name) == "":
			return fmt.Errorf("collector %q request.mailboxes[%d] is empty; name a mailbox, such as INBOX, or a pattern, such as *", x.Name, i)
		case strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }):
			return fmt.Errorf("collector %q request.mailboxes[%d] holds a control character", x.Name, i)
		}
		if _, err := (templateField{fmt.Sprintf("request.mailboxes[%d]", i), name, imapMailboxKind}).parse(); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	}
	return nil
}

// imapTemplates are the mailboxes of an imap collector, which take
// placeholders.
func imapTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	var out []templateField
	for i, name := range imapRequest(c).Mailboxes {
		out = append(out, templateField{fmt.Sprintf("request.mailboxes[%d]", i), name, imapMailboxKind})
	}
	return out
}

// CheckTarget checks an imap collector's target.
func (imapRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseNetTarget("imap", target, imapPorts, false)
	return err
}

// Label is the url label of an imap request: imap://host:port, or imaps://.
func (imapRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	a, err := parseNetTarget("imap", target, imapPorts, false)
	if err != nil {
		return "", err
	}
	return imapURL(a), nil
}

func imapURL(a netAddress) string {
	if a.scheme == "imaps" {
		return "imaps://" + a.hostPort
	}
	return "imap://" + a.hostPort
}

// Method is the http_method label of an imap request: how far the session
// goes.
func (imapRequester) Method(c *model.Collector, _ RequestOverrides) string {
	if c.Request.Username != "" {
		return "STATUS"
	}
	return "CAPABILITY"
}

// Display renders an imap target for logs and labels.
func (imapRequester) Display(target string) string {
	return netDisplay("imap", target, imapPorts, false)
}

// imapMailbox is what the server said of one mailbox. The counts are null
// where the server gave none: all of them for a mailbox it refused STATUS
// for, whose answer is then in text, and size where it does not count one.
type imapMailbox struct {
	Name        string   `json:"name"`
	OK          bool     `json:"ok"`
	Text        string   `json:"text"`
	Messages    *int64   `json:"messages"`
	Unseen      *int64   `json:"unseen"`
	Recent      *int64   `json:"recent"`
	UIDNext     *int64   `json:"uidnext"`
	UIDValidity *int64   `json:"uidvalidity"`
	Size        *int64   `json:"size"`
	Flags       []string `json:"flags"`
}

// imapQuota is one resource of a quota root: STORAGE, in units of 1024
// bytes as IMAP counts it, and in bytes beside it, or MESSAGE.
type imapQuota struct {
	Root       string `json:"root"`
	Resource   string `json:"resource"`
	Usage      int64  `json:"usage"`
	Limit      int64  `json:"limit"`
	UsageBytes *int64 `json:"usage_bytes"`
	LimitBytes *int64 `json:"limit_bytes"`
}

// imapDocument is what a scrape hands the transforms.
type imapDocument struct {
	Greeting      string        `json:"greeting"`
	TLS           bool          `json:"tls"`
	StartTLS      bool          `json:"starttls"`
	Capabilities  []string      `json:"capabilities"`
	Authenticated bool          `json:"authenticated"`
	Mailboxes     []imapMailbox `json:"mailboxes"`
	Quota         []imapQuota   `json:"quota"`
}

// imapSession is one conversation with a server.
type imapSession struct {
	ctx   context.Context
	conn  *netConn
	label string
	// left is what the responses may still take of the response limit.
	left int64
	tag  int
}

// imapAnswer is the server's answer to a command: its untagged lines, each
// without its "* ", and the tagged line's status, OK, NO or BAD, and text.
type imapAnswer struct {
	untagged []string
	status   string
	text     string
}

// line reads one line of a response, with the literals it holds: a line
// that ends {n} goes on with n bytes and the rest of the line, and the
// literal is put into the line as a quoted string, so that what reads the
// line reads one form.
func (s *imapSession) line() (string, error) {
	var whole strings.Builder
	for {
		part, err := netLine(s.conn.r, imapLineLimit-whole.Len())
		if s.left -= int64(len(part)); s.left < 0 {
			return "", imapTooLarge()
		}
		if errors.Is(err, errNetLineTooLong) {
			return "", fmt.Errorf("a line of the response is longer than %d bytes, which no answer to these commands is", imapLineLimit)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("the server closed the connection")
			}
			return "", netFailure(s.ctx, err)
		}
		part = strings.TrimRight(part, "\r\n")
		open := strings.LastIndexByte(part, '{')
		if open < 0 || !strings.HasSuffix(part, "}") {
			whole.WriteString(part)
			return whole.String(), nil
		}
		n, isLength := imapLiteralLength(part[open+1 : len(part)-1])
		if !isLength {
			// Braces that are text, and no literal's length.
			whole.WriteString(part)
			return whole.String(), nil
		}
		if n > imapLineLimit-whole.Len()-open {
			return "", fmt.Errorf("a line of the response is longer than %d bytes, which no answer to these commands is", imapLineLimit)
		}
		if s.left -= int64(n); s.left < 0 {
			return "", imapTooLarge()
		}
		literal := make([]byte, n)
		if _, err := io.ReadFull(s.conn.r, literal); err != nil {
			return "", netFailure(s.ctx, err)
		}
		whole.WriteString(part[:open])
		whole.WriteString(imapQuoted(string(literal)))
	}
}

// imapLiteralLength is the length a literal's braces hold, digits with a +
// after them or none, and whether they hold one.
func imapLiteralLength(braced string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSuffix(braced, "+"))
	return n, err == nil && n >= 0
}

func imapTooLarge() error {
	return model.MarkError(model.Errorf("the server's responses are larger than the response limit; name fewer mailboxes, or raise request.max_response_bytes or limits.max_response_bytes"), model.ErrLimitExceeded)
}

// imapQuoted is text as a quoted string of a command.
func imapQuoted(text string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text) + `"`
}

// command sends one command under a tag of its own and reads the response
// to its tagged line. shown is the command as a debug report shows it, the
// command itself unless it holds a credential.
func (s *imapSession) command(command, shown string) (imapAnswer, error) {
	if shown == "" {
		shown = command
	}
	s.tag++
	tag := "a" + strconv.Itoa(s.tag)
	verb, _, _ := strings.Cut(shown, " ")
	traceRequest(s.ctx, verb, s.label, http.Header{"Imap-Command": {shown}}, "", false)
	failed := func(err error) (imapAnswer, error) {
		err = fmt.Errorf("%s: %w", verb, err)
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return imapAnswer{}, err
	}
	if _, err := io.WriteString(s.conn, tag+" "+command+"\r\n"); err != nil {
		return failed(netFailure(s.ctx, err))
	}
	return s.answer(tag, failed)
}

// answer reads the response to the command sent under tag.
func (s *imapSession) answer(tag string, failed func(error) (imapAnswer, error)) (imapAnswer, error) {
	var answer imapAnswer
	for {
		line, err := s.line()
		if err != nil {
			return failed(err)
		}
		if rest, ok := strings.CutPrefix(line, "* "); ok {
			if len(answer.untagged) >= imapMaxMailboxes*2 {
				return failed(fmt.Errorf("the response has more than %d lines; name the mailboxes by a narrower pattern", imapMaxMailboxes*2))
			}
			answer.untagged = append(answer.untagged, rest)
			continue
		}
		if strings.HasPrefix(line, "+") {
			// A continuation request, which answer's caller answers.
			answer.status, answer.text = "+", strings.TrimSpace(line[1:])
			return answer, nil
		}
		rest, ok := strings.CutPrefix(line, tag+" ")
		if !ok {
			return failed(model.Errorf("the server answered %s, which is no IMAP response to the command; is the target an IMAP server?", model.Quoted(netShown(line))))
		}
		answer.status, answer.text, _ = strings.Cut(rest, " ")
		answer.status, answer.text = strings.ToUpper(answer.status), netShown(answer.text)
		if answer.status != "OK" && answer.status != "NO" && answer.status != "BAD" {
			return failed(model.Errorf("the server answered %s, which is no IMAP response to the command; is the target an IMAP server?", model.Quoted(netShown(line))))
		}
		traceOutcome(s.ctx, answer.status+" "+answer.text)
		return answer, nil
	}
}

// imapRefused is the failure of a command the session cannot go on without.
func imapRefused(what, hostPort string, answer imapAnswer) error {
	return model.Errorf("the server %s answered %s with %s %s", hostPort, what, answer.status, model.Bare(answer.text))
}

// Fetch performs one conversation of an imap collector.
func (imapRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	c = withTargetAccount(c, overrides)
	own := imapRequest(c)
	address, err := parseNetTarget("imap", target, imapPorts, false)
	if err != nil {
		return nil, err
	}
	implicit := address.scheme == "imaps"
	var named []string
	for _, field := range imapTemplates(c, overrides) {
		filled, err := field.render(overrides.Params)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(filled) == "" {
			return nil, fmt.Errorf("%s is empty once its placeholders are filled", field.where)
		}
		named = append(named, filled)
	}
	if len(named) == 0 && c.Request.Username != "" {
		named = []string{"INBOX"}
	}
	ctx, cancel := netBudget(ctx, overrides, imapTimeout)
	defer cancel()
	password, err := netPassword(c)
	if err != nil {
		return nil, err
	}
	cfg, err := netTLSConfig(c, true)
	if err != nil {
		return nil, err
	}
	if ctx, err = netGuard(ctx, c, "imap", address.hostPort); err != nil {
		return nil, err
	}
	label := imapURL(address)
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
	s := &imapSession{ctx: ctx, conn: conn, label: label, left: responseLimit(c)}
	doc := imapDocument{TLS: implicit, Capabilities: []string{}, Mailboxes: []imapMailbox{}, Quota: []imapQuota{}}
	greeting, err := s.line()
	if err != nil {
		err = fmt.Errorf("reading the greeting of %s: %w", address.hostPort, err)
		traceFailedOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	traceOutcome(ctx, imapText(greeting))
	status, text, _ := strings.Cut(strings.TrimPrefix(greeting, "* "), " ")
	if !strings.HasPrefix(greeting, "* ") || !strings.EqualFold(status, "OK") {
		return nil, model.Errorf("the server %s greeted with %s, not * OK: it is no IMAP server, or does not serve this client", address.hostPort, model.Quoted(netShown(greeting)))
	}
	doc.Greeting = imapText(text)
	capabilities := func() error {
		answer, err := s.command("CAPABILITY", "")
		if err != nil {
			return err
		}
		if answer.status != "OK" {
			return imapRefused("CAPABILITY", address.hostPort, answer)
		}
		doc.Capabilities = imapCapabilities(answer.untagged)
		return nil
	}
	if err := capabilities(); err != nil {
		return nil, err
	}
	if !implicit && slices.Contains(doc.Capabilities, "STARTTLS") {
		answer, err := s.command("STARTTLS", "")
		if err != nil {
			return nil, err
		}
		if answer.status != "OK" {
			return nil, imapRefused("STARTTLS", address.hostPort, answer)
		}
		if err := conn.secure(ctx, cfg, address.host, address.hostPort); err != nil {
			return nil, err
		}
		doc.TLS, doc.StartTLS = true, true
		// What the server offered before TLS is not what it offers within.
		if err := capabilities(); err != nil {
			return nil, err
		}
	}
	if user := c.Request.Username; user != "" {
		if !doc.TLS && !own.AllowPlaintext {
			return nil, fmt.Errorf("the server %s offers no STARTTLS, and the account's password is not sent over a connection that is not TLS; use imaps://, or request.allow_plaintext for a server that has none", address.hostPort)
		}
		if err := s.login(user, password, doc.Capabilities, address.hostPort); err != nil {
			return nil, err
		}
		doc.Authenticated = true
		// And what it offers an account is not what it offers a stranger.
		if err := capabilities(); err != nil {
			return nil, err
		}
		if doc.Mailboxes, err = s.mailboxes(named, doc.Capabilities, address.hostPort); err != nil {
			return nil, err
		}
		if own.Quota && slices.Contains(doc.Capabilities, "QUOTA") {
			of := "INBOX"
			if len(doc.Mailboxes) > 0 {
				of = doc.Mailboxes[0].Name
			}
			answer, err := s.command("GETQUOTAROOT "+imapQuoted(imapEncodeName(of)), "")
			if err != nil {
				return nil, err
			}
			if answer.status == "OK" {
				doc.Quota = imapQuotas(answer.untagged)
			}
		}
	}
	_, _ = s.command("LOGOUT", "")
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

// imapText is a greeting as the document and a debug report have it: its
// control characters written as ?, and cut where any failure's text is.
// It is not looked through for credentials as netShown looks, which reads
// the AUTH=PLAIN of a greeting's capabilities as one: a greeting holds
// nothing the scrape sent.
func imapText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, text)
	return model.CutTo(text, model.MaxFailureBytes, false)
}

// imapMaxCapabilities bounds the capabilities kept of a response, and
// imapCapabilityBytes how long one may be: a server names a few dozen, each
// a word.
const (
	imapMaxCapabilities = 200
	imapCapabilityBytes = 100
)

// imapCapabilities are the capabilities of a CAPABILITY response, in upper
// case and sorted, each once. One is kept as the server wrote it, AUTH=PLAIN
// with its mechanism, which is no credential; what is no word of printable
// ASCII, or longer than any capability, is left out.
func imapCapabilities(untagged []string) []string {
	out := []string{}
	for _, line := range untagged {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "CAPABILITY") {
			continue
		}
		for _, capability := range fields[1:] {
			capability = strings.ToUpper(capability)
			if len(capability) > imapCapabilityBytes || len(out) >= imapMaxCapabilities || slices.Contains(out, capability) ||
				strings.ContainsFunc(capability, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
				continue
			}
			out = append(out, capability)
		}
	}
	slices.Sort(out)
	return out
}

// login logs in: with AUTHENTICATE PLAIN where the server offers it, which
// carries any name and password, and with LOGIN otherwise. A refusal fails
// the scrape, saying what the server said, never what was sent.
func (s *imapSession) login(user, password string, offered []string, hostPort string) error {
	var answer imapAnswer
	var err error
	how := "LOGIN"
	switch {
	case slices.Contains(offered, "AUTH=PLAIN"):
		how = "AUTHENTICATE PLAIN"
		credential := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + password))
		if slices.Contains(offered, "SASL-IR") {
			answer, err = s.command("AUTHENTICATE PLAIN "+credential, "AUTHENTICATE PLAIN ***")
		} else if answer, err = s.command("AUTHENTICATE PLAIN", ""); err == nil && answer.status == "+" {
			answer, err = s.continued(credential, "*** (the name and the password)")
		}
		// A server that asks for more has refused what it was given.
		if err == nil && answer.status == "+" {
			answer, err = s.continued("*", "* (to end the login)")
		}
	case slices.Contains(offered, "LOGINDISABLED"):
		return fmt.Errorf("the server %s takes no LOGIN (LOGINDISABLED) and offers no AUTH=PLAIN; it wants a mechanism this type does not speak", hostPort)
	case strings.ContainsFunc(user+password, func(r rune) bool { return r < ' ' || r >= 0x7f }):
		return fmt.Errorf("the server %s offers no AUTH=PLAIN, and LOGIN carries no name or password with a character outside printable ASCII", hostPort)
	default:
		answer, err = s.command("LOGIN "+imapQuoted(user)+" "+imapQuoted(password), "LOGIN "+imapQuoted(user)+" ***")
	}
	if err != nil {
		return err
	}
	if answer.status != "OK" {
		return model.Errorf("the server %s refused %s for %s with %s %s", hostPort, how, user, answer.status, model.Bare(answer.text))
	}
	return nil
}

// continued answers a continuation request with one line and reads on to
// the tagged line of the command it belongs to.
func (s *imapSession) continued(line, shown string) (imapAnswer, error) {
	tag := "a" + strconv.Itoa(s.tag)
	traceRequest(s.ctx, "+", s.label, http.Header{"Imap-Command": {shown}}, "", false)
	failed := func(err error) (imapAnswer, error) {
		traceFailedOutcome(s.ctx, "error: "+err.Error())
		return imapAnswer{}, err
	}
	if _, err := io.WriteString(s.conn, line+"\r\n"); err != nil {
		return failed(netFailure(s.ctx, err))
	}
	return s.answer(tag, failed)
}

// mailboxes asks about each mailbox named: one named by a pattern, with *
// or %, is listed, and each mailbox the list holds that can be selected is
// asked about; any other is asked about by its name.
func (s *imapSession) mailboxes(named, capabilities []string, hostPort string) ([]imapMailbox, error) {
	out := []imapMailbox{}
	seen := map[string]bool{}
	items := "MESSAGES UNSEEN UIDNEXT UIDVALIDITY"
	if slices.Contains(capabilities, "IMAP4REV1") {
		// IMAP4rev2 has no RECENT, and a server of that alone refuses it.
		items = "MESSAGES UNSEEN RECENT UIDNEXT UIDVALIDITY"
	}
	if slices.Contains(capabilities, "STATUS=SIZE") || slices.Contains(capabilities, "IMAP4REV2") {
		items += " SIZE"
	}
	ask := func(name string, flags []string) error {
		if seen[name] {
			return nil
		}
		if len(out) >= imapMaxMailboxes {
			return fmt.Errorf("the mailboxes named come to more than %d; name fewer, or by a narrower pattern", imapMaxMailboxes)
		}
		seen[name] = true
		answer, err := s.command("STATUS "+imapQuoted(imapEncodeName(name))+" ("+items+")", "")
		if err != nil {
			return err
		}
		mailbox := imapMailbox{Name: name, OK: answer.status == "OK", Text: answer.text, Flags: flags}
		if mailbox.Flags == nil {
			mailbox.Flags = []string{}
		}
		if mailbox.OK {
			imapStatus(&mailbox, answer.untagged)
		}
		out = append(out, mailbox)
		return nil
	}
	for _, name := range named {
		if !strings.ContainsAny(name, "*%") {
			if err := ask(name, nil); err != nil {
				return nil, err
			}
			continue
		}
		answer, err := s.command(`LIST "" `+imapQuoted(imapEncodeName(name)), "")
		if err != nil {
			return nil, err
		}
		if answer.status != "OK" {
			return nil, imapRefused("LIST", hostPort, answer)
		}
		for _, line := range answer.untagged {
			listed, flags, ok := imapListed(line)
			if !ok || slices.ContainsFunc(flags, func(flag string) bool {
				return strings.EqualFold(flag, `\Noselect`) || strings.EqualFold(flag, `\NonExistent`)
			}) {
				continue
			}
			if err := ask(listed, flags); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// imapTokens splits a line of a response into its atoms, quoted strings,
// without their quotes, and parenthesised lists, each a token that begins
// with ( and holds its own tokens to the matching ).
func imapTokens(line string) []string {
	var out []string
	for i := 0; i < len(line); {
		switch line[i] {
		case ' ':
			i++
		case '"':
			var text strings.Builder
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				text.WriteByte(line[i])
			}
			i++
			out = append(out, text.String())
		case '(':
			depth, from := 0, i
		within:
			for ; i < len(line); i++ {
				switch line[i] {
				case '"':
					for i++; i < len(line) && line[i] != '"'; i++ {
						if line[i] == '\\' {
							i++
						}
					}
				case '(':
					depth++
				case ')':
					if depth--; depth == 0 {
						i++
						break within
					}
				}
			}
			out = append(out, line[from:min(i, len(line))])
		default:
			from := i
			for i < len(line) && line[i] != ' ' {
				i++
			}
			out = append(out, line[from:i])
		}
	}
	return out
}

// imapList is the tokens within a parenthesised list token.
func imapList(token string) []string {
	return imapTokens(strings.TrimSuffix(strings.TrimPrefix(token, "("), ")"))
}

// imapStatus reads the counts of a STATUS response into mailbox.
func imapStatus(mailbox *imapMailbox, untagged []string) {
	for _, line := range untagged {
		tokens := imapTokens(line)
		if len(tokens) < 3 || !strings.EqualFold(tokens[0], "STATUS") || !strings.HasPrefix(tokens[len(tokens)-1], "(") {
			continue
		}
		pairs := imapList(tokens[len(tokens)-1])
		for i := 0; i+1 < len(pairs); i += 2 {
			n, err := strconv.ParseInt(pairs[i+1], 10, 64)
			if err != nil || n < 0 {
				continue
			}
			switch strings.ToUpper(pairs[i]) {
			case "MESSAGES":
				mailbox.Messages = &n
			case "UNSEEN":
				mailbox.Unseen = &n
			case "RECENT":
				mailbox.Recent = &n
			case "UIDNEXT":
				mailbox.UIDNext = &n
			case "UIDVALIDITY":
				mailbox.UIDValidity = &n
			case "SIZE":
				mailbox.Size = &n
			}
		}
	}
}

// imapListed reads one LIST response: the mailbox's name, decoded, and
// its flags.
func imapListed(line string) (name string, flags []string, ok bool) {
	tokens := imapTokens(line)
	if len(tokens) < 4 || !strings.EqualFold(tokens[0], "LIST") || !strings.HasPrefix(tokens[1], "(") {
		return "", nil, false
	}
	flags = []string{}
	for _, flag := range imapList(tokens[1]) {
		flags = append(flags, netShown(flag))
	}
	return imapDecodeName(tokens[3]), flags, true
}

// imapQuotas reads the QUOTA responses of GETQUOTAROOT: each resource of
// each root, STORAGE also in bytes.
func imapQuotas(untagged []string) []imapQuota {
	out := []imapQuota{}
	for _, line := range untagged {
		tokens := imapTokens(line)
		if len(tokens) < 3 || !strings.EqualFold(tokens[0], "QUOTA") || !strings.HasPrefix(tokens[2], "(") {
			continue
		}
		triples := imapList(tokens[2])
		for i := 0; i+2 < len(triples); i += 3 {
			usage, err1 := strconv.ParseInt(triples[i+1], 10, 64)
			limit, err2 := strconv.ParseInt(triples[i+2], 10, 64)
			if err1 != nil || err2 != nil || usage < 0 || limit < 0 {
				continue
			}
			quota := imapQuota{Root: netShown(imapDecodeName(tokens[1])), Resource: netShown(strings.ToUpper(triples[i])), Usage: usage, Limit: limit}
			if quota.Resource == "STORAGE" && usage < 1<<52 && limit < 1<<52 {
				usageBytes, limitBytes := usage*1024, limit*1024
				quota.UsageBytes, quota.LimitBytes = &usageBytes, &limitBytes
			}
			out = append(out, quota)
		}
	}
	return out
}

// A mailbox's name is written in a command, and comes in a response, in
// modified UTF-7 (RFC 3501 § 5.1.3): printable ASCII as it is, & as &-, and
// anything else as the base64, with , for /, of its UTF-16, between & and -.

var imapBase64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// imapEncodeName is name as a command writes it.
func imapEncodeName(name string) string {
	var out strings.Builder
	var pending []rune
	flush := func() {
		if len(pending) == 0 {
			return
		}
		units := utf16.Encode(pending)
		raw := make([]byte, 0, len(units)*2)
		for _, unit := range units {
			raw = append(raw, byte(unit>>8), byte(unit&0xff))
		}
		out.WriteString("&" + imapBase64.EncodeToString(raw) + "-")
		pending = pending[:0]
	}
	for _, r := range name {
		switch {
		case r == '&':
			flush()
			out.WriteString("&-")
		case r >= ' ' && r < 0x7f:
			flush()
			out.WriteRune(r)
		default:
			pending = append(pending, r)
		}
	}
	flush()
	return out.String()
}

// imapDecodeName is a name of a response as text; one that is not modified
// UTF-7, as a server that sends UTF-8 writes it, is the name as it came.
func imapDecodeName(name string) string {
	if !strings.Contains(name, "&") {
		return strings.ToValidUTF8(name, "�")
	}
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] != '&' {
			out.WriteByte(name[i])
			continue
		}
		end := strings.IndexByte(name[i:], '-')
		if end < 0 {
			return strings.ToValidUTF8(name, "�")
		}
		encoded := name[i+1 : i+end]
		i += end
		if encoded == "" {
			out.WriteByte('&')
			continue
		}
		raw, err := imapBase64.DecodeString(encoded)
		if err != nil || len(raw)%2 != 0 {
			return strings.ToValidUTF8(name, "�")
		}
		units := make([]uint16, len(raw)/2)
		for j := range units {
			units[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}
		for _, r := range utf16.Decode(units) {
			if r == utf8.RuneError {
				return strings.ToValidUTF8(name, "�")
			}
			out.WriteRune(r)
		}
	}
	return strings.ToValidUTF8(out.String(), "�")
}

// imapKeyRules is what the configuration schema says of an imap collector's
// own keys.
func imapKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"mailboxes":       {"maxItems": imapMaxNamed, "description": "imap: the mailboxes STATUS asks about, each by its name, such as INBOX or Archive/2026, or by a pattern with * (any text) or % (any text within one level), such as *, which is listed and each mailbox of the list asked about; each may contain {{param_name}} placeholders. INBOX when left out. A mailbox the server refuses is in the document with what it said, and fails nothing. See docs/IMAP.md."},
		"quota":           {"description": "imap: also ask for the quota of the first mailbox's root, with GETQUOTAROOT, where the server has QUOTA: each resource's usage and limit, STORAGE also in bytes."},
		"allow_plaintext": {"description": "imap: log in over a connection that is not TLS, to a server that offers no STARTTLS. The password is then sent as it is; left out, such a scrape fails before it logs in."},
	}
}
