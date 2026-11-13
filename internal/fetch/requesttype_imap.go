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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The imap request type logs in to a mail server as a mail client does and
// asks how much mail each mailbox holds: the greeting, CAPABILITY,
// STARTTLS, LOGIN or AUTHENTICATE, then STATUS for each mailbox the
// collector names, LIST for each it names by a pattern, GETQUOTAROOT when
// it asks for the quota, and for each of its counts EXAMINE and SEARCH. A
// mailbox is opened only to be searched, read-only, so no message is
// read, marked or moved.
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
	Mailboxes []string    `yaml:"mailboxes,omitempty"`
	Quota     bool        `yaml:"quota,omitempty"`
	Counts    []IMAPCount `yaml:"counts,omitempty"`
}

// IMAPCount is an entry of request.counts: how many messages of a mailbox
// match a search, counted by the server. The mailbox is opened to search
// it, read-only, with EXAMINE, which marks nothing read and changes no
// flag; no message is fetched.
type IMAPCount struct {
	Name    string `yaml:"name,omitempty"`
	Mailbox string `yaml:"mailbox,omitempty"`
	Search  string `yaml:"search,omitempty"`
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
	// imapMaxCounts bounds request.counts: each is a search of a mailbox.
	imapMaxCounts = 20
	// imapMailboxKind is the kind of templated field a mailbox's name is,
	// and imapSearchKind that of a search's criteria.
	imapMailboxKind = "imap_mailbox"
	imapSearchKind  = "imap_search"
)

// imapCountName is what a count's name may be: what a label or a jq field
// is written as without quotes.
var imapCountName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// imapEndsInLiteral is a command that ends in a literal's length, {n} or
// {n+}: the server takes what follows the line as n bytes of the command.
var imapEndsInLiteral = regexp.MustCompile(`\{[0-9]+\+?\}\s*$`)

