package allocator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// ProbeFile is the probe targets file (--probe-targets-file): the probe
// targets the allocator hands Prometheus through HTTP service discovery,
// each already pointed at the replica that probes it.
type ProbeFile struct {
	Jobs []ProbeJob `yaml:"probe_jobs"`
}

// ProbeJob is one /v1/sd/<name> answer: targets probed with one collector.
type ProbeJob struct {
	// Name is the job's place in the API, /v1/sd/<name>.
	Name string `yaml:"name"`
	// Collector is the collector of the configuration every probe of the
	// job names.
	Collector string `yaml:"collector"`
	// Params are sent as query parameters on every probe, as
	// __param_<name> labels.
	Params map[string]string `yaml:"params"`
	// Labels go on every target of the job; one written "" is left out.
	Labels map[string]string `yaml:"labels"`
	// Targets are the probes' targets, each its own instance.
	Targets []string `yaml:"targets"`
	// ShardKey is what a target is allocated by: ShardKeyTarget, the
	// default, or ShardKeyAddress.
	ShardKey string `yaml:"shard_key"`
	// Discovery are sources whose targets are probed besides Targets.
	Discovery []DiscoveryConfig `yaml:"discovery"`
	// TargetTemplate makes a discovered address a target, {{address}}
	// standing for it, such as https://{{address}}/health; the address as
	// it is without one.
	TargetTemplate string `yaml:"target_template"`
	// labelsOf are the labels each discovered target brings from its
	// source, by target; nil for a job as the file wrote it.
	labelsOf map[string]map[string]string
}

// The values of a probe job's shard_key.
const (
	// ShardKeyTarget allocates a probe by its target as written.
	ShardKeyTarget = "target"
	// ShardKeyAddress allocates a probe by the host and port it reaches,
	// so every probe of one server, whatever the path or scheme, goes to
	// one replica and shares its pool.
	ShardKeyAddress = "address"
)

// probeDocument is the file as read: probe_jobs, and x- keys of its own for
// YAML anchors, which rest catches so that any other key can be refused by
// name. Everything below the top level is decoded strictly.
type probeDocument struct {
	Jobs []ProbeJob     `yaml:"probe_jobs"`
	Rest map[string]any `yaml:",inline"`
}

// parseProbeFile reads a probe targets file's text.
func parseProbeFile(path string, raw []byte) (*ProbeFile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var doc probeDocument
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("probe targets file %s is empty; it must define probe_jobs", path)
		}
		return nil, fmt.Errorf("probe targets file %s: %w", path, err)
	}
	var next yaml.Node
	if err := dec.Decode(&next); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("probe targets file %s holds more than one YAML document; keep one", path)
	}
	for _, key := range model.SortedKeys(doc.Rest) {
		if !strings.HasPrefix(key, "x-") {
			return nil, fmt.Errorf("probe targets file %s: %q is not allowed; the file holds probe_jobs, and x- keys of its own for YAML anchors", path, key)
		}
	}
	return &ProbeFile{Jobs: doc.Jobs}, nil
}

