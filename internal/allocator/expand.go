package allocator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/discovery"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// discoveredEntry is a discovered_targets entry of the static target file:
// a static target written once for every target its discovery finds.
type discoveredEntry struct {
	// name is the entry's name, which each of its targets' names begins
	// with.
	name string
	// template is target_template, and byAddress says shard_key: address.
	template  string
	byAddress bool
	discovery []DiscoveryConfig
	// keys are the entry's other keys and their values, every target's;
	// labels are its labels, which win over a discovered target's.
	keys   []*yaml.Node
	labels map[string]string
	// weight is every target's weight, in thousandths, and weightLabel
	// the discovered label a target's own weight is read from, when set.
	weight      int64
	weightLabel string
}

// sampleAddress stands for a discovered address when an entry is checked
// at load: an address of TEST-NET-1 (RFC 5737), which reaches nothing.
const sampleAddress = "192.0.2.1:9100"

// targetName is a static target's name (config's targetNameRE).
var targetName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// readDiscoveredEntries reads and checks the discovered_targets entries.
// Each entry is checked as a static target of the file, with head as its
// top-level keys and sampleAddress in place of a discovered address. So
// whatever the entry writes is checked at load. Only the target and the
// labels of a discovered target are left to check when it is found.
func readDiscoveredEntries(list *yaml.Node, head []*yaml.Node, conf *model.Config, base string, defaultRefresh time.Duration, defaultWeight int64) ([]*discoveredEntry, error) {
	if list.Kind != yaml.SequenceNode || len(list.Content) == 0 {
		return nil, fmt.Errorf("%s must be a list of entries, each a static target with discovery", config.DiscoveredTargetsKey)
	}
	var out []*discoveredEntry
	seen := map[string]bool{}
	for i, node := range list.Content {
		e, err := readDiscoveredEntry(node, head, conf, base, defaultRefresh, defaultWeight)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", config.DiscoveredTargetsKey, i, err)
		}
		if seen[e.name] {
			return nil, fmt.Errorf("%s entry %q is defined twice; give each entry its own name", config.DiscoveredTargetsKey, e.name)
		}
		seen[e.name] = true
		out = append(out, e)
	}
	// A target is named <entry>_<address>, so were one entry's name another's
	// followed by _, the two could name a target alike, and which got the
	// name would turn on what else discovery found.
	for _, a := range out {
		for _, b := range out {
			if a != b && strings.HasPrefix(b.name, a.name+"_") {
				return nil, fmt.Errorf("%s entry %q begins with entry %q's name and _, so a target of one could be named as a target of the other; rename one", config.DiscoveredTargetsKey, b.name, a.name)
			}
		}
	}
	return out, nil
}

func readDiscoveredEntry(node *yaml.Node, head []*yaml.Node, conf *model.Config, base string, defaultRefresh time.Duration, defaultWeight int64) (*discoveredEntry, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("it is not a mapping")
	}
	e := &discoveredEntry{weight: defaultWeight}
	var shardKey string
	var sources, weight *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		switch key.Value {
		case "name":
			e.name = value.Value
		case weightKey:
			weight = value
		case weightLabelKey:
			e.weightLabel = value.Value
		case "target_template":
			e.template = value.Value
		case "shard_key":
			shardKey = value.Value
		case "discovery":
			sources = value
		case "target":
			return nil, errors.New("target is set, but an entry's targets are the ones its discovery finds; write target_template to make a target of each discovered address")
		default:
			e.keys = append(e.keys, key, value)
		}
	}
	if !targetName.MatchString(e.name) {
		return nil, fmt.Errorf("name %q is required and must be letters, digits and underscores, not starting with a digit: each discovered target is named <name>_<address>", e.name)
	}
	if sources == nil {
		return nil, fmt.Errorf("entry %q has no discovery; a target of its own belongs under targets", e.name)
	}
	if weight != nil {
		w, err := nodeWeight(weight, fmt.Sprintf("entry %q weight", e.name))
		if err != nil {
			return nil, err
		}
		e.weight = w
	}
	if e.weightLabel != "" && !model.ValidLabelName(e.weightLabel) {
		return nil, fmt.Errorf("entry %q weight_label %q is not a label name; name the discovered label a target's weight is read from, such as __meta_pue_weight", e.name, e.weightLabel)
	}
	if err := decodeStrict(sources, &e.discovery); err != nil {
		return nil, fmt.Errorf("entry %q discovery: %w", e.name, err)
	}
	if len(e.discovery) == 0 {
		return nil, fmt.Errorf("entry %q discovery must not be empty", e.name)
	}
	if err := checkDiscovery(e.discovery, base, defaultRefresh); err != nil {
		return nil, fmt.Errorf("entry %q: %w", e.name, err)
	}
	if err := discovery.CheckTemplate(e.template); err != nil {
		return nil, fmt.Errorf("entry %q: %w", e.name, err)
	}
	switch shardKey {
	case "":
	case ShardKeyAddress:
		e.byAddress = true
	default:
		return nil, fmt.Errorf("entry %q shard_key %q is not %s; leave it out to allocate each target by its name", e.name, shardKey, ShardKeyAddress)
	}
	t, err := checkStaticTarget(head, e.node(e.name, discovery.FromTemplate(e.template, sampleAddress), nil))
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(&model.StaticTargetFile{Targets: []model.StaticTarget{*t}}, conf)
	}
	if err != nil {
		return nil, fmt.Errorf("entry %q, checked with the address %s in place of a discovered one: %w", e.name, sampleAddress, err)
	}
	e.labels = t.Labels
	return e, nil
}

