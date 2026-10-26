//go:build request_type_ldap

package fetch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The ldap request type searches an LDAP directory, OpenLDAP, Active
// Directory, 389 Directory Server or any other server of LDAPv3, and hands
// the transforms what its searches found as one JSON document.
//
//	request:
//	  type: ldap
//	  bind_dn: cn=exporter,ou=services,dc=example,dc=org  # optional: anonymous without it
//	  password_file: /etc/exporter/ldap-pass               # with bind_dn
//	  start_tls: true                                      # optional
//	  searches:
//	    - name: people
//	      base: ou=people,dc=example,dc=org
//	      filter: (objectClass=person)
//	      count_only: true
//
// The document is {"searches": {name: {"count": N, "references": R,
// "entries": [{"dn": ..., "attributes": {name: [value, ...]}}]}}}, entries
// left out of a count_only search's, which holds none of them.
//
// The probe's target is host, host:port, [v6]:port, or an ldap:// or
// ldaps:// URL of one, port 389, or 636 for ldaps://, when it is left out;
// ldaps://, or a tls block on a target that does not use start_tls, asks
// for TLS from the start, start_tls for StartTLS on a plaintext
// connection, and ldap:// with a tls block but no start_tls is refused, as
// redis:// with one is. The exporter dials the target through the target
// policy, as every type that reaches a server does, and makes one
// connection for each scrape, which is closed when the scrape is over, for
// redis's reasons (requesttype_redis.go): a scrape is a bind and a few
// searches at most seconds apart.
//
// The password is read from password_file at every scrape, as every
// credential file is, and never shown: a debug report shows BIND dn ***.
//
// The type is opt-in, built under its tag alone, and imports nothing
// outside the standard library: the client is the BER of ldapber.go, and
// the filters are parsed by ldapfilter.go.

// LDAPConfig is an ldap collector's own request keys: the DN a simple
// bind is made as, none for an anonymous bind, whether TLS is begun with
// the StartTLS operation on a plaintext connection, and the searches whose
// results the document holds.
type LDAPConfig struct {
	BindDN   string       `yaml:"bind_dn,omitempty"`
	StartTLS bool         `yaml:"start_tls,omitempty"`
	Searches []LDAPSearch `yaml:"searches,omitempty"`
}

// LDAPSearch is one entry of an ldap collector's request.searches: the name
// its result is put under in the document, where it searches, which
// entries it matches and what of them it reads. Scope "" is sub, Filter ""
// is (objectClass=*), no Attributes asks for the entries' DNs alone, and
// MaxEntries 0 is the default bound.
type LDAPSearch struct {
	Name       string   `yaml:"name"`
	Base       string   `yaml:"base,omitempty"`
	Scope      string   `yaml:"scope,omitempty"`
	Filter     string   `yaml:"filter,omitempty"`
	Attributes []string `yaml:"attributes,omitempty"`
	CountOnly  bool     `yaml:"count_only,omitempty"`
	MaxEntries int      `yaml:"max_entries,omitempty"`
}

// ldapSearchNamePattern is what request.searches[].name is written with: a
// name a jq path reaches as .searches.<name>.
const ldapSearchNamePattern = `^[A-Za-z_][A-Za-z0-9_]*$`

// ldapScopeNames are the values of request.searches[].scope.
var ldapScopeNames = []string{"base", "one", "sub"}

// ldapAttributePattern is what an entry of request.searches[].attributes
// is written with: an attribute's name or OID, with options after ; as in
// cn;lang-de, or * for every user attribute and + for every operational
// one. The exporter and the schema hold them to it alike.
const ldapAttributePattern = `^(\*|\+|[A-Za-z][A-Za-z0-9-]*(;[A-Za-z0-9-]+)*|[0-9]+(\.[0-9]+)+(;[A-Za-z0-9-]+)*)$`

// ldapRequest is an ldap collector's own request keys: the zero LDAPConfig
// of a collector built in code whose request has not been validated, which
// sets them (ValidateRequest).
func ldapRequest(c *model.Collector) LDAPConfig {
	own, _ := c.Request.Own.(LDAPConfig)
	return own
}

