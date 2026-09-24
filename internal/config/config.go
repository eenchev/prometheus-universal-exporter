package config

import (
	"context"
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

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
	"gopkg.in/yaml.v3"
)

// namePattern is what a collector's and a label's name may look like.
var namePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Validate checks a configuration and fills in its defaults. It stops at the
// first problem, which the error describes.
func Validate(c *model.Config) error {
	if len(c.Collectors) == 0 {
		if len(c.CollectorFiles) > 0 {
			return errors.New("no collectors: neither collectors nor the files collector_files matched define any")
		}
		return errors.New("collectors must not be empty")
	}
	seen := map[string]bool{}
	for i := range c.Collectors {
		x := &c.Collectors[i]
		if !namePattern.MatchString(x.Name) {
			return fmt.Errorf("collector %q has invalid name", x.Name)
		}
		if seen[x.Name] {
			return fmt.Errorf("duplicate collector %q", x.Name)
		}
		seen[x.Name] = true
		if err := validateCollector(c, x); err != nil {
			return err
		}
	}
	if err := validateWebAuthSettings(c); err != nil {
		return err
	}
	return validateOTLP(&c.OTLP)
}

// validateCollector checks one collector and fills in its defaults, in the
// order its settings are read: the request, the limits, the cache and the
// names, the response and its transform, the error policies, and the metric
// rules.
func validateCollector(c *model.Config, x *model.Collector) error {
	if err := fetch.ValidateRequest(x); err != nil {
		return err
	}
	applyLimitDefaults(&x.Limits)
	if err := validateCache(x); err != nil {
		return err
	}
	if err := transform.ValidateNameEscaping(x); err != nil {
		return err
	}
	if x.MaxConcurrentProbes < 0 {
		return fmt.Errorf("collector %q max_concurrent_probes must not be negative", x.Name)
	}
	if err := normalizeFormats(x); err != nil {
		return err
	}
	for _, policy := range []struct {
		key   string
		value *string
	}{
		{"on_fetch_error", &x.ErrorHandling.OnFetchError},
		{"on_decode_error", &x.ErrorHandling.OnDecodeError},
		{"on_transform_error", &x.ErrorHandling.OnTransformError},
	} {
		if *policy.value == "" {
			*policy.value = model.ErrorPolicyFail
		}
		if err := normalizeErrorPolicy(c, x.Name, "error_handling."+policy.key, policy.value); err != nil {
			return err
		}
	}
	if err := transform.ValidateMetricsPrefix(x); err != nil {
		return err
	}
	for _, lib := range append(x.Transform.Libraries, x.Transform.RequiredLibs...) {
		if err := checkPythonLibrary(x.Name, lib); err != nil {
			return err
		}
	}
	if err := validateMetricRules(c, x); err != nil {
		return err
	}
	return transform.CheckPrometheusTransform(x)
}

// applyLimitDefaults gives every limit left unset its default.
func applyLimitDefaults(l *model.Limits) {
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = 10 << 20
	}
	if l.MaxMetrics <= 0 {
		l.MaxMetrics = 10000
	}
	if l.MaxLabelsPerMetric <= 0 {
		l.MaxLabelsPerMetric = 20
	}
	if l.MaxLabelValueLength <= 0 {
		l.MaxLabelValueLength = 500
	}
	if l.MaxMetricNameLength <= 0 {
		l.MaxMetricNameLength = 200
	}
	if l.MaxHelpLength <= 0 {
		l.MaxHelpLength = 2000
	}
	if l.ScriptTimeout <= 0 {
		l.ScriptTimeout = model.Duration(100 * time.Millisecond)
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = 1 << 20
	}
	if l.MaxCacheEntries <= 0 {
		l.MaxCacheEntries = 1000
	}
}

