package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
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
	OTLP       OTLPConfig  `yaml:"otlp"`
	Web        WebConfig   `yaml:"web"`
}
type WebConfig struct {
	BasicAuth *ExporterBasicAuth `yaml:"basic_auth"`
}
type ExporterBasicAuth struct {
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}
type Collector struct {
	Name          string          `yaml:"name"`
	Request       RequestConfig   `yaml:"request"`
	Response      ResponseConfig  `yaml:"response"`
	Decoder       DecoderConfig   `yaml:"decoder"`
	Transform     TransformConfig `yaml:"transform"`
	Metrics       []MetricRule    `yaml:"metrics"`
	ErrorHandling ErrorHandling   `yaml:"error_handling"`
	Limits        Limits          `yaml:"limits"`
	Cache         Duration        `yaml:"cache"`
}
type RequestConfig struct {
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
	MaxResponseBytes     int64             `yaml:"max_response_bytes"`
	RedirectPolicy       string            `yaml:"redirect_policy"`
	AllowedSchemes       []string          `yaml:"allowed_schemes"`
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
	Format     string            `yaml:"format"`
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
	OnHTTPError      string `yaml:"on_http_error"`
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
}
type Limits struct {
	MaxResponseBytes    int64    `yaml:"max_response_bytes"`
	MaxMetrics          int      `yaml:"max_metrics"`
	MaxLabelsPerMetric  int      `yaml:"max_labels_per_metric"`
	MaxLabelValueLength int      `yaml:"max_label_value_length"`
	MaxMetricNameLength int      `yaml:"max_metric_name_length"`
	MaxHelpLength       int      `yaml:"max_help_length"`
	ScriptTimeout       Duration `yaml:"script_timeout"`
	MaxOutputBytes      int      `yaml:"max_output_bytes"`
	MaxCacheEntries     int      `yaml:"max_cache_entries"`
}
type MetricRule struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Type        MetricType  `yaml:"type"`
	Labels      []LabelRule `yaml:"labels"`
	Expression  string      `yaml:"expression"`
	ErrorMode   string      `yaml:"error_mode"`
	Required    *bool       `yaml:"required"`
}
type LabelRule struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	Value      string `yaml:"value"`
	Expression string `yaml:"expression"`
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
		return fmt.Errorf("collectors must not be empty")
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
		if x.Request.BearerToken != "" && x.Request.BearerTokenFile != "" {
			return fmt.Errorf("collector %q cannot set both request.bearer_token and request.bearer_token_file", x.Name)
		}
		if x.Request.BasicAuth != nil && x.Request.BasicAuthFile != nil {
			return fmt.Errorf("collector %q cannot set both request.basic_auth and request.basic_auth_file", x.Name)
		}
		if x.Request.BasicAuthFile != nil && (strings.TrimSpace(x.Request.BasicAuthFile.Username) == "" || strings.TrimSpace(x.Request.BasicAuthFile.Password) == "") {
			return fmt.Errorf("collector %q basic_auth_file requires username and password paths", x.Name)
		}
		if x.Request.Retry.Attempts < 0 {
			return fmt.Errorf("collector %q request.retry.attempts must not be negative", x.Name)
		}
		if x.Request.Retry.Backoff < 0 {
			return fmt.Errorf("collector %q request.retry.backoff must not be negative", x.Name)
		}
		if (x.Request.BasicAuth != nil || x.Request.BasicAuthFile != nil) && (x.Request.BearerToken != "" || x.Request.BearerTokenFile != "") {
			return fmt.Errorf("collector %q cannot configure basic and bearer authentication together", x.Name)
		}
		if x.Request.Method == "" {
			x.Request.Method = "GET"
		}
		x.Request.Method = strings.ToUpper(x.Request.Method)
		switch x.Request.Method {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		default:
			return fmt.Errorf("collector %q has unsupported method %q", x.Name, x.Request.Method)
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
		if x.Response.Format == "" {
			x.Response.Format = "auto"
		}
		x.Response.Format = strings.ToLower(x.Response.Format)
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
		if x.ErrorHandling.OnHTTPError == "" {
			x.ErrorHandling.OnHTTPError = "fail"
		}
		if x.ErrorHandling.OnDecodeError == "" {
			x.ErrorHandling.OnDecodeError = "fail"
		}
		if x.ErrorHandling.OnTransformError == "" {
			x.ErrorHandling.OnTransformError = "fail"
		}
		for _, p := range []string{x.ErrorHandling.OnHTTPError, x.ErrorHandling.OnDecodeError, x.ErrorHandling.OnTransformError} {
			if p != "fail" && p != "warn" && p != "ignore" {
				return fmt.Errorf("collector %q has invalid error policy %q", x.Name, p)
			}
		}
		for _, lib := range append(x.Transform.Libraries, x.Transform.RequiredLibs...) {
			if !map[string]bool{"beautifulsoup4": true, "bs4": true, "lxml": true, "PyYAML": true, "yaml": true, "python-dateutil": true, "dateutil": true}[lib] {
				return fmt.Errorf("collector %q declares unsupported Python library %q", x.Name, lib)
			}
		}
		for i := range x.Metrics {
			r := &x.Metrics[i]
			if r.ErrorMode == "" {
				r.ErrorMode = "log"
			}
			if r.ErrorMode != "log" && r.ErrorMode != "ignore" {
				return fmt.Errorf("collector %q metric %q has invalid error_mode %q; want log or ignore", x.Name, r.Name, r.ErrorMode)
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
			if x.Transform.Type == "" || x.Transform.Type == "none" || x.Transform.Type == "jq" || x.Transform.Type == "yq" {
				if _, err := gojq.Parse(r.Expression); err != nil {
					return fmt.Errorf("collector %q metric %q expression: %w", x.Name, r.Name, err)
				}
				for _, label := range r.Labels {
					if label.Type == "expression" {
						if _, err := gojq.Parse(label.Expression); err != nil {
							return fmt.Errorf("collector %q metric %q label %q expression: %w", x.Name, r.Name, label.Name, err)
						}
					}
				}
			}
			if x.Transform.Type == "regex" {
				if _, err := regexp.Compile(r.Expression); err != nil {
					return fmt.Errorf("collector %q metric %q regex: %w", x.Name, r.Name, err)
				}
			}
		}
	}
	if c.Web.BasicAuth != nil && c.Web.BasicAuth.Enabled {
		if strings.TrimSpace(c.Web.BasicAuth.Username) == "" || c.Web.BasicAuth.Password == "" {
			return fmt.Errorf("web.basic_auth requires a username and password when enabled")
		}
		for _, collector := range c.Collectors {
			if collector.Request.ForwardAuthorization {
				return fmt.Errorf("web.basic_auth cannot be enabled with collector %q request.forward_authorization", collector.Name)
			}
		}
	}
	if c.OTLP.Enabled {
		if strings.TrimSpace(c.OTLP.Endpoint) == "" {
			return fmt.Errorf("otlp.endpoint is required when OTLP is enabled")
		}
		u, err := url.Parse(c.OTLP.Endpoint)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("otlp.endpoint must be an http or https URL")
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
	}
	return nil
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return nil, err
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

type ConfigManager struct {
	current        atomic.Value
	path           string
	logger         *slog.Logger
	lastMod        time.Time
	targetPath     string
	targetFile     atomic.Pointer[TargetFile]
	targetsLastMod time.Time
}

func NewConfigManager(c *Config, path string, l *slog.Logger) *ConfigManager {
	m := &ConfigManager{path: path, logger: l}
	m.current.Store(c)
	return m
}
func (m *ConfigManager) Get() *Config { return m.current.Load().(*Config) }

// SetTargets installs the scheduled target document and the file it was read
// from. An empty path leaves the feature disabled.
func (m *ConfigManager) SetTargets(path string, f *TargetFile) {
	m.targetPath = path
	if f != nil {
		m.targetFile.Store(f)
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

func (m *ConfigManager) ReloadLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
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

func (m *ConfigManager) reloadConfig() {
	st, err := os.Stat(m.path)
	if err != nil || !st.ModTime().After(m.lastMod) {
		return
	}
	m.lastMod = st.ModTime()
	c, err := LoadConfig(m.path)
	if err != nil {
		m.logger.Error("configuration reload rejected", "error", err)
		return
	}
	// Scheduled targets exist only to feed OTLP, so a configuration that would
	// disable OTLP while they are loaded is rejected exactly as it is at
	// startup, and the last valid configuration stays active.
	if f := m.targetFile.Load(); f != nil {
		if err := f.ValidateAgainst(c); err != nil {
			m.logger.Error("configuration reload rejected", "error", err)
			return
		}
	}
	m.current.Store(c)
	m.logger.Info("configuration reloaded", "collectors", len(c.Collectors))
}

func (m *ConfigManager) reloadTargets() {
	if m.targetPath == "" {
		return
	}
	st, err := os.Stat(m.targetPath)
	if err != nil || !st.ModTime().After(m.targetsLastMod) {
		return
	}
	m.targetsLastMod = st.ModTime()
	f, err := LoadTargetFile(m.targetPath)
	if err == nil {
		err = f.Validate()
	}
	if err == nil {
		err = f.ValidateAgainst(m.Get())
	}
	if err != nil {
		m.logger.Error("scheduled target reload rejected", "error", err)
		return
	}
	m.targetFile.Store(f)
	m.logger.Info("scheduled targets reloaded", "targets", len(f.Targets))
}

func tlsConfig(t TLSConfig) (*tls.Config, error) {
	cfg := &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify, MinVersion: tls.VersionTLS12}
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
			return nil, fmt.Errorf("both tls cert_file and key_file are required")
		}
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