// withLDAPTargetAccount is withTargetAccount with a static target's own
// bind_dn, which comes only with its own password_file, in place of the
// collector's.
func withLDAPTargetAccount(c *model.Collector, o RequestOverrides) *model.Collector {
	x := withTargetAccount(c, o)
	if o.BindDN == "" {
		return x
	}
	if x == c {
		copied := *c
		x = &copied
	}
	own := ldapRequest(x)
	own.BindDN = o.BindDN
	x.Request.Own = own
	return x
}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeLDAP,
		Fields:       []string{"password_file", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       LDAPConfig{},
		KeyRules:     ldapKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "bind_dn", "password_file", "tls"},
		Requester:    ldapRequester{},
		JSONAnswer:   true,
		// ldap is said letter by letter.
		Article:    "an",
		TargetHint: func(*model.Collector) string { return "host, host:port or ldaps://host:port" },
		Templates:  ldapTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "the base or filter of its request.searches or its label values")
		},
		SchemaRule:         ldapSchemaRule,
		CheckTargetRequest: checkLDAPTargetRequest,
		Stage:              "ldap",
		CheckFiles:         checkLDAPFiles,
	})
	templateEscapers["ldap_dn"] = func(where, name, value string) (string, error) {
		written, err := ldapEscapeDNValue(value)
		if err != nil {
			return "", fmt.Errorf("%s: the value of %s: %w", where, name, err)
		}
		return written, nil
	}
	templateEscapers["ldap_filter"] = func(_, _, value string) (string, error) { return ldapEscapeFilterValue(value), nil }
}

// ldapRequester is the ldap request type's Requester.
type ldapRequester struct{}

// Validate checks an ldap collector's request and fills in its defaults.
func (ldapRequester) Validate(c *model.Collector) error {
	return validateLDAPRequest(c)
}

