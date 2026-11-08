//go:build !select_request_types || request_type_dns

package fetch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The dns request type resolves one name and hands the answers to the
// transforms as a JSON document:
//
//	request:
//	  type: dns
//	  record_type: MX            # A when left out
//	  server: 10.0.0.53:53       # the system's resolver when left out
//	  transport: tcp             # udp when left out
//
// The probe's target is the name, such as example.com, or for record_type
// PTR the address whose names are asked for. The record type, the server
// and the transport are the collector's: a probe cannot send the exporter's
// questions to a server the configuration does not name.
//
// The question is the exporter's own (dnswire.go), not Go's resolver's,
// which gives neither the records' TTLs, nor the answer's response code
// and flags, nor records of SOA, CAA or DNSKEY. Every answer is the
// document, whatever its response code: NXDOMAIN, and NOERROR without
// records of the type, are not_found, and SERVFAIL or REFUSED is the
// document's rcode, so that a collector can say a record is missing or a
// server failing; only a server that does not answer, or answers what is
// no answer to the question, fails the scrape.
//
// This file imports nothing outside the standard library.

// DNSConfig is a dns collector's own request keys: the record type asked
// for, A when left out; the server asked, host:port, the system's resolver
// when left out; and udp or tcp.
type DNSConfig struct {
	RecordType string `yaml:"record_type,omitempty"`
	Server     string `yaml:"server,omitempty"`
	Transport  string `yaml:"transport,omitempty"`
}

// dnsRecordTypes are the values of a dns collector's request.record_type.
var dnsRecordTypes = []string{"A", "AAAA", "CNAME", "MX", "NS", "TXT", "SRV", "PTR", "SOA", "CAA", "DNSKEY", "RRSIG"}

// dnsRequest is a dns collector's own request keys: the zero DNSConfig of
// a collector built in code whose request has not been validated, which
// sets them (ValidateRequest).
func dnsRequest(c *model.Collector) DNSConfig {
	own, _ := c.Request.Own.(DNSConfig)
	return own
}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeDNS,
		Fields:       []string{"allowed_targets", "denied_targets", "tls"},
		Config:       DNSConfig{},
		KeyRules:     dnsKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    dnsRequester{},
		JSONAnswer:   true,
		// The server is request.server, or the system's resolver: the
		// target is the name alone.
		TargetHint: func(c *model.Collector) (string, string) {
			if dnsRecordType(c) == "PTR" {
				return "Address to resolve", "192.0.2.1 or 2001:db8::1"
			}
			return "Name to resolve", "example.com"
		},
		AnswerHint: "Paste the lookup's answer as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage:      "dns",
		CheckFiles: checkDNSFiles,
		// A defaults block's tls is a dns collector's for DNS over TLS
		// alone: a file of http and dns collectors shares one.
		DefaultApplies: func(c *model.Collector, key string) bool {
			return key != "tls" || dnsRequest(c).Transport == dnsTLS
		},
	})
}

// checkDNSFiles reads the files of a dns collector's tls block, as DNS
// over TLS reads them at each scrape (RequestType.CheckFiles): none
// without transport tls, nor of a static target, which sets no tls.
func checkDNSFiles(c *model.Collector, t *model.StaticTarget) []string {
	if t != nil || dnsRequest(c).Transport != dnsTLS {
		return nil
	}
	if _, err := tlsConfig(c.Request.TLS); err != nil {
		return []string{fmt.Sprintf("collector %q: request.tls: %v; a scrape would fail on it now (the files are read at each scrape, so one put in place later is used then)", c.Name, err)}
	}
	return nil
}

// dnsRequester is the dns request type's Requester.
type dnsRequester struct{}

// Validate checks a dns collector's request and fills in its defaults.
func (dnsRequester) Validate(c *model.Collector) error {
	return validateDNSRequest(c)
}