// normalizeFormats lower-cases the decoder and transform, infers the decoder a
// transform implies, and refuses what is not known.
func normalizeFormats(x *model.Collector) error {
	if err := decode.CheckCharset(x.Response.Charset); err != nil {
		return fmt.Errorf("collector %q response.charset: %w", x.Name, err)
	}
	x.Decoder.Type = strings.ToLower(x.Decoder.Type)
	if x.Decoder.Type == "" {
		x.Decoder.Type = "auto"
	}
	if x.Decoder.Type == "auto" {
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
	return nil
}

// validateMetricRules checks each metric rule and fills in its defaults.
func validateMetricRules(c *model.Config, x *model.Collector) error {
	for i := range x.Metrics {
		r := &x.Metrics[i]
		if r.ErrorMode == "" {
			r.ErrorMode = model.ErrorModeLog
		}
		if err := normalizeErrorPolicy(c, x.Name, fmt.Sprintf("metric %q error_mode", r.Name), &r.ErrorMode); err != nil {
			return err
		}
		if r.Type == "" {
			r.Type = model.GaugeMetricType
		}
		switch r.Type {
		case model.GaugeMetricType, model.CounterMetricType, model.HistogramMetricType, model.SummaryMetricType, model.UntypedMetricType:
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
			if !namePattern.MatchString(label.Name) {
				return fmt.Errorf("collector %q metric %q has invalid label name %q", x.Name, r.Name, label.Name)
			}
			hasValue, hasExpression := label.Value != "", strings.TrimSpace(label.Expression) != ""
			switch {
			case hasValue && hasExpression:
				return fmt.Errorf("collector %q metric %q label %q sets both value and expression; set value for a static label, or expression to read it from the response", x.Name, r.Name, label.Name)
			case !hasValue && !hasExpression:
				return fmt.Errorf("collector %q metric %q label %q needs a value, for a static label, or an expression, to read it from the response", x.Name, r.Name, label.Name)
			case hasValue && label.Required:
				return fmt.Errorf("collector %q metric %q label %q has a static value, so it cannot be required; its value is always there", x.Name, r.Name, label.Name)
			case label.Required && x.Transform.Type == "python":
				return fmt.Errorf("collector %q metric %q label %q cannot be required: a python transform's labels come from its script, not from label expressions", x.Name, r.Name, label.Name)
			}
		}
		if err := transform.CheckMetricRule(x, r); err != nil {
			return err
		}
	}
	return nil
}

// validateWebAuthSettings checks the exporter's own basic authentication,
// which cannot share the Authorization header with a collector forwarding it.
func validateWebAuthSettings(c *model.Config) error {
	if c.Web.BasicAuth == nil || !c.Web.BasicAuth.Enabled {
		return nil
	}
	if err := validateWebAuth(c.Web.BasicAuth); err != nil {
		return err
	}
	for _, collector := range c.Collectors {
		if collector.Request.ForwardAuthorization {
			return fmt.Errorf("web.basic_auth cannot be enabled with collector %q request.forward_authorization", collector.Name)
		}
	}
	return nil
}

// validateOTLP checks the OTLP export settings and fills in their defaults.
func validateOTLP(o *model.OTLPConfig) error {
	if !o.Enabled {
		return nil
	}
	if strings.TrimSpace(o.Endpoint) == "" {
		return errors.New("otlp.endpoint is required when OTLP is enabled")
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return errors.New("otlp.endpoint must be an http or https URL")
	}
	if o.Timeout <= 0 {
		o.Timeout = model.Duration(5 * time.Second)
	}
	if o.Interval <= 0 {
		o.Interval = model.Duration(30 * time.Second)
	}
	if o.ServiceName == "" {
		o.ServiceName = "prometheus-universal-exporter"
	}
	if o.MaxPendingPoints < 0 {
		return errors.New("otlp.max_pending_points must not be negative")
	}
	if o.MaxPendingPoints == 0 {
		o.MaxPendingPoints = model.DefaultOTLPMaxPendingPoints
	}
	if o.UnreadyAfterFailures < 0 {
		return errors.New("otlp.unready_after_failures must not be negative")
	}
	switch o.Compression {
	case "":
		o.Compression = model.OTLPCompressionGzip
	case model.OTLPCompressionGzip, model.OTLPCompressionNone:
	default:
		return fmt.Errorf("otlp.compression must be %s or %s, not %q", model.OTLPCompressionGzip, model.OTLPCompressionNone, o.Compression)
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

// Load reads the configuration file at path and the collector files it
// lists, and validates the result.
func Load(path string, opts ...LoadOption) (*model.Config, error) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, err
	}
	var c model.Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return nil, yamlError(err)
	}
	if err = mergeCollectorFiles(&c, path, opts); err != nil {
		return nil, err
	}
	if err = Validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Manager holds the configuration in force and the scheduled target file,
// and reloads them: when the watch finds a file changed, on SIGHUP and on
// POST /-/reload. A rejected reload leaves the previous configuration in
// force.
type Manager struct {
	current atomic.Value
	path    string
	logger  *slog.Logger
	lastMod time.Time
	// collectorFiles is the stamp of the collector files the configuration
	// read when last loaded (collectorFilesStamp).
	collectorFiles string
	targetPath     string
	targetFile     atomic.Pointer[model.TargetFile]
	targetsLastMod time.Time
	pythonPath     string
	watchInterval  time.Duration
	expandEnv      bool
	// reloads records how loading each file has gone, for the self-metrics.
	Reloads *reloadStatus
	// reloadMu serializes reloads, whatever triggers them.
	reloadMu sync.Mutex
}

// DefaultWatchInterval is how often an enabled watch re-stats the configuration
// files. Changes are detected by modification time rather than by filesystem
// events, because Kubernetes republishes a mounted ConfigMap by swapping the
// `..data` symlink atomically: that replaces the inode a file-level event watch
// is attached to, so such a watch stops firing after the first change.
const DefaultWatchInterval = 60 * time.Second

// NewManager returns a manager holding c, which was read from path.
func NewManager(c *model.Config, path string, l *slog.Logger) *Manager {
	m := &Manager{path: path, logger: l, Reloads: newReloadStatus()}
	m.current.Store(c)
	if c != nil {
		m.Reloads.loaded(reloadFileConfig)
	}
	if c != nil {
		m.collectorFiles = collectorFilesStamp(path, c.CollectorFiles)
	}
	return m
}

// Get returns the configuration in force.
func (m *Manager) Get() *model.Config { return m.current.Load().(*model.Config) }

// SetWatchInterval enables the configuration watch and sets how often the
// files are re-stated. A non-positive interval leaves the watch disabled, which
// is the default: configuration is then read once at startup and changes take
// effect on restart.
func (m *Manager) SetWatchInterval(interval time.Duration) { m.watchInterval = interval }

// WatchEnabled reports whether ReloadLoop will do anything.
func (m *Manager) WatchEnabled() bool { return m.watchInterval > 0 }

// WatchInterval is how often an enabled watch re-stats the files, which is what
// bounds how stale a running configuration can be.
func (m *Manager) WatchInterval() time.Duration { return m.watchInterval }

// SetEnvExpansion records that the documents were read with ${NAME} expansion,
// so a reload reads them the same way. A reload that quietly stopped expanding
// would replace a working configuration with one full of literal references.
func (m *Manager) SetEnvExpansion(expand bool) { m.expandEnv = expand }

// loadOptions returns the options the documents were first read with.
func (m *Manager) loadOptions() []LoadOption {
	if m.expandEnv {
		return []LoadOption{WithEnvExpansion()}
	}
	return nil
}

// SetPythonPath records the interpreter used to check collector Python
// scripts, so a reloaded configuration is held to the same contract as the one
// the exporter started with.
func (m *Manager) SetPythonPath(path string) { m.pythonPath = path }

// SetTargets installs the scheduled target document and the file it was read
// from. An empty path leaves the feature disabled.
func (m *Manager) SetTargets(path string, f *model.TargetFile) {
	m.targetPath = path
	if f != nil {
		m.targetFile.Store(f)
	}
	if path != "" && f != nil {
		m.Reloads.loaded(ReloadFileTargets)
	}
	if path != "" {
		if st, err := os.Stat(path); err == nil {
			m.targetsLastMod = st.ModTime()
		}
	}
}

// Targets returns the scheduled targets currently in force.
func (m *Manager) Targets() []model.ScheduledTarget {
	f := m.targetFile.Load()
	if f == nil {
		return nil
	}
	return f.Targets
}

// ReloadLoop watches the configuration files when the watch is enabled and
// returns immediately when it is not, so the opt-in costs nothing.
func (m *Manager) ReloadLoop(ctx context.Context) {
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
	ReloadTriggerSignal = "sighup"
	ReloadTriggerHTTP   = "http"
)

// reloadConfig reloads the configuration when the watch finds the file, or
// one of its collector files, changed.
func (m *Manager) reloadConfig() {
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
func (m *Manager) reloadTargets() {
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
func (m *Manager) Reload(trigger string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	err := m.applyConfig(trigger)
	if m.targetPath != "" {
		err = errors.Join(err, m.applyTargets(trigger))
	}
	return err
}

// applyConfig loads, checks and installs the configuration. reloadMu is held.
func (m *Manager) applyConfig(trigger string) error {
	if st, err := os.Stat(m.path); err == nil {
		m.lastMod = st.ModTime()
	}
	m.collectorFiles = collectorFilesStamp(m.path, m.Get().CollectorFiles)
	reject := func(err error) error {
		m.logger.Error("configuration reload rejected", "trigger", trigger, "error", err)
		m.Reloads.record(reloadFileConfig, false)
		return fmt.Errorf("configuration %s: %w", m.path, err)
	}
	c, err := Load(m.path, m.loadOptions()...)
	if err != nil {
		return reject(err)
	}
	if err := transform.ValidatePythonScripts(m.pythonPath, c); err != nil {
		return reject(err)
	}
	// Scheduled targets exist only to feed OTLP, so a configuration that would
	// disable OTLP while they are loaded is rejected exactly as it is at
	// startup, and the last valid configuration stays active.
	if f := m.targetFile.Load(); f != nil {
		if err := ValidateTargetsAgainst(f, c); err != nil {
			return reject(err)
		}
	}
	m.current.Store(c)
	m.Reloads.record(reloadFileConfig, true)
	// Interpreters of scripts this reload removed or changed are stopped now
	// rather than after the idle timeout.
	transform.PythonWorkers().Retain(transform.PythonWorkerKeys(m.pythonPath, c))
	// The new configuration may list other collector files.
	m.collectorFiles = collectorFilesStamp(m.path, c.CollectorFiles)
	LogDeprecations(m.logger, m.path, c)
	m.logger.Info("configuration reloaded", "trigger", trigger, "collectors", len(c.Collectors), "collector_files", len(c.LoadedCollectorFiles))
	return nil
}

// applyTargets loads, checks and installs the scheduled target file. reloadMu
// is held.
func (m *Manager) applyTargets(trigger string) error {
	if st, err := os.Stat(m.targetPath); err == nil {
		m.targetsLastMod = st.ModTime()
	}
	f, err := LoadTargets(m.targetPath, m.loadOptions()...)
	if err == nil {
		err = ValidateTargets(f)
	}
	if err == nil {
		err = ValidateTargetsAgainst(f, m.Get())
	}
	if err != nil {
		m.logger.Error("scheduled target reload rejected", "trigger", trigger, "error", err)
		m.Reloads.record(ReloadFileTargets, false)
		return fmt.Errorf("scheduled target file %s: %w", m.targetPath, err)
	}
	m.targetFile.Store(f)
	m.Reloads.record(ReloadFileTargets, true)
	m.logger.Info("scheduled targets reloaded", "trigger", trigger, "targets", len(f.Targets))
	return nil
}

// checkPythonLibrary rejects a declared library the image does not install.
// BeautifulSoup gets its own message: it was installed until lxml.html took
// over its job, so a configuration written for it needs pointing somewhere.
func checkPythonLibrary(collector, lib string) error {
	if transform.PythonLibraries[lib] {
		return nil
	}
	if lib == "beautifulsoup4" || lib == "bs4" {
		return fmt.Errorf("collector %q declares Python library %q, which the image no longer installs; parse HTML with lxml.html instead and declare lxml", collector, lib)
	}
	return fmt.Errorf("collector %q declares unsupported Python library %q; the supported libraries are lxml, PyYAML and python-dateutil", collector, lib)
}

// LogDeprecations warns once per deprecated spelling a loaded configuration
// used, so the operator hears about it on every start and reload until it is
// changed.
func LogDeprecations(logger *slog.Logger, path string, c *model.Config) {
	for _, message := range c.Deprecations {
		logger.Warn("deprecated configuration", "file", path, "deprecation", message)
	}
}

// normalizeErrorPolicy lower-cases a policy, maps the deprecated "warn" to
// "log" and records that it did, and rejects anything else.
func normalizeErrorPolicy(c *model.Config, collector, key string, value *string) error {
	policy := strings.ToLower(strings.TrimSpace(*value))
	switch policy {
	case model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore:
	case model.ErrorPolicyWarn:
		policy = model.ErrorPolicyLog
		c.Deprecations = append(c.Deprecations, fmt.Sprintf("collector %q %s: %q is deprecated; use %q, which means the same", collector, key, model.ErrorPolicyWarn, model.ErrorPolicyLog))
	default:
		return fmt.Errorf("collector %q %s has invalid value %q; want fail, log or ignore", collector, key, *value)
	}
	*value = policy
	return nil
}

// validateCache checks a collector's cache when the configuration loads.
func validateCache(c *model.Collector) error {
	if c.Cache.TTL < 0 {
		return fmt.Errorf("collector %q cache.ttl must not be negative", c.Name)
	}
	if c.Cache.StaleIfError < 0 {
		return fmt.Errorf("collector %q cache.stale_if_error must not be negative", c.Name)
	}
	return nil
}