// Fetch performs one request of an ldap collector.
func (ldapRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchLDAP(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks an ldap collector's target.
func (ldapRequester) CheckTarget(c *model.Collector, target string, static bool) error {
	return checkLDAPTarget(c, target, static)
}

// Label is the url label of an ldap request.
func (ldapRequester) Label(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	return ldapLabel(target, c, overrides)
}

// Method is the http_method label of an ldap request.
func (ldapRequester) Method(*model.Collector, RequestOverrides) string { return ldapMethod }

// Display renders an ldap target for logs and labels.
func (ldapRequester) Display(target string) string {
	return ldapDisplay(target)
}

// The ports of a target that names none.
const (
	ldapDefaultPort  = "389"
	ldapsDefaultPort = "636"
)

// ldapTimeout bounds a scrape with no deadline of its own.
const ldapTimeout = 30 * time.Second

// ldapMethod is the http_method label of a scrape.
const ldapMethod = "SEARCH"

// DefaultLDAPMaxEntries is how many entries a search that is not count_only
// may match when its max_entries is left out; one more fails the scrape.
const DefaultLDAPMaxEntries = 1000

// ldapPageSize is how many entries a page of a search asks for. Active
// Directory answers at most 1000 a page whatever is asked; 500 keeps each
// message of a page small besides.
const ldapPageSize = 500

// ldapDefaultFilter is the filter of a search that sets none: every entry.
const ldapDefaultFilter = "(objectClass=*)"

// ldapStartTLSOID is the StartTLS extended operation's (RFC 4511 § 4.14).
const ldapStartTLSOID = "1.3.6.1.4.1.1466.20037"

var ldapSearchName = regexp.MustCompile(ldapSearchNamePattern)

// ldapScopes are the scopes' values in a SearchRequest, by name.
var ldapScopes = map[string]int64{"base": 0, "one": 1, "sub": 2}

func validateLDAPRequest(x *model.Collector) error {
	r := &x.Request
	own := ldapRequest(x)
	t := r.TLS
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	if t.ServerName != "" {
		if _, err := canonicalHost(t.ServerName); err != nil {
			return fmt.Errorf("collector %q request.tls.server_name %q: %w", x.Name, t.ServerName, err)
		}
	}
	if own.BindDN != "" {
		if HasPathParams(own.BindDN) {
			return fmt.Errorf("collector %q request.bind_dn %q has a placeholder; the account is used as it is written", x.Name, own.BindDN)
		}
		if strings.TrimSpace(own.BindDN) == "" {
			return fmt.Errorf("collector %q request.bind_dn is blanks alone; name the account's DN, or take the key out for an anonymous bind", x.Name)
		}
		if err := checkLDAPDN(own.BindDN); err != nil {
			return fmt.Errorf("collector %q request.bind_dn %q is not a DN: %w", x.Name, own.BindDN, err)
		}
		if r.PasswordFile == "" {
			return fmt.Errorf("collector %q sets request.bind_dn without request.password_file; a simple bind with a DN and no password is an unauthenticated bind, which a server takes without checking anything, so name the file of the account's password", x.Name)
		}
	}
	if r.PasswordFile != "" {
		if strings.TrimSpace(r.PasswordFile) == "" {
			return fmt.Errorf("collector %q request.password_file is blanks alone; name the file of the password, or take the key out", x.Name)
		}
		if own.BindDN == "" {
			return fmt.Errorf("collector %q sets request.password_file without request.bind_dn; a password is sent with the DN of its account, so name it in bind_dn", x.Name)
		}
	}
	if len(own.Searches) == 0 {
		return fmt.Errorf("collector %q has no request.searches; name at least one search, such as {name: people, base: \"ou=people,dc=example,dc=org\", filter: \"(objectClass=person)\"}", x.Name)
	}
	// The entries are written in place, in the list Own holds.
	names := map[string]bool{}
	for i := range own.Searches {
		if err := checkLDAPSearch(&own.Searches[i], i, names); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	}
	return nil
}

// checkLDAPSearch checks an entry of request.searches.
func checkLDAPSearch(s *LDAPSearch, i int, names map[string]bool) error {
	where := fmt.Sprintf("request.searches[%d]", i)
	if !ldapSearchName.MatchString(s.Name) {
		return fmt.Errorf("%s.name %q is not a name the document can hold the result under; write letters, digits and _, starting with a letter or _, as .searches.%s reaches it", where, s.Name, "people")
	}
	if names[s.Name] {
		return fmt.Errorf("request.searches names %s twice; each search's result is put under its own name", s.Name)
	}
	names[s.Name] = true
	marked, err := ldapMarked(templateField{where + ".base", s.Base, "ldap_dn"})
	if err != nil {
		return err
	}
	if err := checkLDAPDN(marked); err != nil {
		return fmt.Errorf("%s.base %q is not a DN: %w", where, s.Base, err)
	}
	if HasPathParams(s.Scope) {
		return fmt.Errorf("%s.scope %q has a placeholder; a placeholder may stand in base and filter alone", where, s.Scope)
	}
	if _, ok := ldapScopes[s.Scope]; !ok && s.Scope != "" {
		return fmt.Errorf("%s.scope %q is not base, one or sub", where, s.Scope)
	}
	filter := s.Filter
	if filter != "" {
		if filter, err = ldapMarked(templateField{where + ".filter", s.Filter, "ldap_filter"}); err != nil {
			return err
		}
		if _, err := parseLDAPFilter(filter); err != nil {
			return fmt.Errorf("%s.filter: %w", where, err)
		}
	}
	seen := map[string]bool{}
	for j, attribute := range s.Attributes {
		switch {
		case HasPathParams(attribute):
			return fmt.Errorf("%s.attributes[%d] %q has a placeholder; a placeholder may stand in base and filter alone", where, j, attribute)
		case !ldapAttributeName.MatchString(attribute):
			return fmt.Errorf("%s.attributes[%d] %q is not an attribute's name or OID, * or +", where, j, attribute)
		case seen[strings.ToLower(attribute)]:
			return fmt.Errorf("%s.attributes names %s twice", where, attribute)
		}
		seen[strings.ToLower(attribute)] = true
	}
	switch {
	case s.CountOnly && len(s.Attributes) > 0:
		return fmt.Errorf("%s (%s) sets count_only and attributes; a search that only counts its entries reads none of their attributes, so take attributes out", where, s.Name)
	case s.CountOnly && s.MaxEntries != 0:
		return fmt.Errorf("%s (%s) sets count_only and max_entries; a search that only counts holds no entry, and max_entries bounds the entries held, so take max_entries out", where, s.Name)
	case s.MaxEntries < 0:
		return fmt.Errorf("%s.max_entries must not be negative; leave it out for %d", where, DefaultLDAPMaxEntries)
	}
	return nil
}

// ldapSecurity is how a scrape's connection is secured.
type ldapSecurity int

const (
	ldapPlain ldapSecurity = iota
	ldapImplicitTLS
	ldapStartTLS
)

// ldapAddress is a target as a scrape reaches it: host as the policy
// matches it and TLS verifies it, the port, and the scheme it was written
// with, "" for none.
type ldapAddress struct {
	host, port, scheme string
}

// parseLDAPTarget reads a target: host, host:port, [v6]:port, or an ldap://
// or ldaps:// URL of one. Nothing else may come with it: the base is each
// search's, the account the collector's.
func parseLDAPTarget(raw string) (ldapAddress, error) {
	var a ldapAddress
	target := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(target, "ldaps://"):
		a.scheme, target = "ldaps", strings.TrimPrefix(target, "ldaps://")
	case strings.HasPrefix(target, "ldap://"):
		a.scheme, target = "ldap", strings.TrimPrefix(target, "ldap://")
	case strings.Contains(target, "://"):
		scheme, _, _ := strings.Cut(target, "://")
		return a, fmt.Errorf("target %q has the scheme %s://; an ldap target is host, host:port, ldap://host:port or ldaps://host:port", raw, scheme)
	}
	target = strings.TrimSuffix(target, "/")
	if strings.ContainsAny(target, "/?#@") {
		return a, fmt.Errorf("target %q has a path, query or user in it; an ldap target is only the host and its port, the base is each search's and the account request.bind_dn", raw)
	}
	host, port := target, ""
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
	} else if strings.Count(target, ":") == 1 {
		return a, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:389", raw)
	}
	if host == "" {
		return a, fmt.Errorf("target %q names no host; an ldap target is host or host:port", raw)
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return a, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
		}
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return a, err
	}
	a.host, a.port = canonical, port
	return a, nil
}