// decodeStrict decodes node into v refusing unknown keys, as the files'
// own decoders do.
func decodeStrict(node *yaml.Node, v any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// node is the static target the entry writes for a discovered target:
// its name, its target, its labels over the discovered ones, and the
// entry's other keys.
func (e *discoveredEntry) node(name, target string, found map[string]string) *yaml.Node {
	str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{str("name"), str(name), str("target"), str(target)}}
	labels := map[string]string{}
	for name, value := range found {
		labels[name] = value
	}
	for i := 0; i+1 < len(e.keys); i += 2 {
		if e.keys[i].Value == "labels" {
			// Every label the entry writes wins, and is checked as the
			// exporter checks it, at load.
			var own map[string]string
			_ = e.keys[i+1].Decode(&own)
			for name, value := range own {
				labels[name] = value
			}
			continue
		}
		n.Content = append(n.Content, e.keys[i], e.keys[i+1])
	}
	if len(labels) > 0 {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, name := range model.SortedKeys(labels) {
			m.Content = append(m.Content, str(name), str(labels[name]))
		}
		n.Content = append(n.Content, str("labels"), m)
	}
	return n
}

// checkStaticTarget checks one target node as a static target of a file of
// head, with the exporter's loader and its checks of the file alone, and
// returns it as loaded.
func checkStaticTarget(head []*yaml.Node, target *yaml.Node) (*model.StaticTarget, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: slices.Clone(head)}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "targets"}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{target}})
	raw, err := yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	f, err := config.ParseStaticTargets("(discovered target)", raw)
	if err == nil {
		err = config.ValidateStaticTargets(f)
	}
	if err != nil {
		return nil, err
	}
	return &f.Targets[0], nil
}

// sourceSpec is a discovery source of the inputs. It has its key, which stays
// the same while its owner and its configuration do; its kind (probe or
// static); its owner (the probe job or the discovered_targets entry); and the
// owner's weight_label, by which its answers' weights are held (holdWeights).
type sourceSpec struct {
	key, kind, owner string
	cfg              DiscoveryConfig
	weightLabel      string
}

// discoverySources are the discovery sources of the inputs, the probe
// jobs' and then the entries', in file order.
func (in *Inputs) discoverySources() []sourceSpec {
	var out []sourceSpec
	add := func(kind, owner, weightLabel string, list []DiscoveryConfig) {
		for _, d := range list {
			text, _ := json.Marshal(d)
			out = append(out, sourceSpec{key: kind + "/" + owner + "/" + d.Name + "\x00" + string(text) + "\x00" + weightLabel, kind: kind, owner: owner, cfg: d, weightLabel: weightLabel})
		}
	}
	if in.Probes != nil {
		for i := range in.Probes.Jobs {
			add("probe", in.Probes.Jobs[i].Name, in.Probes.Jobs[i].WeightLabel, in.Probes.Jobs[i].Discovery)
		}
	}
	for _, e := range in.entries {
		add("static", e.name, e.weightLabel, e.discovery)
	}
	return out
}

