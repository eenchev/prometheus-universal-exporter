package config

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

var targetNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// LoadStaticTargets reads the static target document. A target file holds
// the addresses and credentials of the scraped things, which is exactly what
// an operator wants to keep out of a committed file. So the file may take
// them from the environment with WithStaticTargetsEnvExpansion. The flag
// --static-targets.expand-env turns that on, independently of the
// configuration's --config.expand-env.
func LoadStaticTargets(path string, opts ...LoadOption) (*model.StaticTargetFile, error) {
	b, err := readDocument(path, opts)
	if err != nil {
		return nil, err
	}
	return ParseStaticTargets(path, b)
}

// DiscoveredTargetsKey is the static target file's key for the targets that
// discovery finds. The target allocator (cmd/pue-allocator) reads every kind
// of source, and expands each entry into written targets before it hands a
// replica its share. The exporter reads an entry of dns_sd sources and asks
// them itself (staticdiscovery.go).
const DiscoveredTargetsKey = "discovered_targets"

// ParseStaticTargets reads the text of a static target document. path names
// the document in messages. Its discovered_targets, when it has them, are
// read into the file's Discovered, and every entry is checked as a target at
// a sample address.
func ParseStaticTargets(path string, b []byte) (*model.StaticTargetFile, error) {
	var err error
	if err = refuseAllocatorKeys(b); err != nil {
		return nil, err
	}
	if err = refuseNotScalarKeys(b); err != nil {
		return nil, err
	}
	// The file is decoded as written, discovered_targets apart, so that a
	// mistake is told at its line, and an alias reads as its anchor
	// wherever the two are.
	var w staticTargetDocument
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = withValueProblems(withoutExtensionKeys(dec.Decode(&w)), b, &w.StaticTargetFile); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("static target file %s is empty; it must define targets", path)
		}
		return nil, yamlError(err)
	}
	if err = oneDocument(dec); err != nil {
		return nil, err
	}
	f := w.StaticTargetFile
	f.Written = len(f.Targets)
	if w.Entries.Kind != 0 {
		if err = discoveredValueProblems(b); err != nil {
			return nil, err
		}
		if f.Discovered, err = readDiscoveredEntries(ResolvedNode(&w.Entries), fileHead(b)); err != nil {
			return nil, err
		}
	}
	return &f, nil
}

// staticTargetDocument is a static target file as it is decoded: the file,
// and its discovered_targets apart, which readDiscoveredEntries reads.
type staticTargetDocument struct {
	model.StaticTargetFile `yaml:",inline"`
	Entries                yaml.Node `yaml:"discovered_targets"`
}

// refuseAllocatorKeys refuses a document with a key that only the target
// allocator reads, and names the allocator instead of reporting an unknown
// key. These keys are default_weight at the top level, and weight and
// weight_label on a target under targets or an entry of discovered_targets.
// A document that is not a YAML mapping is left to the decoder, which says
// what is wrong with it. The same goes for a key merged in from an anchor.
func refuseAllocatorKeys(b []byte) error {
	var doc yaml.Node
	_ = yaml.Unmarshal(b, &doc)
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := doc.Content[0]
	weighted := func(key *yaml.Node) error {
		return fmt.Errorf("line %d: %s is read by the target allocator (pue-allocator --static-targets-file), which places the targets on the replicas by their weights; the exporter scrapes every target of its file, so leave %s out, or run the exporter with the allocator", key.Line, key.Value, key.Value)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		switch key.Value {
		case "default_weight":
			return weighted(key)
		case "targets", DiscoveredTargetsKey:
			if value.Kind != yaml.SequenceNode {
				continue
			}
			for _, t := range value.Content {
				if t.Kind != yaml.MappingNode {
					continue
				}
				for j := 0; j+1 < len(t.Content); j += 2 {
					if k := t.Content[j]; k.Value == "weight" || k.Value == "weight_label" {
						return weighted(k)
					}
				}
			}
		}
	}
	return nil
}

// ValidateStaticTargets checks the document in isolation. ValidateStaticTargetsAgainst
// applies the checks that need the exporter configuration.
func ValidateStaticTargets(f *model.StaticTargetFile) error {
	if len(f.Targets) == 0 && len(f.Discovered) == 0 {
		return errors.New("targets must not be empty")
	}
	return validateStaticTargets(f)
}