// security says how a scrape of a is secured: ldaps://, or a tls block on
// a target that does not use start_tls, is TLS from the start, an empty
// block as much as one with keys (model.TLSConfig.Set), so tls: {} never
// sends the bind's password in plaintext; start_tls
// is StartTLS on ldap:// or a bare host. ldap:// with a tls block and no
// start_tls, and ldaps:// with start_tls, contradict each other.
func (a ldapAddress) security(c *model.Collector) (ldapSecurity, error) {
	configured := c.Request.TLS != model.TLSConfig{}
	switch {
	case a.scheme == "ldaps" && ldapRequest(c).StartTLS:
		return 0, errors.New("target asks for TLS from the start with ldaps://, but the collector sets request.start_tls, which begins TLS on a plaintext connection; use ldap:// or a bare host:port with start_tls")
	case a.scheme == "ldaps":
		return ldapImplicitTLS, nil
	case ldapRequest(c).StartTLS:
		return ldapStartTLS, nil
	case a.scheme == "ldap" && configured:
		return 0, errors.New("target asks for plaintext with ldap://, but the collector sets request.tls; use ldaps://, a bare host:port, or set request.start_tls for StartTLS")
	case configured:
		return ldapImplicitTLS, nil
	}
	return ldapPlain, nil
}

// hostPort is what a scrape of a dials, port 636 for TLS from the start
// and 389 otherwise when the target names none.
func (a ldapAddress) hostPort(security ldapSecurity) string {
	port := a.port
	if port == "" {
		port = ldapDefaultPort
		if security == ldapImplicitTLS {
			port = ldapsDefaultPort
		}
	}
	return net.JoinHostPort(a.host, port)
}

// url is the url label of a scrape: ldap://host:port, or ldaps:// for TLS
// from the start. StartTLS keeps ldap://, the port being LDAP's own.
func (a ldapAddress) url(security ldapSecurity) string {
	scheme := "ldap"
	if security == ldapImplicitTLS {
		scheme = "ldaps"
	}
	return scheme + "://" + a.hostPort(security)
}

// checkLDAPTargetRequest checks a static target's own account and tls
// block, which replace its collector's for that target
// (withTargetAccount), as the collector's are checked: a bind_dn with the
// target's own password_file, since a DN without a password is an
// unauthenticated bind and the collector's password is not another
// account's, and a password_file alone only for a collector with a
// bind_dn, the password being sent with the DN of its account. No value of
// them takes a placeholder: a target's own values are used as written. An
// ldap:// target, which asks for plaintext, with a tls block of its own and
// no start_tls contradicts itself, as with the collector's.
func checkLDAPTargetRequest(c *model.Collector, t *model.StaticTarget) error {
	r := &t.Request
	for _, key := range []struct{ name, value string }{{"request.bind_dn", r.BindDN}, {"request.password_file", r.PasswordFile}} {
		if strings.Contains(key.value, "{{") {
			return fmt.Errorf("%s %q has a placeholder; a static target's account and credential files are used as they are written, with no probe to fill one, so write the value out", key.name, key.value)
		}
	}
	if r.BindDN != "" {
		if strings.TrimSpace(r.BindDN) == "" {
			return errors.New("request.bind_dn is blanks alone; name the account's DN, or take the key out")
		}
		if err := checkLDAPDN(r.BindDN); err != nil {
			return fmt.Errorf("request.bind_dn %q is not a DN: %w", r.BindDN, err)
		}
		if r.PasswordFile == "" {
			return errors.New("sets request.bind_dn without request.password_file; a simple bind with a DN and no password is an unauthenticated bind, which a server takes without checking anything, so name the file of the account's password beside it")
		}
	}
	if r.PasswordFile != "" {
		if strings.TrimSpace(r.PasswordFile) == "" {
			return errors.New("request.password_file is blanks alone; name the file of the password, or take the key out")
		}
		if r.BindDN == "" && ldapRequest(c).BindDN == "" {
			return fmt.Errorf("sets request.password_file without request.bind_dn, and collector %q binds anonymously; a password is sent with the DN of its account, so name it in bind_dn beside it", c.Name)
		}
	}
	if err := checkTargetTLS(t); err != nil {
		return err
	}
	if a, err := parseLDAPTarget(t.Target); err == nil && a.scheme == "ldap" && r.TLS != nil && !ldapRequest(c).StartTLS {
		return errors.New("target asks for plaintext with ldap://, but sets request.tls of its own; use ldaps://, a bare host:port, or set the collector's request.start_tls for StartTLS")
	}
	return nil
}