// Fetch performs one request of a dns collector.
func (dnsRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchDNS(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks a dns collector's target.
func (dnsRequester) CheckTarget(c *model.Collector, target string, _ bool) error {
	_, err := parseDNSTarget(c, target)
	return err
}

// Label is the url label of a dns request.
func (dnsRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	return dnsLabel(target, c, overrides)
}

// Method is the http_method label of a dns request.
func (dnsRequester) Method(*model.Collector, RequestOverrides) string { return dnsMethod }

// Display renders a dns target for logs and labels.
func (dnsRequester) Display(target string) string {
	return dnsDisplay(target)
}

// dnsMethod is the http_method label of a lookup.
const dnsMethod = "QUERY"

// The transports of request.transport.
const (
	dnsUDP = "udp"
	dnsTCP = "tcp"
	dnsTLS = "tls"
)

// dnsLookupTimeout bounds a lookup with no deadline of its own, as
// sshConnectTimeout does a connection.
const dnsLookupTimeout = 30 * time.Second

// dnsRecordType is the record type of c as its validation leaves it: upper
// case, A when left out. A collector not yet validated has it worked out
// the same way.
func dnsRecordType(c *model.Collector) string {
	if t := strings.ToUpper(strings.TrimSpace(dnsRequest(c).RecordType)); t != "" {
		return t
	}
	return "A"
}

func validateDNSRequest(x *model.Collector) error {
	r := dnsRequest(x)
	r.RecordType = dnsRecordType(x)
	x.Request.Own = r
	if !slices.Contains(dnsRecordTypes, r.RecordType) {
		return fmt.Errorf("collector %q has unknown request.record_type %q; want one of %s, in any case", x.Name, r.RecordType, strings.Join(dnsRecordTypes, ", "))
	}
	if r.Server != "" {
		if _, err := ParseDNSServer(r.Server); err != nil {
			return fmt.Errorf("collector %q request.server %w", x.Name, err)
		}
	}
	if r.Transport != "" && r.Transport != dnsUDP && r.Transport != dnsTCP && r.Transport != dnsTLS {
		return fmt.Errorf("collector %q request.transport is %q; want udp, the default, which falls back to TCP for an answer too long for UDP as every resolver does, tcp, or tls, DNS over TLS", x.Name, r.Transport)
	}
	switch {
	case r.Transport == dnsTLS && r.Server == "":
		return fmt.Errorf("collector %q sets request.transport tls without request.server; DNS over TLS is asked of a server the collector names, port 853 when it names none, not of the system's resolvers", x.Name)
	case r.Transport != dnsTLS && x.Request.TLS != (model.TLSConfig{}):
		return fmt.Errorf("collector %q sets request.tls without request.transport tls; the tls block is DNS over TLS's", x.Name)
	}
	if t := x.Request.TLS; (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	return nil
}

// parseDNSTarget reads a target of c: a DNS name, or for record_type PTR an
// IP address. A name is held to what canonicalHost holds a host to, and to
// the lengths of the DNS: at most 253 characters, each label from 1 to 63.
// It is returned in lower case without a trailing dot.
func parseDNSTarget(c *model.Collector, raw string) (string, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", ErrMissingTarget
	}
	if dnsRecordType(c) == "PTR" {
		addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(target, "["), "]"))
		if err != nil || addr.Zone() != "" {
			return "", fmt.Errorf("target %q is not an IP address; a collector with record_type PTR asks for the names of an address, such as 192.0.2.1 or 2001:db8::1", raw)
		}
		return addr.Unmap().String(), nil
	}
	if strings.ContainsAny(target, "/?#@:") {
		return "", fmt.Errorf("target %q is not a DNS name; a dns target is the name alone, such as example.com: the server is request.server, and the record type request.record_type", raw)
	}
	if _, literal := literalAddr(target); literal {
		return "", fmt.Errorf("target %q is an IP address; ask for the names of an address with record_type PTR", raw)
	}
	name, err := CanonicalDNSName(target)
	var refused *TargetRefusedError
	if err != nil && !errors.As(err, &refused) {
		return "", fmt.Errorf("target %w", err)
	}
	return name, err
}

// dnsDisplay renders a target for logs and labels: as written, since a dns
// target holds no credentials, when it is a name or an address.
func dnsDisplay(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.ContainsAny(target, "/?#@ \t") || strings.IndexFunc(target, func(r rune) bool { return notHostCharacter(r) && r != ':' && r != '[' && r != ']' }) >= 0 {
		return "<invalid target>"
	}
	return target
}