// ValidateDiscoveringStaticTargets is ValidateStaticTargets for the target
// allocator's static target file with discovered_targets. Its targets may be
// empty: the discovered ones may be all it has, and none may have been
// discovered yet.
func ValidateDiscoveringStaticTargets(f *model.StaticTargetFile) error {
	return validateStaticTargets(f)
}

func validateStaticTargets(f *model.StaticTargetFile) error {
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
			method, err := fetch.CheckHTTPMethod(t.Request.Method)
			if err != nil {
				return fmt.Errorf("target %q has unsupported method %q; %w", t.Name, t.Request.Method, err)
			}
			t.Request.Method = method
		}
		// A static target is scraped on the exporter's own timer, with no
		// probe to supply a path parameter, so a placeholder here could only
		// ever take its default — or fail every scrape. Write the path out.
		if fetch.HasPathParams(t.Request.Path) {
			return fmt.Errorf("target %q request.path cannot use {{param_...}} placeholders: a static target has no probe to supply them, so write the path out in full", t.Name)
		}
		// Placeholders are also refused in every other value the target writes
		// itself. Those values are sent as written, so a placeholder would
		// reach the target as text. This also holds for a value from an
		// environment reference, since the file is checked after expansion.
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
		// Statuses and codes are normalized here to the form a scrape
		// compares, once, while nothing reads the file yet. The check against
		// the configuration, which a reload repeats on the file in force, only
		// reads them.
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
			// The endpoint is scraped with the series' own labels kept, as the
			// chart's monitor does. So a target's job or instance label would
			// move its series out of the job that scrapes the endpoint.
			if name == "job" || name == "instance" {
				return fmt.Errorf("target %q labels sets %s, which Prometheus sets when it scrapes the static targets endpoint; with honor_labels the target's would replace it, so name the label something else, such as task", t.Name, name)
			}
		}
		// The OTLP resource identity is only used by a target exported over
		// OTLP; set without it, it would be quietly ignored.
		if !t.ExportViaOTLP && t.OTLP.Set() {
			return fmt.Errorf("target %q sets otlp, which only a target with export_via_otlp: true uses", t.Name)
		}
		// As for the exporter-wide otlp block: service_name is the
		// resource's service.name, which the attributes must not set again.
		if _, twice := t.OTLP.ResourceAttributes[otlpServiceNameAttribute]; twice {
			return fmt.Errorf("target %q otlp.resource_attributes sets %s, which otlp.service_name sets; write the name as the target's otlp.service_name", t.Name, otlpServiceNameAttribute)
		}
		// A label moved to the resource is one the target sets, the same on
		// every series of it; one a series has of its own may differ from
		// series to series, which no resource can.
		named := map[string]bool{}
		for _, name := range t.OTLP.LabelsAsResource {
			if _, own := t.Labels[name]; !own {
				return fmt.Errorf("target %q otlp.labels_as_resource names %q, which is not one of its labels; only a label the target's labels set, the same on every series of it, can move to its resource", t.Name, name)
			}
			if named[name] {
				return fmt.Errorf("target %q otlp.labels_as_resource names %q twice", t.Name, name)
			}
			named[name] = true
		}
	}
	return nil
}

// otlpServiceInstanceIDAttribute is the resource attribute a static target's
// otlp.instance_id gives its resource.
const otlpServiceInstanceIDAttribute = "service.instance.id"