// checkedTarget is a discovered static target as checked: the target
// loaded, or why it was refused.
type checkedTarget struct {
	target *model.StaticTarget
	err    error
}

// expand is in with the targets that the discovery sources found added.
// found is by source key; a source that has not yet answered has none.
//   - A probe job's discovered target comes after those the job writes and
//     those of the sources before it. One already listed, or that the
//     collector refuses, is left out.
//   - An entry's discovered target is named by discovery.Name. When the file
//     writes a target of that name, it is named <entry>_ and discovery.HashedAddress
//     instead. So its name depends on its entry, its address and the file
//     alone, and stays the same from one discovery to the next.
//   - A target the checks refuse is left out.
//
// skipped says what was left out. checked holds the targets checked before,
// by their text. The targets checked now are returned in its place, so a
// discovery that changes little checks little.
func (in *Inputs) expand(found map[string][]discovered, checked map[string]checkedTarget) (out *Inputs, skipped []string, now map[string]checkedTarget) {
	now = map[string]checkedTarget{}
	if len(in.sources) == 0 {
		return in, nil, now
	}
	digest := map[string]string{"files": in.digest}
	for _, s := range in.sources {
		if list, ok := found[s.key]; ok {
			text, _ := json.Marshal(groupsOf(list))
			digest["source/"+s.key] = string(text)
		}
	}
	cp := *in
	out = &cp
	out.fallbacks = nil
	out.digest = allocapi.Generation(digest)
	if in.Probes != nil {
		out.Probes = &ProbeFile{Jobs: make([]ProbeJob, len(in.Probes.Jobs))}
		for i := range in.Probes.Jobs {
			out.Probes.Jobs[i] = in.expandProbeJob(&in.Probes.Jobs[i], found, &skipped, &out.fallbacks)
		}
	}
	if len(in.entries) > 0 {
		in.expandStatic(out, found, checked, now, &skipped)
	}
	return out, skipped, now
}

func (in *Inputs) expandProbeJob(j *ProbeJob, found map[string][]discovered, skipped *[]string, fallbacks *[]weightFallback) ProbeJob {
	job := *j
	if len(j.Discovery) == 0 {
		return job
	}
	job.Targets = slices.Clone(j.Targets)
	job.labelsOf = map[string]map[string]string{}
	job.weightOf = map[string]int64{}
	seen := map[string]bool{}
	for _, target := range j.Targets {
		seen[target] = true
	}
	collector := model.CollectorByName(in.Config, j.Collector)
	for _, s := range in.sources {
		if s.kind != "probe" || s.owner != j.Name {
			continue
		}
		for _, d := range found[s.key] {
			target := discovery.FromTemplate(j.TargetTemplate, d.Address)
			if seen[target] {
				continue
			}
			if err := fetch.CheckTarget(collector, target, true); err != nil {
				*skipped = append(*skipped, fmt.Sprintf("probe job %q: target %q from %s: %v", j.Name, target, s.cfg.Name, err))
				continue
			}
			seen[target] = true
			job.Targets = append(job.Targets, target)
			job.labelsOf[target] = probeLabels(d.Labels)
			if j.WeightLabel != "" {
				value, present := d.Labels[j.WeightLabel]
				if w, reason := labelWeight(value, present); reason != "" {
					*fallbacks = append(*fallbacks, weightFallback{kind: "probe", owner: j.Name, source: s.cfg.Name, target: target, reason: reason})
				} else {
					job.weightOf[target] = w
				}
			}
		}
	}
	return job
}

// probeLabels are the labels of a discovered probe target that reach its
// service discovery group. These are the __meta_ ones, which Prometheus
// offers to relabeling and then drops, and those of a label name other than
// job and instance, which the scrape configuration and the allocator set.
// Never another one starting with __, which would steer the probe itself.
func probeLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range labels {
		switch {
		case value == "", !model.ValidLabelName(name), discovery.ReservedTargetLabels[name] && name != "static_target", name == MemberMetaLabel:
		case strings.HasPrefix(name, "__") && !strings.HasPrefix(name, "__meta_"):
		default:
			out[name] = value
		}
	}
	return out
}