// dnsLabel is the url label of a lookup, as RFC 4501 writes a DNS URI:
// dns:name?type=MX with the system's resolver, dns://server/name?type=MX
// with request.server.
func dnsLabel(target string, c *model.Collector, _ RequestOverrides) (string, error) {
	name, err := parseDNSTarget(c, target)
	if err != nil {
		return "", err
	}
	return dnsURI(c, name), nil
}

func dnsURI(c *model.Collector, name string) string {
	query := "?type=" + dnsRecordType(c)
	if dnsRequest(c).Server == "" {
		return "dns:" + name + query
	}
	server := dnsServerOf(dnsRequest(c))
	return "dns://" + server + "/" + name + query
}

// dnsSignatureTimes places each RRSIG's inception and expiration in time,
// from now: the wire writes them as 32-bit serial numbers of seconds since
// 1970, compared as RFC 1982 does, each the time of that number nearest to
// now (RFC 4034 § 3.1.5); and how long each has left.
func dnsSignatureTimes(answers []dnsAnswer, now time.Time) {
	at := func(serial uint32) *int64 {
		t := now.Unix() + int64(int32(serial-uint32(now.Unix()))) //nolint:gosec // G115: serial arithmetic wraps on purpose
		return &t
	}
	for i := range answers {
		a := &answers[i]
		if a.TypeCovered == "" {
			continue
		}
		a.Inception, a.Expiration = at(a.inception), at(a.expiration)
		left := *a.Expiration - now.Unix()
		a.SecondsUntilExpiration = &left
	}
}

// dnsServerOf is r's server as host:port: port 853, DNS over TLS's, for
// transport tls when the server names no port, port 53 otherwise.
func dnsServerOf(r DNSConfig) string {
	server, _ := ParseDNSServer(r.Server)
	if _, _, err := net.SplitHostPort(strings.TrimSpace(r.Server)); err != nil && r.Transport == dnsTLS {
		host, _, _ := net.SplitHostPort(server)
		server = net.JoinHostPort(host, dnsTLSPort)
	}
	return server
}

// dnsTLSPort is DNS over TLS's port (RFC 7858).
const dnsTLSPort = "853"

// dnsDocument is the answer of a lookup as the transforms read it.
type dnsDocument struct {
	Name       string `json:"name"`
	RecordType string `json:"record_type"`
	// Server is request.server as host:port, "" for the system's resolver.
	Server    string `json:"server"`
	Transport string `json:"transport"`
	// Rcode is the answer's response code by name, such as NOERROR,
	// NXDOMAIN or SERVFAIL.
	Rcode    string `json:"rcode"`
	NotFound bool   `json:"not_found"`
	// The answer's flags: AA, RA and AD.
	Authoritative      bool        `json:"authoritative"`
	RecursionAvailable bool        `json:"recursion_available"`
	AuthenticatedData  bool        `json:"authenticated_data"`
	Count              int         `json:"count"`
	DurationSeconds    float64     `json:"duration_seconds"`
	Answers            []dnsAnswer `json:"answers"`
}

