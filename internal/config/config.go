package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
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
	if x.Limits.MaxResponseBytes < 0 {
		return fmt.Errorf("collector %q limits.max_response_bytes must not be negative", x.Name)
	}
	if m := x.Limits.MaxScriptMemory; m < 0 || (m > 0 && m < minScriptMemory) {
		return fmt.Errorf("collector %q limits.max_script_memory must be 0, for no limit, or at least 32MiB, since it bounds the Python interpreter and its libraries too; got %d bytes", x.Name, m)
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
	decoderUnset := strings.TrimSpace(x.Decoder.Type) == ""
	if err := normalizeFormats(x); err != nil {
		return err
	}
	// Left unset, and not implied by the transform, the decoder is chosen
	// for each response. That works, but a response that changes its
	// Content-Type, or a file its extension, silently changes how it is
	// read, so the operator is told once on every start and reload.
	if decoderUnset && x.Decoder.Type == "auto" {
		c.Warnings = append(c.Warnings, undecidedDecoderWarning(x))
	}
	// Retries of a POST or PATCH need retry.non_idempotent, since sending one
	// again may repeat what it did; without it they are not made, which the
	// operator who set attempts is told.
	if x.Request.Retry.Attempts > 0 && !x.Request.Retry.NonIdempotent && !fetch.IdempotentMethod(x.Request.Method) {
		c.Warnings = append(c.Warnings, fmt.Sprintf("collector %q sets request.retry.attempts, but its method %s is not idempotent, so a failed request is not retried; set request.retry.non_idempotent to retry it anyway", x.Name, strings.ToUpper(x.Request.Method)))
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
		if err := normalizeErrorPolicy(x.Name, "error_handling."+policy.key, policy.value); err != nil {
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
	if err := checkCSVColumns(x); err != nil {
		return err
	}
	return transform.CheckTransformSettings(x)
}

// checkCSVColumns requires a csv transform reading rows without a header row
// to name its columns by number, from 1: with response.csv.header false
// there is no name to find a column by, and a rule naming one would find
// nothing in any row.
func checkCSVColumns(x *model.Collector) error {
	if x.Transform.Type != "csv" || x.Response.CSV.Header == nil || *x.Response.CSV.Header {
		return nil
	}
	check := func(what, column string) error {
		if n, err := strconv.Atoi(column); err != nil || n < 1 || strconv.Itoa(n) != column {
			return fmt.Errorf("collector %q %s reads column %q, but response.csv.header is false, so columns are named by number, from 1; write the column's number, such as \"2\"", x.Name, what, column)
		}
		return nil
	}
	for _, rule := range x.Metrics {
		if err := check(fmt.Sprintf("metric %q", rule.Name), rule.Expression); err != nil {
			return err
		}
		for _, label := range rule.Labels {
			if label.Static() {
				continue
			}
			if err := check(fmt.Sprintf("metric %q label %q", rule.Name, label.Name), label.Expression); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyLimitDefaults gives every limit left unset its default.
// limits.max_response_bytes is not filled in: left unset, request.max_response_bytes
// alone decides, and 10 MiB when neither is set (fetch.responseLimit).
// minScriptMemory is the least limits.max_script_memory: less than an
// interpreter with its libraries needs to start.
const minScriptMemory = 32 << 20

func applyLimitDefaults(l *model.Limits) {
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

// normalizeFormats lower-cases the transform and decoder, refuses a missing or
// unknown transform and an unknown decoder, and infers the decoder a transform
// implies.
func normalizeFormats(x *model.Collector) error {
	if err := decode.CheckCharset(x.Response.Charset); err != nil {
		return fmt.Errorf("collector %q response.charset: %w", x.Name, err)
	}
	x.Transform.Type = strings.ToLower(strings.TrimSpace(x.Transform.Type))
	switch {
	case x.Transform.Type == "":
		return fmt.Errorf("collector %q has no transform.type; it is required: %s", x.Name, strings.Join(model.TransformTypes, ", "))
	case !slices.Contains(model.TransformTypes, x.Transform.Type):
		return fmt.Errorf("collector %q has unknown transform %q; want one of %s", x.Name, x.Transform.Type, strings.Join(model.TransformTypes, ", "))
	}
	x.Decoder.Type = strings.ToLower(strings.TrimSpace(x.Decoder.Type))
	if x.Decoder.Type == "" {
		x.Decoder.Type = "auto"
	}
	if !slices.Contains(model.DecoderTypes, x.Decoder.Type) {
		return fmt.Errorf("collector %q has unknown decoder %q; want one of %s", x.Name, x.Decoder.Type, strings.Join(model.DecoderTypes, ", "))
	}
	if x.Decoder.Type == "auto" && x.Request.Type == fetch.RequestTypeGraphite {
		// The render API answers JSON a jq rule could read as it is, but
		// the graphite decoder is what a graphite collector is for; decoder
		// json reads the answer as it came.
		x.Decoder.Type = "graphite"
	}
	if x.Request.Type == fetch.RequestTypeGRPC {
		// A call is answered as JSON, whatever else its decoder could be.
		switch x.Decoder.Type {
		case "auto", "json":
			x.Decoder.Type = "json"
		default:
			return fmt.Errorf("collector %q decodes with %s, but a grpc collector's answer is JSON; leave decoder.type out, or set json", x.Name, x.Decoder.Type)
		}
		switch x.Transform.Type {
		case "jq", "yq", "python":
		default:
			return fmt.Errorf("collector %q transforms with %s, which cannot read the JSON a grpc call is answered with; use jq, yq or python", x.Name, x.Transform.Type)
		}
	}
	if x.Decoder.Type == "auto" {
		switch x.Transform.Type {
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
	if x.Transform.Type == "python" && strings.TrimSpace(x.Transform.Script) == "" {
		return fmt.Errorf("collector %q Python transform requires a script", x.Name)
	}
	return checkGraphiteResponse(x)
}

// checkGraphiteResponse checks response.graphite; value left out is last. It
// applies to the graphite decoder, which a decoder chosen per response may
// turn out to be; a collector whose decoder is another refuses it, since it
// would be ignored. The graphite decoder's document is JSON-like, so only the
// transforms that read one can map it.
func checkGraphiteResponse(x *model.Collector) error {
	g := &x.Response.Graphite
	set := *g != model.GraphiteConfig{}
	if set && x.Decoder.Type != "graphite" && x.Decoder.Type != "auto" {
		return fmt.Errorf("collector %q sets response.graphite, which applies to the graphite decoder, but its decoder is %s", x.Name, x.Decoder.Type)
	}
	g.Value = strings.ToLower(strings.TrimSpace(g.Value))
	if g.Value != "" && !slices.Contains(model.GraphiteValues, g.Value) {
		return fmt.Errorf("collector %q response.graphite.value is %q; want one of %s", x.Name, g.Value, strings.Join(model.GraphiteValues, ", "))
	}
	if g.MaxAge < 0 {
		return fmt.Errorf("collector %q response.graphite.max_age must not be negative", x.Name)
	}
	g.InvalidLines = strings.ToLower(strings.TrimSpace(g.InvalidLines))
	if g.InvalidLines != "" && !slices.Contains(model.GraphiteInvalidLines, g.InvalidLines) {
		return fmt.Errorf("collector %q response.graphite.invalid_lines is %q; want fail or skip", x.Name, g.InvalidLines)
	}
	if x.Decoder.Type == "graphite" {
		switch x.Transform.Type {
		case "jq", "yq", "python":
		default:
			return fmt.Errorf("collector %q decodes Graphite series, which a %s transform cannot read; use jq, yq or python, whose rules read the series document, as in items: .series[]", x.Name, x.Transform.Type)
		}
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
		if err := normalizeErrorPolicy(x.Name, fmt.Sprintf("metric %q error_mode", r.Name), &r.ErrorMode); err != nil {
			return err
		}
		// A prometheus transform's rule without a type keeps the type of
		// the series it passes through: a counter stays a counter, a
		// histogram a histogram. Every other rule makes its own samples,
		// gauges unless it says otherwise.
		if r.Type == "" && x.Transform.Type != "prometheus" {
			r.Type = model.GaugeMetricType
		}
		switch r.Type {
		case model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType:
		case "":
		case model.HistogramMetricType, model.SummaryMetricType:
			// Only a series that is one already has buckets or quantiles to
			// expose; a rule reading one number would expose a histogram
			// with a single plain sample, which no parser accepts.
			if x.Transform.Type != "prometheus" {
				return fmt.Errorf("collector %q metric %q has type %s, which only a prometheus transform can give, passing through a %s that has its buckets or quantiles; a %s rule reads one value, so use gauge, counter or untyped", x.Name, r.Name, r.Type, r.Type, x.Transform.Type)
			}
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
			if err := model.CheckLabelName(label.Name); err != nil {
				return fmt.Errorf("collector %q metric %q: %w", x.Name, r.Name, err)
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
	return transform.CheckLabelValueMapsAgree(x)
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

type loadOptions struct {
	expandEnv bool
	// flag is the flag that turned expansion on, named when a reference
	// cannot be expanded.
	flag string
}

// Each document has its own flag for expansion: the configuration and its
// collector files --config.expand-env, the static target file
// --static-targets.expand-env. The static target file carries the addresses
// and credentials of what is scraped, which an operator may want from the
// environment while the configuration is committed as written, or the other
// way round.
const (
	configEnvFlag        = "--config.expand-env"
	staticTargetsEnvFlag = "--static-targets.expand-env"
)

// WithEnvExpansion substitutes ${NAME} references from the process environment
// before the configuration is parsed. It is what --config.expand-env turns on.
func WithEnvExpansion() LoadOption {
	return func(o *loadOptions) { o.expandEnv, o.flag = true, configEnvFlag }
}

// WithStaticTargetsEnvExpansion does for the static target file what
// WithEnvExpansion does for the configuration. It is what
// --static-targets.expand-env turns on.
func WithStaticTargetsEnvExpansion() LoadOption {
	return func(o *loadOptions) { o.expandEnv, o.flag = true, staticTargetsEnvFlag }
}

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
	return expandEnvironment(path, b, options.flag)
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
	if err = withoutExtensionKeys(dec.Decode(&c)); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("configuration file %s is empty; it must define collectors", path)
		}
		return nil, yamlError(err)
	}
	if err = oneDocument(dec); err != nil {
		return nil, err
	}
	if err = mergeCollectorFiles(&c, path, opts); err != nil {
		return nil, err
	}
	if err = Validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Manager holds the configuration in force and the static target file,
// and reloads them: when the watch finds a file changed, on SIGHUP and on
// POST /-/reload. A rejected reload leaves the previous configuration in
// force.
type Manager struct {
	current atomic.Value
	path    string
	logger  *slog.Logger
	// lastMod and lastSize are the configuration file's modification time
	// and size when it was last read: a change to either is a change, so a
	// file replaced by one with an older time, as cp -p and rsync -t leave
	// it, is read too.
	lastMod  time.Time
	lastSize int64
	// collectorFiles is the stamp of the collector files the configuration
	// read when last loaded (collectorFilesStamp), and watchedFiles the
	// collector_files entries it was taken from: those of the file last
	// read, even when it was refused, so fixing a collector file it added
	// is a change the watch sees.
	collectorFiles string
	watchedFiles   []string
	targetPath     string
	targetFile     atomic.Pointer[model.StaticTargetFile]
	targetsLastMod time.Time
	// targetsLastSize is lastSize for the static target file.
	targetsLastSize int64
	pythonPath      string
	watchInterval   time.Duration
	expandEnv       bool
	// expandStaticTargetsEnv is expandEnv for the static target file.
	expandStaticTargetsEnv bool
	// configWaits and targetsWaits say that file's last reload was refused
	// only because the other file, as in force, disagrees with it, so a
	// change to the other file reads it again (apply).
	configWaits, targetsWaits bool
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
		m.watchCollectorFiles(c.CollectorFiles)
	}
	// The file as read at startup is not a change for the first watch tick.
	if st, err := os.Stat(path); err == nil && c != nil {
		m.lastMod, m.lastSize = st.ModTime(), st.Size()
	}
	return m
}

// Stamp is what the watch compares files against to see a change: their
// modification times and sizes, and the collector files a configuration
// lists. Taken before the files are read, it makes a change written while
// they were being read a change the next watch tick sees, rather than part of
// what was read.
type Stamp struct {
	mod            time.Time
	size           int64
	found          bool
	entries        []string
	collectorFiles string
}

// TakeStamp stamps the file at path as it is now. For a configuration file,
// listsCollectorFiles, it stamps the collector files it lists too.
func TakeStamp(path string, listsCollectorFiles bool, opts ...LoadOption) Stamp {
	var s Stamp
	if st, err := os.Stat(path); err == nil {
		s.mod, s.size, s.found = st.ModTime(), st.Size(), true
	}
	if listsCollectorFiles {
		if listed, ok := listedCollectorFiles(path, opts); ok {
			s.entries = listed
			s.collectorFiles = collectorFilesStamp(path, listed)
		}
	}
	return s
}

// UseStamp makes s, taken before the configuration in force was read, what
// the watch compares the configuration and its collector files against.
func (m *Manager) UseStamp(s Stamp) {
	if s.found {
		m.lastMod, m.lastSize = s.mod, s.size
	}
	if s.entries != nil {
		m.watchedFiles, m.collectorFiles = s.entries, s.collectorFiles
	}
}

// UseTargetsStamp is UseStamp for the static target file.
func (m *Manager) UseTargetsStamp(s Stamp) {
	if s.found {
		m.targetsLastMod, m.targetsLastSize = s.mod, s.size
	}
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

// SetEnvExpansion records that the configuration was read with ${NAME}
// expansion, so a reload reads it the same way. A reload that quietly stopped
// expanding would replace a working configuration with one full of literal
// references.
func (m *Manager) SetEnvExpansion(expand bool) { m.expandEnv = expand }

// SetStaticTargetsEnvExpansion is SetEnvExpansion for the static target file.
func (m *Manager) SetStaticTargetsEnvExpansion(expand bool) { m.expandStaticTargetsEnv = expand }

// loadOptions returns the options the configuration was first read with.
func (m *Manager) loadOptions() []LoadOption {
	if m.expandEnv {
		return []LoadOption{WithEnvExpansion()}
	}
	return nil
}

// staticTargetsLoadOptions returns the options the static target file was
// first read with.
func (m *Manager) staticTargetsLoadOptions() []LoadOption {
	if m.expandStaticTargetsEnv {
		return []LoadOption{WithStaticTargetsEnvExpansion()}
	}
	return nil
}

// SetPythonPath records the interpreter used to check collector Python
// scripts, so a reloaded configuration is held to the same contract as the one
// the exporter started with.
func (m *Manager) SetPythonPath(path string) { m.pythonPath = path }

// SetTargets installs the static target document and the file it was read
// from. An empty path leaves the feature disabled.
func (m *Manager) SetTargets(path string, f *model.StaticTargetFile) {
	m.targetPath = path
	if f != nil {
		m.targetFile.Store(f)
	}
	if path != "" && f != nil {
		m.Reloads.loaded(ReloadFileStaticTargets)
	}
	if path != "" {
		if st, err := os.Stat(path); err == nil {
			m.targetsLastMod, m.targetsLastSize = st.ModTime(), st.Size()
		}
	}
}

// StaticTargets returns the static targets currently in force.
func (m *Manager) StaticTargets() []model.StaticTarget {
	f := m.targetFile.Load()
	if f == nil {
		return nil
	}
	return f.Targets
}

// StaticTargetFile returns the static target file in force, nil without one.
// A reload that changes the targets stores a new file, so the pointer tells a
// reader whether the targets changed since it last looked.
func (m *Manager) StaticTargetFile() *model.StaticTargetFile { return m.targetFile.Load() }

// StaticTargetConcurrency is how many static targets are scraped at once, as
// the file in force says.
func (m *Manager) StaticTargetConcurrency() int {
	return m.targetFile.Load().ScrapeConcurrency()
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
			m.reloadChanged()
		}
	}
}

// The configuration is reloaded when the watch sees a file change
// (reloadChanged), on SIGHUP, and on POST /-/reload when the lifecycle API is
// enabled (Reload). Every reload goes through apply under reloadMu, so two
// triggers at once never interleave, and each is logged with what triggered
// it.
const (
	reloadTriggerWatch  = "watch"
	ReloadTriggerSignal = "sighup"
	ReloadTriggerHTTP   = "http"
)

// The configuration and the static target file are checked against each
// other — a target names a collector, and one exported over OTLP needs OTLP
// on — so a change that spans both, such as removing a collector and the
// target that uses it, is only valid as a whole. apply therefore reads the
// files it reloads together and installs them together when they agree. A
// file refused only because the other one, as in force, disagrees with it
// waits: when the other file changes, the watch reads it again with it,
// although its own modification time has been seen. Without this a change
// written to both files, in either order or at once, left the configuration
// refused until it was touched again.

// reloadChanged reloads what the watch finds changed: the configuration, when
// the file or one of its collector files changed, and the static target file,
// when it changed; and with either, the other when it waits for it.
func (m *Manager) reloadChanged() {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	configChanged, targetsChanged := m.configChanged(), m.targetsChanged()
	doConfig := configChanged || targetsChanged && m.configWaits
	doTargets := targetsChanged || configChanged && m.targetsWaits
	if !doConfig && !doTargets {
		return
	}
	_ = m.apply(reloadTriggerWatch, doConfig, doTargets)
}

// configChanged reports whether the configuration file, or one of its
// collector files, changed since it was last read.
func (m *Manager) configChanged() bool {
	st, err := os.Stat(m.path)
	if err != nil {
		return false
	}
	// A collector file edited, added or removed is a change too, although the
	// configuration file itself is untouched.
	return !st.ModTime().Equal(m.lastMod) || st.Size() != m.lastSize || collectorFilesStamp(m.path, m.watchedFiles) != m.collectorFiles
}

// targetsChanged reports whether the static target file changed since it was
// last read.
func (m *Manager) targetsChanged() bool {
	if m.targetPath == "" {
		return false
	}
	st, err := os.Stat(m.targetPath)
	return err == nil && (!st.ModTime().Equal(m.targetsLastMod) || st.Size() != m.targetsLastSize)
}

// Reload reloads the configuration, and the static target file when there
// is one, now, whether or not they changed, and returns why either was
// rejected. A rejected file leaves the previous one in force.
func (m *Manager) Reload(trigger string) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	return m.apply(trigger, true, m.targetPath != "")
}

// apply reads the configuration when doConfig and the static target file when
// doTargets, and installs what is valid. reloadMu is held.
func (m *Manager) apply(trigger string, doConfig, doTargets bool) error {
	var errs []error
	var cfg *model.Config
	var targets *model.StaticTargetFile
	if doConfig {
		c, err := m.loadConfig()
		if err != nil {
			errs = append(errs, m.rejectConfig(trigger, err, false))
		}
		cfg = c
	}
	if doTargets {
		f, err := m.loadTargets()
		if err != nil {
			errs = append(errs, m.rejectTargets(trigger, err, false))
		}
		targets = f
	}
	if cfg == nil && targets == nil {
		return errors.Join(errs...)
	}
	// Together: each read file with the other as read, or as in force.
	pairConfig, pairTargets := cfg, targets
	if pairConfig == nil {
		pairConfig = m.Get()
	}
	if pairTargets == nil {
		pairTargets = m.targetFile.Load()
	}
	if agree(pairTargets, pairConfig) == nil {
		if cfg != nil {
			m.installConfig(trigger, cfg)
		}
		if targets != nil {
			m.installTargets(trigger, targets)
		}
		return errors.Join(errs...)
	}
	// They disagree. When both were read, one may still go alone, with the
	// other in force; what is left is refused, and waits for the other file.
	if cfg != nil && agree(m.targetFile.Load(), cfg) == nil {
		m.installConfig(trigger, cfg)
		cfg = nil
	}
	if targets != nil && agree(targets, m.Get()) == nil {
		m.installTargets(trigger, targets)
		targets = nil
	}
	if cfg != nil {
		errs = append(errs, m.rejectConfig(trigger, agree(m.targetFile.Load(), cfg), true))
	}
	if targets != nil {
		errs = append(errs, m.rejectTargets(trigger, agree(targets, m.Get()), true))
	}
	return errors.Join(errs...)
}

// agree checks a static target file against a configuration; no file agrees
// with every configuration.
func agree(f *model.StaticTargetFile, c *model.Config) error {
	if f == nil {
		return nil
	}
	return ValidateStaticTargetsAgainst(f, c)
}

// watchCollectorFiles makes entries the collector files the watch stamps.
func (m *Manager) watchCollectorFiles(entries []string) {
	m.watchedFiles = entries
	m.collectorFiles = collectorFilesStamp(m.path, entries)
}

// listedCollectorFiles is the collector_files a configuration file lists,
// read without the rest of it, which may be what is wrong with it.
func listedCollectorFiles(path string, opts []LoadOption) ([]string, bool) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, false
	}
	var listed struct {
		CollectorFiles []string `yaml:"collector_files"`
	}
	if yaml.Unmarshal(b, &listed) != nil {
		return nil, false
	}
	return listed.CollectorFiles, true
}

// loadConfig reads and checks the configuration on its own. reloadMu is held.
func (m *Manager) loadConfig() (*model.Config, error) {
	if st, err := os.Stat(m.path); err == nil {
		m.lastMod, m.lastSize = st.ModTime(), st.Size()
	}
	// The collector files watched from now are the ones this file lists,
	// read on its own, so a refused file that added one is read again
	// when that one is fixed; a file that cannot say keeps the old list.
	entries := m.Get().CollectorFiles
	if listed, ok := listedCollectorFiles(m.path, m.loadOptions()); ok {
		entries = listed
	}
	m.watchCollectorFiles(entries)
	c, err := Load(m.path, m.loadOptions()...)
	if err != nil {
		return nil, err
	}
	if err := transform.ValidatePythonScripts(m.pythonPath, c); err != nil {
		return nil, err
	}
	return c, nil
}

// loadTargets reads and checks the static target file on its own. reloadMu is
// held.
func (m *Manager) loadTargets() (*model.StaticTargetFile, error) {
	if st, err := os.Stat(m.targetPath); err == nil {
		m.targetsLastMod, m.targetsLastSize = st.ModTime(), st.Size()
	}
	f, err := LoadStaticTargets(m.targetPath, m.staticTargetsLoadOptions()...)
	if err == nil {
		err = ValidateStaticTargets(f)
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// installConfig puts c in force. reloadMu is held.
func (m *Manager) installConfig(trigger string, c *model.Config) {
	m.current.Store(c)
	m.configWaits = false
	m.Reloads.record(reloadFileConfig, true)
	// Interpreters of scripts this reload removed or changed are stopped now
	// rather than after the idle timeout.
	transform.PythonWorkers().Retain(transform.PythonWorkerKeys(m.pythonPath, c))
	// The new configuration may list other collector files than loadConfig
	// took them to be; only then are they stamped again, since stamping them
	// now would make a change written while they were read part of what was.
	if !slices.Equal(m.watchedFiles, c.CollectorFiles) {
		m.watchCollectorFiles(c.CollectorFiles)
	}
	LogNotices(m.logger, m.path, c)
	m.logger.Info("configuration reloaded", "trigger", trigger, "collectors", len(c.Collectors), "collector_files", len(c.LoadedCollectorFiles))
}

// installTargets puts f in force. reloadMu is held.
func (m *Manager) installTargets(trigger string, f *model.StaticTargetFile) {
	m.targetFile.Store(f)
	m.targetsWaits = false
	m.Reloads.record(ReloadFileStaticTargets, true)
	m.logger.Info("static targets reloaded", "trigger", trigger, "targets", len(f.Targets))
}

// rejectConfig refuses a configuration, which stays as it was. waits says it
// was refused only by the static target file in force, and is read again when
// that changes. reloadMu is held.
func (m *Manager) rejectConfig(trigger string, err error, waits bool) error {
	m.configWaits = waits
	attrs := []any{"trigger", trigger, "error", err}
	if waits {
		attrs = append(attrs, "retried_when", "the static target file changes")
	}
	m.logger.Error("configuration reload rejected", attrs...)
	m.Reloads.record(reloadFileConfig, false)
	return fmt.Errorf("configuration %s: %w", m.path, err)
}

// rejectTargets is rejectConfig for the static target file.
func (m *Manager) rejectTargets(trigger string, err error, waits bool) error {
	m.targetsWaits = waits
	attrs := []any{"trigger", trigger, "error", err}
	if waits {
		attrs = append(attrs, "retried_when", "the configuration changes")
	}
	m.logger.Error("static target reload rejected", attrs...)
	m.Reloads.record(ReloadFileStaticTargets, false)
	return fmt.Errorf("static target file %s: %w", m.targetPath, err)
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

// LogNotices logs what Validate recorded about a loaded configuration
// without refusing it: once per deprecated spelling it used and once per
// warning, so the operator hears about each on every start and reload until it
// is changed.
func LogNotices(logger *slog.Logger, path string, c *model.Config) {
	for _, message := range c.Deprecations {
		logger.Warn("deprecated configuration", "file", path, "deprecation", message)
	}
	for _, message := range c.Warnings {
		logger.Warn("configuration warning", "file", path, "warning", message)
	}
}

// undecidedDecoderWarning says how a collector without a decoder.type, whose
// transform implies none, decodes what it reads.
func undecidedDecoderWarning(x *model.Collector) string {
	how := "by the Content-Type header of each response"
	if x.Request.Type == fetch.RequestTypeLocalFile {
		how = "by the extension of each file"
	}
	return fmt.Sprintf("collector %q sets no decoder.type, so it decodes %s, and by the content when that does not say; set decoder.type to fix the decoder", x.Name, how)
}

// normalizeErrorPolicy lower-cases a policy and rejects anything but fail,
// log and ignore.
func normalizeErrorPolicy(collector, key string, value *string) error {
	policy := strings.ToLower(strings.TrimSpace(*value))
	switch policy {
	case model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore:
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
