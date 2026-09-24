package model

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a Go duration as the configuration writes it: 500ms, 30s, 1m30s.
type Duration time.Duration

// UnmarshalYAML reads a duration, reporting a bad one with its line like any
// other decoding error, so every problem in the file is reported at once.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return lineError(n, "expected a duration such as 30s, not %s", describeNode(n))
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return lineError(n, "%q is not a duration; write one such as 500ms, 30s or 1m30s", n.Value)
	}
	*d = Duration(v)
	return nil
}

// Config is the configuration file: its collectors, the files of further
// collectors, the OTLP export and the settings of the exporter's own web
// endpoints.
type Config struct {
	Collectors []Collector `yaml:"collectors"`
	// CollectorFiles lists further files of collectors, as paths or glob
	// patterns relative to the configuration file (config/collectorfiles.go).
	CollectorFiles []string   `yaml:"collector_files"`
	OTLP           OTLPConfig `yaml:"otlp"`
	Web            WebConfig  `yaml:"web"`
	// CollectorSources records the file each collector was defined in, and
	// LoadedCollectorFiles the collector files read, in order. Neither is read
	// from the document.
	CollectorSources     map[string]string `yaml:"-"`
	LoadedCollectorFiles []string          `yaml:"-"`
	// Deprecations lists the deprecated spellings Validate accepted and
	// normalised, one message each, for startup, reload and --dry-run to
	// report. None is accepted at present; a spelling kept for a while after
	// it is replaced appends its message here. It is not read from the
	// document.
	Deprecations []string `yaml:"-"`
	// Warnings lists what Validate accepted but the operator should know
	// about, such as a collector that leaves its decoder to each response, one
	// message each, reported like Deprecations.
	Warnings []string `yaml:"-"`
}

// WebConfig is the web block: the exporter's own HTTP endpoints.
type WebConfig struct {
	BasicAuth   *ExporterBasicAuth `yaml:"basic_auth"`
	SelfMetrics SelfMetricsConfig  `yaml:"self_metrics"`
}

// SelfMetricsConfig controls how much the exporter reports about itself.
type SelfMetricsConfig struct {
	// Verbose adds a series per collector, request URL and method. Section 22
	// warns against unbounded self-metric labels, so it is opt-in and the
	// number of tracked requests is capped.
	Verbose bool `yaml:"verbose"`
	// ResourceMetrics adds the familiar go_ and process_ series describing the
	// exporter's own CPU and memory. Opt-in because reading them is not free:
	// runtime.ReadMemStats briefly stops the world on every scrape.
	ResourceMetrics bool `yaml:"resource_metrics_enabled"`
}

// ExporterBasicAuth is web.basic_auth, the credentials a client must present
// to the exporter's endpoints.
type ExporterBasicAuth struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// UsernameFile and PasswordFile read the credential from files instead,
	// such as a mounted Kubernetes Secret, so it stays out of the
	// configuration; see config/webauth.go.
	UsernameFile string `yaml:"username_file"`
	PasswordFile string `yaml:"password_file"`
}

// Collector is one entry of collectors: how to reach a target, read its
// response and turn it into metrics. Probes name it with ?collector=.
type Collector struct {
	Name string `yaml:"name"`
	// MetricsPrefix, when set, is joined with "_" to the front of every metric
	// the collector exports; see transform/metricsprefix.go.
	MetricsPrefix string          `yaml:"metrics_prefix"`
	Request       RequestConfig   `yaml:"request"`
	Response      ResponseConfig  `yaml:"response"`
	Decoder       DecoderConfig   `yaml:"decoder"`
	Transform     TransformConfig `yaml:"transform"`
	Metrics       []MetricRule    `yaml:"metrics"`
	ErrorHandling ErrorHandling   `yaml:"error_handling"`
	Limits        Limits          `yaml:"limits"`
	// Cache answers repeats of a probe from memory, and can stand in for a
	// trip that fails; see cache.go and exporter/stalecache.go.
	Cache CacheConfig `yaml:"cache"`
	// Coalesce shares one upstream request among identical probes that arrive
	// while it is in flight. Unset means true; see exporter/probeflight.go.
	Coalesce *bool `yaml:"coalesce"`
	// MaxConcurrentProbes bounds the collector's trips to its targets at
	// once; unset or 0 means DefaultMaxConcurrentProbes. See exporter/triplimit.go.
	MaxConcurrentProbes int `yaml:"max_concurrent_probes"`
	// NameEscaping says what to do with a metric or label name that is not a
	// classic Prometheus name: fail, the default, underscores or values. See
	// transform/nameescaping.go.
	NameEscaping string `yaml:"name_escaping"`
}