// weightFallback is a discovered target whose weight_label gave no weight,
// for reason, and which takes its entry's or job's weight: of kind static
// or probe, owner the entry or the job, from source.
type weightFallback struct {
	kind, owner, source, target, reason string
}

// candidate is a target an entry's discovery found.
type candidate struct {
	entry           *discoveredEntry
	address, target string
	labels          map[string]string
	name            string
	// weight is its weight, in thousandths, and fallback why its entry's
	// weight_label gave none, from source, when it did not.
	weight           int64
	fallback, source string
}

func (in *Inputs) expandStatic(out *Inputs, found map[string][]discovered, checked, now map[string]checkedTarget, skipped *[]string) {
	var candidates []*candidate
	for _, e := range in.entries {
		addresses := map[string]bool{}
		for _, s := range in.sources {
			if s.kind != "static" || s.owner != e.name {
				continue
			}
			for _, d := range found[s.key] {
				if addresses[d.Address] {
					continue
				}
				addresses[d.Address] = true
				c := &candidate{entry: e, address: d.Address, target: discovery.FromTemplate(e.template, d.Address), labels: discovery.StaticLabels(d.Labels), name: discovery.Name(e.name, d.Address), weight: e.weight}
				if e.weightLabel != "" {
					value, present := d.Labels[e.weightLabel]
					if w, reason := labelWeight(value, present); reason != "" {
						c.fallback, c.source = reason, s.cfg.Name
					} else {
						c.weight = w
					}
				}
				candidates = append(candidates, c)
			}
		}
	}
	if len(candidates) == 0 {
		return
	}
	for _, c := range candidates {
		// A target the file writes keeps its name, and the discovered one
		// takes the form that is the address's digest: it stays so while
		// the file does, whatever else discovery finds.
		if _, written := in.targetIndex[c.name]; written {
			c.name = c.entry.name + "_" + discovery.HashedAddress(c.address)
		}
	}
	targets := &model.StaticTargetFile{Interval: in.Targets.Interval, Concurrency: in.Targets.Concurrency, Targets: slices.Clone(in.Targets.Targets)}
	static := &staticDocument{head: in.static.head, targets: slices.Clone(in.static.targets)}
	shardKeys := slices.Clone(in.shardKeys)
	weights := slices.Clone(in.weights)
	origins := make([]string, len(in.Targets.Targets), len(in.Targets.Targets)+len(candidates))
	copy(origins, in.origins)
	index := make(map[string]int, len(targets.Targets)+len(candidates))
	for name, i := range in.targetIndex {
		index[name] = i
	}
	for _, c := range candidates {
		if _, taken := index[c.name]; taken {
			*skipped = append(*skipped, fmt.Sprintf("%s entry %q: target %q from %s is named %s, as another target is", config.DiscoveredTargetsKey, c.entry.name, c.target, c.address, c.name))
			continue
		}
		node := c.entry.node(c.name, c.target, c.labels)
		text, _ := yaml.Marshal(node)
		result, known := checked[string(text)]
		if !known {
			t, err := checkStaticTarget(in.static.head, node)
			if err == nil {
				err = config.ValidateStaticTargetsAgainst(&model.StaticTargetFile{Targets: []model.StaticTarget{*t}}, in.Config)
			}
			result = checkedTarget{target: t, err: err}
		}
		now[string(text)] = result
		if result.err != nil {
			*skipped = append(*skipped, fmt.Sprintf("%s entry %q: target %q from %s: %v", config.DiscoveredTargetsKey, c.entry.name, c.target, c.address, result.err))
			continue
		}
		index[c.name] = len(targets.Targets)
		targets.Targets = append(targets.Targets, *result.target)
		static.targets = append(static.targets, node)
		key := c.name
		if c.entry.byAddress {
			key = c.address
		}
		shardKeys = append(shardKeys, key)
		weights = append(weights, c.weight)
		origins = append(origins, c.entry.name)
		if c.fallback != "" {
			out.fallbacks = append(out.fallbacks, weightFallback{kind: "static", owner: c.entry.name, source: c.source, target: c.name, reason: c.fallback})
		}
	}
	out.Targets, out.static, out.shardKeys, out.targetIndex, out.weights, out.origins = targets, static, shardKeys, index, weights, origins
}
