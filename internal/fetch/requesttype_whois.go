//go:build request_type_whois

package fetch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The whois request type reads a domain's registration: when it expires,
// who its registrar is, its status and its name servers. Its target is the
// domain. A scrape asks the registry's WHOIS server (RFC 3912: the name and
// a line end over TCP port 43, the answer until the server closes), which
// request.whois_server names or, left out, IANA's server does for the domain's
// top-level domain, and answers one JSON document: the dates the answer
// holds, read from the many ways registries write them, the registrar, the
// status values and the name servers, every `key: value` line by its key,
// and the text as it came.
//
//	request:
//	  type: whois
//	  # server: whois.nic.example
//
// WHOIS has no format, so the document is what could be read: a date no
// line holds is null, and the rule that reads it fails, rather than a
// domain looking as if it never expired. A registry that knows no such
// domain is not a failure either: the document says registered: false.
//
// Registries limit how often one address may ask, some to a few questions
// a day, so a whois collector is cached for hours (docs/WHOIS.md), and what
// IANA said of a top-level domain is kept for a day.

// WhoisConfig is a whois collector's own request keys.
type WhoisConfig struct {
	// Server is the WHOIS server asked, host or host:port; IANA's server
	// is asked which it is when left out.
	Server string `yaml:"whois_server"`
	// QueryPrefix is written before the domain in the question, for a
	// server that wants more than the name, as DENIC's "-T dn,ace ".
	QueryPrefix string `yaml:"query_prefix"`
}

// whoisRequest is a whois collector's own keys.
func whoisRequest(c *model.Collector) WhoisConfig {
	own, _ := c.Request.Own.(WhoisConfig)
	return own
}

const (
	whoisMethod      = "QUERY"
	whoisDefaultPort = "43"
	whoisTimeout     = 30 * time.Second
	// whoisMaxPrefix is the most characters of request.query_prefix.
	whoisMaxPrefix = 64
	// whoisReferralTTL is how long what IANA said of a top-level domain is
	// kept, and whoisReferralsKept how many of them are.
	whoisReferralTTL   = 24 * time.Hour
	whoisReferralsKept = 512
	// whoisMaxFields and whoisMaxValues bound the fields of the document:
	// the keys read, and the values of one key.
	whoisMaxFields = 256
	whoisMaxValues = 32
)

// whoisRootServer is the server that says which server answers for a
// top-level domain; a test names a stand-in.
var whoisRootServer = "whois.iana.org:" + whoisDefaultPort

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeWhois,
		Fields:       []string{"max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       WhoisConfig{},
		KeyRules:     whoisKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    whoisRequester{},
		JSONAnswer:   true,
		TargetHint:   func(*model.Collector) (string, string) { return "Domain", "example.com" },
		AnswerHint:   "Paste the answer as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "whois",
	})
}

// whoisRequester is the whois request type's Requester.
type whoisRequester struct{}

// Validate checks a whois collector's request: the server is host or
// host:port, and the prefix one short line.
func (whoisRequester) Validate(c *model.Collector) error {
	r := whoisRequest(c)
	if r.Server != "" {
		if _, err := parseWhoisServer(r.Server); err != nil {
			return fmt.Errorf("collector %q request.whois_server: %w", c.Name, err)
		}
	}
	switch {
	case len(r.QueryPrefix) > whoisMaxPrefix:
		return fmt.Errorf("collector %q request.query_prefix is %d characters; it is what a server wants before the domain, such as \"-T dn,ace \", at most %d", c.Name, len(r.QueryPrefix), whoisMaxPrefix)
	case strings.ContainsFunc(r.QueryPrefix, func(c rune) bool { return c < 0x20 || c == 0x7f }):
		return fmt.Errorf("collector %q request.query_prefix has a control character; the question is one line, the prefix and the domain", c.Name)
	}
	return nil
}

// whoisKeyRules is what the configuration schema says of a whois
// collector's own keys.
func whoisKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"whois_server": {"description": "whois: the WHOIS server asked, host or host:port, port 43 when it names none. Left out, IANA's server is asked which server answers for the domain's top-level domain, and that one is asked. See docs/WHOIS.md#the-server-asked."},
		"query_prefix": {"maxLength": whoisMaxPrefix, "pattern": `^[^\x00-\x1f\x7f]*$`, "description": "whois: text written before the domain in the question, for a server that wants more than the name, such as \"-T dn,ace \" for DENIC; at most 64 characters, none of them a control character."},
	}
}