// RequestConfig is a collector's request block: how the target is reached.
// Which keys apply depends on Type.
type RequestConfig struct {
	// Type selects how the collector reaches its data. It is required; see
	// fetch/requesttype.go for the types and the keys each accepts.
	Type                 string            `yaml:"type"`
	Method               string            `yaml:"method"`
	Path                 string            `yaml:"path"`
	Query                map[string]string `yaml:"query"`
	Headers              map[string]string `yaml:"headers"`
	Body                 string            `yaml:"body"`
	BasicAuth            *BasicAuth        `yaml:"basic_auth"`
	BasicAuthFile        *BasicAuthFile    `yaml:"basic_auth_file"`
	BearerToken          string            `yaml:"bearer_token"`
	BearerTokenFile      string            `yaml:"bearer_token_file"`
	ForwardAuthorization bool              `yaml:"forward_authorization"`
	ForwardHeaders       []string          `yaml:"forward_headers"`
	TLS                  TLSConfig         `yaml:"tls"`
	Retry                RetryConfig       `yaml:"retry"`
	MaxResponseBytes     ByteSize          `yaml:"max_response_bytes"`
	FollowRedirects      bool              `yaml:"follow_redirects"`
	EnableHTTP2          bool              `yaml:"enable_http2"`
	AllowedSchemes       []string          `yaml:"allowed_schemes"`
	// Root and MaxAge belong to the localfile type: the directory it may read
	// under, and how old a file may be before a scrape refuses it as stale.
	Root   string   `yaml:"root"`
	MaxAge Duration `yaml:"max_age"`
	// Files, MaxFiles and MaxTotalBytes turn a localfile collector into a
	// directory reader: every file of the directory whose name matches one of
	// the patterns is read and checked on its own (fetch/localfile_directory.go).
	Files         []string `yaml:"files"`
	MaxFiles      int      `yaml:"max_files"`
	MaxTotalBytes ByteSize `yaml:"max_total_bytes"`
	// Targets, From and Until belong to the graphite type: the Graphite
	// expressions asked of the render API, each sent as a target parameter,
	// and the window they are rendered over (fetch/requesttype_graphite.go).
	Targets []string `yaml:"targets"`
	From    string   `yaml:"from"`
	Until   string   `yaml:"until"`
}

// RetryConfig is request.retry: how often a failed request is tried again,
// and how long to wait before each attempt.
type RetryConfig struct {
	Attempts int      `yaml:"attempts"`
	Backoff  Duration `yaml:"backoff"`
	// NonIdempotent lets a request whose method is not idempotent, such as
	// POST, be retried. Unset, only GET, HEAD, OPTIONS, TRACE, PUT and DELETE
	// are: sending a POST again may repeat what it did.
	NonIdempotent bool `yaml:"non_idempotent"`
}