func checkLDAPTarget(c *model.Collector, target string, _ bool) error {
	a, err := parseLDAPTarget(target)
	if err != nil {
		return err
	}
	_, err = a.security(c)
	return err
}

func ldapLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	c = withLDAPTargetAccount(c, overrides)
	a, err := parseLDAPTarget(target)
	if err != nil {
		return "", err
	}
	security, err := a.security(c)
	if err != nil {
		return "", err
	}
	return a.url(security), nil
}

// ldapDisplay renders a target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func ldapDisplay(target string) string {
	if _, err := parseLDAPTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// ldapSearchResult is a search's result as the document holds it. Entries
// is nil for a count_only search, which leaves it out.
type ldapSearchResult struct {
	Count      int          `json:"count"`
	References int          `json:"references"`
	Entries    *[]ldapEntry `json:"entries,omitempty"`
}

// ldapDocument is a scrape as the transforms read it.
type ldapDocument struct {
	Searches map[string]*ldapSearchResult `json:"searches"`
}

// ldapCall is a search of a scrape, its placeholders filled and its filter
// encoded.
type ldapCall struct {
	search *LDAPSearch
	base   string
	filter string
	ber    []byte
}

// ldapCalls is what a scrape of c searches, each base and filter filled
// from the probe's parameters, and the filter parsed again: a value
// escaped as it is (ldapEscapeFilterValue) cannot change its structure,
// which the load checked, and the parse is what encodes it.
func ldapCalls(c *model.Collector, params map[string]string) ([]ldapCall, error) {
	searches := ldapRequest(c).Searches
	calls := make([]ldapCall, 0, len(searches))
	for i := range searches {
		s := &searches[i]
		where := fmt.Sprintf("request.searches[%d]", i)
		base, err := templateField{where + ".base", s.Base, "ldap_dn"}.render(params)
		if err != nil {
			return nil, err
		}
		filter := ldapDefaultFilter
		if s.Filter != "" {
			if filter, err = (templateField{where + ".filter", s.Filter, "ldap_filter"}).render(params); err != nil {
				return nil, err
			}
		}
		ber, err := parseFilledLDAPFilter(filter)
		if err != nil {
			return nil, fmt.Errorf("%s.filter: %w", where, err)
		}
		calls = append(calls, ldapCall{search: s, base: base, filter: filter, ber: ber})
	}
	return calls, nil
}

