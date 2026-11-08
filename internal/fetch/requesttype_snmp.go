//go:build request_type_snmp

package fetch

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The snmp request type reads an SNMP agent, as snmp_exporter does: a
// switch, a router, a UPS, a printer. A collector names the objects it
// reads by their numeric identifiers, each by a name of its own: get, the
// objects read one by one, sysUpTime.0 and the like, and walk, the tables
// read whole, each a set of columns whose rows are merged by their index,
// so that ifDescr comes beside ifHCInOctets in one row. The answer is a
// JSON document of them, which jq, yq or python read:
//
//	request:
//	  type: snmp
//	  snmp_version: 2            # 1, 2 (v2c) or 3
//	  community_file: /etc/exporter/snmp-community
//	  get:
//	    sysUpTime: 1.3.6.1.2.1.1.3.0
//	  walk:
//	    interfaces:
//	      ifDescr: 1.3.6.1.2.1.2.2.1.2
//	      ifHCInOctets: 1.3.6.1.2.1.31.1.1.1.6
//
// The probe's target is the agent, host or host:port, port 161 when it
// names none; the requests go over UDP, through the target policy, one
// socket for the scrape. A request unanswered within request_timeout is sent
// again, retries times. v1 walks with GetNext, v2c and v3 with GetBulk of
// max_repetitions; an object the agent does not have is left out of the
// document, and a v1 agent's noSuchName, which names it, has it taken out
// of the request and the rest asked again.
//
// SNMPv3 speaks the User-based Security Model (snmpusm.go): a scrape
// first asks the agent its engine ID, boots and time, which an
// unauthenticated Report answers, localizes the user's passwords to that
// engine, and signs and encrypts each request as security_level asks.
//
// MIBs are not read: the names are the collector's, the numbers the
// agent's, so the type needs neither net-snmp nor a MIB directory, and
// imports nothing outside the standard library. It is opt-in, built under
// its tag alone.

// SNMPConfig is an snmp collector's own request keys. The v3 user's name is
// request.username, of the common part.
type SNMPConfig struct {
	// Version is 1, 2 for v2c, or 3; 2 when left out.
	Version int `yaml:"snmp_version,omitempty"`
	// CommunityFile is the file of v1's and v2c's community.
	CommunityFile string `yaml:"community_file,omitempty"`
	// SecurityLevel, AuthProtocol, AuthPasswordFile, PrivProtocol,
	// PrivPasswordFile and ContextName are a v3 user's.
	SecurityLevel    string `yaml:"security_level,omitempty"`
	AuthProtocol     string `yaml:"auth_protocol,omitempty"`
	AuthPasswordFile string `yaml:"auth_password_file,omitempty"`
	PrivProtocol     string `yaml:"priv_protocol,omitempty"`
	PrivPasswordFile string `yaml:"priv_password_file,omitempty"`
	ContextName      string `yaml:"context_name,omitempty"`
	// Get are the objects read one by one, by name; Walk the tables read
	// whole, each its columns by name.
	Get  map[string]string            `yaml:"get,omitempty"`
	Walk map[string]map[string]string `yaml:"walk,omitempty"`
	// Hex are the names, of get or table.column of walk, whose values are
	// written as hexadecimal bytes whatever they hold.
	Hex []string `yaml:"hex,omitempty"`
	// MaxRepetitions is how many values a GetBulk asks for; RequestTimeout
	// how long a request waits for its answer, and Retries how many times
	// one unanswered is sent again; WalkLimit the most values of a column.
	MaxRepetitions int            `yaml:"max_repetitions,omitempty"`
	RequestTimeout model.Duration `yaml:"request_timeout,omitempty"`
	Retries        *int           `yaml:"retries,omitempty"`
	WalkLimit      int            `yaml:"walk_limit,omitempty"`
}

// snmpRequest is an snmp collector's own request keys: the zero SNMPConfig
// of a collector built in code whose request has not been validated, which
// sets them (ValidateRequest).
func snmpRequest(c *model.Collector) SNMPConfig {
	own, _ := c.Request.Own.(SNMPConfig)
	return own
}

// The defaults and bounds of an snmp collector's keys.
const (
	snmpDefaultPort           = "161"
	snmpDefaultMaxRepetitions = 25
	snmpMaxMaxRepetitions     = 255
	snmpDefaultRequestTimeout = time.Second
	snmpMinRequestTimeout     = 10 * time.Millisecond
	snmpDefaultRetries        = 1
	snmpMaxRetries            = 5
	snmpDefaultWalkLimit      = 10000
	snmpMaxWalkLimit          = 1000000
	snmpMaxGets               = 256
	snmpMaxTables             = 32
	snmpMaxColumns            = 64
	// snmpGetChunk is the most objects of one Get, which an agent's
	// answer of ordinary values fits in a datagram.
	snmpGetChunk = 32
	// snmpTimeout bounds a scrape with no deadline of its own.
	snmpTimeout = 30 * time.Second
	// snmpMaxMessage is the most bytes of a message the exporter takes,
	// which it tells the agent as v3's msgMaxSize.
	snmpMaxMessage = 65507
)

// The http_method labels of a scrape, and the methods of its requests in a
// debug report.
const (
	snmpMethodGet      = http.MethodGet
	snmpMethodWalk     = "WALK"
	snmpMethodDiscover = "DISCOVER"
)

// The security levels of v3.
const (
	snmpNoAuthNoPriv = "noAuthNoPriv"
	snmpAuthNoPriv   = "authNoPriv"
	snmpAuthPriv     = "authPriv"
)

// snmpNamePattern is a name of get, walk and a column: a word as jq reads
// it after a dot.
var snmpNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeSNMP,
		Fields:       []string{"username", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       SNMPConfig{},
		KeyRules:     snmpKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout", "community_file", "username", "auth_password_file", "priv_password_file", "request_timeout", "retries"},
		Requester:    snmpRequester{},
		JSONAnswer:   true,
		Article:      "an",
		TargetHint:   func(*model.Collector) (string, string) { return "Agent", "host or host:" + snmpDefaultPort },
		AnswerHint:   "Paste the JSON document the exporter makes of what the agent answered, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		SchemaRule:         snmpSchemaRule,
		CheckTargetRequest: checkSNMPTargetRequest,
		CheckFiles:         checkSNMPFiles,
		Stage:              "snmp",
	})
}

// snmpRequester is the snmp request type's Requester.
type snmpRequester struct{}

// Validate checks an snmp collector's request and fills in its defaults.
func (snmpRequester) Validate(c *model.Collector) error {
	r := snmpRequest(c)
	if r.Version == 0 {
		r.Version = 2
	}
	if err := validateSNMPAccount(c, r); err != nil {
		return err
	}
	if err := validateSNMPObjects(c.Name, r); err != nil {
		return err
	}
	if r.MaxRepetitions != 0 && r.Version == 1 {
		return fmt.Errorf("collector %q sets request.max_repetitions with snmp_version 1; SNMPv1 has no GetBulk, and walks with GetNext, one value a request", c.Name)
	}
	switch {
	case r.MaxRepetitions == 0:
		r.MaxRepetitions = snmpDefaultMaxRepetitions
	case r.MaxRepetitions < 0 || r.MaxRepetitions > snmpMaxMaxRepetitions:
		return fmt.Errorf("collector %q request.max_repetitions is %d; it is how many values a GetBulk asks for, from 1 to %d", c.Name, r.MaxRepetitions, snmpMaxMaxRepetitions)
	}
	switch {
	case r.RequestTimeout == 0:
		r.RequestTimeout = model.Duration(snmpDefaultRequestTimeout)
	case time.Duration(r.RequestTimeout) < snmpMinRequestTimeout:
		return fmt.Errorf("collector %q request.request_timeout is %s; it is how long a request waits for its answer before it is sent again, at least %s", c.Name, time.Duration(r.RequestTimeout), snmpMinRequestTimeout)
	}
	if r.Retries == nil {
		n := snmpDefaultRetries
		r.Retries = &n
	} else if *r.Retries < 0 || *r.Retries > snmpMaxRetries {
		return fmt.Errorf("collector %q request.retries is %d; it is how many times a request unanswered is sent again, from 0 to %d", c.Name, *r.Retries, snmpMaxRetries)
	}
	switch {
	case r.WalkLimit == 0:
		r.WalkLimit = snmpDefaultWalkLimit
	case r.WalkLimit < 0 || r.WalkLimit > snmpMaxWalkLimit:
		return fmt.Errorf("collector %q request.walk_limit is %d; it is the most values a column of a walk may have, from 1 to %d", c.Name, r.WalkLimit, snmpMaxWalkLimit)
	}
	c.Request.Own = r
	return nil
}