// dnsAnswer is one record, with the fields of its type alone.
type dnsAnswer struct {
	Address    string  `json:"address,omitempty"`
	Target     string  `json:"target,omitempty"`
	Host       string  `json:"host,omitempty"`
	Name       string  `json:"name,omitempty"`
	Text       *string `json:"text,omitempty"`
	Preference *uint16 `json:"preference,omitempty"`
	Priority   *uint16 `json:"priority,omitempty"`
	Weight     *uint16 `json:"weight,omitempty"`
	Port       *uint16 `json:"port,omitempty"`
	// SOA's: Host is its primary server, Mailbox its responsible person's
	// address as a name.
	Mailbox string  `json:"mailbox,omitempty"`
	Serial  *uint32 `json:"serial,omitempty"`
	Refresh *uint32 `json:"refresh,omitempty"`
	Retry   *uint32 `json:"retry,omitempty"`
	Expire  *uint32 `json:"expire,omitempty"`
	Minimum *uint32 `json:"minimum,omitempty"`
	// CAA's and DNSKEY's flags; CAA's tag and value; DNSKEY's protocol,
	// algorithm and key tag.
	Flags     *uint16 `json:"flags,omitempty"`
	Tag       string  `json:"tag,omitempty"`
	Value     *string `json:"value,omitempty"`
	Protocol  *uint8  `json:"protocol,omitempty"`
	Algorithm *uint8  `json:"algorithm,omitempty"`
	KeyTag    *uint16 `json:"key_tag,omitempty"`
	// RRSIG's: the type of the records it signs, the labels of its owner,
	// the TTL the records were signed with, when it was made and when it
	// expires, in seconds since 1970, how long it has left, and the zone
	// whose key signed it. expiration and inception are the wire's serial
	// numbers, placed in time when the document is made (dnsSignatureTimes).
	TypeCovered            string  `json:"type_covered,omitempty"`
	Labels                 *uint8  `json:"labels,omitempty"`
	OriginalTTL            *uint32 `json:"original_ttl,omitempty"`
	Inception              *int64  `json:"inception,omitempty"`
	Expiration             *int64  `json:"expiration,omitempty"`
	SecondsUntilExpiration *int64  `json:"seconds_until_expiration,omitempty"`
	Signer                 string  `json:"signer,omitempty"`
	TTL                    uint32  `json:"ttl"`

	expiration, inception uint32
}

// fetchDNS resolves a scrape's name and answers its records as JSON.
func fetchDNS(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	name, err := parseDNSTarget(c, target)
	if err != nil {
		return nil, err
	}
	if err := checkDNSPolicy(policyOf(c), name); err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dnsLookupTimeout)
		defer cancel()
	}
	r := dnsRequest(c)
	doc := dnsDocument{Name: name, RecordType: dnsRecordType(c), Transport: cmp.Or(r.Transport, dnsUDP), Answers: []dnsAnswer{}}
	shownServer := "the system's resolver"
	if r.Server != "" {
		doc.Server = dnsServerOf(r)
		shownServer = doc.Server
	}
	// The record type goes in a header of its own: a debug report masks the
	// values of a URL's query, where the url label has it.
	uri, _, _ := strings.Cut(dnsURI(c, name), "?")
	traceRequest(ctx, dnsMethod, uri, http.Header{"Dns-Type": {doc.RecordType}, "Dns-Server": {cmp.Or(doc.Server, "system")}, "Dns-Transport": {doc.Transport}}, "", false)
	// The record type is one of the set the configuration takes, no text
	// of the server's, and its span tells it.
	traceFacts(ctx, nameFact("dns.question.type", doc.RecordType))
	reply, server, err := lookupDNS(ctx, c, doc.RecordType, name)
	doc.DurationSeconds = time.Since(start).Seconds()
	if err != nil {
		if server != "" && doc.Server == "" {
			shownServer = server
		}
		failure := dnsFailure(ctx, doc.RecordType, name, shownServer, err)
		traceFailedOutcome(ctx, "error: "+failure.Error())
		return nil, failure
	}
	doc.Rcode, doc.Authoritative, doc.RecursionAvailable, doc.AuthenticatedData = reply.rcode, reply.authoritative, reply.recursionAvailable, reply.authentic
	doc.Answers = reply.answers
	dnsSignatureTimes(doc.Answers, time.Now())
	sortDNSAnswers(doc.Answers)
	doc.Count = len(doc.Answers)
	doc.NotFound = doc.Rcode == "NXDOMAIN" || doc.Rcode == "NOERROR" && doc.Count == 0
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// How many records came, the response code, and whether the name was
	// not found.
	traceFacts(ctx, intFact("dns.answer.count", doc.Count), nameFact("dns.response.code", doc.Rcode))
	if doc.NotFound {
		traceFacts(ctx, boolFact("dns.response.not_found", true))
	}
	switch {
	case doc.Rcode != "NOERROR" && doc.Rcode != "NXDOMAIN":
		traceOutcome(ctx, doc.Rcode)
	case doc.NotFound:
		traceOutcome(ctx, "not found")
	case doc.Count == 1:
		traceOutcome(ctx, "1 record")
	default:
		traceOutcome(ctx, fmt.Sprintf("%d records", doc.Count))
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

// dnsFailure words a failed lookup. The resolver's own error names the
// server of the system's configuration, which is not the one asked when
// request.server is set, so the server is named here instead; a lookup cut
// short by the scrape's deadline wraps the context's error.
func dnsFailure(ctx context.Context, recordType, name, server string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("resolving %s %s on %s: %w", recordType, name, server, ctx.Err())
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		reason := dnsErr.Err
		if dnsErr.IsTimeout {
			reason = "the server did not answer in time (" + reason + ")"
		}
		return fmt.Errorf("resolving %s %s on %s: %s", recordType, name, server, reason)
	}
	return fmt.Errorf("resolving %s %s on %s: %w", recordType, name, server, err)
}