// fetchLDAP makes a scrape's connection, secures it, binds, and answers the
// document of the searches.
func fetchLDAP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	// A static target's own account and tls block, in place of the
	// collector's.
	c = withLDAPTargetAccount(c, overrides)
	address, err := parseLDAPTarget(target)
	if err != nil {
		return nil, err
	}
	security, err := address.security(c)
	if err != nil {
		return nil, err
	}
	calls, err := ldapCalls(c, overrides.Params)
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
		ctx, cancel = context.WithTimeout(ctx, ldapTimeout)
		defer cancel()
	}
	var password string
	if c.Request.PasswordFile != "" {
		if password, err = ReadCredentialFile(c.Request.PasswordFile); err != nil {
			return nil, fmt.Errorf("reading request.password_file: %w", err)
		}
		if password == "" {
			return nil, fmt.Errorf("request.password_file %s is empty; a simple bind with a DN and an empty password is an unauthenticated bind, which a server takes without checking anything, so it is never sent", c.Request.PasswordFile)
		}
	}
	var cfg *tls.Config
	if security != ldapPlain {
		if cfg, err = tlsConfig(c.Request.TLS); err != nil {
			return nil, fmt.Errorf("request.tls: %w", err)
		}
		if cfg.ServerName == "" {
			cfg.ServerName = address.host
		}
	}
	hostPort := address.hostPort(security)
	// The host's name now; the address the connection is made to when it
	// is made, as an http request's connections are checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "ldap", Host: hostPort}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	label := address.url(security)
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = ldapFailure(ctx, fmt.Errorf("connecting to %s: %w", hostPort, err))
		}
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	// Every read and write ends at the scrape's deadline, or as soon as it
	// is cancelled.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if security == ldapImplicitTLS {
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, ldapFailure(ctx, fmt.Errorf("TLS handshake with %s: %w", hostPort, err))
		}
		conn = tlsConn
	}
	session := newLDAPConn(conn, responseLimit(c))
	if security == ldapStartTLS {
		if err := ldapStartTLSOn(ctx, session, label, cfg, hostPort); err != nil {
			return nil, err
		}
	}
	if err := ldapBind(ctx, session, label, ldapRequest(c).BindDN, password); err != nil {
		return nil, err
	}
	doc := ldapDocument{Searches: map[string]*ldapSearchResult{}}
	for _, call := range calls {
		result, err := ldapSearch(ctx, session, label, call, deadline)
		if err != nil {
			return nil, err
		}
		doc.Searches[call.search.Name] = result
	}
	session.unbind()
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// The document is held to the limit the messages were: JSON writes a
	// control character as six bytes, \u0001, and a quote or backslash as
	// two, so messages within the limit can make a document past it.
	if limit := responseLimit(c); int64(len(body)) > limit {
		return nil, model.MarkError(model.Errorf("the document of the searches is %d bytes as JSON, larger than the response limit of %d bytes, the values' control characters, quotes and backslashes taking more as JSON escapes them; raise request.max_response_bytes or limits.max_response_bytes, or narrow the searches", len(body), model.Size(limit)), model.ErrLimitExceeded)
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

// ldapStartTLSOn sends StartTLS and, once the server agrees, makes the TLS
// handshake on the connection.
func ldapStartTLSOn(ctx context.Context, session *ldapConn, label string, cfg *tls.Config, hostPort string) error {
	traceRequest(ctx, "STARTTLS", label, http.Header{"Ldap-Operation": {"STARTTLS"}}, "", false)
	result, err := ldapOperation(session, berElement(ldapExtendedRequest, berString(ldapExtendedName, ldapStartTLSOID)), ldapExtendedResponse)
	if err == nil && result.code != 0 {
		err = result.err()
	}
	if err != nil {
		err = ldapFailure(ctx, fmt.Errorf("STARTTLS: %w", err))
		traceOutcome(ctx, "error: "+err.Error())
		return err
	}
	tlsConn := tls.Client(session.conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		err = ldapFailure(ctx, fmt.Errorf("STARTTLS: TLS handshake with %s: %w", hostPort, err))
		traceOutcome(ctx, "error: "+err.Error())
		return err
	}
	if err := session.startTLS(tlsConn); err != nil {
		err = fmt.Errorf("STARTTLS: %w", err)
		traceOutcome(ctx, "error: "+err.Error())
		return err
	}
	traceOutcome(ctx, "success, "+tls.VersionName(tlsConn.ConnectionState().Version))
	return nil
}

// ldapShownBind is a bind as a debug report and a failure show it: its DN,
// or anonymous, and its password as ***.
func ldapShownBind(dn string) string {
	if dn == "" {
		return "BIND anonymous"
	}
	return "BIND " + ldapShown(dn) + " ***"
}

// ldapBind makes a simple bind, as dn with password, or anonymous with
// neither, which also says the client speaks LDAPv3.
func ldapBind(ctx context.Context, session *ldapConn, label, dn, password string) error {
	shown := ldapShownBind(dn)
	traceRequest(ctx, "BIND", label, http.Header{"Ldap-Operation": {shown}}, "", false)
	op := berElement(ldapBindRequest, berInt(berInteger, 3), berString(berOctetString, dn), berString(ldapSimpleAuth, password))
	result, err := ldapOperation(session, op, ldapBindResponse)
	if err == nil && result.code != 0 {
		err = result.err()
	}
	if err != nil {
		err = ldapFailure(ctx, fmt.Errorf("%s: %w", shown, err))
		traceOutcome(ctx, "error: "+err.Error())
		return err
	}
	traceOutcome(ctx, "success")
	return nil
}

// ldapOperation sends an operation and reads the one response it has, of
// the tag given.
func ldapOperation(session *ldapConn, op []byte, response byte) (*ldapResult, error) {
	id, err := session.send(op)
	if err != nil {
		return nil, err
	}
	m, err := session.await(id)
	if err != nil {
		return nil, err
	}
	if m.op != response {
		return nil, fmt.Errorf("%w: the response's operation has the tag 0x%02x, not 0x%02x", errLDAPProtocol, m.op, response)
	}
	return decodeLDAPResult(m.content)
}

// ldapShownSearch is a search as a debug report and a failure show it.
func ldapShownSearch(call ldapCall) string {
	attributes := "1.1"
	if len(call.search.Attributes) > 0 {
		attributes = strings.Join(call.search.Attributes, ",")
	}
	return fmt.Sprintf("SEARCH %s base=%q scope=%s filter=%s attributes=%s", call.search.Name, ldapShown(call.base), ldapScopeOf(call.search), ldapShown(call.filter), attributes)
}

func ldapScopeOf(s *LDAPSearch) string {
	if s.Scope == "" {
		return "sub"
	}
	return s.Scope
}

// ldapSearch runs a search, a page at a time, and answers its result.
func ldapSearch(ctx context.Context, session *ldapConn, label string, call ldapCall, deadline time.Time) (*ldapSearchResult, error) {
	s := call.search
	shown := ldapShownSearch(call)
	attributes := s.Attributes
	if len(attributes) == 0 {
		// No attribute: the entries' DNs alone (RFC 4511 § 4.5.1.8).
		attributes = []string{"1.1"}
	}
	encoded := make([][]byte, len(attributes))
	for i, a := range attributes {
		encoded[i] = berString(berOctetString, a)
	}
	maxEntries := s.MaxEntries
	if maxEntries == 0 {
		maxEntries = DefaultLDAPMaxEntries
	}
	result := &ldapSearchResult{}
	var entries []ldapEntry
	cookie := ""
	for page := 1; ; page++ {
		pageCtx := context.WithValue(ctx, pageKey{}, page)
		traceRequest(pageCtx, "SEARCH", label, http.Header{"Ldap-Operation": {shown}}, "", false)
		// The server is told how long is left, so it gives up when the
		// scrape does.
		timeLimit := int64(math.Ceil(time.Until(deadline).Seconds()))
		op := berElement(ldapSearchRequest,
			berString(berOctetString, call.base),
			berInt(berEnumerated, ldapScopes[ldapScopeOf(s)]),
			berInt(berEnumerated, 0), // neverDerefAliases
			berInt(berInteger, 0),    // no size limit: max_entries is the exporter's to keep
			berInt(berInteger, max(timeLimit, 1)),
			berBool(false),
			call.ber,
			berElement(berSequence, encoded...),
		)
		before := result.Count
		var err error
		cookie, err = ldapSearchPage(session, op, cookie, s, result, &entries, maxEntries)
		if err != nil {
			err = ldapFailure(ctx, fmt.Errorf("%s: %w", shown, err))
			traceOutcome(pageCtx, "error: "+err.Error())
			return nil, err
		}
		outcome := fmt.Sprintf("success, %d entries", result.Count-before)
		if cookie != "" {
			outcome += ", more to come"
		}
		traceOutcome(pageCtx, outcome)
		if cookie == "" {
			break
		}
	}
	if !s.CountOnly {
		if entries == nil {
			entries = []ldapEntry{}
		}
		result.Entries = &entries
	}
	return result, nil
}

// ldapSearchPage sends a page of a search and reads its entries,
// references and end, adding them to result; it returns the cookie of the
// next page, "" when there is none.
func ldapSearchPage(session *ldapConn, op []byte, cookie string, s *LDAPSearch, result *ldapSearchResult, entries *[]ldapEntry, maxEntries int) (string, error) {
	id, err := session.send(op, ldapPagedControl(ldapPageSize, cookie))
	if err != nil {
		return "", err
	}
	for {
		m, err := session.await(id)
		if err != nil {
			return "", err
		}
		switch m.op {
		case ldapSearchEntry:
			entry, err := decodeLDAPEntry(m.content, !s.CountOnly)
			if err != nil {
				return "", err
			}
			result.Count++
			if !s.CountOnly {
				if result.Count > maxEntries {
					return "", fmt.Errorf("the search matches more than its max_entries, %d; narrow its base or filter, raise max_entries, or set count_only if the number is all the metrics need", maxEntries)
				}
				*entries = append(*entries, *entry)
			}
		case ldapSearchReference:
			result.References++
		case ldapSearchDone:
			done, err := decodeLDAPResult(m.content)
			if err != nil {
				return "", err
			}
			if done.code != 0 {
				return "", done.err()
			}
			return ldapPagedCookie(m.controls)
		default:
			return "", fmt.Errorf("%w: a response of the tag 0x%02x to a search", errLDAPProtocol, m.op)
		}
	}
}

// ldapFailure is the failure of a read, a write or a handshake: the
// scrape's deadline or cancellation when that is what ended it, since the
// connection's deadline is set to make it end. The connection's deadline is
// the scrape's, and its timer can fire before the context's own, so a
// connection that timed out is the deadline's failure whether or not the
// context says so yet.
func ldapFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%w)", context.DeadlineExceeded, err)
	}
	return err
}