// validateSNMPAccount checks the keys of the account a collector reads as:
// v1's and v2c's community file, required, and none of v3's; v3's user,
// its security level, and the protocols and password files that level
// asks for, each refused beside a level that does not use it.
func validateSNMPAccount(c *model.Collector, r SNMPConfig) error {
	name := c.Name
	v3 := []struct {
		key string
		set bool
	}{
		{"username", c.Request.Username != ""},
		{"security_level", r.SecurityLevel != ""},
		{"auth_protocol", r.AuthProtocol != ""},
		{"auth_password_file", r.AuthPasswordFile != ""},
		{"priv_protocol", r.PrivProtocol != ""},
		{"priv_password_file", r.PrivPasswordFile != ""},
		{"context_name", r.ContextName != ""},
	}
	switch r.Version {
	case 1, 2:
		for _, key := range v3 {
			if key.set {
				return fmt.Errorf("collector %q sets request.%s with snmp_version %d; it is a key of SNMPv3's user, and v%s reads with request.community_file", name, key.key, r.Version, snmpVersionName(r.Version))
			}
		}
		if strings.TrimSpace(r.CommunityFile) == "" {
			return fmt.Errorf("collector %q has no request.community_file; SNMP v%s sends a community with each request, which the agent answers to, so name the file that holds it", name, snmpVersionName(r.Version))
		}
		return nil
	case 3:
	default:
		return fmt.Errorf("collector %q request.snmp_version is %d; it is 1, 2 for v2c, or 3", name, r.Version)
	}
	if r.CommunityFile != "" {
		return fmt.Errorf("collector %q sets request.community_file with snmp_version 3; SNMPv3 has no community, its user is request.username with its passwords", name)
	}
	if strings.TrimSpace(c.Request.Username) == "" {
		return fmt.Errorf("collector %q has no request.username; an SNMPv3 request is made as a user of the agent, so name it", name)
	}
	if strings.ContainsAny(c.Request.Username, "{}") {
		return fmt.Errorf("collector %q request.username %q has a placeholder or a brace; the user is written out, and no probe parameter sets it", name, c.Request.Username)
	}
	auth := r.AuthProtocol != "" || r.AuthPasswordFile != ""
	priv := r.PrivProtocol != "" || r.PrivPasswordFile != ""
	switch r.SecurityLevel {
	case snmpNoAuthNoPriv:
		if auth || priv {
			return fmt.Errorf("collector %q sets request.auth_protocol, auth_password_file, priv_protocol or priv_password_file with security_level noAuthNoPriv, which neither signs nor encrypts; set security_level authNoPriv or authPriv, or take them out", name)
		}
	case snmpAuthNoPriv, snmpAuthPriv:
		if _, ok := snmpAuths[r.AuthProtocol]; !ok {
			return fmt.Errorf("collector %q request.auth_protocol is %q; with security_level %s it is one of MD5, SHA, SHA224, SHA256, SHA384 and SHA512", name, r.AuthProtocol, r.SecurityLevel)
		}
		if strings.TrimSpace(r.AuthPasswordFile) == "" {
			return fmt.Errorf("collector %q has no request.auth_password_file; security_level %s signs each request with the user's authentication password, so name the file that holds it", name, r.SecurityLevel)
		}
		if r.SecurityLevel == snmpAuthNoPriv && priv {
			return fmt.Errorf("collector %q sets request.priv_protocol or priv_password_file with security_level authNoPriv, which does not encrypt; set security_level authPriv, or take them out", name)
		}
		if r.SecurityLevel == snmpAuthPriv {
			if _, ok := snmpPrivs[r.PrivProtocol]; !ok {
				return fmt.Errorf("collector %q request.priv_protocol is %q; with security_level authPriv it is one of DES, AES, AES192, AES256, AES192C and AES256C", name, r.PrivProtocol)
			}
			if strings.TrimSpace(r.PrivPasswordFile) == "" {
				return fmt.Errorf("collector %q has no request.priv_password_file; security_level authPriv encrypts each request with the user's privacy password, so name the file that holds it", name)
			}
		}
	case "":
		return fmt.Errorf("collector %q has no request.security_level; an SNMPv3 user is noAuthNoPriv, authNoPriv or authPriv, as the agent has it configured", name)
	default:
		return fmt.Errorf("collector %q request.security_level is %q; it is noAuthNoPriv, authNoPriv or authPriv", name, r.SecurityLevel)
	}
	return nil
}

// snmpVersionName is how a version is written: 1, 2c or 3.
func snmpVersionName(version int) string {
	if version == 2 {
		return "2c"
	}
	return strconv.Itoa(version)
}

// validateSNMPObjects checks get, walk and hex: at least one object, each
// name a word, each identifier one (parseSNMPOID), no object of get twice,
// a column not named index, which each row is keyed by, and every name of
// hex one of get or a column of walk.
func validateSNMPObjects(name string, r SNMPConfig) error {
	if len(r.Get) == 0 && len(r.Walk) == 0 {
		return fmt.Errorf("collector %q reads nothing; name the objects it reads under request.get, or the tables under request.walk", name)
	}
	if len(r.Get) > snmpMaxGets {
		return fmt.Errorf("collector %q request.get names %d objects, more than the %d a collector reads", name, len(r.Get), snmpMaxGets)
	}
	if len(r.Walk) > snmpMaxTables {
		return fmt.Errorf("collector %q request.walk names %d tables, more than the %d a collector walks", name, len(r.Walk), snmpMaxTables)
	}
	known := map[string]bool{}
	seen := map[string]string{}
	for _, object := range model.SortedKeys(r.Get) {
		if !snmpNamePattern.MatchString(object) {
			return fmt.Errorf("collector %q request.get names %q; a name is letters, digits and _, not beginning with a digit, as a jq rule reads it after a dot", name, object)
		}
		oid, err := parseSNMPOID(r.Get[object])
		if err != nil {
			return fmt.Errorf("collector %q request.get.%s %q: %w", name, object, r.Get[object], err)
		}
		if other, twice := seen[oid.String()]; twice {
			return fmt.Errorf("collector %q request.get names %s twice, as %s and %s; read it once", name, oid, other, object)
		}
		seen[oid.String()], known[object] = object, true
	}
	for _, table := range model.SortedKeys(r.Walk) {
		columns := r.Walk[table]
		switch {
		case !snmpNamePattern.MatchString(table):
			return fmt.Errorf("collector %q request.walk names %q; a name is letters, digits and _, not beginning with a digit, as a jq rule reads it after a dot", name, table)
		case len(columns) == 0:
			return fmt.Errorf("collector %q request.walk.%s names no column; name each column walked by the identifier of its object, such as ifDescr: 1.3.6.1.2.1.2.2.1.2", name, table)
		case len(columns) > snmpMaxColumns:
			return fmt.Errorf("collector %q request.walk.%s names %d columns, more than the %d of a table", name, table, len(columns), snmpMaxColumns)
		}
		for _, column := range model.SortedKeys(columns) {
			switch {
			case column == "index":
				return fmt.Errorf("collector %q request.walk.%s names a column index, which each row has of its own: the identifier of the row under the column; name the column otherwise", name, table)
			case !snmpNamePattern.MatchString(column):
				return fmt.Errorf("collector %q request.walk.%s names %q; a name is letters, digits and _, not beginning with a digit, as a jq rule reads it after a dot", name, table, column)
			}
			if _, err := parseSNMPOID(columns[column]); err != nil {
				return fmt.Errorf("collector %q request.walk.%s.%s %q: %w", name, table, column, columns[column], err)
			}
			known[table+"."+column] = true
		}
	}
	for _, h := range r.Hex {
		if !known[h] {
			return fmt.Errorf("collector %q request.hex names %q, which is no object of request.get nor table.column of request.walk", name, h)
		}
	}
	return nil
}