// imapPorts are the schemes of an imap target and their default ports.
var imapPorts = map[string]int{"": 143, "imap": 143, "imaps": 993}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeIMAP,
		Fields:       []string{"username", "password_file", "auth", "xoauth2", "allow_plaintext", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
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
			return unusedParamsIn(c, unused, "its request.mailboxes, request.counts or label values")
		},
		CheckTargetRequest: checkNetTargetLogin,
		CheckFiles: func(c *model.Collector, t *model.StaticTarget) []string {
			return append(accountFileWarnings(c, t), xoauth2FileWarnings(c, t)...)
		},
	})
	templateEscapers[imapMailboxKind] = func(where, name, value string) (string, error) {
		if strings.ContainsFunc(value, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return "", fmt.Errorf("%s: the value of %s holds a control character, which a mailbox's name written into a command may not", where, name)
		}
		return value, nil
	}
	// A probe's value is written into a search within double quotes only
	// (Validate), where it is one string whatever it holds, but for what
	// ends the string, and what a search sent without a charset may not
	// hold: so no probe adds a criterion to a search.
	templateEscapers[imapSearchKind] = func(where, name, value string) (string, error) {
		if strings.ContainsFunc(value, func(r rune) bool { return r < ' ' || r >= 0x7f || r == '"' || r == '\\' }) {
			return "", fmt.Errorf("%s: the value of %s holds a double quote, a backslash or a character that is not printable ASCII, which a search's quoted text may not take from a probe", where, name)
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
	if err := checkNetLogin(x, "a login sends both"); err != nil {
		return err
	}
	switch {
	case r.Username == "" && (len(own.Mailboxes) > 0 || own.Quota || len(own.Counts) > 0):
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
	if len(own.Counts) > imapMaxCounts {
		return fmt.Errorf("collector %q request.counts has %d entries, more than %d; each is a search of a mailbox at every scrape", x.Name, len(own.Counts), imapMaxCounts)
	}
	names := map[string]bool{}
	for i, count := range own.Counts {
		at := fmt.Sprintf("request.counts[%d]", i)
		switch {
		case !imapCountName.MatchString(count.Name):
			return fmt.Errorf("collector %q %s.name %q is not a name: letters, digits and underscores, not starting with a digit", x.Name, at, count.Name)
		case names[count.Name]:
			return fmt.Errorf("collector %q %s.name %q names two counts; a rule reads a count by its name", x.Name, at, count.Name)
		case strings.TrimSpace(count.Search) == "":
			return fmt.Errorf("collector %q %s has no search; write the criteria of an IMAP SEARCH, such as UNSEEN or UNANSWERED BEFORE {{date_imap:-1d}}", x.Name, at)
		case strings.ContainsFunc(count.Search, func(r rune) bool { return r < ' ' || r >= 0x7f }):
			return fmt.Errorf("collector %q %s.search holds a character that is not printable ASCII, which a search sent without a charset may not", x.Name, at)
		case imapEndsInLiteral.MatchString(count.Search):
			return fmt.Errorf("collector %q %s.search ends in {n}, which a server reads as the length of text sent after the command, and waits for; write the text within double quotes", x.Name, at)
		case strings.ContainsAny(count.Mailbox, "*%"):
			return fmt.Errorf("collector %q %s.mailbox %q is a pattern; a search is of one mailbox, so name it", x.Name, at, count.Mailbox)
		case strings.ContainsFunc(count.Mailbox, func(r rune) bool { return r < ' ' || r == 0x7f }):
			return fmt.Errorf("collector %q %s.mailbox holds a control character", x.Name, at)
		}
		names[count.Name] = true
	}
	for _, field := range imapCountTemplates(own) {
		placeholders, err := field.parse()
		if err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
		for _, p := range placeholders {
			if field.kind == imapSearchKind && p.Clock == nil && !imapWithinQuotes(field.text[:p.start]) {
				return fmt.Errorf("collector %q %s: {{%s}} is not within double quotes; a probe's value is one quoted string of a search, as in FROM \"{{%s}}\", so that it cannot add a criterion", x.Name, field.where, p.Name, p.Name)
			}
		}
	}
	return nil
}

// imapWithinQuotes says whether what follows before is within a quoted
// string, as a server reads one: a backslash takes the character after it,
// within quotes alone.
func imapWithinQuotes(before string) bool {
	within := false
	for i := 0; i < len(before); i++ {
		switch {
		case within && before[i] == '\\':
			i++
		case before[i] == '"':
			within = !within
		}
	}
	return within
}

// imapTemplates are the mailboxes of an imap collector and the mailbox and
// the search of each of its counts, which take placeholders.
func imapTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	own := imapRequest(c)
	var out []templateField
	for i, name := range own.Mailboxes {
		out = append(out, templateField{fmt.Sprintf("request.mailboxes[%d]", i), name, imapMailboxKind})
	}
	return append(out, imapCountTemplates(own)...)
}

// imapCountTemplates are the templated fields of the counts: of each, its
// mailbox, INBOX when it names none, and its search, in that order.
func imapCountTemplates(own IMAPConfig) []templateField {
	var out []templateField
	for i, count := range own.Counts {
		mailbox := count.Mailbox
		if mailbox == "" {
			mailbox = "INBOX"
		}
		out = append(out,
			templateField{fmt.Sprintf("request.counts[%d].mailbox", i), mailbox, imapMailboxKind},
			templateField{fmt.Sprintf("request.counts[%d].search", i), count.Search, imapSearchKind})
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

// imapCounted is the answer to one of request.counts: how many messages of
// the mailbox match, null when the server refused to open the mailbox or
// to search it, whose answer is then in text.
type imapCounted struct {
	Name    string `json:"name"`
	Mailbox string `json:"mailbox"`
	OK      bool   `json:"ok"`
	Text    string `json:"text"`
	Count   *int64 `json:"count"`
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
	Counts        []imapCounted `json:"counts"`
}

// imapSession is one conversation with a server.
type imapSession struct {
	ctx   context.Context
	conn  *netConn
	label string
	// left is what the responses may still take of the response limit.
	left int64
	tag  int
	// lineLimit is the most a line of a response may be: imapLineLimit,
	// but for the answer to a search a server lists every match of, whose
	// line is listed.
	lineLimit int
	listed    bool
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
		part, err := netLine(s.conn.r, s.lineLimit-whole.Len())
		if s.left -= int64(len(part)); s.left < 0 {
			return "", imapTooLarge()
		}
		if errors.Is(err, errNetLineTooLong) {
			if s.listed {
				// The matches of a search, listed past what the responses
				// may still be.
				return "", imapTooLarge()
			}
			return "", fmt.Errorf("a line of the response is longer than %d bytes, which no answer to these commands is", s.lineLimit)
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
		if n > s.lineLimit-whole.Len()-open {
			return "", fmt.Errorf("a line of the response is longer than %d bytes, which no answer to these commands is", s.lineLimit)
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
	return model.MarkError(model.Errorf("the server's responses are larger than the response limit; name fewer mailboxes, narrow a search, or raise request.max_response_bytes or limits.max_response_bytes"), model.ErrLimitExceeded)
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
	var filled []string
	for _, field := range imapTemplates(c, overrides) {
		text, err := field.render(overrides.Params)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s is empty once its placeholders are filled", field.where)
		}
		filled = append(filled, text)
	}
	// The mailboxes come first, then each count's mailbox and search.
	named, counts := filled[:len(own.Mailboxes)], filled[len(own.Mailboxes):]
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
	s := &imapSession{ctx: ctx, conn: conn, label: label, left: responseLimit(c), lineLimit: imapLineLimit}
	doc := imapDocument{TLS: implicit, Capabilities: []string{}, Mailboxes: []imapMailbox{}, Quota: []imapQuota{}, Counts: []imapCounted{}}
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
		if !doc.TLS && !c.Request.AllowPlaintext {
			return nil, fmt.Errorf("the server %s offers no STARTTLS, and the account's password is not sent over a connection that is not TLS; use imaps://, or request.allow_plaintext for a server that has none", address.hostPort)
		}
		if err := s.login(c, password, doc.Capabilities, address.hostPort); err != nil {
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
		if doc.Counts, err = s.counts(own.Counts, counts, doc.Capabilities); err != nil {
			return nil, err
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

// login logs in, with the mechanism request.auth names, or else with
// AUTHENTICATE PLAIN where the server offers it, which carries any name and
// password, and with LOGIN otherwise. A refusal fails the scrape, saying
// what the server said, never what was sent.
func (s *imapSession) login(c *model.Collector, password string, offered []string, hostPort string) error {
	user, mechanism := c.Request.Username, c.Request.Auth
	if mechanism == "" {
		mechanism = netAuthLogin
		if slices.Contains(offered, "AUTH=PLAIN") {
			mechanism = netAuthPlain
		}
	}
	// sasl sends a mechanism's credential: in the command where the server
	// takes it there, and in answer to its continuation request otherwise.
	sasl := func(name, credential string) (imapAnswer, error) {
		if !slices.Contains(offered, "AUTH="+name) {
			return imapAnswer{}, fmt.Errorf("the server %s does not offer AUTH=%s, which request.auth asks for", hostPort, name)
		}
		var answer imapAnswer
		var err error
		if slices.Contains(offered, "SASL-IR") {
			answer, err = s.command("AUTHENTICATE "+name+" "+credential, "AUTHENTICATE "+name+" ***")
		} else if answer, err = s.command("AUTHENTICATE "+name, ""); err == nil && answer.status == "+" {
			answer, err = s.continued(credential, "*** (the credential)")
		}
		// A server that asks for more has refused what it was given: an
		// XOAUTH2 refusal comes as a continuation whose text is the error,
		// which an empty line answers, and any other is given up with *.
		if err == nil && answer.status == "+" {
			if name == "XOAUTH2" {
				answer, err = s.continued("", "(an empty line, to end the refused XOAUTH2)")
			} else {
				answer, err = s.continued("*", "* (to end the login)")
			}
		}
		return answer, err
	}
	b64 := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	var answer imapAnswer
	var err error
	how := "LOGIN"
	switch mechanism {
	case netAuthPlain:
		how = "AUTHENTICATE PLAIN"
		answer, err = sasl("PLAIN", b64("\x00"+user+"\x00"+password))
	case netAuthXOAuth2:
		how = "AUTHENTICATE XOAUTH2"
		var token string
		if token, err = netAccessToken(s.ctx, c.Name, c.Request.XOAuth2); err != nil {
			return err
		}
		answer, err = sasl("XOAUTH2", b64("user="+user+"\x01auth=Bearer "+token+"\x01\x01"))
		if err == nil && answer.status != "OK" {
			netTokens.forget(c.Name, c.Request.XOAuth2)
		}
	default:
		switch {
		case slices.Contains(offered, "LOGINDISABLED"):
			return fmt.Errorf("the server %s takes no LOGIN (LOGINDISABLED) and offers no AUTH=PLAIN; set request.auth to a mechanism it offers, such as xoauth2", hostPort)
		case strings.ContainsFunc(user+password, func(r rune) bool { return r < ' ' || r >= 0x7f }):
			return fmt.Errorf("the server %s offers no AUTH=PLAIN, and LOGIN carries no name or password with a character outside printable ASCII", hostPort)
		}
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

// counts answers each of request.counts: its mailbox is opened read-only,
// with EXAMINE, unless the count before it asked for the same, and
// searched. A
// server with ESEARCH answers the number of matches; any other lists them,
// and they are counted, a line of that answer being as long as the
// response limit leaves room for. A mailbox the server does not open, and
// a search it refuses, as it does criteria it cannot read, is the count's
// answer, and fails nothing.
func (s *imapSession) counts(asked []IMAPCount, filled, capabilities []string) ([]imapCounted, error) {
	out := []imapCounted{}
	counted := slices.Contains(capabilities, "ESEARCH") || slices.Contains(capabilities, "IMAP4REV2")
	opened, refused := "", imapAnswer{}
	for i, count := range asked {
		mailbox, search := filled[2*i], filled[2*i+1]
		result := imapCounted{Name: count.Name, Mailbox: mailbox}
		if mailbox != opened {
			answer, err := s.command("EXAMINE "+imapQuoted(imapEncodeName(mailbox)), "")
			if err != nil {
				return nil, err
			}
			if answer.status == "+" {
				return nil, imapAskedForMore("EXAMINE")
			}
			opened, refused = mailbox, imapAnswer{}
			if answer.status != "OK" {
				refused = answer
			}
		}
		if refused.status != "" {
			result.Text = refused.text
			out = append(out, result)
			continue
		}
		command := "SEARCH " + search
		if counted {
			command = "SEARCH RETURN (COUNT) " + search
		} else {
			s.lineLimit, s.listed = int(min(max(s.left, imapLineLimit), 1<<30)), true
		}
		answer, err := s.command(command, "")
		s.lineLimit, s.listed = imapLineLimit, false
		if err != nil {
			return nil, err
		}
		if answer.status == "+" {
			return nil, imapAskedForMore("SEARCH")
		}
		result.OK, result.Text = answer.status == "OK", answer.text
		if result.OK {
			n := imapSearchCount(answer.untagged)
			result.Count = &n
		}
		out = append(out, result)
	}
	return out, nil
}

// imapAskedForMore is the failure of a command the server answered with a
// continuation request: it waits for more of the command, and would read
// the next one as that, so the session ends there.
func imapAskedForMore(command string) error {
	return fmt.Errorf("the server asked for more of %s, as it does of a command that ends in a literal's length; the session cannot go on", command)
}

// imapSearchCount is how many messages a search matched: the COUNT of an
// ESEARCH response, or the numbers of the SEARCH responses, none of either
// being no match.
func imapSearchCount(untagged []string) int64 {
	var n int64
	for _, line := range untagged {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case strings.EqualFold(fields[0], "ESEARCH"):
			for i := 1; i+1 < len(fields); i++ {
				if strings.EqualFold(fields[i], "COUNT") {
					if count, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil && count >= 0 {
						return count
					}
				}
			}
		case strings.EqualFold(fields[0], "SEARCH"):
			n += int64(len(fields) - 1)
		}
	}
	return n
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
		"mailboxes":      {"maxItems": imapMaxNamed, "description": "imap: the mailboxes STATUS asks about, each by its name, such as INBOX or Archive/2026, or by a pattern with * (any text) or % (any text within one level), such as *, which is listed and each mailbox of the list asked about; each may contain {{param_name}} placeholders. INBOX when left out. A mailbox the server refuses is in the document with what it said, and fails nothing. See docs/IMAP.md."},
		"quota":          {"description": "imap: also ask for the quota of the first mailbox's root, with GETQUOTAROOT, where the server has QUOTA: each resource's usage and limit, STORAGE also in bytes."},
		"counts":         {"maxItems": imapMaxCounts, "description": "imap: counts of the messages of a mailbox that match a search, as the server counts them: each a name, a mailbox, INBOX when left out, and a search, the criteria of an IMAP SEARCH. The mailbox is opened read-only to search it, which marks nothing read; no message is fetched. See docs/IMAP.md#counting-messages-by-a-search."},
		"counts.name":    {"description": "What the count is read by in the document, .counts[] | select(.name == \"...\"): letters, digits and underscores."},
		"counts.mailbox": {"description": "The mailbox searched, by its name; INBOX when left out. May contain {{param_name}} placeholders."},
		"counts.search":  {"description": "The criteria of the search, as IMAP writes them, in printable ASCII: UNSEEN, UNANSWERED, FLAGGED, FROM \"billing@example.com\", BEFORE {{date_imap:-1d}}, LARGER 1000000, or several, which all must match. May contain {{param_name}} and clock placeholders."},
	}
}