// checkLDAPFiles reads the files a scrape reads, for --dry-run: the
// password file, and the tls block's when the collector sets one, or those
// a static target names of its own.
func checkLDAPFiles(c *model.Collector, t *model.StaticTarget) []string {
	return accountFileWarnings(c, t)
}

// ldapSchemaRule is what an ldap collector's request is held to beyond its keys,
// as the exporter holds it (validateLDAPRequest): searches, and
// bind_dn and password_file each only beside the other, since a simple
// bind with a DN and no password is an unauthenticated one and a password
// is sent with its account's DN.
func ldapSchemaRule() map[string]any {
	return map[string]any{
		"required": []string{"searches"},
		"allOf": []any{
			map[string]any{"if": SchemaWrittenKey("bind_dn"), "then": SchemaWrittenKey("password_file")},
			map[string]any{"if": SchemaWrittenKey("password_file"), "then": SchemaWrittenKey("bind_dn")},
		},
	}
}

// ldapTemplates are the base and the filter of an ldap collector's
// searches, each value escaped as its place escapes one (ldapfilter.go).
func ldapTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	var out []templateField
	for i, search := range ldapRequest(c).Searches {
		if HasPathParams(search.Base) {
			out = append(out, templateField{fmt.Sprintf("request.searches[%d].base", i), search.Base, "ldap_dn"})
		}
		if HasPathParams(search.Filter) {
			out = append(out, templateField{fmt.Sprintf("request.searches[%d].filter", i), search.Filter, "ldap_filter"})
		}
	}
	return out
}