var (
	// probeJobName is what a job may be called: its name is a segment of
	// the path Prometheus asks, /v1/sd/<name>, written as is.
	probeJobName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	// labelName is a Prometheus label name; a parameter is sent as the
	// label __param_<name>, so its name must make one too.
	labelName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// checkProbeFile checks the probe jobs against the configuration they will
// be probed with: each names a collector it has, each target is one that
// collector's request type takes, and each parameter one a probe of that
// collector may be sent.
func checkProbeFile(f *ProbeFile, c *model.Config, base string) error {
	if len(f.Jobs) == 0 {
		return errors.New("probe_jobs must not be empty")
	}
	seen := map[string]bool{}
	for i := range f.Jobs {
		job := &f.Jobs[i]
		if !probeJobName.MatchString(job.Name) {
			return fmt.Errorf("probe job %d has the name %q; a name is letters, digits, _, . and -, not starting with . or -, as it is part of the path /v1/sd/<name>", i, job.Name)
		}
		if seen[job.Name] {
			return fmt.Errorf("probe job %q is defined twice; give each job its own name", job.Name)
		}
		seen[job.Name] = true
		if err := checkProbeJob(job, c, base); err != nil {
			return fmt.Errorf("probe job %q: %w", job.Name, err)
		}
	}
	return nil
}

func checkProbeJob(job *ProbeJob, c *model.Config, base string) error {
	collector := model.CollectorByName(c, job.Collector)
	if strings.TrimSpace(job.Collector) == "" {
		return errors.New("collector is required: the collector of the configuration that probes the job's targets")
	}
	if collector == nil {
		return fmt.Errorf("collector %q is not a collector of the configuration", job.Collector)
	}
	switch job.ShardKey {
	case "", ShardKeyTarget, ShardKeyAddress:
	default:
		return fmt.Errorf("shard_key %q is neither %s nor %s", job.ShardKey, ShardKeyTarget, ShardKeyAddress)
	}
	if len(job.Targets) == 0 && len(job.Discovery) == 0 {
		return errors.New("targets must not be empty, unless discovery finds them")
	}
	if err := checkDiscovery(job.Discovery, base); err != nil {
		return err
	}
	if err := checkTargetTemplate(job.TargetTemplate); err != nil {
		return err
	}
	if job.TargetTemplate != "" && len(job.Discovery) == 0 {
		return errors.New("target_template is set without discovery, whose targets it makes")
	}
	targets := map[string]bool{}
	for _, target := range job.Targets {
		if strings.TrimSpace(target) == "" {
			return errors.New("targets has an empty entry")
		}
		if targets[target] {
			return fmt.Errorf("target %q is listed twice", target)
		}
		targets[target] = true
		// Checked as a static target's is, not as a probe's: the target is
		// known now, so one that every probe would fail with is refused
		// here, rather than at every probe.
		if err := fetch.CheckTarget(collector, target, true); err != nil {
			return fmt.Errorf("target %q: %w", target, err)
		}
	}
	query := url.Values{}
	for _, name := range model.SortedKeys(job.Params) {
		switch {
		case !labelName.MatchString(name):
			return fmt.Errorf("params has %q, which cannot be sent as the label __param_%s; a parameter name is letters, digits and underscores", name, name)
		case name == "target" || name == "collector":
			return fmt.Errorf("params has %q, which the allocator sets itself on every probe", name)
		case name == "debug":
			return errors.New("params has debug, which turns a probe into a report rather than metrics")
		}
		query.Set(name, job.Params[name])
	}
	if _, err := fetch.ParseRequestOverrides(query); err != nil {
		return fmt.Errorf("params: %w", err)
	}
	if err := fetch.CheckOverrideParams(collector, query); err != nil {
		return err
	}
	for _, name := range model.SortedKeys(job.Labels) {
		switch {
		case !labelName.MatchString(name):
			return fmt.Errorf("labels has %q, which is not a label name: letters, digits and underscores, not starting with a digit", name)
		case strings.HasPrefix(name, "__"):
			return fmt.Errorf("labels has %q; a label starting with __ is Prometheus's own", name)
		case name == "instance":
			return errors.New("labels has instance, which the allocator sets to each probe's target")
		}
	}
	return nil
}

// shardKeyOf is what the probe of target in job is allocated by.
func shardKeyOf(job *ProbeJob, target string) string {
	if job.ShardKey == ShardKeyAddress {
		return addressOf(target)
	}
	return target
}

// addressOf is the host and port a target reaches: for a URL its host, with
// :80 or :443 for an http or https one that names no port; anything else,
// host:port or a host alone, as written. Host names are compared without
// their case.
func addressOf(target string) string {
	if !strings.Contains(target, "://") {
		return strings.ToLower(target)
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return strings.ToLower(target)
	}
	host := strings.ToLower(u.Host)
	if u.Port() == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			host += ":80"
		case "https":
			host += ":443"
		}
	}
	return host
}