// checkTargetResourceKeys refuses a target whose otlp.labels_as_resource or
// otlp.instance_id would add an attribute its resource already has, from its
// own otlp.resource_attributes or from the exporter-wide ones it keeps. The
// resource would then carry the key twice. An attribute written "" counts as
// left out (exporter/statictarget.go's targetResource): the target keeps the
// exporter-wide attribute of that name, if there is one. A label name
// has no dot, so it can be neither service.name nor service.instance.id.
func checkTargetResourceKeys(t *model.StaticTarget, o model.OTLPConfig) error {
	from := func(key string) string {
		if t.OTLP.ResourceAttributes[key] != "" {
			return "its otlp.resource_attributes"
		}
		if o.ResourceAttributes[key] != "" {
			return "the exporter's otlp.resource_attributes"
		}
		return ""
	}
	for _, name := range t.OTLP.LabelsAsResource {
		if source := from(name); source != "" {
			return fmt.Errorf("target %q otlp.labels_as_resource names %q, which its resource has already as an attribute, from %s; rename the label, or leave it on the target's points", t.Name, name, source)
		}
	}
	if t.OTLP.InstanceID {
		if source := from(otlpServiceInstanceIDAttribute); source != "" {
			return fmt.Errorf("target %q sets otlp.instance_id, which makes the target's name its resource's %s, and its resource has that attribute already, from %s; leave one of the two out", t.Name, otlpServiceInstanceIDAttribute, source)
		}
	}
	return checkTargetExporterInstance(t, o)
}

// checkTargetExporterInstance refuses a target whose resource would already
// have the otlp.exporter_instance attribute. The attribute can come from:
//   - its own otlp.resource_attributes, written "" or not, as the
//     exporter-wide ones are held to it (checkExporterInstance);
//   - its otlp.labels_as_resource;
//   - otlp.instance_id, as service.instance.id.
//
// Either would replace the replica's name on the target's resource, or be
// replaced by it.
func checkTargetExporterInstance(t *model.StaticTarget, o model.OTLPConfig) error {
	e := o.ExporterInstance
	if !e.Enabled {
		return nil
	}
	attribute := e.Attribute
	if attribute == "" {
		attribute = model.DefaultOTLPExporterInstanceAttribute
	}
	if _, has := t.OTLP.ResourceAttributes[attribute]; has {
		return fmt.Errorf("target %q otlp.resource_attributes sets %s, which otlp.exporter_instance gives every resource the exporter sends; name the target's attribute something else, or name the exporter's with otlp.exporter_instance.attribute", t.Name, attribute)
	}
	if slices.Contains(t.OTLP.LabelsAsResource, attribute) {
		return fmt.Errorf("target %q otlp.labels_as_resource names %q, which otlp.exporter_instance gives every resource the exporter sends; rename the label, or name the exporter's attribute with otlp.exporter_instance.attribute", t.Name, attribute)
	}
	if t.OTLP.InstanceID && attribute == otlpServiceInstanceIDAttribute {
		return fmt.Errorf("target %q sets otlp.instance_id, which makes the target's name its resource's %s, and otlp.exporter_instance.attribute is %s too; leave one of the two out", t.Name, otlpServiceInstanceIDAttribute, otlpServiceInstanceIDAttribute)
	}
	return nil
}

// StaticTargetLabel is the label the static targets endpoint puts on every
// series of a target, naming it: the targets share one endpoint, and it keeps
// their series apart.
const StaticTargetLabel = "static_target"

// staticTargetSeriesLabels are the labels the exporter adds to a static
// target's series after the scrape's validation, longest first. That
// validation holds every other label name to limits.max_label_name_length.
// The labels are static_target, on every series the endpoint serves and on
// its OTLP exports (the exporter's withStaticTargetLabel), and collector and
// target, on its health series (staticTargetHealthMetrics). A label added
// before the validation, such as the file label of a collector that reads a
// directory, fails the scrape when it is too long, like any label.
var staticTargetSeriesLabels = []struct{ name, on string }{
	{StaticTargetLabel, "every series of it"},
	{"collector", "its health series"},
	{"target", "its health series"},
}

// ValidateStaticTargetsAgainst enforces the preconditions that depend on the
// exporter configuration: every target must name a configured collector, and a
// target exported over OTLP needs OTLP export enabled. It writes into neither
// value. A reload checks the file in force against the configuration it read,
// and the file it read against the configuration in force, while scrapes read
// both.
func ValidateStaticTargetsAgainst(f *model.StaticTargetFile, c *model.Config) error {
	if hook := targetsValidatedHook.Load(); hook != nil {
		(*hook)()
	}
	if err := checkTargetsAgainst(f.Targets, c); err != nil {
		return err
	}
	// An entry of discovered_targets is checked as the target it makes of a
	// sample address, as when the file was read.
	for i := range f.Discovered {
		e := &f.Discovered[i]
		if err := checkTargetsAgainst([]model.StaticTarget{e.Sample}, c); err != nil {
			return fmt.Errorf("%s entry %q, checked with the address %s in place of a discovered one: %w", DiscoveredTargetsKey, e.Name, discoverySampleAddress, err)
		}
	}
	return nil
}

