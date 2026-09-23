package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("duration must be a scalar")
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Collectors []Collector `yaml:"collectors"`
	// CollectorFiles lists further files of collectors, as paths or glob
	// patterns relative to the configuration file (collectorfiles.go).
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
	// report. It is not read from the document.
	Deprecations []string `yaml:"-"`
}
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
type ExporterBasicAuth struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// UsernameFile and PasswordFile read the credential from files instead,
	// such as a mounted Kubernetes Secret, so it stays out of the
	// configuration; see webauth.go.
	UsernameFile string `yaml:"username_file"`
	PasswordFile string `yaml:"password_file"`
}
type Collector struct {
	Name string `yaml:"name"`
	// MetricsPrefix, when set, is joined with "_" to the front of every metric
	// the collector exports; see metricsprefix.go.
	MetricsPrefix string          `yaml:"metrics_prefix"`
	Request       RequestConfig   `yaml:"request"`
	Response      ResponseConfig  `yaml:"response"`
	Decoder       DecoderConfig   `yaml:"decoder"`
	Transform     TransformConfig `yaml:"transform"`
	Metrics       []MetricRule    `yaml:"metrics"`
	ErrorHandling ErrorHandling   `yaml:"error_handling"`
	Limits        Limits          `yaml:"limits"`
	Cache         Duration        `yaml:"cache"`
	// Coalesce shares one upstream request among identical probes that arrive
	// while it is in flight. Unset means true; see probeflight.go.
	Coalesce *bool `yaml:"coalesce"`
	// MaxConcurrentProbes bounds the collector's trips to its targets at
	// once; unset or 0 means DefaultMaxConcurrentProbes. See triplimit.go.
	MaxConcurrentProbes int `yaml:"max_concurrent_probes"`
	// NameEscaping says what to do with a metric or label name that is not a
	// classic Prometheus name: fail, the default, underscores or values. See
	// nameescaping.go.
	NameEscaping string `yaml:"name_escaping"`
}
type RequestConfig struct {
	// Type selects how the collector reaches its data. It is required; see
	// requesttype.go for the types and the keys each accepts.
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
	// the patterns is read and checked on its own (localfile_directory.go).
	Files         []string `yaml:"files"`
	MaxFiles      int      `yaml:"max_files"`
	MaxTotalBytes ByteSize `yaml:"max_total_bytes"`
}
type RetryConfig struct {
	Attempts int      `yaml:"attempts"`
	Backoff  Duration `yaml:"backoff"`
}
type BasicAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}
type BasicAuthFile struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}
type TLSConfig struct {
	CAFile             string `yaml:"ca_file"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}
type ResponseConfig struct {
	Format string `yaml:"format"`
	// Charset names the encoding of the response when the target does not
	// declare it, or declares it wrongly (textencoding.go).
	Charset    string            `yaml:"charset"`
	CSV        CSVConfig         `yaml:"csv"`
	Namespaces map[string]string `yaml:"namespaces"`
}
type CSVConfig struct {
	Header    *bool  `yaml:"header"`
	Delimiter string `yaml:"delimiter"`
	TrimSpace bool   `yaml:"trim_space"`
}
type DecoderConfig struct {
	Type string `yaml:"type"`
}
type ErrorHandling struct {
	OnFetchError     string `yaml:"on_fetch_error"`
	OnDecodeError    string `yaml:"on_decode_error"`
	OnTransformError string `yaml:"on_transform_error"`
	AllowMissingKeys bool   `yaml:"allow_missing_keys"`
}
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
type MetricRule struct {
	Name string `yaml:"name"`
	// Items, for the jq and yq transforms, selects the things the metric is
	// about; the expression and the labels are then evaluated once per item.
	// See transformJQItems.
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

type LabelRule struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	Value      string `yaml:"value"`
	Expression string `yaml:"expression"`
	// Truncate cuts a value longer than limits.max_label_value_length to fit,
	// instead of failing the scrape. See truncateLabelValue.
	Truncate bool `yaml:"truncate"`
}
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

func (c *Config) Validate() error {
	if len(c.Collectors) == 0 {
		if len(c.CollectorFiles) > 0 {
			return errors.New("no collectors: neither collectors nor the files collector_files matched define any")
		}
		return errors.New("collectors must not be empty")
	}
	seen := map[string]bool{}
	for i := range c.Collectors {
		x := &c.Collectors[i]
		if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(x.Name) {
			return fmt.Errorf("collector %q has invalid name", x.Name)
		}
		if seen[x.Name] {
			return fmt.Errorf("duplicate collector %q", x.Name)
		}
		seen[x.Name] = true
		if err := validateRequest(x); err != nil {
			return err
		}
		if x.Limits.MaxResponseBytes <= 0 {
			x.Limits.MaxResponseBytes = 10 << 20
		}
		if x.Limits.MaxMetrics <= 0 {
			x.Limits.MaxMetrics = 10000
		}
		if x.Limits.MaxLabelsPerMetric <= 0 {
			x.Limits.MaxLabelsPerMetric = 20
		}
		if x.Limits.MaxLabelValueLength <= 0 {
			x.Limits.MaxLabelValueLength = 500
		}
		if x.Limits.MaxMetricNameLength <= 0 {
			x.Limits.MaxMetricNameLength = 200
		}
		if x.Limits.MaxHelpLength <= 0 {
			x.Limits.MaxHelpLength = 2000
		}
		if x.Limits.ScriptTimeout <= 0 {
			x.Limits.ScriptTimeout = Duration(100 * time.Millisecond)
		}
		if x.Limits.MaxOutputBytes <= 0 {
			x.Limits.MaxOutputBytes = 1 << 20
		}
		if x.Limits.MaxCacheEntries <= 0 {
			x.Limits.MaxCacheEntries = 1000
		}
		if x.Cache < 0 {
			return fmt.Errorf("collector %q cache must not be negative", x.Name)
		}
		if err := validateNameEscaping(x); err != nil {
			return err
		}
		if x.MaxConcurrentProbes < 0 {
			return fmt.Errorf("collector %q max_concurrent_probes must not be negative", x.Name)
		}
		if x.Response.Format == "" {
			x.Response.Format = "auto"
		}
		x.Response.Format = strings.ToLower(x.Response.Format)
		if err := checkCharset(x.Response.Charset); err != nil {
			return fmt.Errorf("collector %q response.charset: %w", x.Name, err)
		}
		if x.Decoder.Type == "" {
			x.Decoder.Type = x.Response.Format
		}
		if x.Decoder.Type == "" {
			x.Decoder.Type = "auto"
		}
		if x.Response.Format == "auto" && x.Decoder.Type == "auto" {
			switch strings.ToLower(x.Transform.Type) {
			case "regex":
				x.Decoder.Type = "text"
			case "csv":
				x.Decoder.Type = "csv"
			case "css":
				x.Decoder.Type = "html"
			case "prometheus":
				x.Decoder.Type = "prometheus"
			}
		}
		x.Decoder.Type = strings.ToLower(x.Decoder.Type)
		if !map[string]bool{"json": true, "yaml": true, "xml": true, "csv": true, "html": true, "prometheus": true, "text": true, "auto": true}[x.Decoder.Type] {
			return fmt.Errorf("collector %q has unknown decoder %q", x.Name, x.Decoder.Type)
		}
		if x.Transform.Type != "" {
			x.Transform.Type = strings.ToLower(x.Transform.Type)
			if !map[string]bool{"none": true, "jq": true, "yq": true, "xpath": true, "css": true, "csv": true, "regex": true, "python": true, "prometheus": true}[x.Transform.Type] {
				return fmt.Errorf("collector %q has unknown transform %q", x.Name, x.Transform.Type)
			}
		}
		if x.Transform.Type == "python" && strings.TrimSpace(x.Transform.Script) == "" {
			return fmt.Errorf("collector %q Python transform requires a script", x.Name)
		}
		if x.ErrorHandling.OnFetchError == "" {
			x.ErrorHandling.OnFetchError = "fail"
		}
		if x.ErrorHandling.OnDecodeError == "" {
			x.ErrorHandling.OnDecodeError = "fail"
		}
		if x.ErrorHandling.OnTransformError == "" {
			x.ErrorHandling.OnTransformError = "fail"
		}
		for _, policy := range []struct {
			key   string
			value *string
		}{
			{"on_fetch_error", &x.ErrorHandling.OnFetchError},
			{"on_decode_error", &x.ErrorHandling.OnDecodeError},
			{"on_transform_error", &x.ErrorHandling.OnTransformError},
		} {
			if err := c.normalizeErrorPolicy(x.Name, "error_handling."+policy.key, policy.value); err != nil {
				return err
			}
		}
		if err := validateMetricsPrefix(x); err != nil {
			return err
		}
		for _, lib := range append(x.Transform.Libraries, x.Transform.RequiredLibs...) {
			if err := checkPythonLibrary(x.Name, lib); err != nil {
				return err
			}
		}
		for i := range x.Metrics {
			r := &x.Metrics[i]
			if r.ErrorMode == "" {
				r.ErrorMode = ErrorModeLog
			}
			if err := c.normalizeErrorPolicy(x.Name, fmt.Sprintf("metric %q error_mode", r.Name), &r.ErrorMode); err != nil {
				return err
			}
			if r.Type == "" {
				r.Type = GaugeMetricType
			}
			switch r.Type {
			case GaugeMetricType, CounterMetricType, HistogramMetricType, SummaryMetricType, UntypedMetricType:
			default:
				return fmt.Errorf("collector %q metric %q has invalid type %q", x.Name, r.Name, r.Type)
			}
			if strings.TrimSpace(r.Name) == "" && x.Transform.Type != "prometheus" && x.Transform.Type != "python" {
				return fmt.Errorf("collector %q has a metric without a name", x.Name)
			}
			if strings.TrimSpace(r.Expression) == "" && x.Transform.Type != "python" && x.Transform.Type != "prometheus" {
				return fmt.Errorf("collector %q metric %q has no expression", x.Name, r.Name)
			}
			for _, label := range r.Labels {
				if strings.TrimSpace(label.Name) == "" {
					return fmt.Errorf("collector %q metric %q has a label without a name", x.Name, r.Name)
				}
				if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(label.Name) {
					return fmt.Errorf("collector %q metric %q has invalid label name %q", x.Name, r.Name, label.Name)
				}
				switch label.Type {
				case "string":
					if label.Expression != "" {
						return fmt.Errorf("collector %q metric %q label %q of type string cannot set expression", x.Name, r.Name, label.Name)
					}
				case "expression":
					if strings.TrimSpace(label.Expression) == "" {
						return fmt.Errorf("collector %q metric %q label %q of type expression requires expression", x.Name, r.Name, label.Name)
					}
					if label.Value != "" {
						return fmt.Errorf("collector %q metric %q label %q of type expression cannot set value", x.Name, r.Name, label.Name)
					}
				default:
					return fmt.Errorf("collector %q metric %q label %q has invalid type %q; want string or expression", x.Name, r.Name, label.Name, label.Type)
				}
			}
			if err := checkMetricRule(x, r); err != nil {
				return err
			}
		}
		if err := checkPrometheusTransform(x); err != nil {
			return err
		}
	}
	if c.Web.BasicAuth != nil && c.Web.BasicAuth.Enabled {
		if err := c.Web.BasicAuth.validate(); err != nil {
			return err
		}
		for _, collector := range c.Collectors {
			if collector.Request.ForwardAuthorization {
				return fmt.Errorf("web.basic_auth cannot be enabled with collector %q request.forward_authorization", collector.Name)
			}
		}
	}
	if c.OTLP.Enabled {
		if strings.TrimSpace(c.OTLP.Endpoint) == "" {
			return errors.New("otlp.endpoint is required when OTLP is enabled")
		}
		u, err := url.Parse(c.OTLP.Endpoint)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return errors.New("otlp.endpoint must be an http or https URL")
		}
		if c.OTLP.Timeout <= 0 {
			c.OTLP.Timeout = Duration(5 * time.Second)
		}
		if c.OTLP.Interval <= 0 {
			c.OTLP.Interval = Duration(30 * time.Second)
		}
		if c.OTLP.ServiceName == "" {
			c.OTLP.ServiceName = "prometheus-universal-exporter"
		}
		if c.OTLP.MaxPendingPoints < 0 {
			return errors.New("otlp.max_pending_points must not be negative")
		}
		if c.OTLP.MaxPendingPoints == 0 {
			c.OTLP.MaxPendingPoints = DefaultOTLPMaxPendingPoints
		}
		if c.OTLP.UnreadyAfterFailures < 0 {
			return errors.New("otlp.unready_after_failures must not be negative")
		}
		switch c.OTLP.Compression {
		case "":
			c.OTLP.Compression = OTLPCompressionGzip
		case OTLPCompressionGzip, OTLPCompressionNone:
		default:
			return fmt.Errorf("otlp.compression must be %s or %s, not %q", OTLPCompressionGzip, OTLPCompressionNone, c.OTLP.Compression)
		}
	}
	return nil
}

// LoadOption adjusts how a configuration document is read. Options are
// variadic so that reading a file without them — which is what every test and
// every default path does — stays the plain call it was.
type LoadOption func(*loadOptions)

type loadOptions struct{ expandEnv bool }

// WithEnvExpansion substitutes ${NAME} references from the process environment
// before the document is parsed. It is what --config.export-env turns on.
func WithEnvExpansion() LoadOption { return func(o *loadOptions) { o.expandEnv = true } }

func readDocument(path string, opts []LoadOption) ([]byte, error) {
	var options loadOptions
	for _, apply := range opts {
		apply(&options)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !options.expandEnv {
		return b, nil
	}
	return expandEnvironment(path, b)
}

func LoadConfig(path string, opts ...LoadOption) (*Config, error) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return nil, err
	}
	if err = mergeCollectorFiles(&c, path, opts); err != nil {
		return nil, err
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

type ConfigManager struct {
	current atomic.Value
	path    string
	logger  *slog.Logger
	lastMod time.Time
	// collectorFiles is the stamp of the collector files the configuration
	// read when last loaded (collectorFilesStamp).
	collectorFiles string
	targetPath     string
	targetFile     atomic.Pointer[TargetFile]
	targetsLastMod time.Time
	pythonPath     string
	watchInterval  time.Duration
	expandEnv      bool
	// reloads records how loading each file has gone, for the self-metrics.
	reloads *reloadStatus
	// reloadMu serializes reloads, whatever triggers them.
	reloadMu sync.Mutex
}

// DefaultWatchInterval is how often an enabled watch re-stats the configuration
// files. Changes are detected by modification time rather than by filesystem
// events, because Kubernetes republishes a mounted ConfigMap by swapping the
// `..data` symlink atomically: that replaces the inode a file-level event watch
// is attached to, so such a watch stops firing after the first change.
const DefaultWatchInterval = 60 * time.Second

func NewConfigManager(c *Config, path string, l *slog.Logger) *ConfigManager {
	m := &ConfigManager{path: path, logger: l, reloads: newReloadStatus()}
	m.current.Store(c)
	if c != nil {
		m.reloads.loaded(reloadFileConfig)
	}
	if c != nil {
		m.collectorFiles = collectorFilesStamp(path, c.CollectorFiles)
	}
	return m
}
func (m *ConfigManager) Get() *Config { return m.current.Load().(*Config) }

// SetWatchInterval enables the configuration watch and sets how often the
// files are re-stated. A non-positive interval leaves the watch disabled, which
// is the default: configuration is then read once at startup and changes take
// effect on restart.
func (m *ConfigManager) SetWatchInterval(interval time.Duration) { m.watchInterval = interval }

// WatchEnabled reports whether ReloadLoop will do anything.
func (m *ConfigManager) WatchEnabled() bool { return m.watchInterval > 0 }

// WatchInterval is how often an enabled watch re-stats the files, which is what
// bounds how stale a running configuration can be.
func (m *ConfigManager) WatchInterval() time.Duration { return m.watchInterval }

// SetEnvExpansion records that the documents were read with ${NAME} expansion,
// so a reload reads them the same way. A reload that quietly stopped expanding
// would replace a working configuration with one full of literal references.
func (m *ConfigManager) SetEnvExpansion(expand bool) { m.expandEnv = expand }

// loadOptions returns the options the documents were first read with.
func (m *ConfigManager) loadOptions() []LoadOption {
	if m.expandEnv {
		return []LoadOption{WithEnvExpansion()}
	}
	return nil
}

// SetPythonPath records the interpreter used to check collector Python
// scripts, so a reloaded configuration is held to the same contract as the one
// the exporter started with.
func (m *ConfigManager) SetPythonPath(path string) { m.pythonPath = path }

// SetTargets installs the scheduled target document and the file it was read
// from. An empty path leaves the feature disabled.
func (m *ConfigManager) SetTargets(path string, f *TargetFile) {
	m.targetPath = path
	if f != nil {
		m.targetFile.Store(f)
	}
	if path != "" && f != nil {
		m.reloads.loaded(reloadFileTargets)
	}
	if path != "" {
		if st, err := os.Stat(path); err == nil {
			m.targetsLastMod = st.ModTime()
		}
	}
}

// Targets returns the scheduled targets currently in force.
func (m *ConfigManager) Targets() []ScheduledTarget {
	f := m.targetFile.Load()
	if f == nil {
		return nil
	}
	return f.Targets
}

// ReloadLoop watches the configuration files when the watch is enabled and
// returns immediately when it is not, so the opt-in costs nothing.
func (m *ConfigManager) ReloadLoop(ctx context.Context) {
	if !m.WatchEnabled() {
		return
	}
	ticker := time.NewTicker(m.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reloadConfig()
			m.reloadTargets()
		}
	}
}

// The configuration is reloaded when the watch sees a file change
// (reloadConfig, reloadTargets), on SIGHUP, and on POST /-/reload when the
// lifecycle API is enabled (Reload). Every reload goes through applyConfig and
// applyTargets under reloadMu, so two triggers at once never interleave, and
// each is logged with what triggered it.
const (
	reloadTriggerWatch  = "watch"
	reloadTriggerSignal = "sighup"
	reloadTriggerHTTP   = "http"
)

// reloadConfig reloads the configuration when the watch finds the file, or
// one of its collector files, changed.
func (m *ConfigManager) reloadConfig() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	st, err := os.Stat(m.path)
	if err != nil {
		return
	}
	// A collector file edited, added or removed is a change too, although the
	// configuration file itself is untouched.
	stamp := collectorFilesStamp(m.path, m.Get().CollectorFiles)
	if !st.ModTime().After(m.lastMod) && stamp == m.collectorFiles {
		return
	}
	_ = m.applyConfig(reloadTriggerWatch)
}

// reloadTargets reloads the scheduled target file when the watch finds it
// changed.
func (m *ConfigManager) reloadTargets() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if m.targetPath == "" {
		return
	}
	st, err := os.Stat(m.targetPath)
	if err != nil || !st.ModTime().After(m.targetsLastMod) {
		return
	}
	_ = m.applyTargets(reloadTriggerWatch)
}

// Reload reloads the configuration, and the scheduled target file when there
// is one, now, whether or not they changed, and returns why either was
// rejected. A rejected file leaves the previous one in force.
func (m *ConfigManager) Reload(trigger string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	err := m.applyConfig(trigger)
	if m.targetPath != "" {
		err = errors.Join(err, m.applyTargets(trigger))
	}
	return err
}

// applyConfig loads, checks and installs the configuration. reloadMu is held.
func (m *ConfigManager) applyConfig(trigger string) error {
	if st, err := os.Stat(m.path); err == nil {
		m.lastMod = st.ModTime()
	}
	m.collectorFiles = collectorFilesStamp(m.path, m.Get().CollectorFiles)
	reject := func(err error) error {
		m.logger.Error("configuration reload rejected", "trigger", trigger, "error", err)
		m.reloads.record(reloadFileConfig, false)
		return fmt.Errorf("configuration %s: %w", m.path, err)
	}
	c, err := LoadConfig(m.path, m.loadOptions()...)
	if err != nil {
		return reject(err)
	}
	if err := ValidatePythonScripts(m.pythonPath, c); err != nil {
		return reject(err)
	}
	// Scheduled targets exist only to feed OTLP, so a configuration that would
	// disable OTLP while they are loaded is rejected exactly as it is at
	// startup, and the last valid configuration stays active.
	if f := m.targetFile.Load(); f != nil {
		if err := f.ValidateAgainst(c); err != nil {
			return reject(err)
		}
	}
	m.current.Store(c)
	m.reloads.record(reloadFileConfig, true)
	// Interpreters of scripts this reload removed or changed are stopped now
	// rather than after the idle timeout.
	pythonWorkers().retain(pythonWorkerKeys(m.pythonPath, c))
	// The new configuration may list other collector files.
	m.collectorFiles = collectorFilesStamp(m.path, c.CollectorFiles)
	logDeprecations(m.logger, m.path, c)
	m.logger.Info("configuration reloaded", "trigger", trigger, "collectors", len(c.Collectors), "collector_files", len(c.LoadedCollectorFiles))
	return nil
}

// applyTargets loads, checks and installs the scheduled target file. reloadMu
// is held.
func (m *ConfigManager) applyTargets(trigger string) error {
	if st, err := os.Stat(m.targetPath); err == nil {
		m.targetsLastMod = st.ModTime()
	}
	f, err := LoadTargetFile(m.targetPath, m.loadOptions()...)
	if err == nil {
		err = f.Validate()
	}
	if err == nil {
		err = f.ValidateAgainst(m.Get())
	}
	if err != nil {
		m.logger.Error("scheduled target reload rejected", "trigger", trigger, "error", err)
		m.reloads.record(reloadFileTargets, false)
		return fmt.Errorf("scheduled target file %s: %w", m.targetPath, err)
	}
	m.targetFile.Store(f)
	m.reloads.record(reloadFileTargets, true)
	m.logger.Info("scheduled targets reloaded", "trigger", trigger, "targets", len(f.Targets))
	return nil
}

func tlsConfig(t TLSConfig) (*tls.Config, error) {
	// The exporter deliberately exposes request.tls.insecure_skip_verify and the
	// matching per-scrape override as a documented, opt-in setting for targets
	// whose certificate cannot be validated. TLS stays enabled and the minimum
	// version is pinned.
	cfg := &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // G402: documented opt-in, defaults to false
	if t.CAFile != "" {
		b, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("no certificates found in %s", t.CAFile)
		}
		cfg.RootCAs = pool
	}
	if t.CertFile != "" || t.KeyFile != "" {
		if t.CertFile == "" || t.KeyFile == "" {
			return nil, errors.New("both tls cert_file and key_file are required")
		}
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// pythonLibraries are the third-party libraries the image installs, under the
// names a collector may declare them by: the package name or the import name.
var pythonLibraries = map[string]bool{"lxml": true, "PyYAML": true, "yaml": true, "python-dateutil": true, "dateutil": true}

// checkPythonLibrary rejects a declared library the image does not install.
// BeautifulSoup gets its own message: it was installed until lxml.html took
// over its job, so a configuration written for it needs pointing somewhere.
func checkPythonLibrary(collector, lib string) error {
	if pythonLibraries[lib] {
		return nil
	}
	if lib == "beautifulsoup4" || lib == "bs4" {
		return fmt.Errorf("collector %q declares Python library %q, which the image no longer installs; parse HTML with lxml.html instead and declare lxml", collector, lib)
	}
	return fmt.Errorf("collector %q declares unsupported Python library %q; the supported libraries are lxml, PyYAML and python-dateutil", collector, lib)
}

// logDeprecations warns once per deprecated spelling a loaded configuration
// used, so the operator hears about it on every start and reload until it is
// changed.
func logDeprecations(logger *slog.Logger, path string, c *Config) {
	for _, message := range c.Deprecations {
		logger.Warn("deprecated configuration", "file", path, "deprecation", message)
	}
}