// dnsName is a name of an answer as the document shows it: without the
// trailing dot the resolver gives it with.
func dnsName(name string) string {
	return strings.TrimSuffix(name, ".")
}

// sortDNSAnswers puts the records in an order of their own, by priority or
// preference first where the type has one, so that two lookups of the same
// records give the same document: the resolver shuffles records of the same
// priority, as RFC 2782 has a client do.
func sortDNSAnswers(answers []dnsAnswer) {
	value := func(p *uint16) int {
		if p == nil {
			return 0
		}
		return int(*p)
	}
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	slices.SortStableFunc(answers, func(a, b dnsAnswer) int {
		return cmp.Or(
			cmp.Compare(value(a.Preference), value(b.Preference)),
			cmp.Compare(value(a.Priority), value(b.Priority)),
			cmp.Compare(value(b.Weight), value(a.Weight)),
			strings.Compare(a.Address+a.Target+a.Host+a.Name+text(a.Text)+a.Tag+text(a.Value)+a.TypeCovered+a.Signer, b.Address+b.Target+b.Host+b.Name+text(b.Text)+b.Tag+text(b.Value)+b.TypeCovered+b.Signer),
			cmp.Compare(value(a.Port), value(b.Port)),
			cmp.Compare(value(a.KeyTag), value(b.KeyTag)),
		)
	})
}

// checkDNSPolicy holds a lookup to the collector's target lists. A name is
// matched by name alone: a lookup connects to nothing at the name, so what
// it resolves to is the answer, not a host reached, and allowed_targets
// that lists only networks allows no name. An address, a PTR collector's
// target, is matched by address, the cloud metadata ones refused as every
// collector refuses them.
func checkDNSPolicy(p *targetPolicy, name string) error {
	if p == nil {
		return nil
	}
	nameAllowed, err := p.checkName(name)
	if err != nil {
		return err
	}
	if addr, err := netip.ParseAddr(name); err == nil {
		return p.checkAddr(name, addr, nameAllowed)
	}
	if !nameAllowed && len(p.allowNames)+len(p.allowNets) > 0 {
		return &TargetRefusedError{Host: name, Reason: "it is not in request.allowed_targets; a dns collector's names are matched by name, never by the addresses they resolve to"}
	}
	return nil
}

// dnsKeyRules is what the configuration schema says of a dns collector's
// own keys.
func dnsKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"record_type": {"type": "string", "pattern": SchemaOptionalPattern(SchemaCaselessOneOf(dnsRecordTypes...)), "description": "dns: the record type asked for, in any case and with blanks around it or not: A, the default, AAAA, CNAME, MX, NS, TXT, SRV, SOA, CAA, DNSKEY, RRSIG, the name's DNSSEC signatures and when they expire, or PTR, whose target is an IP address. See docs/DNS.md."},
		"server":      {"not": SchemaOnlyBlanks(), "description": "dns: the server every question is sent to, host or host:port, port 53 when left out; the system's resolver, as /etc/resolv.conf names it, when the key is left out."},
		"transport":   {"enum": SchemaOptionalEnum([]string{"udp", "tcp", "tls"}), "description": "dns: udp, the default, which asks again over TCP for an answer too long for UDP, tcp, which asks over TCP alone, or tls, DNS over TLS (RFC 7858), of request.server alone, port 853 when it names none, verified with request.tls. See docs/DNS.md#dns-over-tls."},
	}
}