// checkTargetsAgainst is what ValidateStaticTargetsAgainst checks of each
// target.
func checkTargetsAgainst(targets []model.StaticTarget, c *model.Config) error {
	// A target's credential file may be a reference to a secret store of
	// the configuration, as a collector's may.
	references := fetch.NewSecretReferences(c.SecretStores)
	for i := range targets {
		if err := references.Check("target", targets[i].Name, &targets[i]); err != nil {
			return err
		}
	}
	for i := range targets {
		t := &targets[i]
		if !t.ExportViaOTLP {
			continue
		}
		if !c.OTLP.Enabled {
			return fmt.Errorf("target %q sets export_via_otlp, which needs OTLP export; set otlp.enabled: true and otlp.endpoint, or leave the target to the static targets endpoint", t.Name)
		}
		if strings.TrimSpace(c.OTLP.Endpoint) == "" {
			return fmt.Errorf("target %q sets export_via_otlp, which needs otlp.endpoint", t.Name)
		}
		if err := checkTargetResourceKeys(t, c.OTLP); err != nil {
			return err
		}
	}
	collectors := collectorsByName(c)
	// The placeholders of a collector are found once for the check, for its
	// first target, and not again for each of its others.
	var params fetch.RequestParamsCheck
	for i := range targets {
		t := &targets[i]
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
		// A target's labels go on every series of its collector, after the
		// scrape's validation. So they are checked here against the
		// collector's limits.max_label_name_length, which applies to every
		// other label name of those series.
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
		// value (fetch/labelparams.go), must be filled by the target's params
		// or a default, since nothing else can fill it. Every param must fill
		// a placeholder, since an unused one is a misspelling. Checking both
		// here makes them startup errors that name both sides. A parameter's
		// value may hold only clock placeholders, and none when it fills a
		// label value.
		if err := fetch.CheckTargetParams(collector, t); err != nil {
			return err
		}
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
// against the configuration needs that collector. Finding it among the
// configuration's collectors (model.CollectorByName) costs time in
// proportion to the configuration's size. The check did that four times per
// target, and once more per configuration to learn which descriptor files it
// opens (targetsChecked). For 10,000 targets of as many collectors that took
// seconds of a reload, which checked the pair up to five times. So the
// collectors are indexed by name once per check, and each target's collector
// is looked up there. The exporter keeps the same index for the configuration
// it follows (internal/exporter, fingerprintGeneration). That one lives with
// what the exporter keeps for a configuration in force, and this check also
// runs on a configuration that is not in force yet.

// targetsValidatedHook, set by tests, is called at each check of a static
// target file against a configuration (ValidateStaticTargetsAgainst). A test
// can then count the checks a reload makes: one for each pair it asks about,
// however often it asks (Manager.apply).
var targetsValidatedHook atomic.Pointer[func()]

// collectorsIndexedHook, set by tests, is called whenever a configuration's
// collectors are indexed by name (collectorsByName). It is called once per
// indexing, not per collector, so a test can count the indexings and see
// that there is one per check, not one per static target.
var collectorsIndexedHook atomic.Pointer[func()]

// collectorsByName maps the collectors of c by name. Each entry is the one
// model.CollectorByName(c, name) returns: the collector of c itself, not a
// copy, and the first of two that share a name, which is why the loop runs
// from the last. A loaded configuration has no two collectors of one name
// (Validate), and a name it does not have is not in the map.
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
// target writes itself: its target, request.body, header values, a graphite
// collector's request.targets, and a grpc collector's request.message and
// metadata values. Its request.path is checked with the collector's stricter
// rule. Such values are sent as written, with no probe to fill a placeholder.
// The target's params are what fill the collector's placeholders.
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