// ldapKeyRules is what the configuration schema says of a ldap
// collector's own keys.
func ldapKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"bind_dn":                 {"not": SchemaOnlyBlanks(), "description": "ldap: the DN a simple bind is made as, such as cn=exporter,ou=services,dc=example,dc=org, with password_file, which it requires; left out, the bind is anonymous. ldap is in a build with OPT_IN_REQUEST_TYPES naming it alone. See docs/LDAP.md#binding."},
		"start_tls":               {"description": "ldap: begin TLS with the StartTLS operation on a plaintext connection, port 389 when the target names none, verifying the server as request.tls says. Not with an ldaps:// target. See docs/LDAP.md#tls."},
		"searches":                {"minItems": 1, "description": "ldap, required: the searches whose results the document holds under .searches.<name>, each {name, base, scope, filter, attributes, count_only, max_entries}. See docs/LDAP.md#searches."},
		"searches[]":              {"required": []string{"name"}},
		"searches[].name":         {"pattern": ldapSearchNamePattern, "description": "The name the result is put under in the document's searches: letters, digits and _, not starting with a digit, unique among the collector's searches."},
		"searches[].base":         {"description": "The DN the search starts at, such as ou=people,dc=example,dc=org; left out or \"\", the root DSE. May contain {{param_name}} placeholders in its attribute values, filled escaped as RFC 4514 escapes a value."},
		"searches[].scope":        {"enum": SchemaOptionalEnum(ldapScopeNames), "description": "base, the base entry alone; one, the entries right under it; sub, the base and everything under it. Defaults to sub."},
		"searches[].filter":       {"description": "An RFC 4515 filter in parentheses, such as (&(objectClass=user)(cn=a*)); defaults to (objectClass=*). May contain {{param_name}} placeholders in its values, filled escaped as RFC 4515 escapes a value, so a probe cannot change the filter's structure."},
		"searches[].attributes":   {"description": "The attributes each entry is read with, such as [cn, mail], * for every user attribute or + for every operational one; left out, the entries' DNs alone. Leave binary attributes, such as objectGUID or jpegPhoto, out: a value that is not UTF-8 fails the scrape."},
		"searches[].attributes[]": {"pattern": ldapAttributePattern, "description": "An attribute's name or OID, with options after ;, or * or +."},
		"searches[].count_only":   {"description": "Count the entries the search matches, holding none of them: the result has count and references and no entries. Not with attributes or max_entries."},
		"searches[].max_entries":  {"minimum": 0, "description": "The most entries a search that is not count_only may match; one more fails the scrape. Defaults to 1000."},
	}
}