// Fetch performs one request of a whois collector.
func (whoisRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchWhois(ctx, target, c, overrides)
}

// CheckTarget checks a whois collector's target, which is a domain.
func (whoisRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseWhoisDomain(target)
	return err
}

// Label is the url label of a whois request: whois:// and the domain.
func (whoisRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	domain, err := parseWhoisDomain(target)
	if err != nil {
		return "", err
	}
	return "whois://" + domain, nil
}

// Method is the http_method label of a whois request.
func (whoisRequester) Method(*model.Collector, RequestOverrides) string { return whoisMethod }

// Display renders a whois target for logs and labels: as written, since it
// holds no credentials, when it is a domain at all.
func (whoisRequester) Display(target string) string {
	if _, err := parseWhoisDomain(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// parseWhoisDomain reads a target: a domain name of two labels or more, an
// internationalised one in its ASCII form, as every host is written.
func parseWhoisDomain(raw string) (string, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", ErrMissingTarget
	}
	if strings.Contains(target, "://") || strings.ContainsAny(target, "/?#@: \t") {
		return "", fmt.Errorf("target %q is not a domain; a whois target is the name alone, such as example.com", raw)
	}
	domain, err := canonicalHost(target)
	if err != nil {
		return "", err
	}
	if net.ParseIP(domain) != nil || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.Contains(domain, "..") {
		return "", fmt.Errorf("target %q is not a domain; a whois target is a name under a top-level domain, such as example.com", raw)
	}
	return domain, nil
}

// parseWhoisServer reads a server: host, host:port or [v6]:port, port 43
// when it names none.
func parseWhoisServer(raw string) (string, error) {
	server := strings.TrimSpace(raw)
	if server == "" || strings.Contains(server, "://") || strings.ContainsAny(server, "/?#@ \t") {
		return "", fmt.Errorf("%q is not a WHOIS server; write host or host:port, such as whois.nic.example", raw)
	}
	host, port := server, whoisDefaultPort
	if h, p, err := net.SplitHostPort(server); err == nil {
		host, port = h, p
	} else if strings.Contains(strings.Trim(server, "[]"), ":") && !strings.HasPrefix(server, "[") {
		return "", fmt.Errorf("%q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:43", raw)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("%q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return "", err
	}
	if canonical == "" {
		return "", fmt.Errorf("%q names no host", raw)
	}
	return net.JoinHostPort(canonical, port), nil
}

// whoisDocument is a domain's registration as the transforms read it. A
// date the answer does not hold, or holds in a way that could not be read,
// is null.
type whoisDocument struct {
	Domain string `json:"domain"`
	// Server is the server that answered, host:port, and ReferredBy the
	// server that named it, empty when request.whois_server did.
	Server     string `json:"server"`
	ReferredBy string `json:"referred_by"`
	// Registered is false when the server said it knows no such domain.
	Registered      bool     `json:"registered"`
	Registrar       string   `json:"registrar"`
	RegistrarIANAID string   `json:"registrar_iana_id"`
	Created         *string  `json:"created"`
	CreatedSeconds  *float64 `json:"created_timestamp_seconds"`
	Updated         *string  `json:"updated"`
	UpdatedSeconds  *float64 `json:"updated_timestamp_seconds"`
	Expires         *string  `json:"expires"`
	ExpiresSeconds  *float64 `json:"expires_timestamp_seconds"`
	// Status is the status values, each without the URL that may follow.
	Status      []string `json:"status"`
	NameServers []string `json:"name_servers"`
	DNSSEC      string   `json:"dnssec"`
	// Fields is every `key: value` line, by its key in lower case.
	Fields map[string][]string `json:"fields"`
	// Text is the answer as it came.
	Text            string  `json:"text"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// fetchWhois asks for a domain's registration and answers it as JSON.
func fetchWhois(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	domain, err := parseWhoisDomain(target)
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
		ctx, cancel = context.WithTimeout(ctx, whoisTimeout)
		defer cancel()
	}
	// The policy is asked of each server a question goes to, by its name
	// here and by the address its connection is made to.
	var guard *policyGuard
	if policy := policyOf(c); policy != nil {
		guard = &policyGuard{policy: policy, hosts: map[string]bool{}}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	limit := responseLimit(c)
	r := whoisRequest(c)
	doc := whoisDocument{Domain: domain}
	if r.Server != "" {
		if doc.Server, err = parseWhoisServer(r.Server); err != nil {
			return nil, err
		}
	} else {
		if doc.Server, err = whoisReferrals.server(ctx, guard, domain, limit); err != nil {
			return nil, err
		}
		doc.ReferredBy = whoisRootServer
	}
	text, err := whoisAsk(ctx, guard, doc.Server, r.QueryPrefix+domain, limit)
	if err != nil {
		return nil, err
	}
	doc.read(text)
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

// whoisAsk sends one question to server and reads the answer until the
// server closes, held to limit.
func whoisAsk(ctx context.Context, guard *policyGuard, server, question string, limit int64) (string, error) {
	at := traceRequestAt(ctx, whoisMethod, "whois://"+server+"/"+url.PathEscape(question), nil, "", false)
	text, err := whoisExchange(ctx, guard, server, question, limit)
	if err != nil {
		at.outcome("error: "+err.Error(), true)
		return "", err
	}
	at.outcome(strconv.Itoa(len(text))+" bytes", false)
	return text, nil
}

// whoisExchange is one question and its answer over a connection of its
// own, made through the target policy.
func whoisExchange(ctx context.Context, guard *policyGuard, server, question string, limit int64) (string, error) {
	if guard != nil {
		if err := guard.check(ctx, &url.URL{Scheme: "whois", Host: server}); err != nil {
			return "", err
		}
	}
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", server)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = fmt.Errorf("connecting to %s: %w", server, err)
		}
		return "", err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	// A scrape cancelled before its deadline ends the read too.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := io.WriteString(conn, question+"\r\n"); err != nil {
		return "", fmt.Errorf("sending the question to %s: %w", server, err)
	}
	answer, err := io.ReadAll(io.LimitReader(conn, limit+1))
	switch {
	case int64(len(answer)) > limit:
		return "", model.MarkError(model.Errorf("the answer of %s is larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", server, model.Size(limit)), model.ErrLimitExceeded)
	case err != nil && ctx.Err() != nil:
		return "", fmt.Errorf("reading the answer of %s: %w", server, ctx.Err())
	case err != nil:
		return "", fmt.Errorf("reading the answer of %s: %w", server, err)
	case len(strings.TrimSpace(string(answer))) == 0:
		return "", fmt.Errorf("%s closed the connection without an answer; a registry does so to an address that asked too often", server)
	}
	return strings.ToValidUTF8(string(answer), "�"), nil
}

// whoisReferralCache keeps which server IANA named for a top-level domain.
type whoisReferralCache struct {
	mu   sync.Mutex
	kept map[string]whoisReferral
}

type whoisReferral struct {
	server string
	at     time.Time
}

var whoisReferrals = &whoisReferralCache{}

// server is the WHOIS server of domain's top-level domain, as the root
// server names it: asked once, and again when the answer is a day old.
func (k *whoisReferralCache) server(ctx context.Context, guard *policyGuard, domain string, limit int64) (string, error) {
	tld := domain[strings.LastIndexByte(domain, '.')+1:]
	key := whoisRootServer + "\x00" + tld
	k.mu.Lock()
	kept, found := k.kept[key]
	k.mu.Unlock()
	if found && time.Since(kept.at) < whoisReferralTTL {
		// The policy is asked of a server every time it is used.
		return kept.server, nil
	}
	text, err := whoisAsk(ctx, guard, whoisRootServer, tld, limit)
	if err != nil {
		return "", err
	}
	fields, _ := whoisFields(text)
	named := slices.Concat(fields["refer"], fields["whois"])
	if len(named) == 0 {
		return "", fmt.Errorf("%s names no WHOIS server for .%s; set request.whois_server to the registry's, if it has one", whoisRootServer, tld)
	}
	server, err := parseWhoisServer(named[0])
	if err != nil {
		return "", fmt.Errorf("%s names the WHOIS server of .%s as %s: %w", whoisRootServer, tld, netShownLine(named[0]), err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.kept == nil || len(k.kept) >= whoisReferralsKept {
		k.kept = map[string]whoisReferral{}
	}
	k.kept[key] = whoisReferral{server: server, at: time.Now()}
	return server, nil
}

// netShownLine is text a server sent, as an error shows it: quoted, and no
// longer than a failure's line.
func netShownLine(text string) string {
	return strconv.Quote(model.CutTo(text, 200, false))
}

// whoisBracketed is a line that names its key in brackets, as JPRS writes.
var whoisBracketed = regexp.MustCompile(`^(?:[a-z]\. )?\[([^\]]{1,64})\]\s*(.*)$`)

// whoisFields reads the `key: value` lines of an answer, by key in lower
// case, and the keys in the order they came. A key whose value is on the
// lines under it, further in, has those lines for values, and so has a
// heading written without a colon; a field under either is under its own
// key and under the two together, as "registrar organization". Comment lines are
// passed over, and nothing is read past the line that says when the
// database was last updated, under which registries write their terms.
func whoisFields(text string) (fields map[string][]string, order []string) {
	fields = map[string][]string{}
	add := func(key, value string) {
		values, known := fields[key]
		if !known {
			if len(fields) >= whoisMaxFields {
				return
			}
			order = append(order, key)
		}
		if value != "" && len(values) < whoisMaxValues {
			values = append(values, value)
		}
		fields[key] = values
	}
	// open is the key whose values the lines further in are, and indent
	// how far in its own line was.
	open, indent := "", 0
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(nil, len(text)+1)
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), " \t\r")
		line := strings.TrimLeft(raw, " \t")
		in := len(raw) - len(line)
		switch {
		case line == "":
			open = ""
			continue
		case strings.HasPrefix(line, ">>>"):
			return fields, order
		case strings.HasPrefix(line, "%") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		}
		if m := whoisBracketed.FindStringSubmatch(line); m != nil {
			open = ""
			add(whoisKey(m[1]), strings.TrimSpace(m[2]))
			continue
		}
		key, value, isField := whoisCut(line)
		switch {
		case isField && value == "":
			add(key, "")
			open, indent = key, in
		case isField && open != "" && in > indent:
			// A field under a heading is the heading's too, as the
			// Organization under a Registrar is the registrar's.
			add(key, value)
			add(open+" "+key, value)
		case isField:
			add(key, value)
			open = ""
		case open != "" && in > indent:
			add(open, line)
		case whoisHeading(line):
			// A heading without a colon, as "Nameservers" over the lines
			// that name them.
			open, indent = whoisKey(line), in
		default:
			open = ""
		}
	}
	return fields, order
}

// whoisHeading says whether a line that is no field may be a heading the
// lines further in are under: a few words that start with a letter.
func whoisHeading(line string) bool {
	if len(line) > 40 || len(strings.Fields(line)) > 3 {
		return false
	}
	c := line[0]
	return (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') && !strings.ContainsAny(line, ".,;!?()")
}

// whoisCut cuts a line into its key and value at the first colon that ends
// a key: one followed by blanks or the line's end, after a key that starts
// with a letter and is no longer than a key is.
func whoisCut(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 || i > 64 || i+1 < len(line) && line[i+1] != ' ' && line[i+1] != '\t' {
		return "", "", false
	}
	if c := line[0]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return "", "", false
	}
	return whoisKey(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// whoisKey is a key as the document names it: in lower case, its blanks
// single, without the dots some registries lead to the colon with.
func whoisKey(key string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimRight(key, ". \t")), " "))
}

// The keys registries write a registration's facts under, each list in the
// order of preference: a registry's own date before the registrar's.
var (
	whoisExpiresKeys     = []string{"registry expiry date", "registrar registration expiration date", "expiration date", "expiry date", "expires", "expires on", "expire date", "expire", "expiry", "expiration time", "paid-till", "renewal date", "valid until", "domain expiration date", "record expires on", "有効期限"}
	whoisCreatedKeys     = []string{"creation date", "created", "created on", "created date", "registered on", "registered", "registered date", "registration date", "registration time", "domain registration date", "record created", "domain record activated", "登録年月日"}
	whoisUpdatedKeys     = []string{"updated date", "last updated", "last updated on", "last updated date", "last modified", "last-update", "last update", "modified", "changed", "domain record last updated", "最終更新"}
	whoisRegistrarKeys   = []string{"registrar", "sponsoring registrar", "registrar organization", "registrar name"}
	whoisStatusKeys      = []string{"domain status", "status", "state"}
	whoisNameServerKeys  = []string{"name server", "name servers", "nameserver", "nameservers", "nserver"}
	whoisNotFoundPhrases = []string{"no match", "not found", "no data found", "no entries found", "no object found", "nothing found", "no such domain", "is available", "is free", "status: free", "status: available", "not registered", "no information available"}
)

// read fills the document from an answer's text.
func (d *whoisDocument) read(text string) {
	d.Text = text
	fields, _ := whoisFields(text)
	d.Fields = fields
	first := func(keys []string) string {
		for _, key := range keys {
			if values := fields[key]; len(values) > 0 {
				return values[0]
			}
		}
		return ""
	}
	date := func(keys []string) (*string, *float64) {
		for _, key := range keys {
			for _, value := range fields[key] {
				if at, ok := parseWhoisDate(value); ok {
					written, seconds := at.UTC().Format(time.RFC3339), float64(at.Unix())
					return &written, &seconds
				}
			}
		}
		return nil, nil
	}
	d.Registrar = first(whoisRegistrarKeys)
	d.RegistrarIANAID = first([]string{"registrar iana id"})
	d.DNSSEC = first([]string{"dnssec"})
	d.Created, d.CreatedSeconds = date(whoisCreatedKeys)
	d.Updated, d.UpdatedSeconds = date(whoisUpdatedKeys)
	d.Expires, d.ExpiresSeconds = date(whoisExpiresKeys)
	d.Status, d.NameServers = []string{}, []string{}
	for _, key := range whoisStatusKeys {
		for _, value := range fields[key] {
			// "clientTransferProhibited https://icann.org/epp#..." is the
			// status and where it is explained.
			if status, _, _ := strings.Cut(value, " http"); status != "" && !slices.Contains(d.Status, status) {
				d.Status = append(d.Status, status)
			}
		}
		if len(d.Status) > 0 {
			break
		}
	}
	for _, key := range whoisNameServerKeys {
		for _, value := range fields[key] {
			// A name server may be followed by its addresses.
			if words := strings.Fields(value); len(words) > 0 {
				if name := strings.ToLower(strings.TrimSuffix(words[0], ".")); !slices.Contains(d.NameServers, name) {
					d.NameServers = append(d.NameServers, name)
				}
			}
		}
	}
	// A registration is one with a date, a registrar or name servers;
	// without any, the answer is asked whether it says there is none.
	d.Registered = true
	if d.Expires == nil && d.Created == nil && d.Registrar == "" && len(d.NameServers) == 0 {
		lower := strings.ToLower(text)
		for _, phrase := range whoisNotFoundPhrases {
			if strings.Contains(lower, phrase) {
				d.Registered = false
				break
			}
		}
	}
}

// whoisDateLayouts are the ways registries write a date, the day before
// the month only where the month is a name or the parts are dotted: a date
// of numbers and slashes or dashes that begins with the day reads as
// another date in half the world, and is not guessed at.
var whoisDateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999 Z0700",
	"2006-01-02 15:04:05.999999999 MST",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02 15:04:05",
	"2006/01/02",
	"2006.01.02 15:04:05",
	"2006.01.02",
	"2006. 01. 02.",
	"20060102",
	"02-Jan-2006 15:04:05 MST",
	"02-Jan-2006 15:04:05",
	"02-Jan-2006",
	"2-Jan-2006",
	"02.01.2006 15:04:05",
	"02.01.2006",
	"2 Jan 2006",
	"2 January 2006",
	"January 2 2006",
	"Jan 2 2006",
	"Mon Jan 2 15:04:05 MST 2006",
	"Mon Jan 2 2006",
}

// parseWhoisDate reads a date as a registry wrote it, in UTC when it names
// no zone: the whole value, then without what follows in brackets, then its
// first words alone, since a date is often followed by a remark.
func parseWhoisDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	tries := []string{value}
	if i := strings.IndexAny(value, "(["); i > 0 {
		tries = append(tries, strings.TrimSpace(value[:i]))
	}
	if words := strings.Fields(value); len(words) > 1 {
		tries = append(tries, strings.Join(words[:min(len(words), 3)], " "), strings.Join(words[:2], " "), words[0])
	}
	for _, text := range tries {
		for _, layout := range whoisDateLayouts {
			if at, err := time.Parse(layout, text); err == nil && at.Year() >= 1980 && at.Year() < 2200 {
				return at, true
			}
		}
	}
	return time.Time{}, false
}
