package allocator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
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
}

// sampleAddress stands for a discovered address when an entry is checked
// at load: an address of TEST-NET-1 (RFC 5737), which reaches nothing.
const sampleAddress = "192.0.2.1:9100"

// targetName is a static target's name (config's targetNameRE).
var targetName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// splitDiscovered takes discovered_targets out of a static target file's
// text: the text of the rest, which the exporter's loader reads, and the
// entries, with aliases and merge keys replaced by what they stand for.
// Text without the key, or that is no mapping, is given back as it is.
func splitDiscovered(raw []byte) ([]byte, *yaml.Node) {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return raw, nil
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != config.DiscoveredTargetsKey {
			continue
		}
		entries := resolved(root.Content[i+1])
		root.Content = slices.Delete(root.Content, i, i+2)
		var b bytes.Buffer
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if enc.Encode(&doc) != nil || enc.Close() != nil {
			return raw, nil
		}
		return b.Bytes(), entries
	}
	return raw, nil
}

// readDiscoveredEntries reads and checks the discovered_targets entries.
// Each entry is checked as a static target of the file, head its top-level
// keys, with sampleAddress in place of a discovered address: whatever the
// entry writes is checked at load, and only the target and the labels of a
// discovered target are left to check when it is found.
func readDiscoveredEntries(list *yaml.Node, head []*yaml.Node, conf *model.Config, base string) ([]*discoveredEntry, error) {
	if list.Kind != yaml.SequenceNode || len(list.Content) == 0 {
		return nil, fmt.Errorf("%s must be a list of entries, each a static target with discovery", config.DiscoveredTargetsKey)
	}
	var out []*discoveredEntry
	seen := map[string]bool{}
	for i, node := range list.Content {
		e, err := readDiscoveredEntry(node, head, conf, base)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", config.DiscoveredTargetsKey, i, err)
		}
		if seen[e.name] {
			return nil, fmt.Errorf("%s entry %q is defined twice; give each entry its own name", config.DiscoveredTargetsKey, e.name)
		}
		seen[e.name] = true
		out = append(out, e)
	}
	return out, nil
}

func readDiscoveredEntry(node *yaml.Node, head []*yaml.Node, conf *model.Config, base string) (*discoveredEntry, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("it is not a mapping")
	}
	e := &discoveredEntry{}
	var shardKey string
	var discovery *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		switch key.Value {
		case "name":
			e.name = value.Value
		case "target_template":
			e.template = value.Value
		case "shard_key":
			shardKey = value.Value
		case "discovery":
			discovery = value
		case "target":
			return nil, errors.New("target is set, but an entry's targets are the ones its discovery finds; write target_template to make a target of each discovered address")
		default:
			e.keys = append(e.keys, key, value)
		}
	}
	if !targetName.MatchString(e.name) {
		return nil, fmt.Errorf("name %q is required and must be letters, digits and underscores, not starting with a digit: each discovered target is named <name>_<address>", e.name)
	}
	if discovery == nil {
		return nil, fmt.Errorf("entry %q has no discovery; a target of its own belongs under targets", e.name)
	}
	if err := decodeStrict(discovery, &e.discovery); err != nil {
		return nil, fmt.Errorf("entry %q discovery: %w", e.name, err)
	}
	if len(e.discovery) == 0 {
		return nil, fmt.Errorf("entry %q discovery must not be empty", e.name)
	}
	if err := checkDiscovery(e.discovery, base); err != nil {
		return nil, fmt.Errorf("entry %q: %w", e.name, err)
	}
	if err := checkTargetTemplate(e.template); err != nil {
		return nil, fmt.Errorf("entry %q: %w", e.name, err)
	}
	switch shardKey {
	case "":
	case ShardKeyAddress:
		e.byAddress = true
	default:
		return nil, fmt.Errorf("entry %q shard_key %q is not %s; leave it out to allocate each target by its name", e.name, shardKey, ShardKeyAddress)
	}
	t, err := checkStaticTarget(head, e.node(e.name, fromTemplate(e.template, sampleAddress), nil))
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

// sourceSpec is a discovery source of the inputs: its key, which is the
// same while its owner and its configuration are, its kind (probe or
// static) and owner (the probe job or the discovered_targets entry).
type sourceSpec struct {
	key, kind, owner string
	cfg              DiscoveryConfig
}

// discoverySources are the discovery sources of the inputs, the probe
// jobs' and then the entries', in file order.
func (in *Inputs) discoverySources() []sourceSpec {
	var out []sourceSpec
	add := func(kind, owner string, list []DiscoveryConfig) {
		for _, d := range list {
			text, _ := json.Marshal(d)
			out = append(out, sourceSpec{key: kind + "/" + owner + "/" + d.Name + "\x00" + string(text), kind: kind, owner: owner, cfg: d})
		}
	}
	if in.Probes != nil {
		for i := range in.Probes.Jobs {
			add("probe", in.Probes.Jobs[i].Name, in.Probes.Jobs[i].Discovery)
		}
	}
	for _, e := range in.entries {
		add("static", e.name, e.discovery)
	}
	return out
}

