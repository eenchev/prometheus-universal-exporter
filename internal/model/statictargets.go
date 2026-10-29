package model

import (
	"cmp"
	"reflect"

	"gopkg.in/yaml.v3"
)

// StaticTargetFile is the optional static target document, loaded with
// --static-targets-file. Its targets are scraped by the exporter itself, each
// on its own interval, and their latest results are served together on the
// static targets endpoint (--web.static-targets-path) for Prometheus to scrape.
// A target with export_via_otlp is also delivered over OTLP.
type StaticTargetFile struct {
	// Interval is how often a target that sets none is scraped. Required.
	Interval Duration `yaml:"interval"`
	// Concurrency is how many targets are scraped at once;
	// DefaultStaticTargetConcurrency when unset or 0.
	Concurrency int            `yaml:"concurrency"`
	Targets     []StaticTarget `yaml:"targets"`
}

// DefaultStaticTargetConcurrency is how many static targets are scraped at
// once when the file does not say, so a large file cannot open an unbounded
// number of connections.
const DefaultStaticTargetConcurrency = 8

// ScrapeConcurrency is how many of the file's targets are scraped at once.
func (f *StaticTargetFile) ScrapeConcurrency() int {
	if f == nil || f.Concurrency <= 0 {
		return DefaultStaticTargetConcurrency
	}
	return f.Concurrency
}

// StaticTarget describes one fully specified request. Every per-scrape
// parameter the /probe endpoint accepts is available here, alongside the labels
// and, for a target exported over OTLP, the resource identity it carries.
type StaticTarget struct {
	Name      string `yaml:"name"`
	Collector string `yaml:"collector"`
	Target    string `yaml:"target"`
	// Interval is how often the target is scraped, whatever Prometheus scrapes
	// the endpoint on and otlp.interval exports on. Validation fills it from
	// the file's interval, so it is always set on a loaded target.
	Interval Duration            `yaml:"interval"`
	Request  TargetRequestConfig `yaml:"request"`
	Labels   map[string]string   `yaml:"labels"`
	// ExportViaOTLP delivers the target's results over OTLP as well, on
	// otlp.interval, besides serving them on the static targets endpoint.
	ExportViaOTLP bool `yaml:"export_via_otlp"`
	// OTLP is the target's OTLP resource identity; only with ExportViaOTLP.
	OTLP TargetOTLPConfig `yaml:"otlp"`
	// Params fills the collector's {{param_<name>}} placeholders, as the
	// param_<name> probe parameters fill them for a probe
	// (fetch/requesttemplate.go).
	Params map[string]string `yaml:"params"`
	// ShardKey is what the target allocator (cmd/pue-allocator) hashes to
	// choose the replica that scrapes the target, in place of its name, so
	// targets that share a key share a replica. The exporter itself does
	// not read it.
	ShardKey string `yaml:"shard_key"`
}

// TargetRequestConfig mirrors the collector request block and the /probe
// override parameters. Values set here replace the collector's own settings for
// this target only.
type TargetRequestConfig struct {
	Method             string             `yaml:"method"`
	Path               string             `yaml:"path"`
	PathSet            bool               `yaml:"-"`
	Body               string             `yaml:"body"`
	BodySet            bool               `yaml:"-"`
	Timeout            Duration           `yaml:"timeout"`
	InsecureSkipVerify *bool              `yaml:"insecure_skip_verify"`
	FollowRedirects    *bool              `yaml:"follow_redirects"`
	EnableHTTP2        *bool              `yaml:"enable_http2"`
	Retry              *TargetRetryConfig `yaml:"retry"`
	Headers            map[string]string  `yaml:"headers"`
	BasicAuth          *BasicAuth         `yaml:"basic_auth"`
	BasicAuthFile      *BasicAuthFile     `yaml:"basic_auth_file"`
	BearerToken        string             `yaml:"bearer_token"`
	BearerTokenFile    string             `yaml:"bearer_token_file"`
	// Targets, From and Until replace a graphite collector's own.
	Targets []string `yaml:"targets"`
	From    string   `yaml:"from"`
	Until   string   `yaml:"until"`
	// Message replaces a grpc collector's request message, and Metadata is
	// sent besides its metadata, a key of both taking the target's value.
	Message  string            `yaml:"message"`
	Metadata map[string]string `yaml:"metadata"`
	// AcceptStatus and AcceptCodes replace the collector's
	// request.accept_status and accept_codes for this target.
	AcceptStatus []string `yaml:"accept_status"`
	AcceptCodes  []string `yaml:"accept_codes"`
	// Username and the credential files replace an sshfile, sshexec, sql
	// or redis collector's for this target, and BindDN and PasswordFile an ldap
	// collector's: the account it reads as.
	Username       string `yaml:"username"`
	BindDN         string `yaml:"bind_dn"`
	PrivateKeyFile string `yaml:"private_key_file"`
	PassphraseFile string `yaml:"passphrase_file"`
	PasswordFile   string `yaml:"password_file"`
	// TLS replaces, as a whole block, a redis, ldap, tcp, sql, grpc, http
	// or graphite collector's request.tls for this target; nil keeps the
	// collector's. Written, even as tls: {}, it asks for TLS
	// (TLSConfig.Set), so a target can ask for TLS its collector does not,
	// but not leave out TLS its collector asks for; for http and graphite
	// the URL's scheme decides, and tls: {} verifies with the system roots.
	TLS *TLSConfig `yaml:"tls,omitempty"`
	// Connections replaces, key by key, a sql collector's
	// request.connections for this target.
	Connections *SQLConnections `yaml:"connections,omitempty"`
	// Login replaces an http or graphite collector's request.login for
	// this target.
	Login *LoginConfig `yaml:"login,omitempty"`
	// RateLimit replaces an http or graphite collector's
	// request.rate_limit for this target's requests.
	RateLimit *RateLimitConfig `yaml:"rate_limit,omitempty"`
	// Order is the keys the block sets, and those of each mapping it holds
	// directly as retry.codes, in the order the file has them
	// (keyorder.go), so that of several keys the check against the
	// target's collector refuses it names the first in the file. Two
	// targets that differ in it alone scrape alike (SameStaticTarget).
	Order []string `yaml:"-"`
}

