package config

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

var targetNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// LoadStaticTargets reads the static target document. A target file carries
// the addresses and credentials of the things being scraped, which is exactly
// the material an operator wants to keep out of a committed file, so it may
// take them from the environment: WithStaticTargetsEnvExpansion, which
// --static-targets.expand-env turns on, independently of the configuration's
// --config.expand-env.
func LoadStaticTargets(path string, opts ...LoadOption) (*model.StaticTargetFile, error) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, err
	}
	var f model.StaticTargetFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = withValueProblems(withoutExtensionKeys(dec.Decode(&f)), b, &f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("static target file %s is empty; it must define targets", path)
		}
		return nil, yamlError(err)
	}
	if err = oneDocument(dec); err != nil {
		return nil, err
	}
	return &f, nil
}

// ValidateStaticTargets checks the document in isolation. ValidateStaticTargetsAgainst
// applies the checks that need the exporter configuration.
func ValidateStaticTargets(f *model.StaticTargetFile) error {
	if len(f.Targets) == 0 {
		return errors.New("targets must not be empty")
	}
	// The file's interval is required: a target scraped on a default nobody
	// wrote down is a scrape rate nobody chose.
	switch {
	case f.Interval == 0:
		return errors.New("interval is required: how often a target that sets none is scraped, such as 1m")
	case f.Interval < model.Duration(time.Second):
		return fmt.Errorf("interval %s is under the least, 1s", time.Duration(f.Interval))
	}
	// Past the limit targets wait for a slot within their interval, and one
	// that finds none is skipped; the limit only bounds, so a negative one has
	// no meaning.
	if f.Concurrency < 0 {
		return errors.New("concurrency must not be negative; leave it out, or 0, for the default, 8")
	}
	defaultInterval := f.Interval
	seen := map[string]bool{}
	for i := range f.Targets {
		t := &f.Targets[i]
		if strings.TrimSpace(t.Collector) == "" {
			return fmt.Errorf("target %d has no collector", i)
		}
		if t.Name == "" {
			t.Name = fmt.Sprintf("%s_%d", t.Collector, i)
		}
		if !targetNameRE.MatchString(t.Name) {
			return fmt.Errorf("target %q has invalid name", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate target %q", t.Name)
		}
		seen[t.Name] = true
		if t.Request.Method != "" {
			t.Request.Method = strings.ToUpper(t.Request.Method)
			switch t.Request.Method {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
			default:
				return fmt.Errorf("target %q has unsupported method %q", t.Name, t.Request.Method)
			}
		}
		// A static target is scraped on the exporter's own timer, with no
		// probe to supply a path parameter, so a placeholder here could only
		// ever take its default — or fail every scrape. Write the path out.
		if fetch.HasPathParams(t.Request.Path) {
			return fmt.Errorf("target %q request.path cannot use {{param_...}} placeholders: a static target has no probe to supply them, so write the path out in full", t.Name)
		}
		// Nor anywhere else the target writes a value of its own: they are
		// sent as written, so a placeholder would reach the target as text.
		// This holds for a value an environment reference supplied too,
		// since the file is checked after it is expanded.
		if err := refuseParamPlaceholders(t); err != nil {
			return err
		}
		for name := range t.Params {
			if !fetch.PathParamName.MatchString(name) {
				return fmt.Errorf("target %q params has %q, which is not a parameter name; names are %s<name>, with letters, digits and underscores, as the placeholders they fill", t.Name, name, fetch.PathParamPrefix)
			}
		}
		if t.Request.Timeout < 0 {
			return fmt.Errorf("target %q request.timeout must not be negative", t.Name)
		}
		// Statuses and codes are written as a scrape compares them here,
		// once, while nothing reads the file yet: the check against the
		// configuration, which a reload repeats on the file in force,
		// only reads them.
		fetch.NormalizeTargetRequest(t)
		switch {
		case t.Interval < 0:
			return fmt.Errorf("target %q interval must not be negative", t.Name)
		case t.Interval == 0:
			t.Interval = defaultInterval
		}
		if t.Interval < model.Duration(time.Second) {
			return fmt.Errorf("target %q interval %s is under the least, 1s", t.Name, time.Duration(t.Interval))
		}
		// A scrape is bounded by its interval, so the next one never finds it
		// still running; a longer timeout could never take effect.
		if t.Request.Timeout > t.Interval {
			return fmt.Errorf("target %q request.timeout %s is longer than its interval %s; a scrape must end before the next is due", t.Name, time.Duration(t.Request.Timeout), time.Duration(t.Interval))
		}
		if retry := t.Request.Retry; retry != nil {
			if retry.Attempts != nil && (*retry.Attempts < 0 || *retry.Attempts > fetch.MaxRetryAttempts) {
				return fmt.Errorf("target %q request.retry.attempts must be from 0 to %d", t.Name, fetch.MaxRetryAttempts)
			}
			if retry.Backoff != nil && *retry.Backoff < 0 {
				return fmt.Errorf("target %q request.retry.backoff must not be negative", t.Name)
			}
		}
		if t.Request.BearerToken != "" && t.Request.BearerTokenFile != "" {
			return fmt.Errorf("target %q cannot set both request.bearer_token and request.bearer_token_file", t.Name)
		}
		if t.Request.BasicAuth != nil && t.Request.BasicAuthFile != nil {
			return fmt.Errorf("target %q cannot set both request.basic_auth and request.basic_auth_file", t.Name)
		}
		if (t.Request.BasicAuth != nil || t.Request.BasicAuthFile != nil) && (t.Request.BearerToken != "" || t.Request.BearerTokenFile != "") {
			return fmt.Errorf("target %q cannot configure basic and bearer authentication together", t.Name)
		}
		if t.Request.BasicAuthFile != nil && (strings.TrimSpace(t.Request.BasicAuthFile.Username) == "" || strings.TrimSpace(t.Request.BasicAuthFile.Password) == "") {
			return fmt.Errorf("target %q basic_auth_file requires username and password paths", t.Name)
		}
		for name := range t.Request.Headers {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("target %q has a request header without a name", t.Name)
			}
		}
		for name := range t.Labels {
			if !model.ValidLabelName(name) {
				return fmt.Errorf("target %q has invalid label name %q", t.Name, name)
			}
			if err := model.CheckLabelName(name); err != nil {
				return fmt.Errorf("target %q: %w", t.Name, err)
			}
			// static_target names the target on the endpoint, so the
			// target's series cannot claim it for something else.
			if name == StaticTargetLabel {
				return fmt.Errorf("target %q labels sets %s, which the static targets endpoint sets to the target's name", t.Name, StaticTargetLabel)
			}
			// Prometheus sets job and instance on every series it scrapes.
			// The endpoint is scraped keeping the series' own labels, as the
			// chart's monitor does, so a target's job or instance would move
			// its series out of the job that scrapes the endpoint.
			if name == "job" || name == "instance" {
				return fmt.Errorf("target %q labels sets %s, which Prometheus sets when it scrapes the static targets endpoint; with honor_labels the target's would replace it, so name the label something else, such as task", t.Name, name)
			}
		}
		// The OTLP resource identity is only used by a target exported over
		// OTLP; set without it, it would be quietly ignored.
		if !t.ExportViaOTLP && (t.OTLP.ServiceName != "" || len(t.OTLP.ResourceAttributes) > 0) {
			return fmt.Errorf("target %q sets otlp, which only a target with export_via_otlp: true uses", t.Name)
		}
		// As for the exporter-wide otlp block: service_name is the
		// resource's service.name, which the attributes must not set again.
		if _, twice := t.OTLP.ResourceAttributes[otlpServiceNameAttribute]; twice {
			return fmt.Errorf("target %q otlp.resource_attributes sets %s, which otlp.service_name sets; write the name as the target's otlp.service_name", t.Name, otlpServiceNameAttribute)
		}
	}
	return nil
}

