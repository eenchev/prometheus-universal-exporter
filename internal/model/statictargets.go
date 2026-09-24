package model

import (
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
	return out
}

// TargetOTLPConfig overrides the exporter-wide OTLP resource identity for one
// target, so metrics from different targets arrive as distinct resources.
type TargetOTLPConfig struct {
	ServiceName        string            `yaml:"service_name"`
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
}

// UnmarshalYAML records whether path and body were present, because an empty
// string is a meaningful override for both.
func (t *TargetRequestConfig) UnmarshalYAML(n *yaml.Node) error {
	type plain TargetRequestConfig
	// Node.Decode does not refuse unknown keys the way the file's decoder
	// does, so they are checked here, or a misspelt key would be ignored.
	if err := checkKnownKeys(n, reflect.TypeOf(plain{}), "model.TargetRequestConfig"); err != nil {
		return err
	}
	var out plain
	if err := n.Decode(&out); err != nil {
		return err
	}
	*t = TargetRequestConfig(out)
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "path":
			t.PathSet = true
		case "body":
			t.BodySet = true
		}
	}
	return nil
}
