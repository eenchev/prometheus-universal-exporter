package model

import (
	"reflect"

	"gopkg.in/yaml.v3"
)

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
	// Params fills the collector's {{param_<name>}} placeholders, as the
	// param_<name> probe parameters fill them for a probe
	// (fetch/requesttemplate.go).
	Params map[string]string `yaml:"params"`
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
	FollowRedirects    *bool             `yaml:"follow_redirects"`
	EnableHTTP2        *bool             `yaml:"enable_http2"`
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