// StaticTargetLabel is the label the static targets endpoint puts on every
// series of a target, naming it: the targets share one endpoint, and it keeps
// their series apart.
const StaticTargetLabel = "static_target"

// staticTargetSeriesLabels are the labels the exporter adds to the series of
// a static target after the scrape's validation, which holds every other
// label name to limits.max_label_name_length, longest first: static_target
// on every series the endpoint serves and OTLP exports of it (the exporter's
// withStaticTargetLabel), and collector and target on its health series
// (staticTargetHealthMetrics). A label added before the validation, such as
// the file label of a collector that reads a directory, fails the scrape
// when it is too long, as any label does.
var staticTargetSeriesLabels = []struct{ name, on string }{
	{StaticTargetLabel, "every series of it"},
	{"collector", "its health series"},
	{"target", "its health series"},
}

// ValidateStaticTargetsAgainst enforces the preconditions that depend on the
// exporter configuration: every target must name a configured collector, and a
// target exported over OTLP needs OTLP export enabled. It writes into neither:
// a reload checks the file in force against the configuration it read, and
// the file it read against the configuration in force, while scrapes read
// both.
func ValidateStaticTargetsAgainst(f *model.StaticTargetFile, c *model.Config) error {
	if hook := targetsValidatedHook.Load(); hook != nil {
		(*hook)()
	}
	for i := range f.Targets {
		t := &f.Targets[i]
		if !t.ExportViaOTLP {
			continue
		}
		if !c.OTLP.Enabled {
			return fmt.Errorf("target %q sets export_via_otlp, which needs OTLP export; set otlp.enabled: true and otlp.endpoint, or leave the target to the static targets endpoint", t.Name)
		}
		if strings.TrimSpace(c.OTLP.Endpoint) == "" {
			return fmt.Errorf("target %q sets export_via_otlp, which needs otlp.endpoint", t.Name)
		}
	}
	collectors := collectorsByName(c)
	// The placeholders of a collector are found once for the check, for its
	// first target, and not again for each of its others.
	var params fetch.RequestParamsCheck
	for i := range f.Targets {
		t := &f.Targets[i]
		collector := collectors[t.Collector]
		if collector == nil {
			return fmt.Errorf("target %q references unknown collector %q", t.Name, t.Collector)
		}
		// What a target may be depends on the collector's request type: an
		// absolute URL for http, a file under request.root for localfile,
		// which may also leave it out.
		if err := fetch.CheckTarget(collector, t.Target, true); err != nil {
			if errors.Is(err, fetch.ErrMissingTarget) {
				return fmt.Errorf("target %q has no target address", t.Name)
			}
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		if err := fetch.CheckTargetRequest(t, collector); err != nil {
			return err
		}
		if err := checkRetriesFitTheInterval(t, collector); err != nil {
			return err
		}
		// A target's labels go on every series of its collector, past the
		// scrape's validation, so they are held here to the collector's
		// limits.max_label_name_length, which every other label name of
		// those series is held to.
		for _, name := range model.SortedKeys(t.Labels) {
			if limit := collector.Limits.MaxLabelNameLength; limit > 0 && len(name) > limit {
				return fmt.Errorf("target %q label name %q is %d bytes, longer than limits.max_label_name_length %d of collector %q; shorten it or raise the collector's limit", t.Name, name, len(name), limit, t.Collector)
			}
		}
		// So are the labels the exporter itself adds to the target's
		// series past the validation.
		for _, added := range staticTargetSeriesLabels {
			if limit := collector.Limits.MaxLabelNameLength; limit > 0 && len(added.name) > limit {
				return fmt.Errorf("target %q gets the label %s, %d bytes, on %s, longer than limits.max_label_name_length %d of collector %q; raise the collector's limit to %d or more", t.Name, added.name, len(added.name), added.on, limit, t.Collector, len(added.name))
			}
		}
		// Every placeholder of the collector, in its request or in a label
		// value (fetch/labelparams.go), must be filled, by the target's
		// params or a default, since nothing else can fill it; and every
		// param must fill one, since an unused one is a misspelling.
		// Catching both here makes them startup errors naming both sides.
		unused, err := params.CheckRequestParams(collector, fetch.TargetOverrides(t))
		var missing *fetch.MissingParamError
		if errors.As(err, &missing) {
			// Only a path the target sets itself replaces the
			// collector's placeholder; a query value, header or body
			// placeholder has no such way around it.
			alternatives := "set it under the target's params, or give the placeholder a default"
			if missing.Where == "request.path" {
				alternatives = "set it under the target's params, give the placeholder a default, or set request.path on the target"
			}
			return fmt.Errorf("target %q uses collector %q, whose %s needs %s, a parameter without a default; a static target has no probe to supply it, so %s", t.Name, t.Collector, missing.Where, missing.Name, alternatives)
		}
		if err != nil {
			return fmt.Errorf("target %q uses collector %q: %w", t.Name, t.Collector, err)
		}
		if len(unused) > 0 {
			return fmt.Errorf("target %q params %s are not used by collector %q: no placeholder in its request or its label values names them", t.Name, strings.Join(unused, ", "), t.Collector)
		}
	}
	return nil
}

// A static target names its collector, and every check of the target
// against the configuration needs that collector. Going through the
// configuration's collectors for it (model.CollectorByName) costs as much as
// the configuration is large, and the check went through them four times
// for every target, and once more for each configuration to learn which
// descriptor files it opens (targetsChecked): for 10,000 targets of as many
// collectors that was seconds of a reload, which checked the pair up to
// five times. So where the collectors are is noted once for a check, by their
// names, and each target's is found there. The exporter keeps the same for
// the configuration it follows (internal/exporter, fingerprintGeneration);
// that one lives with what the exporter keeps for a configuration in force,
// and the check is also of a configuration that is not in force yet.

// targetsValidatedHook, set by tests, is called at each check of a static
// target file against a configuration (ValidateStaticTargetsAgainst), so a
// test can count the checks a reload makes: one for each pair of the two it
// asks about, however often it asks (Manager.apply).
var targetsValidatedHook atomic.Pointer[func()]

// collectorsIndexedHook, set by tests, is called whenever the collectors of
// a configuration are gone through to note where each is (collectorsByName):
// once for each time, not for each collector, so a test can count the times
// and see that it is once for a check, not once for each static target.
var collectorsIndexedHook atomic.Pointer[func()]

// collectorsByName is the collectors of c by their names, each the one
// model.CollectorByName(c, name) returns: the very collector of c and not a
// copy, and of two that share a name the first, which is why they are gone
// through from the last. A loaded configuration has no two of one name
// (Validate), and a name it has none of is not in the map.
func collectorsByName(c *model.Config) map[string]*model.Collector {
	if hook := collectorsIndexedHook.Load(); hook != nil {
		(*hook)()
	}
	byName := make(map[string]*model.Collector, len(c.Collectors))
	for i := len(c.Collectors) - 1; i >= 0; i-- {
		byName[c.Collectors[i].Name] = &c.Collectors[i]
	}
	return byName
}

// paramPlaceholder is `{{param_`, spaces allowed after the braces, which
// opens a probe parameter placeholder (fetch/requesttemplate.go). Only it is
// looked for: a body may well contain braces of its own.
var paramPlaceholder = regexp.MustCompile(`\{\{\s*` + fetch.PathParamPrefix)

// refuseParamPlaceholders refuses a {{param_...}} placeholder in a value the
// target writes itself — its target, request.body, header values, a
// graphite collector's request.targets and a grpc collector's
// request.message and metadata values; its
// request.path is checked with the collector's stricter rule. Such values are
// sent as written, with no probe to fill a placeholder; the target's params
// are what fill the collector's.
func refuseParamPlaceholders(t *model.StaticTarget) error {
	refuse := func(where string) error {
		return fmt.Errorf("target %q %s cannot use {{param_...}} placeholders: a static target's own values are sent as written, with no probe to fill them; write the value out in full, or fill the collector's placeholders under params", t.Name, where)
	}
	if paramPlaceholder.MatchString(t.Target) {
		return refuse("target")
	}
	if paramPlaceholder.MatchString(t.Request.Body) {
		return refuse("request.body")
	}
	if paramPlaceholder.MatchString(t.Request.Message) {
		return refuse("request.message")
	}
	for _, name := range model.SortedKeys(t.Request.Metadata) {
		if paramPlaceholder.MatchString(t.Request.Metadata[name]) {
			return refuse("request.metadata " + name)
		}
	}
	names := make([]string, 0, len(t.Request.Headers))
	for name := range t.Request.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if paramPlaceholder.MatchString(t.Request.Headers[name]) {
			return refuse("request.headers " + name)
		}
	}
	for i, expression := range t.Request.Targets {
		if paramPlaceholder.MatchString(expression) {
			return refuse(fmt.Sprintf("request.targets[%d]", i))
		}
	}
	return nil
}

// checkRetriesFitTheInterval refuses retries that could never all be made: a
// scrape ends with its interval, so when the waits between the attempts alone
// fill it, the last retries are cut off every time a target fails. Retries
// that fit may still be cut short by slow attempts; that depends on the
// target, not the file.
func checkRetriesFitTheInterval(t *model.StaticTarget, c *model.Collector) error {
	if c == nil {
		return nil
	}
	retry := t.Request.Retry.Over(c.Request.Retry)
	attempts, backoff := retry.Attempts, time.Duration(retry.Backoff)
	if attempts <= 0 || backoff <= 0 {
		return nil
	}
	waiting := time.Duration(attempts) * backoff
	if waiting < time.Duration(t.Interval) {
		return nil
	}
	return fmt.Errorf("target %q retries %d times, %s apart, which is %s of waiting alone, and a scrape ends with its interval, %s: the last retries could never be made; lower request.retry.attempts or backoff, or raise the interval", t.Name, attempts, backoff, waiting, time.Duration(t.Interval))
}