// Fetch performs one request of an snmp collector.
func (snmpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchSNMP(ctx, target, c, overrides)
}

// CheckTarget checks an snmp collector's target.
func (snmpRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseSNMPTarget(target)
	return err
}

// Label is the url label of an snmp request: snmp://host:port.
func (snmpRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseSNMPTarget(target)
	if err != nil {
		return "", err
	}
	return "snmp://" + address.hostPort, nil
}

// Method is the http_method label of an snmp request: WALK when the
// collector walks a table, GET when it only reads objects one by one.
func (snmpRequester) Method(c *model.Collector, _ RequestOverrides) string {
	if len(snmpRequest(c).Walk) > 0 {
		return snmpMethodWalk
	}
	return snmpMethodGet
}

// Display renders an snmp target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func (snmpRequester) Display(target string) string {
	if _, err := parseSNMPTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// snmpAddress is a target as a scrape reaches it.
type snmpAddress struct {
	host, hostPort string
}

// parseSNMPTarget reads a target: host, host:port or [v6]:port, port 161
// when it names none. Nothing else may come with it.
func parseSNMPTarget(raw string) (snmpAddress, error) {
	target := strings.TrimSpace(raw)
	switch {
	case target == "":
		return snmpAddress{}, ErrMissingTarget
	case strings.Contains(target, "://"):
		return snmpAddress{}, fmt.Errorf("target %q has a scheme; an snmp target is the agent, host or host:port, such as switch-1.example.com or 192.0.2.1:161", raw)
	case strings.ContainsAny(target, "/?#@"):
		return snmpAddress{}, fmt.Errorf("target %q has a path, query or user in it; an snmp target is only the agent's host and its port", raw)
	}
	host, port := target, snmpDefaultPort
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
	} else if strings.Count(target, ":") == 1 {
		return snmpAddress{}, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:161", raw)
	}
	if host == "" {
		return snmpAddress{}, fmt.Errorf("target %q names no host; an snmp target is host or host:port", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return snmpAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return snmpAddress{}, err
	}
	return snmpAddress{host: canonical, hostPort: net.JoinHostPort(canonical, port)}, nil
}

// checkSNMPTargetRequest checks a static target's own keys against its
// collector: a community for v1 and v2c, a user and its passwords for v3
// as its level uses them, each written out, and request_timeout and retries
// held as the collector's are.
func checkSNMPTargetRequest(c *model.Collector, t *model.StaticTarget) error {
	r, own := snmpRequest(c), &t.Request
	for _, key := range []struct{ name, value string }{
		{"community_file", own.CommunityFile}, {"username", own.Username}, {"auth_password_file", own.AuthPasswordFile}, {"priv_password_file", own.PrivPasswordFile},
	} {
		switch {
		case key.value == "":
			continue
		case strings.Contains(key.value, "{{"):
			return fmt.Errorf("request.%s %q has a placeholder; a static target's account is used as it is written, with no probe to fill one, so write the value out", key.name, key.value)
		case strings.TrimSpace(key.value) == "":
			return fmt.Errorf("request.%s is blanks alone; write it, or take the key out", key.name)
		}
		switch {
		case r.Version != 3 && key.name != "community_file":
			return fmt.Errorf("sets request.%s, and collector %q reads with SNMP v%s, whose account is its community; set request.community_file", key.name, c.Name, snmpVersionName(r.Version))
		case r.Version == 3 && key.name == "community_file":
			return fmt.Errorf("sets request.community_file, and collector %q reads with SNMPv3, which has no community; set request.username and its password files", c.Name)
		case key.name == "auth_password_file" && r.SecurityLevel == snmpNoAuthNoPriv:
			return fmt.Errorf("sets request.auth_password_file, and collector %q is of security_level noAuthNoPriv, which signs nothing", c.Name)
		case key.name == "priv_password_file" && r.SecurityLevel != snmpAuthPriv:
			return fmt.Errorf("sets request.priv_password_file, and collector %q is of security_level %s, which encrypts nothing", c.Name, r.SecurityLevel)
		}
	}
	if own.RequestTimeout != nil && *own.RequestTimeout != 0 && time.Duration(*own.RequestTimeout) < snmpMinRequestTimeout {
		return fmt.Errorf("request.request_timeout is %s; it is how long a request waits for its answer before it is sent again, at least %s", time.Duration(*own.RequestTimeout), snmpMinRequestTimeout)
	}
	if own.Retries != nil && (*own.Retries < 0 || *own.Retries > snmpMaxRetries) {
		return fmt.Errorf("request.retries is %d; it is how many times a request unanswered is sent again, from 0 to %d", *own.Retries, snmpMaxRetries)
	}
	return nil
}

// checkSNMPFiles reads, for --dry-run, the files of the account a scrape
// of c reads as, or of static target t's own in place of the collector's:
// each that cannot be read, is empty, or, of v3, holds a password shorter
// than 8 characters, is what a scrape would fail on now.
func checkSNMPFiles(c *model.Collector, t *model.StaticTarget) []string {
	r := snmpRequest(c)
	who := fmt.Sprintf("collector %q", c.Name)
	files := map[string]string{"community_file": r.CommunityFile, "auth_password_file": r.AuthPasswordFile, "priv_password_file": r.PrivPasswordFile}
	if t != nil {
		who = fmt.Sprintf("target %q", t.Name)
		files = map[string]string{"community_file": t.Request.CommunityFile, "auth_password_file": t.Request.AuthPasswordFile, "priv_password_file": t.Request.PrivPasswordFile}
	}
	var warnings []string
	for _, key := range []string{"community_file", "auth_password_file", "priv_password_file"} {
		if files[key] == "" {
			continue
		}
		if _, err := snmpReadSecret(key, files[key], key != "community_file"); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", who, err))
		}
	}
	return warnings
}

// snmpReadSecret reads the community or a password from its file, key
// the key that names it: what it holds, blanks around it taken off, which
// is not empty and, of a v3 password, at least 8 characters.
func snmpReadSecret(key, path string, password bool) ([]byte, error) {
	secret, err := ReadCredentialFile(path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("request.%s: %w", key, err)
	case secret == "":
		return nil, fmt.Errorf("request.%s %s is empty", key, path)
	case password && len(secret) < 8:
		return nil, fmt.Errorf("request.%s %s %w", key, path, errSNMPPasswordShort)
	}
	return []byte(secret), nil
}