// SameStaticTarget says whether two static targets scrape alike: they are
// equal but for the order their request blocks have their keys in, which
// only the check of a block reads (TargetRequestConfig.Order), so that a
// reload does not take a target whose keys were moved for a changed one.
func SameStaticTarget(a, b StaticTarget) bool {
	a.Request.Order, b.Request.Order = nil, nil
	return reflect.DeepEqual(a, b)
}

// TargetRetryConfig is a static target's request.retry. Each key it sets
// replaces the collector's, and each it leaves out keeps the collector's, as
// the retry_attempts and retry_backoff probe parameters each replace one.
type TargetRetryConfig struct {
	Attempts      *int      `yaml:"attempts"`
	Backoff       *Duration `yaml:"backoff"`
	NonIdempotent *bool     `yaml:"non_idempotent"`
	// Codes replaces a grpc collector's retry.codes.
	Codes []string `yaml:"codes"`
	// Statuses replaces an http or graphite collector's retry.statuses.
	Statuses []string `yaml:"statuses"`
	// RetryAfter replaces an http or graphite collector's retry.retry_after.
	RetryAfter *bool `yaml:"retry_after"`
}

// Over is the retry a target makes: its own settings over the collector's.
func (r *TargetRetryConfig) Over(collector RetryConfig) RetryConfig {
	out := collector
	if r == nil {
		return out
	}
	if r.Attempts != nil {
		out.Attempts = *r.Attempts
	}
	if r.Backoff != nil {
		out.Backoff = *r.Backoff
	}
	if r.NonIdempotent != nil {
		out.NonIdempotent = *r.NonIdempotent
	}
	if r.Codes != nil {
		out.Codes = r.Codes
	}
	if r.Statuses != nil {
		out.Statuses = r.Statuses
	}
	if r.RetryAfter != nil {
		out.RetryAfter = *r.RetryAfter
	}
	return out
}

// TargetOTLPConfig overrides the exporter-wide OTLP resource identity for one
// target, so metrics from different targets arrive as distinct resources.
type TargetOTLPConfig struct {
	ServiceName        string            `yaml:"service_name"`
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
}

// UnmarshalYAML records whether path and body were present, because an empty
// string is a meaningful override for both. A key a merge key (<<) supplies
// is present like one written out, and a key that is an alias is the key its
// anchor holds (KeyName): *p : x with &p body sets the body. It records the
// order of the keys too (Order).
func (t *TargetRequestConfig) UnmarshalYAML(n *yaml.Node) error {
	type plain TargetRequestConfig
	// Node.Decode does not refuse unknown keys the way the file's decoder
	// does, so they are checked here, or a misspelt key would be ignored;
	// the values are decoded all the same, so a value of the wrong kind is
	// said beside them, in the order of the file (decodeBlock).
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return notAMapping(n, "model.TargetRequestConfig")
	}
	var out plain
	problems, err := decodeBlock(n, &out, unknownKeys(n, reflect.TypeOf(out), "model.TargetRequestConfig"))
	if err != nil || len(problems) > 0 {
		return cmp.Or(err, keyProblemsError(problems))
	}
	*t = TargetRequestConfig(out)
	for _, entry := range MappingEntries(n) {
		switch KeyName(entry.Key) {
		case "path":
			t.PathSet = true
		case "body":
			t.BodySet = true
		}
	}
	// A path or a body written "" is set too (PathSet, BodySet).
	t.Order = keyOrder(n, reflect.ValueOf(t).Elem(), "path", "body")
	return nil
}