// BasicAuth is request.basic_auth, credentials written in the configuration.
type BasicAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// BasicAuthFile is request.basic_auth_file: the paths of files holding the
// username and the password, read on every request so a rotated secret is
// picked up.
type BasicAuthFile struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// TLSConfig is a tls block: the CA to verify the server with, a client
// certificate and key, and whether verification is skipped.
type TLSConfig struct {
	CAFile             string `yaml:"ca_file"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

// ResponseConfig is a collector's response block: how to read the body the
// target returns. Which decoder reads it is decoder.type.
type ResponseConfig struct {
	// Charset names the encoding of the response when the target does not
	// declare it, or declares it wrongly (decode/textencoding.go).
	Charset    string            `yaml:"charset"`
	CSV        CSVConfig         `yaml:"csv"`
	Namespaces map[string]string `yaml:"namespaces"`
	Graphite   GraphiteConfig    `yaml:"graphite"`
}

// GraphiteConfig is response.graphite: how the graphite decoder turns each
// series' points into the one value a metric rule reads (decode/graphite.go).
type GraphiteConfig struct {
	// Value is how the points of a series become its value: last, the
	// newest point, by default, or max, min, avg or sum.
	Value string `yaml:"value"`
	// MaxAge leaves out a series whose newest point is older than this, so
	// a series whose writer stopped is not exported with its last value.
	MaxAge Duration `yaml:"max_age"`
	// InvalidLines is what a carbon line that cannot be read does: fail,
	// the default, fails the decode; skip leaves the line out, counted and
	// logged.
	InvalidLines string `yaml:"invalid_lines"`
}

// GraphiteValues are the values of response.graphite.value.
var GraphiteValues = []string{"last", "max", "min", "avg", "sum"}

// GraphiteInvalidLines are the values of response.graphite.invalid_lines.
var GraphiteInvalidLines = []string{"fail", "skip"}

// CSVConfig is response.csv: how a CSV body is split into rows and fields.
type CSVConfig struct {
	Header    *bool  `yaml:"header"`
	Delimiter string `yaml:"delimiter"`
	TrimSpace bool   `yaml:"trim_space"`
}

// DecoderConfig is a collector's decoder block. Type names the decoder that
// reads the response: json, yaml, xml, csv, html, prometheus, text or
// graphite. Unset or
// auto, the transform's type picks it when it implies one, and otherwise the
// response's content type or its content.
type DecoderConfig struct {
	Type string `yaml:"type"`
}

// ErrorHandling is a collector's error_handling block: the policy, fail, log
// or ignore, for a failed fetch, decode or transform, and whether a missing
// value fails its metric rule.
type ErrorHandling struct {
	OnFetchError     string `yaml:"on_fetch_error"`
	OnDecodeError    string `yaml:"on_decode_error"`
	OnTransformError string `yaml:"on_transform_error"`
	AllowMissingKeys bool   `yaml:"allow_missing_keys"`
}

// OTLPConfig is the otlp block: where and how often probe results, the
// self-metrics and the static targets with export_via_otlp are pushed.
type OTLPConfig struct {
	Enabled            bool              `yaml:"enabled"`
	Endpoint           string            `yaml:"endpoint"`
	Headers            map[string]string `yaml:"headers"`
	Timeout            Duration          `yaml:"timeout"`
	Interval           Duration          `yaml:"interval"`
	TLS                TLSConfig         `yaml:"tls"`
	InsecureSkipVerify bool              `yaml:"insecure_skip_verify"`
	ServiceName        string            `yaml:"service_name"`
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
	// Compression of the export requests: gzip, the default, or none.
	Compression string `yaml:"compression"`
	// MaxPendingPoints bounds the data points waiting for export while the
	// endpoint is failing; past it the oldest are dropped.
	MaxPendingPoints int `yaml:"max_pending_points"`
	// UnreadyAfterFailures makes /ready answer 503 after this many failed
	// exports in a row; 0, the default, leaves readiness to the configuration.
	UnreadyAfterFailures int `yaml:"unready_after_failures"`
}

// DefaultOTLPMaxPendingPoints is otlp.max_pending_points when unset.
const DefaultOTLPMaxPendingPoints = 100000

// The values of otlp.compression.
const (
	OTLPCompressionGzip = "gzip"
	OTLPCompressionNone = "none"
)

// Limits is a collector's limits block: bounds on what one scrape may read,
// produce and spend. Zero means the default.
type Limits struct {
	MaxResponseBytes    ByteSize `yaml:"max_response_bytes"`
	MaxMetrics          int      `yaml:"max_metrics"`
	MaxLabelsPerMetric  int      `yaml:"max_labels_per_metric"`
	MaxLabelValueLength int      `yaml:"max_label_value_length"`
	MaxMetricNameLength int      `yaml:"max_metric_name_length"`
	MaxHelpLength       int      `yaml:"max_help_length"`
	ScriptTimeout       Duration `yaml:"script_timeout"`
	MaxOutputBytes      ByteSize `yaml:"max_output_bytes"`
	MaxCacheEntries     int      `yaml:"max_cache_entries"`
}

// MetricRule is one entry of a collector's metrics: a metric, the expression
// that produces its value and the labels it carries.
type MetricRule struct {
	Name string `yaml:"name"`
	// Items, for the jq, yq and css transforms, selects the things the metric
	// is about; the expression and the labels are then evaluated once per
	// item. See transformJQItems and transformCSSItems.
	Items       string      `yaml:"items"`
	Description string      `yaml:"description"`
	Type        MetricType  `yaml:"type"`
	Labels      []LabelRule `yaml:"labels"`
	Expression  string      `yaml:"expression"`
	ErrorMode   string      `yaml:"error_mode"`
	Required    *bool       `yaml:"required"`
}

// What a metric rule does when it cannot produce its value. The first two keep
// the scrape going without that metric, so the response carries every metric
// that could be extracted, or none at all; fail stops the scrape at the first
// failure, so a response is either complete or an error.
const (
	// ErrorModeIgnore drops the metric silently.
	ErrorModeIgnore = "ignore"
	// ErrorModeLog drops the metric and logs why. It is the default.
	ErrorModeLog = "log"
	// ErrorModeFail logs the failure and fails the whole scrape.
	ErrorModeFail = "fail"
)

// LabelRule is one label of a metric rule. It sets exactly one of Value, a
// static value exported as written, and Expression, evaluated against the
// response in the collector's transform language.
type LabelRule struct {
	Name       string `yaml:"name"`
	Value      string `yaml:"value"`
	Expression string `yaml:"expression"`
	// Truncate cuts a value longer than limits.max_label_value_length to fit,
	// instead of failing the scrape. See truncateLabelValue.
	Truncate bool `yaml:"truncate"`
	// Required makes a series whose expression gives this label no value, or
	// an empty one, a failure of the metric rule, handled by its error_mode.
	// Unset, such a series is exported without the label.
	Required bool `yaml:"required"`
}

// Static reports whether the label has a static value rather than an
// expression.
func (l LabelRule) Static() bool { return l.Expression == "" }

// DecoderTypes are the values of decoder.type: auto, which chooses a decoder
// for each response, and the decoders. Validation and the JSON Schema both
// read this list, and a test keeps decode.Decode handling every decoder in it.
var DecoderTypes = []string{"auto", "json", "yaml", "xml", "csv", "html", "prometheus", "text", "graphite"}

// TransformTypes are the values of transform.type, which a collector must set.
// Validation and the JSON Schema both read this list, and a test keeps the
// transform package handling every type in it.
var TransformTypes = []string{"jq", "yq", "xpath", "css", "csv", "regex", "python", "prometheus"}

// TransformConfig is a collector's transform block: the language its
// expressions are written in, the scripts, and the renaming and filtering
// applied to the metrics produced.
type TransformConfig struct {
	Type         string            `yaml:"type"`
	PreScript    string            `yaml:"pre_script"`
	Script       string            `yaml:"script"`
	Libraries    []string          `yaml:"libraries"`
	RequiredLibs []string          `yaml:"required_libs"`
	Include      []string          `yaml:"include"`
	Exclude      []string          `yaml:"exclude"`
	Rename       map[string]string `yaml:"rename"`
	Labels       map[string]string `yaml:"labels"`
	RemoveLabels []string          `yaml:"remove_labels"`
	RenameLabels map[string]string `yaml:"rename_labels"`
}

// Configuration validation checks everything about a metric rule that can be
// known before a scrape: that its name is a Prometheus metric name, and that
// every expression it holds compiles in the language its transform speaks.
// Compiling here also fills the expression caches (expr/exprcache.go), so a scrape
// runs programs that already exist. A mistake is therefore reported at
// startup, on reload and by --dry-run, naming the collector, the rule and the
// label, instead of failing — or, for a CSS selector, silently matching
// nothing — on every scrape.

// The error policy vocabulary is shared by error_handling and error_mode:
// fail stops the scrape, log carries on and logs why, ignore carries on
// quietly.
const (
	ErrorPolicyFail   = ErrorModeFail
	ErrorPolicyLog    = ErrorModeLog
	ErrorPolicyIgnore = ErrorModeIgnore
)

// CollectorByName returns the collector of cfg with the given name, or nil.
func CollectorByName(cfg *Config, name string) *Collector {
	for i := range cfg.Collectors {
		if cfg.Collectors[i].Name == name {
			return &cfg.Collectors[i]
		}
	}
	return nil
}

// CacheConfig is a collector's cache.
type CacheConfig struct {
	// TTL is how long a result answers repeats of its probe.
	TTL Duration `yaml:"ttl"`
	// StaleIfError is how long after TTL a result stands in for a trip that
	// fails.
	StaleIfError Duration `yaml:"stale_if_error"`
}

var cacheConfigKeys = []string{"ttl", "stale_if_error"}

// UnmarshalYAML refuses the keys it does not know, which a custom decoder
// would otherwise let through, and explains the one-value form.
func (c *CacheConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return fmt.Errorf("line %d: cache is a mapping: write cache: {ttl: %s} to answer repeats of a probe for %s, and add stale_if_error to answer with the last good result when the target fails", n.Line, n.Value, n.Value)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: cache must be a mapping with ttl and stale_if_error", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if key := n.Content[i].Value; !slices.Contains(cacheConfigKeys, key) {
			return fmt.Errorf("line %d: cache has the unknown key %q; it takes %s", n.Content[i].Line, key, strings.Join(cacheConfigKeys, " and "))
		}
	}
	type plain CacheConfig
	return n.Decode((*plain)(c))
}

// CacheTTL is how long a collector's result answers repeats of its probe.
func CacheTTL(c *Collector) time.Duration { return time.Duration(c.Cache.TTL) }

// StaleIfError is how long after its TTL a collector's result stands in for a
// trip that fails.
func StaleIfError(c *Collector) time.Duration { return time.Duration(c.Cache.StaleIfError) }

// UsesCache reports whether the collector keeps results at all.
func UsesCache(c *Collector) bool { return CacheTTL(c) > 0 || StaleIfError(c) > 0 }
