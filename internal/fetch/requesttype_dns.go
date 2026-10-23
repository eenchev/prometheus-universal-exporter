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
// The lookup is Go's own resolver (net.Resolver with PreferGo), which needs
// no library: it gives the records' data and tells a name that does not
// exist, or has no records of the type, from a failure, but gives neither
// the records' TTLs nor the answer's response code. It has no lookup of
// CNAME records alone, so that question the exporter sends itself
// (dnscname.go). A name not found is an answer, with no records and
// not_found true, so that a collector can say a record is missing; a
// timeout, a refusal or a server failure fails the scrape.
//
// This file imports nothing outside the standard library.

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeDNS,
		Fields:       []string{"record_type", "server", "transport", "allowed_targets", "denied_targets"},
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Validate:     validateDNSRequest,
		Fetch:        fetchDNS,
		CheckTarget: func(c *model.Collector, target string, _ bool) error {
			_, err := parseDNSTarget(c, target)
			return err
		},
		Label:   dnsLabel,
		Method:  func(*model.Collector, RequestOverrides) string { return dnsMethod },
		Display: dnsDisplay,
		Stage:   "dns",
	})
}

// dnsMethod is the http_method label of a lookup.
const dnsMethod = "QUERY"

// The transports of request.transport.
const (
	dnsUDP = "udp"
	dnsTCP = "tcp"
)

// dnsLookupTimeout bounds a lookup with no deadline of its own, as
// sshConnectTimeout does a connection.
const dnsLookupTimeout = 30 * time.Second

// dnsRecordType is the record type of c as its validation leaves it: upper
// case, A when left out. A collector not yet validated has it worked out
// the same way.
func dnsRecordType(c *model.Collector) string {
	if t := strings.ToUpper(strings.TrimSpace(c.Request.RecordType)); t != "" {
		return t
	}
	return "A"
}

func validateDNSRequest(x *model.Collector) error {
	r := &x.Request
	r.RecordType = dnsRecordType(x)
	if !slices.Contains(model.DNSRecordTypes, r.RecordType) {
		return fmt.Errorf("collector %q has unknown request.record_type %q; want one of %s, in any case", x.Name, r.RecordType, strings.Join(model.DNSRecordTypes, ", "))
	}
	if r.Server != "" {
		if _, err := ParseDNSServer(r.Server); err != nil {
			return fmt.Errorf("collector %q request.server %w", x.Name, err)
		}
	}
	if r.Transport != "" && r.Transport != dnsUDP && r.Transport != dnsTCP {
		return fmt.Errorf("collector %q request.transport is %q; want udp, the default, which falls back to TCP for an answer too long for UDP as every resolver does, or tcp", x.Name, r.Transport)
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
	if c.Request.Server == "" {
		return "dns:" + name + query
	}
	server, _ := ParseDNSServer(c.Request.Server)
	return "dns://" + server + "/" + name + query
}

// dnsDocument is the answer of a lookup as the transforms read it.
type dnsDocument struct {
	Name       string `json:"name"`
	RecordType string `json:"record_type"`
	// Server is request.server as host:port, "" for the system's resolver.
	Server          string      `json:"server"`
	Transport       string      `json:"transport"`
	NotFound        bool        `json:"not_found"`
	Count           int         `json:"count"`
	DurationSeconds float64     `json:"duration_seconds"`
	Answers         []dnsAnswer `json:"answers"`
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
}

// dnsResolver is the resolver of c: DNSResolver with request.server and
// request.transport.
func dnsResolver(c *model.Collector) *net.Resolver {
	server := ""
	if c.Request.Server != "" {
		server, _ = ParseDNSServer(c.Request.Server)
	}
	return DNSResolver(server, c.Request.Transport == dnsTCP)
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
	doc := dnsDocument{Name: name, RecordType: dnsRecordType(c), Transport: cmp.Or(c.Request.Transport, dnsUDP), Answers: []dnsAnswer{}}
	shownServer := "the system's resolver"
	if c.Request.Server != "" {
		doc.Server, _ = ParseDNSServer(c.Request.Server)
		shownServer = doc.Server
	}
	// The record type goes in a header of its own: a debug report masks the
	// values of a URL's query, where the url label has it.
	uri, _, _ := strings.Cut(dnsURI(c, name), "?")
	traceRequest(ctx, dnsMethod, uri, http.Header{"Dns-Type": {doc.RecordType}, "Dns-Server": {cmp.Or(doc.Server, "system")}, "Dns-Transport": {doc.Transport}}, "", false)
	answers, err := dnsLookup(ctx, c, doc.RecordType, name)
	doc.DurationSeconds = time.Since(start).Seconds()
	var dnsErr *net.DNSError
	switch {
	case err != nil && len(answers) > 0:
		// Records with malformed names are left out by the resolver, which
		// gives the others with the error.
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		doc.NotFound = true
	case err != nil:
		failure := dnsFailure(ctx, doc.RecordType, name, shownServer, err)
		traceOutcome(ctx, "error: "+failure.Error())
		return nil, failure
	}
	sortDNSAnswers(answers)
	if answers != nil {
		doc.Answers = answers
	}
	doc.Count = len(doc.Answers)
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	switch {
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

// dnsLookup asks resolver for the records of recordType of name. The name
// is asked as an absolute one, with a trailing dot, so that the search
// domains of the system's configuration are never appended to it.
func dnsLookup(ctx context.Context, c *model.Collector, recordType, name string) ([]dnsAnswer, error) {
	if recordType == "CNAME" {
		// Go's resolver has no lookup of CNAME records alone
		// (dnscname.go).
		return lookupCNAME(ctx, c, name)
	}
	resolver := dnsResolver(c)
	absolute := name + "."
	var answers []dnsAnswer
	switch recordType {
	case "A", "AAAA":
		network := "ip4"
		if recordType == "AAAA" {
			network = "ip6"
		}
		addrs, err := resolver.LookupNetIP(ctx, network, absolute)
		for _, addr := range addrs {
			answers = append(answers, dnsAnswer{Address: addr.Unmap().String()})
		}
		return answers, err
	case "MX":
		records, err := resolver.LookupMX(ctx, absolute)
		for _, mx := range records {
			answers = append(answers, dnsAnswer{Host: dnsName(mx.Host), Preference: &mx.Pref})
		}
		return answers, err
	case "NS":
		records, err := resolver.LookupNS(ctx, absolute)
		for _, ns := range records {
			answers = append(answers, dnsAnswer{Host: dnsName(ns.Host)})
		}
		return answers, err
	case "TXT":
		records, err := resolver.LookupTXT(ctx, absolute)
		for _, text := range records {
			answers = append(answers, dnsAnswer{Text: &text})
		}
		return answers, err
	case "SRV":
		_, records, err := resolver.LookupSRV(ctx, "", "", absolute)
		for _, srv := range records {
			answers = append(answers, dnsAnswer{Target: dnsName(srv.Target), Port: &srv.Port, Priority: &srv.Priority, Weight: &srv.Weight})
		}
		return answers, err
	case "PTR":
		names, err := resolver.LookupAddr(ctx, name)
		for _, n := range names {
			answers = append(answers, dnsAnswer{Name: dnsName(n)})
		}
		return answers, err
	}
	return nil, fmt.Errorf("unknown record type %q", recordType)
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
			strings.Compare(a.Address+a.Target+a.Host+a.Name+text(a.Text), b.Address+b.Target+b.Host+b.Name+text(b.Text)),
			cmp.Compare(value(a.Port), value(b.Port)),
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