// checkedTarget is a discovered static target as checked: the target
// loaded, or why it was refused.
type checkedTarget struct {
	target *model.StaticTarget
	err    error
}

// expand is in with the targets the discovery sources found added, found
// by source key, a source that has not yet answered having none. A probe
// job's discovered target comes after those it writes and those of the
// sources before; one listed already, or that the collector refuses, is
// left out. An entry's discovered target is named <entry>_<address>, every
// character a name cannot hold written _, and, when that name is another
// target's too, followed by _ and 8 hex digits of the address's SHA-256, so
// the name of a target stays the same from one discovery to the next. A
// target the checks refuse is left out. skipped says what was left out.
// checked holds the targets checked before, by their text, and the
// targets checked now are returned in its place, so a discovery that
// changes little checks little.
func (in *Inputs) expand(found map[string][]discovered, checked map[string]checkedTarget) (out *Inputs, skipped []string, now map[string]checkedTarget) {
	now = map[string]checkedTarget{}
	if len(in.sources) == 0 {
		return in, nil, now
	}
	digest := map[string]string{"files": in.digest}
	for _, s := range in.sources {
		if list, ok := found[s.key]; ok {
			text, _ := json.Marshal(list)
			digest["source/"+s.key] = string(text)
		}
	}
	cp := *in
	out = &cp
	out.digest = allocapi.Generation(digest)
	if in.Probes != nil {
		out.Probes = &ProbeFile{Jobs: make([]ProbeJob, len(in.Probes.Jobs))}
		for i := range in.Probes.Jobs {
			out.Probes.Jobs[i] = in.expandProbeJob(&in.Probes.Jobs[i], found, &skipped)
		}
	}
	if len(in.entries) > 0 {
		in.expandStatic(out, found, checked, now, &skipped)
	}
	return out, skipped, now
}

func (in *Inputs) expandProbeJob(j *ProbeJob, found map[string][]discovered, skipped *[]string) ProbeJob {
	job := *j
	if len(j.Discovery) == 0 {
		return job
	}
	job.Targets = slices.Clone(j.Targets)
	job.labelsOf = map[string]map[string]string{}
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
			target := fromTemplate(j.TargetTemplate, d.Address)
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
		}
	}
	return job
}

// probeLabels are the labels of a discovered probe target that reach its
// service discovery group: the __meta_ ones, which Prometheus offers to
// relabeling and then drops, and those of a label name but job and
// instance, which the scrape configuration and the allocator set; never
// another starting with __, which would steer the probe itself.
func probeLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range labels {
		switch {
		case value == "", !model.ValidLabelName(name), reservedTargetLabels[name] && name != "static_target", name == MemberMetaLabel:
		case strings.HasPrefix(name, "__") && !strings.HasPrefix(name, "__meta_"):
		default:
			out[name] = value
		}
	}
	return out
}

// staticLabels are the labels of a discovered static target that it
// keeps: those the exporter takes on a static target, none starting with
// __, which only relabeling, absent here, would read.
func staticLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range labels {
		if value == "" || strings.HasPrefix(name, "__") || reservedTargetLabels[name] || model.CheckLabelName(name) != nil {
			continue
		}
		out[name] = value
	}
	return out
}

// candidate is a target an entry's discovery found.
type candidate struct {
	entry           *discoveredEntry
	address, target string
	labels          map[string]string
	name            string
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
				candidates = append(candidates, &candidate{entry: e, address: d.Address, target: fromTemplate(e.template, d.Address), labels: staticLabels(d.Labels), name: e.name + "_" + sanitizeLabelName(d.Address)})
			}
		}
	}
	if len(candidates) == 0 {
		return
	}
	count := map[string]int{}
	for i := range in.Targets.Targets {
		count[in.Targets.Targets[i].Name]++
	}
	for _, c := range candidates {
		count[c.name]++
	}
	for _, c := range candidates {
		if count[c.name] > 1 {
			sum := sha256.Sum256([]byte(c.address))
			c.name += "_" + hex.EncodeToString(sum[:4])
		}
	}
	targets := &model.StaticTargetFile{Interval: in.Targets.Interval, Concurrency: in.Targets.Concurrency, Targets: slices.Clone(in.Targets.Targets)}
	static := &staticDocument{head: in.static.head, targets: slices.Clone(in.static.targets)}
	shardKeys := slices.Clone(in.shardKeys)
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
	}
	out.Targets, out.static, out.shardKeys, out.targetIndex = targets, static, shardKeys, index
}
