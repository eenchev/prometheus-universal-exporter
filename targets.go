package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var targetNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// TargetFile is the optional scheduled-target document. Its targets are
// scraped by the exporter itself on the OTLP export interval and are only
// delivered over OTLP, so the file is rejected unless OTLP export is enabled.
type TargetFile struct {
	Targets []ScheduledTarget `yaml:"targets"`
}

// ScheduledTarget describes one fully specified request. Every per-scrape
// parameter the /probe endpoint accepts is available here, alongside the labels
// and OTLP resource identity the exported metrics carry.
type ScheduledTarget struct {
	Name      string              `yaml:"name"`
	Collector string              `yaml:"collector"`
	Target    string              `yaml:"target"`
	Request   TargetRequestConfig `yaml:"request"`
	Labels    map[string]string   `yaml:"labels"`
	OTLP      TargetOTLPConfig    `yaml:"otlp"`
}

// TargetRequestConfig mirrors the collector request block and the /probe
// override parameters. Values set here replace the collector's own settings for
// this target only.
type TargetRequestConfig struct {
	Method             string            `yaml:"method"`
	Path               string            `yaml:"path"`
	PathSet            bool              `yaml:"-"`
	Body               string            `yaml:"body"`
	BodySet            bool              `yaml:"-"`
	Timeout            Duration          `yaml:"timeout"`
	InsecureSkipVerify *bool             `yaml:"insecure_skip_verify"`
	Retry              *RetryConfig      `yaml:"retry"`
	Headers            map[string]string `yaml:"headers"`
	BasicAuth          *BasicAuth        `yaml:"basic_auth"`
	BasicAuthFile      *BasicAuthFile    `yaml:"basic_auth_file"`
	BearerToken        string            `yaml:"bearer_token"`
	BearerTokenFile    string            `yaml:"bearer_token_file"`
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

func LoadTargetFile(path string) (*TargetFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f TargetFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = dec.Decode(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks the document in isolation. ValidateAgainst applies the checks
// that need the exporter configuration.
func (f *TargetFile) Validate() error {
	if len(f.Targets) == 0 {
		return fmt.Errorf("targets must not be empty")
	}
	seen := map[string]bool{}
	for i := range f.Targets {
		t := &f.Targets[i]
		if strings.TrimSpace(t.Collector) == "" {
			return fmt.Errorf("target %d has no collector", i)
		}
		if strings.TrimSpace(t.Target) == "" {
			return fmt.Errorf("target %q has no target address", t.Collector)
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
		if u, err := url.Parse(t.Target); err != nil || u.Host == "" {
			return fmt.Errorf("target %q must have an absolute target URL", t.Name)
		}
		if t.Request.Method != "" {
			t.Request.Method = strings.ToUpper(t.Request.Method)
			switch t.Request.Method {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
			default:
				return fmt.Errorf("target %q has unsupported method %q", t.Name, t.Request.Method)
			}
		}
		if t.Request.Timeout < 0 {
			return fmt.Errorf("target %q request.timeout must not be negative", t.Name)
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
			if !labelNameRE.MatchString(name) {
				return fmt.Errorf("target %q has invalid label name %q", t.Name, name)
			}
		}
	}
	return nil
}

// ValidateAgainst enforces the preconditions that depend on the exporter
// configuration: scheduled targets exist only to feed OTLP, and every target
// must name a configured collector.
func (f *TargetFile) ValidateAgainst(c *Config) error {
	if !c.OTLP.Enabled {
		return fmt.Errorf("scheduled targets require OTLP export; set otlp.enabled: true or remove the target file")
	}
	if strings.TrimSpace(c.OTLP.Endpoint) == "" {
		return fmt.Errorf("scheduled targets require otlp.endpoint")
	}
	known := map[string]bool{}
	for i := range c.Collectors {
		known[c.Collectors[i].Name] = true
	}
	for i := range f.Targets {
		if !known[f.Targets[i].Collector] {
			return fmt.Errorf("target %q references unknown collector %q", f.Targets[i].Name, f.Targets[i].Collector)
		}
	}
	return nil
}

// overrides translates the target request block into the same per-scrape
// override structure the /probe endpoint produces.
func (t *ScheduledTarget) overrides() RequestOverrides {
	out := RequestOverrides{Method: t.Request.Method, Timeout: time.Duration(t.Request.Timeout)}
	if t.Request.PathSet {
		out.PathSet = true
		out.Path = t.Request.Path
	}
	if t.Request.BodySet {
		body := t.Request.Body
		out.Body = &body
	}
	if t.Request.InsecureSkipVerify != nil {
		value := *t.Request.InsecureSkipVerify
		out.InsecureSkipVerify = &value
	}
	if t.Request.Retry != nil {
		attempts := t.Request.Retry.Attempts
		backoff := time.Duration(t.Request.Retry.Backoff)
		out.RetryAttempts = &attempts
		out.RetryBackoff = &backoff
	}
	return out
}

// headers builds the headers sent to the target. Scheduled targets are operator
// configuration rather than caller input, so they are applied directly instead
// of through the collector's forwarding allowlist. Credentials are resolved
// here so that they are part of the cache key and can never be shared with a
// request that did not present them.
func (t *ScheduledTarget) headers() (http.Header, error) {
	out := make(http.Header)
	for name, value := range t.Request.Headers {
		out.Set(name, value)
	}
	username, password := "", ""
	if t.Request.BasicAuth != nil {
		username, password = t.Request.BasicAuth.Username, t.Request.BasicAuth.Password
	}
	if t.Request.BasicAuthFile != nil {
		value, err := readCredentialFile(t.Request.BasicAuthFile.Username)
		if err != nil {
			return nil, fmt.Errorf("reading basic auth username file: %w", err)
		}
		username = value
		value, err = readCredentialFile(t.Request.BasicAuthFile.Password)
		if err != nil {
			return nil, fmt.Errorf("reading basic auth password file: %w", err)
		}
		password = value
		if username == "" || password == "" {
			return nil, fmt.Errorf("basic auth credential files must not be empty")
		}
	}
	if username != "" || password != "" {
		request := &http.Request{Header: make(http.Header)}
		request.SetBasicAuth(username, password)
		out.Set("Authorization", request.Header.Get("Authorization"))
	}
	token := t.Request.BearerToken
	if t.Request.BearerTokenFile != "" {
		value, err := readCredentialFile(t.Request.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading bearer token file: %w", err)
		}
		if value == "" {
			return nil, fmt.Errorf("bearer token file %s is empty", t.Request.BearerTokenFile)
		}
		token = value
	}
	if token != "" {
		out.Set("Authorization", "Bearer "+token)
	}
	return out, nil
}

// cacheQuery reproduces the /probe query a caller would have to send to make
// the same request, so a scheduled scrape and an equivalent probe share cache
// entries and a differing one never does.
func (t *ScheduledTarget) cacheQuery() url.Values {
	values := url.Values{"target": {t.Target}, "collector": {t.Collector}}
	if t.Request.Method != "" {
		values.Set("method", t.Request.Method)
	}
	if t.Request.PathSet {
		values.Set("path", t.Request.Path)
	}
	if t.Request.BodySet {
		values.Set("body", t.Request.Body)
	}
	if t.Request.Timeout > 0 {
		values.Set("timeout", time.Duration(t.Request.Timeout).String())
	}
	if t.Request.InsecureSkipVerify != nil {
		values.Set("insecure_skip_verify", strconv.FormatBool(*t.Request.InsecureSkipVerify))
	}
	if t.Request.Retry != nil {
		values.Set("retry_attempts", strconv.Itoa(t.Request.Retry.Attempts))
		values.Set("retry_backoff", time.Duration(t.Request.Retry.Backoff).String())
	}
	return values
}

// resource resolves the OTLP resource identity for this target, with the
// exporter-wide service name and attributes as the defaults.
func (t *ScheduledTarget) resource(cfg OTLPConfig) otlpResourceIdentity {
	identity := otlpResourceIdentity{ServiceName: cfg.ServiceName, Attributes: map[string]string{}}
	for key, value := range cfg.ResourceAttributes {
		identity.Attributes[key] = value
	}
	if t.OTLP.ServiceName != "" {
		identity.ServiceName = t.OTLP.ServiceName
	}
	for key, value := range t.OTLP.ResourceAttributes {
		identity.Attributes[key] = value
	}
	return identity
}