// snmpDocument is a scrape as the transforms read it.
type snmpDocument struct {
	// Get are the objects of request.get the agent has, by name.
	Get map[string]snmpGot `json:"get,omitempty"`
	// Walk are the tables of request.walk, by name: each its rows in the
	// order of their index, each the index and the columns it has.
	Walk map[string][]map[string]any `json:"walk,omitempty"`
	// Version is 1, 2c or 3; Address the agent's address the requests went
	// to; EngineID, of v3, the agent's engine, in hexadecimal.
	Version  string `json:"version"`
	Address  string `json:"address"`
	EngineID string `json:"engine_id,omitempty"`
	// Requests is how many requests the scrape sent, each sent again
	// counted again.
	Requests        int     `json:"requests"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// snmpGot is an object of get: the name of its type and its value.
type snmpGot struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// fetchSNMP reads a scrape's agent and answers what it said as JSON.
func fetchSNMP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseSNMPTarget(target)
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
		ctx, cancel = context.WithTimeout(ctx, snmpTimeout)
		defer cancel()
	}
	// The host's name now; the address the datagrams go to when the socket
	// is connected, as an ntp request's is checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "snmp", Host: address.hostPort}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	s, err := newSNMPSession(c, overrides, address)
	if err != nil {
		return nil, err
	}
	conn, err := policyDialer((&net.Dialer{}).DialContext)(ctx, "udp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = fmt.Errorf("connecting to %s: %w", address.hostPort, err)
		}
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	s.conn = conn
	label := "snmp://" + address.hostPort
	doc := snmpDocument{Version: snmpVersionName(s.version), Address: conn.RemoteAddr().String()}
	if s.version == 3 {
		traceRequest(ctx, snmpMethodDiscover, label, http.Header{"Snmp-User": {s.user}}, "", false)
		if err := s.discover(ctx); err != nil {
			traceFailedOutcome(ctx, "error: "+err.Error())
			return nil, err
		}
		traceOutcome(ctx, fmt.Sprintf("engine %x, boots %d, time %d", s.engineID, s.boots, s.engineTime))
		doc.EngineID = hex.EncodeToString(s.engineID)
	}
	r := s.r
	hexNames := map[string]bool{}
	for _, h := range r.Hex {
		hexNames[h] = true
	}
	if len(r.Get) > 0 {
		names := model.SortedKeys(r.Get)
		shown := make([]string, len(names))
		for i, name := range names {
			shown[i] = name + "=" + r.Get[name]
		}
		traceRequest(ctx, snmpMethodGet, label, http.Header{"Snmp-Objects": {strings.Join(shown, ", ")}}, "", false)
		before := s.requests
		got, err := s.get(ctx, names, hexNames)
		if err != nil {
			traceFailedOutcome(ctx, "error: "+err.Error())
			return nil, err
		}
		traceOutcome(ctx, fmt.Sprintf("%d of %d objects, in %d requests", len(got), len(names), s.requests-before))
		doc.Get = got
	}
	if len(r.Walk) > 0 {
		doc.Walk = map[string][]map[string]any{}
		for _, table := range model.SortedKeys(r.Walk) {
			columns := model.SortedKeys(r.Walk[table])
			shown := make([]string, len(columns))
			for i, column := range columns {
				shown[i] = column + "=" + r.Walk[table][column]
			}
			traceRequest(ctx, snmpMethodWalk, label, http.Header{"Snmp-Table": {table}, "Snmp-Columns": {strings.Join(shown, ", ")}}, "", false)
			before := s.requests
			rows, err := s.walkTable(ctx, table, hexNames)
			if err != nil {
				traceFailedOutcome(ctx, "error: "+err.Error())
				return nil, err
			}
			traceOutcome(ctx, fmt.Sprintf("%d rows of %d columns, in %d requests", len(rows), len(columns), s.requests-before))
			doc.Walk[table] = rows
		}
	}
	doc.Requests, doc.DurationSeconds = s.requests, time.Since(start).Seconds()
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if limit := responseLimit(c); int64(len(body)) > limit {
		return nil, model.MarkError(model.Errorf("the JSON document of what %s answered is %d bytes, larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes, or walk fewer columns", address.hostPort, len(body), model.Size(limit)), model.ErrLimitExceeded)
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

// snmpSession is a scrape's requests to its agent: the socket, the
// account, and of v3 the engine and the keys localized to it.
type snmpSession struct {
	conn      net.Conn
	r         SNMPConfig
	address   snmpAddress
	version   int
	community []byte
	timeout   time.Duration
	retries   int
	requests  int
	// The v3 user, its protocols and passwords, and, once discovered, the
	// engine's ID, boots and time, when that time was read, and the keys
	// localized to it.
	user                 string
	auth                 *snmpAuth
	priv                 *snmpPriv
	authPass, privPass   []byte
	engineID             []byte
	boots, engineTime    uint32
	timeRead             time.Time
	authKey, privKey     []byte
	salt                 uint64
	resynced, discovered bool
}

// newSNMPSession reads the account a scrape reads as: the collector's, or
// a static target's own in its place, and its request_timeout and retries.
func newSNMPSession(c *model.Collector, o RequestOverrides, address snmpAddress) (*snmpSession, error) {
	r := snmpRequest(c)
	s := &snmpSession{r: r, address: address, version: r.Version, timeout: time.Duration(r.RequestTimeout)}
	if r.Retries != nil {
		s.retries = *r.Retries
	}
	if o.RequestTimeout != nil && *o.RequestTimeout != 0 {
		s.timeout = *o.RequestTimeout
	}
	if o.Retries != nil {
		s.retries = *o.Retries
	}
	pick := func(own, collector string) string {
		if own != "" {
			return own
		}
		return collector
	}
	var err error
	if s.version != 3 {
		s.community, err = snmpReadSecret("community_file", pick(o.CommunityFile, r.CommunityFile), false)
		return s, err
	}
	s.user = pick(o.Username, c.Request.Username)
	if r.SecurityLevel != snmpNoAuthNoPriv {
		auth := snmpAuths[r.AuthProtocol]
		s.auth = &auth
		if s.authPass, err = snmpReadSecret("auth_password_file", pick(o.AuthPasswordFile, r.AuthPasswordFile), true); err != nil {
			return nil, err
		}
	}
	if r.SecurityLevel == snmpAuthPriv {
		priv := snmpPrivs[r.PrivProtocol]
		s.priv = &priv
		if s.privPass, err = snmpReadSecret("priv_password_file", pick(o.PrivPasswordFile, r.PrivPasswordFile), true); err != nil {
			return nil, err
		}
	}
	var seed [8]byte
	_, _ = rand.Read(seed[:])
	s.salt = binary.BigEndian.Uint64(seed[:])
	return s, nil
}

// snmpPDU is a PDU an agent answered: its kind, request-id, error-status
// and error-index, and its variable bindings.
type snmpPDU struct {
	tag           byte
	requestID     int64
	status, index int64
	varbinds      []snmpVarbind
	// timeReport is a Report that the request was outside the engine's
	// time window, whose time the session now has.
	timeReport bool
}

// snmpVarbind is a variable binding: an object's identifier and its value,
// the tag of its type and its content.
type snmpVarbind struct {
	oid     snmpOID
	tag     byte
	content []byte
}

// errSNMPIgnore is a datagram that is not the answer waited for: another
// request's, sent again before its answer came, or one not of SNMP.
var errSNMPIgnore = errors.New("not the answer to this request")

// snmpRequestID is a request-id or a msgID: random, positive, so that an
// answer to another scrape's request, on a port the system gave this one
// since, is not taken for this one's.
func snmpRequestID() int64 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint32(b[:]) & 0x7fffffff)
}

// exchange sends a PDU of kind tag, its two integers after the request-id
// a and b, error-status and error-index or GetBulk's non-repeaters and
// max-repetitions, and returns the answer to it: sent again after
// request_timeout, retries times, and, of v3, once more after a Report that
// the request was outside the engine's time window, with the time the
// Report brings.
func (s *snmpSession) exchange(ctx context.Context, tag byte, a, b int, oids []snmpOID) (snmpPDU, error) {
	requestID := snmpRequestID()
	pdu := snmpPDUBytes(tag, requestID, a, b, oids)
	buf := make([]byte, snmpMaxMessage)
	// The msgIDs of v3 the request was sent with, each sending its own: an
	// answer to an earlier one that comes late is the answer still.
	var msgIDs []int64
	for attempt := 0; attempt <= s.retries; attempt++ {
		message, msgID, err := s.encode(pdu)
		if err != nil {
			return snmpPDU{}, err
		}
		msgIDs = append(msgIDs, msgID)
		s.requests++
		if _, err := s.conn.Write(message); err != nil {
			return snmpPDU{}, snmpFailure(ctx, fmt.Errorf("sending a request to %s: %w", s.address.hostPort, err))
		}
		wait := time.Now().Add(s.timeout)
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(wait) {
			wait = deadline
		}
		_ = s.conn.SetReadDeadline(wait)
		for {
			n, err := s.conn.Read(buf)
			if err != nil {
				if ctx.Err() != nil {
					return snmpPDU{}, snmpFailure(ctx, fmt.Errorf("waiting for the answer of %s, %d requests sent: %w", s.address.hostPort, attempt+1, err))
				}
				if errors.Is(err, os.ErrDeadlineExceeded) {
					// The scrape's deadline, which the socket's can reach
					// before the context's timer says so, ends the
					// requests as the deadline's.
					if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
						return snmpPDU{}, fmt.Errorf("waiting for the answer of %s, %d requests sent: %w", s.address.hostPort, attempt+1, context.DeadlineExceeded)
					}
					break
				}
				// An ICMP port unreachable, which a connected socket
				// reads as a refusal: nothing listens on the port.
				return snmpPDU{}, fmt.Errorf("reading the answer of %s: %w; no agent listens on the port", s.address.hostPort, err)
			}
			got, err := s.decode(buf[:n], msgIDs, requestID)
			if errors.Is(err, errSNMPIgnore) {
				continue
			}
			if err != nil {
				return snmpPDU{}, err
			}
			if got.timeReport {
				// The engine's time is the Report's now: the request is
				// sent again with it, once, not counted as a retry.
				if s.resynced {
					return snmpPDU{}, fmt.Errorf("the agent at %s said twice that the request was outside its time window (usmStatsNotInTimeWindows)", s.address.hostPort)
				}
				s.resynced = true
				attempt--
				break
			}
			return got, nil
		}
	}
	tries := s.retries + 1
	return snmpPDU{}, fmt.Errorf("no answer from %s to %d requests, each waited for %s; check the agent is up, request.community_file or the v3 user, and that it answers the exporter's address", s.address.hostPort, tries, s.timeout)
}

// snmpPDUBytes is a request PDU: its request-id, the two integers, and a
// variable binding of each identifier with a NULL value.
func snmpPDUBytes(tag byte, requestID int64, a, b int, oids []snmpOID) []byte {
	varbinds := make([][]byte, len(oids))
	for i, oid := range oids {
		varbinds[i] = snmpSeq(snmpSequence, snmpOIDBytes(oid), []byte{snmpNull, 0})
	}
	return snmpSeq(tag, snmpInt(requestID), snmpInt(int64(a)), snmpInt(int64(b)), snmpSeq(snmpSequence, varbinds...))
}

// encode makes the message of a PDU: v1's and v2c's of the community, or
// v3's, signed and encrypted as the user's level asks; and its msgID, 0
// but of v3.
func (s *snmpSession) encode(pdu []byte) ([]byte, int64, error) {
	if s.version != 3 {
		return snmpSeq(snmpSequence, snmpInt(int64(s.version-1)), snmpTLV(snmpOctetString, s.community), pdu), 0, nil
	}
	msgID := snmpRequestID()
	var flags byte = 0x04 // reportable
	if s.auth != nil {
		flags |= 0x01
	}
	if s.priv != nil {
		flags |= 0x02
	}
	// Discovery names no engine, no time and no user; after it, the time
	// is the engine's as discovery read it, and the seconds since.
	var engineTime uint32
	user := ""
	if s.discovered {
		engineTime = s.engineTime + uint32(min(time.Since(s.timeRead)/time.Second, math.MaxUint32/2)) //nolint:gosec // G115: bounded
		user = s.user
	}
	scoped := snmpSeq(snmpSequence, snmpTLV(snmpOctetString, s.engineID), snmpTLV(snmpOctetString, []byte(s.r.ContextName)), pdu)
	data := scoped
	var privParams []byte
	if s.priv != nil {
		s.salt++
		privParams = make([]byte, 8)
		if s.priv.des {
			binary.BigEndian.PutUint32(privParams, s.boots)
			binary.BigEndian.PutUint32(privParams[4:], uint32(s.salt)) //nolint:gosec // G115: a counter, wrapping
		} else {
			binary.BigEndian.PutUint64(privParams, s.salt)
		}
		sealed, err := snmpEncrypt(*s.priv, s.privKey, s.boots, engineTime, privParams, scoped)
		if err != nil {
			return nil, 0, err
		}
		data = snmpTLV(snmpOctetString, sealed)
	}
	var authParams []byte
	if s.auth != nil {
		authParams = make([]byte, s.auth.trunc)
	}
	message, at := snmpV3Message(msgID, flags, s.engineID, s.boots, engineTime, user, authParams, privParams, data)
	if s.auth != nil {
		copy(message[at:], snmpSign(*s.auth, s.authKey, message))
	}
	return message, msgID, nil
}

// snmpV3Message is a v3 message, and where in it its authentication
// parameters' content is, which the signature is written over.
func snmpV3Message(msgID int64, flags byte, engineID []byte, boots, engineTime uint32, user string, authParams, privParams, data []byte) ([]byte, int) {
	global := snmpSeq(snmpSequence, snmpInt(msgID), snmpInt(snmpMaxMessage), snmpTLV(snmpOctetString, []byte{flags}), snmpInt(3))
	head := slices.Concat(snmpTLV(snmpOctetString, engineID), snmpInt(int64(boots)), snmpInt(int64(engineTime)), snmpTLV(snmpOctetString, []byte(user)))
	authTLV := snmpTLV(snmpOctetString, authParams)
	usm := snmpSeq(snmpSequence, head, authTLV, snmpTLV(snmpOctetString, privParams))
	security := snmpTLV(snmpOctetString, usm)
	body := slices.Concat(snmpInt(3), global, security, data)
	message := snmpTLV(snmpSequence, body)
	// The authentication parameters' content is after the message's
	// header, the version, the global data, the security parameters'
	// header, the USM sequence's header, what comes before it in the
	// sequence, and its own header.
	at := (len(message) - len(body)) + len(snmpInt(3)) + len(global) + (len(security) - len(usm)) + (len(usm) - (len(head) + len(authTLV) + len(snmpTLV(snmpOctetString, privParams)))) + len(head) + (len(authTLV) - len(authParams))
	return message, at
}

// discover asks the engine its ID, boots and time (RFC 3414 § 4): an
// unauthenticated request of no engine, which the agent answers with a
// Report bringing them; then localizes the user's keys to it.
func (s *snmpSession) discover(ctx context.Context) error {
	auth, priv := s.auth, s.priv
	s.auth, s.priv = nil, nil
	_, err := s.exchange(ctx, pduGetRequest, 0, 0, nil)
	s.auth, s.priv = auth, priv
	if err != nil {
		return err
	}
	if len(s.engineID) == 0 {
		return fmt.Errorf("the agent at %s answered discovery with no engine ID", s.address.hostPort)
	}
	s.discovered = true
	if s.auth != nil {
		s.authKey = snmpLocalKey(*s.auth, s.authPass, s.engineID)
	}
	if s.priv != nil {
		s.privKey = snmpPrivKey(*s.auth, *s.priv, s.privPass, s.engineID)
	}
	return nil
}

// decode reads an answer to the request of msgID and requestID: the PDU,
// of v3 its signature checked and its scoped PDU decrypted, and the
// engine's ID, boots and time taken from discovery's Report and from a
// Report of the time window.
func (s *snmpSession) decode(msg []byte, msgIDs []int64, requestID int64) (snmpPDU, error) {
	start, end, err := snmpExpect(msg, 0, len(msg), snmpSequence, "the message")
	if err != nil {
		return snmpPDU{}, errSNMPIgnore
	}
	version, pos, err := snmpIntAt(msg, start, end, "the version")
	if err != nil {
		return snmpPDU{}, errSNMPIgnore
	}
	if s.version != 3 {
		if version != int64(s.version-1) {
			return snmpPDU{}, errSNMPIgnore
		}
		_, pos, err = snmpExpect(msg, pos, end, snmpOctetString, "the community")
		if err != nil {
			return snmpPDU{}, s.malformed(err)
		}
		pdu, err := snmpDecodePDU(msg, pos, end)
		if err != nil {
			return snmpPDU{}, s.malformed(err)
		}
		if pdu.requestID != requestID {
			return snmpPDU{}, errSNMPIgnore
		}
		if pdu.tag != pduResponse {
			return snmpPDU{}, fmt.Errorf("the agent at %s answered with a PDU of the tag 0x%02x, not a Response", s.address.hostPort, pdu.tag)
		}
		return pdu, nil
	}
	if version != 3 {
		return snmpPDU{}, errSNMPIgnore
	}
	return s.decodeV3(msg, pos, end, msgIDs, requestID)
}

// malformed is the failure of an answer that is not SNMP as it should be.
func (s *snmpSession) malformed(err error) error {
	return fmt.Errorf("the agent at %s answered a message that is not well formed: %w", s.address.hostPort, err)
}

// decodeV3 reads the rest of a v3 message, from pos.
func (s *snmpSession) decodeV3(msg []byte, pos, end int, msgIDs []int64, requestID int64) (snmpPDU, error) {
	gStart, gEnd, err := snmpExpect(msg, pos, end, snmpSequence, "the global data")
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	gotID, at, err := snmpIntAt(msg, gStart, gEnd, "the msgID")
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	if !slices.Contains(msgIDs, gotID) {
		return snmpPDU{}, errSNMPIgnore
	}
	_, at, err = snmpIntAt(msg, at, gEnd, "the msgMaxSize")
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	fStart, fEnd, err := snmpExpect(msg, at, gEnd, snmpOctetString, "the msgFlags")
	if err != nil || fEnd-fStart != 1 {
		return snmpPDU{}, s.malformed(fmt.Errorf("the msgFlags are not one byte: %w", err))
	}
	flags := msg[fStart]
	secStart, secEnd, err := snmpExpect(msg, gEnd, end, snmpOctetString, "the security parameters")
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	usmStart, usmEnd, err := snmpExpect(msg, secStart, secEnd, snmpSequence, "the USM parameters")
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	var fields [6][2]int
	at = usmStart
	for i, tag := range []byte{snmpOctetString, snmpInteger, snmpInteger, snmpOctetString, snmpOctetString, snmpOctetString} {
		fs, fe, err := snmpExpect(msg, at, usmEnd, tag, "a USM parameter")
		if err != nil {
			return snmpPDU{}, s.malformed(err)
		}
		fields[i], at = [2]int{fs, fe}, fe
	}
	engineID := msg[fields[0][0]:fields[0][1]]
	boots, err := snmpIntValue(msg[fields[1][0]:fields[1][1]])
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	engineTime, err := snmpIntValue(msg[fields[2][0]:fields[2][1]])
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	if s.discovered && string(engineID) != string(s.engineID) {
		return snmpPDU{}, fmt.Errorf("the agent at %s answered for the engine %x, not %x, which discovery found", s.address.hostPort, engineID, s.engineID)
	}
	if flags&0x01 != 0 {
		if s.auth == nil || s.authKey == nil {
			return snmpPDU{}, errSNMPIgnore
		}
		zeroed := append([]byte{}, msg...)
		clear(zeroed[fields[4][0]:fields[4][1]])
		if want := snmpSign(*s.auth, s.authKey, zeroed); !hmac.Equal(msg[fields[4][0]:fields[4][1]], want) {
			return snmpPDU{}, fmt.Errorf("the answer of %s is signed wrong; the agent's key for the user is not the one request.auth_password_file makes, or the answer was changed on the way", s.address.hostPort)
		}
	}
	var scoped []byte
	switch {
	case flags&0x02 != 0:
		if s.priv == nil || s.privKey == nil || flags&0x01 == 0 {
			return snmpPDU{}, s.malformed(errors.New("the answer is encrypted, and the request was not"))
		}
		sStart, sEnd, err := snmpExpect(msg, secEnd, end, snmpOctetString, "the encrypted PDU")
		if err != nil {
			return snmpPDU{}, s.malformed(err)
		}
		scoped, err = snmpDecrypt(*s.priv, s.privKey, uint32(boots), uint32(engineTime), msg[fields[5][0]:fields[5][1]], msg[sStart:sEnd]) //nolint:gosec // G115: the engine's counters, 31 bits
		if err != nil {
			return snmpPDU{}, fmt.Errorf("decrypting the answer of %s: %w", s.address.hostPort, err)
		}
	default:
		scoped = msg[secEnd:end]
	}
	sStart, sEnd, err := snmpExpect(scoped, 0, len(scoped), snmpSequence, "the scoped PDU")
	if err != nil {
		if flags&0x02 != 0 {
			return snmpPDU{}, fmt.Errorf("decrypting the answer of %s gave no scoped PDU; check request.priv_protocol and the password of request.priv_password_file", s.address.hostPort)
		}
		return snmpPDU{}, s.malformed(err)
	}
	_, at, err = snmpExpect(scoped, sStart, sEnd, snmpOctetString, "the context engine ID")
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	_, at, err = snmpExpect(scoped, at, sEnd, snmpOctetString, "the context name")
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	pdu, err := snmpDecodePDU(scoped, at, sEnd)
	if err != nil {
		return snmpPDU{}, s.malformed(err)
	}
	if pdu.tag == pduReport {
		var oid string
		if len(pdu.varbinds) > 0 {
			oid = pdu.varbinds[0].oid.String()
		}
		switch {
		case !s.discovered && oid == snmpUnknownEngineID:
			s.engineID, s.boots, s.engineTime, s.timeRead = append([]byte{}, engineID...), uint32(boots), uint32(engineTime), time.Now() //nolint:gosec // G115: the engine's counters, 31 bits
			return pdu, nil
		case s.discovered && oid == snmpNotInTimeWindow:
			s.boots, s.engineTime, s.timeRead = uint32(boots), uint32(engineTime), time.Now() //nolint:gosec // G115: the engine's counters, 31 bits
			pdu.timeReport = true
			return pdu, nil
		}
		if why, ok := snmpReports[oid]; ok {
			return snmpPDU{}, fmt.Errorf("the agent at %s refused the request: %s", s.address.hostPort, why)
		}
		return snmpPDU{}, fmt.Errorf("the agent at %s refused the request with a Report of %s", s.address.hostPort, oid)
	}
	if pdu.requestID != requestID {
		return snmpPDU{}, errSNMPIgnore
	}
	if pdu.tag != pduResponse {
		return snmpPDU{}, fmt.Errorf("the agent at %s answered with a PDU of the tag 0x%02x, not a Response", s.address.hostPort, pdu.tag)
	}
	if s.auth != nil && flags&0x01 == 0 {
		return snmpPDU{}, fmt.Errorf("the agent at %s answered without signing its answer, which security_level %s asks of it", s.address.hostPort, s.r.SecurityLevel)
	}
	if s.priv != nil && flags&0x02 == 0 {
		return snmpPDU{}, fmt.Errorf("the agent at %s answered without encrypting its answer, which security_level %s asks of it", s.address.hostPort, s.r.SecurityLevel)
	}
	return pdu, nil
}

// snmpDecodePDU reads the PDU at pos.
func snmpDecodePDU(buf []byte, pos, end int) (snmpPDU, error) {
	tag, start, stop, err := snmpNext(buf, pos, end)
	if err != nil {
		return snmpPDU{}, fmt.Errorf("reading the PDU: %w", err)
	}
	pdu := snmpPDU{tag: tag}
	var at int
	if pdu.requestID, at, err = snmpIntAt(buf, start, stop, "the request-id"); err != nil {
		return snmpPDU{}, err
	}
	if pdu.status, at, err = snmpIntAt(buf, at, stop, "the error-status"); err != nil {
		return snmpPDU{}, err
	}
	if pdu.index, at, err = snmpIntAt(buf, at, stop, "the error-index"); err != nil {
		return snmpPDU{}, err
	}
	vStart, vEnd, err := snmpExpect(buf, at, stop, snmpSequence, "the variable bindings")
	if err != nil {
		return snmpPDU{}, err
	}
	for at = vStart; at < vEnd; {
		bStart, bEnd, err := snmpExpect(buf, at, vEnd, snmpSequence, "a variable binding")
		if err != nil {
			return snmpPDU{}, err
		}
		oStart, oEnd, err := snmpExpect(buf, bStart, bEnd, snmpTagOID, "a variable binding's name")
		if err != nil {
			return snmpPDU{}, err
		}
		oid, err := snmpOIDValue(buf[oStart:oEnd])
		if err != nil {
			return snmpPDU{}, err
		}
		valueTag, cStart, cEnd, err := snmpNext(buf, oEnd, bEnd)
		if err != nil {
			return snmpPDU{}, fmt.Errorf("reading the value of %s: %w", oid, err)
		}
		pdu.varbinds = append(pdu.varbinds, snmpVarbind{oid: oid, tag: valueTag, content: buf[cStart:cEnd]})
		at = bEnd
	}
	return pdu, nil
}

// snmpErrorStatuses are the names of a PDU's error-status (RFC 3416 § 3).
var snmpErrorStatuses = []string{"noError", "tooBig", "noSuchName", "badValue", "readOnly", "genErr", "noAccess", "wrongType", "wrongLength", "wrongEncoding", "wrongValue", "noCreation", "inconsistentValue", "resourceUnavailable", "commitFailed", "undoFailed", "authorizationError", "notWritable", "inconsistentName"}

// snmpStatusName is an error-status by its name.
func snmpStatusName(status int64) string {
	if status >= 0 && status < int64(len(snmpErrorStatuses)) {
		return snmpErrorStatuses[status]
	}
	return "error-status " + strconv.FormatInt(status, 10)
}

// get reads the objects of request.get named names, snmpGetChunk to a
// request: those the agent has, as each is written (snmpValue). A v1
// agent's noSuchName takes the object it names out of the request, and
// the rest are asked again; a tooBig halves the request.
func (s *snmpSession) get(ctx context.Context, names []string, hexNames map[string]bool) (map[string]snmpGot, error) {
	out := map[string]snmpGot{}
	for len(names) > 0 {
		chunk := names[:min(snmpGetChunk, len(names))]
		names = names[len(chunk):]
		for len(chunk) > 0 {
			oids := make([]snmpOID, len(chunk))
			for i, name := range chunk {
				oids[i], _ = parseSNMPOID(s.r.Get[name])
			}
			pdu, err := s.exchange(ctx, pduGetRequest, 0, 0, oids)
			if err != nil {
				return nil, err
			}
			switch {
			case pdu.status == 2 && s.version == 1 && pdu.index >= 1 && pdu.index <= int64(len(chunk)):
				chunk = slices.Delete(slices.Clone(chunk), int(pdu.index-1), int(pdu.index))
				continue
			case pdu.status == 1 && len(chunk) > 1:
				half := len(chunk) / 2
				names = append(slices.Clone(chunk[half:]), names...)
				chunk = chunk[:half]
				continue
			case pdu.status != 0:
				return nil, fmt.Errorf("the agent at %s answered the Get of %s with %s", s.address.hostPort, snmpNamed(chunk, oids, pdu.index), snmpStatusName(pdu.status))
			case len(pdu.varbinds) != len(oids):
				return nil, fmt.Errorf("the agent at %s answered a Get of %d objects with %d values", s.address.hostPort, len(oids), len(pdu.varbinds))
			}
			for i, vb := range pdu.varbinds {
				if vb.oid.compare(oids[i]) != 0 {
					return nil, fmt.Errorf("the agent at %s answered the Get of %s with %s in its place", s.address.hostPort, oids[i], vb.oid)
				}
				typeName, value, ok, err := snmpValue(vb, hexNames[chunk[i]])
				if err != nil {
					return nil, fmt.Errorf("the value of %s (%s) from %s: %w", chunk[i], oids[i], s.address.hostPort, err)
				}
				if ok {
					out[chunk[i]] = snmpGot{Type: typeName, Value: value}
				}
			}
			chunk = nil
		}
	}
	return out, nil
}

// snmpNamed names the object an error-index points at, or every object of
// a request when it points at none.
func snmpNamed(names []string, oids []snmpOID, index int64) string {
	if index >= 1 && index <= int64(len(names)) {
		return fmt.Sprintf("%s (%s)", names[index-1], oids[index-1])
	}
	return strings.Join(names, ", ")
}

// walkTable walks each column of a table and merges the columns into rows
// by their index, in the order of the index.
func (s *snmpSession) walkTable(ctx context.Context, table string, hexNames map[string]bool) ([]map[string]any, error) {
	rows := map[string]map[string]any{}
	indexes := map[string]snmpOID{}
	columns := s.r.Walk[table]
	for _, column := range model.SortedKeys(columns) {
		root, _ := parseSNMPOID(columns[column])
		values, err := s.walk(ctx, root, table+"."+column)
		if err != nil {
			return nil, err
		}
		for _, vb := range values {
			index := vb.oid[len(root):]
			key := index.String()
			_, value, ok, err := snmpValue(vb, hexNames[table+"."+column])
			if err != nil {
				return nil, fmt.Errorf("the value of %s.%s at %s from %s: %w", table, column, vb.oid, s.address.hostPort, err)
			}
			if !ok {
				continue
			}
			row, found := rows[key]
			if !found {
				row = map[string]any{"index": key}
				rows[key], indexes[key] = row, index
			}
			row[column] = value
		}
	}
	keys := model.SortedKeys(rows)
	slices.SortFunc(keys, func(a, b string) int { return indexes[a].compare(indexes[b]) })
	out := make([]map[string]any, len(keys))
	for i, key := range keys {
		out[i] = rows[key]
	}
	return out, nil
}

// walk reads the values under root: v1 with GetNext, one a request, until
// noSuchName; v2c and v3 with GetBulk of max_repetitions, halved on a
// tooBig, until a value is past root or endOfMibView. Each value is past
// the one before it, as a walk of an agent that loops is not; and there are
// at most walk_limit of them.
func (s *snmpSession) walk(ctx context.Context, root snmpOID, name string) ([]snmpVarbind, error) {
	var out []snmpVarbind
	last := root
	repetitions := s.r.MaxRepetitions
	for {
		var pdu snmpPDU
		var err error
		if s.version == 1 {
			pdu, err = s.exchange(ctx, pduGetNextRequest, 0, 0, []snmpOID{last})
		} else {
			pdu, err = s.exchange(ctx, pduGetBulkRequest, 0, repetitions, []snmpOID{last})
		}
		if err != nil {
			return nil, err
		}
		switch {
		case s.version == 1 && pdu.status == 2:
			return out, nil
		case pdu.status == 1 && repetitions > 1:
			repetitions /= 2
			continue
		case pdu.status != 0:
			return nil, fmt.Errorf("the agent at %s answered the walk of %s (%s) with %s", s.address.hostPort, name, root, snmpStatusName(pdu.status))
		case len(pdu.varbinds) == 0:
			return nil, fmt.Errorf("the agent at %s answered the walk of %s (%s) with no value", s.address.hostPort, name, root)
		}
		for _, vb := range pdu.varbinds {
			if vb.tag == snmpEndOfMibView || !vb.oid.under(root) {
				return out, nil
			}
			if vb.oid.compare(last) <= 0 {
				return nil, fmt.Errorf("the agent at %s answered the walk of %s with %s after %s, which is not past it; the agent's walk would go round", s.address.hostPort, name, vb.oid, last)
			}
			out = append(out, vb)
			if len(out) > s.r.WalkLimit {
				return nil, model.MarkError(model.Errorf("the walk of %s (%s) at %s has more than request.walk_limit's %d values; raise it, or walk a narrower column", name, root, s.address.hostPort, model.Size(s.r.WalkLimit)), model.ErrLimitExceeded)
			}
			last = vb.oid
		}
	}
}

// snmpValue is a value as the document writes it, the name of its type
// and whether there is one: an integer of any type as a number, a string
// as text when it is printable UTF-8 and as hexadecimal bytes when it is
// not, or when hex, an identifier and an address in dotted form, an
// Opaque of a float as the number, and nothing of an exception, an object
// the agent does not have.
func snmpValue(vb snmpVarbind, asHex bool) (string, any, bool, error) {
	c := vb.content
	switch vb.tag {
	case snmpInteger:
		v, err := snmpIntValue(c)
		return "Integer", json.Number(strconv.FormatInt(v, 10)), true, err
	case snmpCounter32, snmpGauge32, snmpTimeTicks, snmpUinteger32, snmpCounter64:
		v, err := snmpUintValue(c)
		return map[byte]string{snmpCounter32: "Counter32", snmpGauge32: "Gauge32", snmpTimeTicks: "TimeTicks", snmpUinteger32: "Uinteger32", snmpCounter64: "Counter64"}[vb.tag], json.Number(strconv.FormatUint(v, 10)), true, err
	case snmpOctetString:
		if !asHex && snmpPrintable(c) {
			return "OctetString", string(c), true, nil
		}
		return "OctetString", snmpHex(c), true, nil
	case snmpTagOID:
		oid, err := snmpOIDValue(c)
		if err != nil {
			return "", nil, false, err
		}
		return "OID", oid.String(), true, nil
	case snmpIPAddress:
		if len(c) != 4 {
			return "", nil, false, fmt.Errorf("an IpAddress of %d bytes", len(c))
		}
		return "IpAddress", net.IP(c).String(), true, nil
	case snmpOpaque:
		if f, ok := snmpOpaqueFloat(c); ok {
			return "Opaque", f, true, nil
		}
		return "Opaque", snmpHex(c), true, nil
	case snmpNsapAddr:
		return "NsapAddress", snmpHex(c), true, nil
	case snmpNull:
		return "Null", nil, true, nil
	case snmpNoSuchObject, snmpNoSuchInstance, snmpEndOfMibView:
		return "", nil, false, nil
	}
	return "", nil, false, fmt.Errorf("a value of the tag 0x%02x, which is no SNMP type", vb.tag)
}

// snmpPrintable says whether bytes are text: UTF-8 of no control
// character but a tab and line ends.
func snmpPrintable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// snmpHex is bytes in hexadecimal, two digits each, by colons, as a MAC
// address is written: 00:1a:2b:3c:4d:5e.
func snmpHex(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = hex.EncodeToString([]byte{x})
	}
	return strings.Join(parts, ":")
}

// snmpOpaqueFloat reads an Opaque that holds a float or a double, as
// net-snmp's agents write them (tag 0x9f78 and 0x9f79, of
// draft-perkins-opaque-01): a finite number, which JSON can write.
func snmpOpaqueFloat(c []byte) (float64, bool) {
	if len(c) < 3 || c[0] != 0x9f {
		return 0, false
	}
	var f float64
	switch {
	case c[1] == 0x78 && c[2] == 4 && len(c) == 7:
		f = float64(math.Float32frombits(binary.BigEndian.Uint32(c[3:])))
	case c[1] == 0x79 && c[2] == 8 && len(c) == 11:
		f = math.Float64frombits(binary.BigEndian.Uint64(c[3:]))
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// snmpSchemaRule is what an snmp collector's request is held to beyond its
// keys, as the exporter holds it (validateSNMPAccount): with snmp_version
// 3 a username and a security_level, and without it a community_file;
// with authNoPriv and authPriv an auth_protocol and its password file, and
// with authPriv a priv_protocol and its password file too.
func snmpSchemaRule() map[string]any {
	both := func(a, b string) map[string]any {
		return map[string]any{"allOf": []any{SchemaWrittenKey(a), SchemaWrittenKey(b)}}
	}
	level := func(levels ...string) map[string]any {
		return map[string]any{"required": []string{"security_level"}, "properties": map[string]any{"security_level": map[string]any{"enum": levels}}}
	}
	return map[string]any{
		"allOf": []any{
			map[string]any{
				"if":   map[string]any{"required": []string{"snmp_version"}, "properties": map[string]any{"snmp_version": map[string]any{"const": 3}}},
				"then": both("username", "security_level"),
				"else": SchemaWrittenKey("community_file"),
			},
			map[string]any{"if": level(snmpAuthNoPriv, snmpAuthPriv), "then": both("auth_protocol", "auth_password_file")},
			map[string]any{"if": level(snmpAuthPriv), "then": both("priv_protocol", "priv_password_file")},
		},
	}
}

// snmpKeyRules is what the configuration schema says of an snmp
// collector's own keys.
func snmpKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"snmp_version":       {"minimum": 0, "maximum": 3, "description": "snmp: the SNMP version, 1, 2 for v2c, or 3; 2 when left out. 1 and 2 read with community_file, 3 as username with security_level. snmp is in a build with OPT_IN_REQUEST_TYPES naming it alone. See docs/SNMP.md."},
		"community_file":     {"description": "snmp: with snmp_version 1 or 2, the file of the community the agent answers to, read at each scrape, blanks around it taken off; required with them, refused with 3. See docs/SNMP.md#versions-and-accounts."},
		"security_level":     {"enum": SchemaOptionalEnum([]string{snmpNoAuthNoPriv, snmpAuthNoPriv, snmpAuthPriv}), "description": "snmp: with snmp_version 3, required: noAuthNoPriv, authNoPriv, which signs each request with auth_protocol, or authPriv, which encrypts it with priv_protocol too. See docs/SNMP.md#snmpv3."},
		"auth_protocol":      {"enum": SchemaOptionalEnum([]string{"MD5", "SHA", "SHA224", "SHA256", "SHA384", "SHA512"}), "description": "snmp: with security_level authNoPriv or authPriv, how requests are signed: HMAC of MD5, SHA (SHA-1), SHA224, SHA256, SHA384 or SHA512 (RFC 3414, RFC 7860), as the user is configured on the agent."},
		"auth_password_file": {"description": "snmp: with security_level authNoPriv or authPriv, the file of the user's authentication password, at least 8 characters, read at each scrape."},
		"priv_protocol":      {"enum": SchemaOptionalEnum([]string{"DES", "AES", "AES192", "AES256", "AES192C", "AES256C"}), "description": "snmp: with security_level authPriv, how requests are encrypted: DES, AES (AES-128, RFC 3826), AES192 and AES256, their keys extended as draft-blumenthal-aes-usm-04 does, or AES192C and AES256C, as Cisco's agents and draft-reeder-snmpv3-usm-3desede do. See docs/SNMP.md#snmpv3."},
		"priv_password_file": {"description": "snmp: with security_level authPriv, the file of the user's privacy password, at least 8 characters, read at each scrape."},
		"context_name":       {"description": "snmp: with snmp_version 3, the context the objects are read in; the default context when left out."},
		"get":                {"maxProperties": snmpMaxGets, "additionalProperties": map[string]any{"type": "string"}, "description": "snmp: the objects read one by one, each by a name of the collector's, letters, digits and _, and its numeric identifier, such as sysUpTime: 1.3.6.1.2.1.1.3.0. The document has each the agent has under get, its type and value. At most 256. See docs/SNMP.md#a-collector."},
		"walk":               {"maxProperties": snmpMaxTables, "description": "snmp: the tables read whole, each by a name, its columns by name and the numeric identifier of the column, such as interfaces: {ifDescr: 1.3.6.1.2.1.2.2.1.2}. The document has each table under walk, its rows by their index, each the index and the values of its columns. At most 32 tables of 64 columns. See docs/SNMP.md#walking-tables."},
		"walk.*":             {"minProperties": 1, "maxProperties": snmpMaxColumns, "additionalProperties": map[string]any{"type": "string"}},
		"hex":                {"description": "snmp: names of get, and table.column of walk, whose values are written as hexadecimal bytes by colons even when they read as text, such as a MAC address of printable bytes."},
		"max_repetitions":    {"minimum": 0, "maximum": snmpMaxMaxRepetitions, "description": "snmp: with snmp_version 2 or 3, how many values a GetBulk of a walk asks for, from 1 to 255; 25 when left out. An agent that answers tooBig is asked for half as many."},
		"request_timeout":    {"not": SchemaLeastDuration(snmpMinRequestTimeout, true), "description": "snmp: how long a request waits for its answer before it is sent again, at least 10ms; 1s when left out. A static target may set its own. Every request ends at the scrape's deadline too."},
		"retries":            {"minimum": 0, "maximum": snmpMaxRetries, "description": "snmp: how many times a request unanswered within request_timeout is sent again, from 0 to 5; 1 when left out. A static target may set its own."},
		"walk_limit":         {"minimum": 0, "maximum": snmpMaxWalkLimit, "description": "snmp: the most values a column of a walk may have, past which the scrape fails as a limit exceeded; 10000 when left out."},
	}
}

// snmpFailure words a failure that ctx ending brought about as that,
// wrapping its error: a scrape cancelled, or past its deadline.
func snmpFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	return err
}
