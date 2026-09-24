package config

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

var targetNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// LoadStaticTargets reads the static target document. It takes the same
// options as Load: a target file carries the addresses and credentials of
// the things being scraped, which is exactly the material an operator wants to
// keep out of a committed file, so --config.export-env applies to both.
func LoadStaticTargets(path string, opts ...LoadOption) (*model.StaticTargetFile, error) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, err
	}
	var f model.StaticTargetFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = dec.Decode(&f); err != nil {
		return nil, yamlError(err)
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
		for name := range t.Params {
			if !fetch.PathParamName.MatchString(name) {
				return fmt.Errorf("target %q params has %q, which is not a parameter name; names are %s<name>, with letters, digits and underscores, as the placeholders they fill", t.Name, name, fetch.PathParamPrefix)
			}
		}
		if t.Request.Timeout < 0 {
			return fmt.Errorf("target %q request.timeout must not be negative", t.Name)
		}
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
		if t.Request.Retry != nil {
			if t.Request.Retry.Attempts < 0 {
				return fmt.Errorf("target %q request.retry.attempts must not be negative", t.Name)
			}
			if t.Request.Retry.Backoff < 0 {
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
			if !model.LabelNameRE.MatchString(name) {
				return fmt.Errorf("target %q has invalid label name %q", t.Name, name)
			}
			// static_target names the target on the endpoint, so the
			// target's series cannot claim it for something else.
			if name == StaticTargetLabel {
				return fmt.Errorf("target %q labels sets %s, which the static targets endpoint sets to the target's name", t.Name, StaticTargetLabel)
			}
		}
		// The OTLP resource identity is only used by a target exported over
		// OTLP; set without it, it would be quietly ignored.
		if !t.ExportViaOTLP && (t.OTLP.ServiceName != "" || len(t.OTLP.ResourceAttributes) > 0) {
			return fmt.Errorf("target %q sets otlp, which only a target with export_via_otlp: true uses", t.Name)
		}
	}
	return nil
}

// StaticTargetLabel is the label the static targets endpoint puts on every
// series of a target, naming it: the targets share one endpoint, and it keeps
// their series apart.
const StaticTargetLabel = "static_target"

// ValidateStaticTargetsAgainst enforces the preconditions that depend on the
// exporter configuration: every target must name a configured collector, and a
// target exported over OTLP needs OTLP export enabled.
func ValidateStaticTargetsAgainst(f *model.StaticTargetFile, c *model.Config) error {
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
	known := map[string]bool{}
	for i := range c.Collectors {
		known[c.Collectors[i].Name] = true
	}
	for i := range f.Targets {
		t := &f.Targets[i]
		if !known[t.Collector] {
			return fmt.Errorf("target %q references unknown collector %q", t.Name, t.Collector)
		}
		// What a target may be depends on the collector's request type: an
		// absolute URL for http, a file under request.root for localfile,
		// which may also leave it out.
		if err := fetch.CheckTarget(model.CollectorByName(c, t.Collector), t.Target, true); err != nil {
			if errors.Is(err, fetch.ErrMissingTarget) {
				return fmt.Errorf("target %q has no target address", t.Name)
			}
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		if err := fetch.CheckTargetRequest(t, model.CollectorByName(c, t.Collector)); err != nil {
			return err
		}
		// Every placeholder of the collector's request must be filled, by the
		// target's params or a default, since nothing else can fill it; and
		// every param must fill one, since an unused one is a misspelling.
		// Catching both here makes them startup errors naming both sides.
		if collector := model.CollectorByName(c, t.Collector); collector != nil {
			unused, err := fetch.CheckRequestParams(collector, fetch.TargetOverrides(t))
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
				return fmt.Errorf("target %q params %s are not used by collector %q: no placeholder in its request names them", t.Name, strings.Join(unused, ", "), t.Collector)
			}
		}
	}
	return nil
}
