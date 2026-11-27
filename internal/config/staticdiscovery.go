package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/discovery"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// A static target file's discovered_targets are static targets written once
// for every target their discovery finds. The target allocator reads every
// kind of source (internal/allocator). The exporter reads an entry whose
// sources are dns_sd ones, asks them itself (staticdiscoveryrun.go), and
// puts the targets they make in force beside the file's own, as a reload
// would. The keys, the names of the targets and the checks are the
// allocator's, so one file reads alike in both.

// discoverySampleAddress stands for a discovered address when an entry is
// checked at load: an address of TEST-NET-1 (RFC 5737), which reaches
// nothing.
const discoverySampleAddress = "192.0.2.1:9100"

// DefaultMaxDiscoveredTargets bounds the targets of one source's answer
// that sets no max_targets, as the allocator's --discovery.max-targets does
// by default.
const DefaultMaxDiscoveredTargets = 10000

// discoverySourceName is what a source may be called: it is a label value,
// and a word in logs.
var discoverySourceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// SplitDiscoveredTargets takes discovered_targets out of a static target file's
// text. It returns the text of the rest, which the exporter's loader reads,
// and the entries, with aliases and merge keys replaced by what they stand
// for. Text without the key, or that is no mapping, is given back as it is.
func SplitDiscoveredTargets(raw []byte) ([]byte, *yaml.Node) {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return raw, nil
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != DiscoveredTargetsKey {
			continue
		}
		entries := ResolvedNode(root.Content[i+1])
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

// discoveredTargetsDocument is the type the values written in a file's
// discovered_targets are checked by, as checkValues checks those of the
// rest of the file: its one key, discovered_targets, of entries of every key
// of a static target but target, and the entry's own. The checks read the
// file's schema, which describes the entries.
var discoveredTargetsDocument = func() reflect.Type {
	target := reflect.TypeOf(model.StaticTarget{})
	var fields []reflect.StructField
	for i := range target.NumField() {
		f := target.Field(i)
		if name, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); f.IsExported() && name != "target" && name != "-" && name != "" {
			fields = append(fields, reflect.StructField{Name: f.Name, Type: f.Type, Tag: f.Tag})
		}
	}
	fields = append(fields,
		reflect.StructField{Name: "TargetTemplate", Type: reflect.TypeOf(""), Tag: `yaml:"target_template"`},
		reflect.StructField{Name: "Discovery", Type: reflect.TypeOf([]exporterDiscoverySource{}), Tag: `yaml:"discovery"`},
	)
	return reflect.StructOf([]reflect.StructField{{Name: "Entries", Type: reflect.SliceOf(reflect.StructOf(fields)), Tag: `yaml:"discovered_targets"`}})
}()

// discoveredValueProblems are the values written in raw's
// discovered_targets that are not of the kind their keys take, such as a
// boolean where a name goes, each with its line in raw.
func discoveredValueProblems(raw []byte) error {
	// Decoded on its own, which other keys of the file it leaves alone, the
	// document says what is of the wrong kind for the decoder too, such as
	// a boolean where a port goes.
	v := reflect.New(discoveredTargetsDocument)
	return yamlError(withValueProblems(yaml.Unmarshal(raw, v.Interface()), raw, v.Interface()))
}

// fileHead are the top-level keys of a static target file's text and their
// values, other than targets and the file's x- keys: those every target of
// an entry is read with.
func fileHead(raw []byte) []*yaml.Node {
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := doc.Content[0]
	var head []*yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if key := root.Content[i]; key.Value != "targets" && !strings.HasPrefix(key.Value, "x-") {
			head = append(head, ResolvedNode(key), ResolvedNode(root.Content[i+1]))
		}
	}
	return head
}

// mappingText is the YAML of a mapping of the keys and values of content,
// without the lines they were written on, so two files that write an entry
// alike read as the same entry wherever they write it.
func mappingText(content []*yaml.Node) string {
	if len(content) == 0 {
		return ""
	}
	raw, err := yaml.Marshal(&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content})
	if err != nil {
		return ""
	}
	return string(raw)
}

// mappingOf is the keys and values of a mapping that mappingText wrote.
func mappingOf(text string) []*yaml.Node {
	var doc yaml.Node
	if text == "" || yaml.Unmarshal([]byte(text), &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0].Content
}

// readDiscoveredEntries reads and checks the exporter's discovered_targets
// entries. Each is checked as a static target of the file, with head as
// its top-level keys and discoverySampleAddress in place of a discovered
// address, so whatever the entry writes is checked at load. Only the target
// and the labels of a discovered target are left to check when it is
// found.
func readDiscoveredEntries(list *yaml.Node, head []*yaml.Node) ([]model.DiscoveredEntry, error) {
	if list.Kind != yaml.SequenceNode || len(list.Content) == 0 {
		return nil, fmt.Errorf("%s must be a list of entries, each a static target with discovery", DiscoveredTargetsKey)
	}
	var out []model.DiscoveredEntry
	seen := map[string]bool{}
	for i, node := range list.Content {
		e, err := readDiscoveredEntry(node, head)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", DiscoveredTargetsKey, i, err)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("%s entry %q is defined twice; give each entry its own name", DiscoveredTargetsKey, e.Name)
		}
		seen[e.Name] = true
		out = append(out, *e)
	}
	// A target is named <entry>_<address>, so were one entry's name another's
	// followed by _, the two could name a target alike, and which got the
	// name would turn on what else discovery found.
	for _, a := range out {
		for _, b := range out {
			if a.Name != b.Name && strings.HasPrefix(b.Name, a.Name+"_") {
				return nil, fmt.Errorf("%s entry %q begins with entry %q's name and _, so a target of one could be named as a target of the other; rename one", DiscoveredTargetsKey, b.Name, a.Name)
			}
		}
	}
	return out, nil
}

func readDiscoveredEntry(node *yaml.Node, head []*yaml.Node) (*model.DiscoveredEntry, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("it is not a mapping")
	}
	e := &model.DiscoveredEntry{Head: mappingText(head)}
	var keys []*yaml.Node
	var sources *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		switch key.Value {
		case "name":
			e.Name = value.Value
		case "target_template":
			e.Template = value.Value
		case "discovery":
			sources = value
		case "shard_key":
			// The allocator's, which allocates the entry's targets by their
			// addresses; the exporter scrapes every one.
			if value.Value != "" && value.Value != "address" {
				return nil, fmt.Errorf("shard_key %q is not address; leave it out to allocate each target by its name", value.Value)
			}
		case "target":
			return nil, errors.New("target is set, but an entry's targets are the ones its discovery finds; write target_template to make a target of each discovered address")
		default:
			keys = append(keys, key, value)
		}
	}
	if !targetNameRE.MatchString(e.Name) {
		return nil, fmt.Errorf("name %q is required and must be letters, digits and underscores, not starting with a digit: each discovered target is named <name>_<address>", e.Name)
	}
	if sources == nil {
		return nil, fmt.Errorf("entry %q has no discovery; a target of its own belongs under targets", e.Name)
	}
	e.Keys = mappingText(keys)
	if err := decodeSources(sources, &e.Sources); err != nil {
		return nil, fmt.Errorf("entry %q discovery: %w", e.Name, err)
	}
	if len(e.Sources) == 0 {
		return nil, fmt.Errorf("entry %q discovery must not be empty", e.Name)
	}
	if err := checkDiscoverySources(e.Sources); err != nil {
		return nil, fmt.Errorf("entry %q: %w", e.Name, err)
	}
	if err := discovery.CheckTemplate(e.Template); err != nil {
		return nil, fmt.Errorf("entry %q: %w", e.Name, err)
	}
	sample, err := discoveredTarget(e, discoveryTargetNode(e, e.Name, discovery.FromTemplate(e.Template, discoverySampleAddress), nil))
	if err != nil {
		return nil, fmt.Errorf("entry %q, checked with the address %s in place of a discovered one: %w", e.Name, discoverySampleAddress, err)
	}
	e.Sample = *sample
	return e, nil
}

// discoverySourceKeys are the keys a source of an entry may have, the
// allocator's kinds of source among them, which are refused by their name,
// and dnsSDKeys those of its dns_sd block.
var (
	discoverySourceKeys = []string{"name", "dns_sd", "labels", "max_targets", "file_sd", "http_sd", "kubernetes"}
	dnsSDKeys           = []string{"names", "type", "port", "server", "refresh_interval", "timeout"}
)

// decodeSources decodes an entry's discovery list, as written, into out,
// refusing a key a source does not take, each refusal with the line of the
// file it is on.
func decodeSources(node *yaml.Node, out *[]model.DiscoverySource) error {
	if node.Kind == yaml.SequenceNode {
		for _, source := range node.Content {
			if source.Kind != yaml.MappingNode {
				continue
			}
			for i := 0; i+1 < len(source.Content); i += 2 {
				key, value := source.Content[i], source.Content[i+1]
				if !slices.Contains(discoverySourceKeys, key.Value) {
					return fmt.Errorf("line %d: a source has no key %s; it has %s", key.Line, key.Value, strings.Join(discoverySourceKeys[:4], ", "))
				}
				if key.Value != "dns_sd" || value.Kind != yaml.MappingNode {
					continue
				}
				for j := 0; j+1 < len(value.Content); j += 2 {
					if k := value.Content[j]; !slices.Contains(dnsSDKeys, k.Value) {
						return fmt.Errorf("line %d: dns_sd has no key %s; it has %s", k.Line, k.Value, strings.Join(dnsSDKeys, ", "))
					}
				}
			}
		}
	}
	if err := node.Decode(out); err != nil {
		return yamlError(err)
	}
	return nil
}

// checkDiscoverySources checks an entry's sources, as the allocator checks
// a dns_sd one, and refuses the allocator's other kinds by name. It fills in
// each source's name, its kind and its place, such as dns_sd_0, when it has
// none.
func checkDiscoverySources(list []model.DiscoverySource) error {
	names := map[string]bool{}
	for i := range list {
		d := &list[i]
		for kind, set := range map[string]bool{"file_sd": d.FileSD != nil, "http_sd": d.HTTPSD != nil, "kubernetes": d.Kubernetes != nil} {
			if set {
				return fmt.Errorf("discovery source %d is a %s source, which the target allocator (pue-allocator --static-targets-file) asks; the exporter asks dns_sd sources alone, so run the exporter with the allocator, or find the targets by DNS", i, kind)
			}
		}
		if d.DNSSD == nil {
			return fmt.Errorf("discovery source %d must set dns_sd, the one kind of source the exporter asks", i)
		}
		if d.Name == "" {
			d.Name = fmt.Sprintf("dns_sd_%d", i)
		}
		if !discoverySourceName.MatchString(d.Name) {
			return fmt.Errorf("discovery source %d has the name %q; a name is letters, digits, _, . and -, not starting with . or -", i, d.Name)
		}
		if names[d.Name] {
			return fmt.Errorf("discovery source %q is named twice; give each source its own name", d.Name)
		}
		names[d.Name] = true
		if err := checkDiscoverySource(d); err != nil {
			return fmt.Errorf("discovery source %q: %w", d.Name, err)
		}
	}
	return nil
}

func checkDiscoverySource(d *model.DiscoverySource) error {
	if d.MaxTargets < 0 {
		return fmt.Errorf("max_targets must not be negative; leave it out, or 0, for %d", DefaultMaxDiscoveredTargets)
	}
	for _, name := range model.SortedKeys(d.Labels) {
		from := d.Labels[name]
		switch {
		case !model.ValidLabelName(name):
			return fmt.Errorf("labels sets %q, which is not a label name", name)
		case strings.HasPrefix(name, "__"):
			return fmt.Errorf("labels sets %q; a label starting with __ is Prometheus's own", name)
		case discovery.ReservedTargetLabels[name]:
			return fmt.Errorf("labels sets %s, which the allocator, the exporter or Prometheus sets on every target", name)
		case !model.ValidLabelName(from):
			return fmt.Errorf("labels sets %s from %q, which is not a label name", name, from)
		}
	}
	refresh, text := discoveryRefresh(d)
	if refresh < time.Second {
		return fmt.Errorf("refresh_interval %s is under the least, 1s", refresh)
	}
	return discovery.CheckDNSSD(d.DNSSD, refresh, text)
}

// discoveryRefresh is how often a source is asked, its refresh_interval or
// dns_sd's default, and the words that name it.
func discoveryRefresh(d *model.DiscoverySource) (time.Duration, string) {
	if set := d.DNSSD.RefreshInterval; set != 0 {
		return time.Duration(set), fmt.Sprintf("its refresh_interval %s", time.Duration(set))
	}
	return discovery.DefaultDNSSDRefresh, fmt.Sprintf("dns_sd's default refresh interval %s", discovery.DefaultDNSSDRefresh)
}

// discoveryTimeout is how long one ask of a source may take: its timeout,
// or discovery.DefaultTimeout, cut to its refresh interval.
func discoveryTimeout(d *model.DiscoverySource) time.Duration {
	refresh, _ := discoveryRefresh(d)
	if set := d.DNSSD.Timeout; set != 0 {
		return time.Duration(set)
	}
	return min(discovery.DefaultTimeout, refresh)
}

// discoverySourceKey names a source of an entry for as long as the entry's
// name and the source's configuration stay the same, so a reload that keeps
// both keeps what the source found.
func discoverySourceKey(e *model.DiscoveredEntry, d *model.DiscoverySource) string {
	text, _ := json.Marshal(d)
	return e.Name + "\x00" + string(text)
}

// discoveryTargetNode is the static target an entry writes for a
// discovered target: its name, its target, its labels over the discovered
// ones, and the entry's other keys.
func discoveryTargetNode(e *model.DiscoveredEntry, name, target string, found map[string]string) *yaml.Node {
	str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{str("name"), str(name), str("target"), str(target)}}
	labels := map[string]string{}
	for name, value := range found {
		labels[name] = value
	}
	keys := mappingOf(e.Keys)
	for i := 0; i+1 < len(keys); i += 2 {
		if keys[i].Value == "labels" {
			// Every label the entry writes wins, and is checked as any
			// target's is, at load.
			var own map[string]string
			_ = keys[i+1].Decode(&own)
			for name, value := range own {
				labels[name] = value
			}
			continue
		}
		n.Content = append(n.Content, keys[i], keys[i+1])
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

// discoveredTarget reads target, a node of e, as a static target of a file
// of e's head, with the loader and the checks of the file alone, and
// returns it as loaded.
func discoveredTarget(e *model.DiscoveredEntry, target *yaml.Node) (*model.StaticTarget, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: mappingOf(e.Head)}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "targets"}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{target}})
	raw, err := yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	f, err := ParseStaticTargets("(discovered target)", raw)
	if err == nil {
		err = ValidateStaticTargets(f)
	}
	if err != nil {
		return nil, err
	}
	return &f.Targets[0], nil
}

// discoveryChecks are the discovered targets checked, by their text, with
// the configuration they were checked against, so a discovery that changes
// little checks little.
type discoveryChecks struct {
	config  *model.Config
	targets map[string]discoveryCheck
}

type discoveryCheck struct {
	target *model.StaticTarget
	err    error
}

// expandDiscovered is f with the targets its entries make of found, by
// source key, after the targets it writes, each checked against c, and
// what was left out, each as a sentence.
//   - An entry's targets are those its sources found, the first source to
//     give an address taking it, in order.
//   - A target is named by discovery.Name, or, when the file writes a target
//     of that name, <entry>_ and discovery.HashedAddress. So its name depends
//     on its entry, its address and the file alone, and stays the same from
//     one discovery to the next.
//   - A target the checks refuse is left out.
//
// checks holds the targets checked before; those checked now are returned
// in its place.
func expandDiscovered(f *model.StaticTargetFile, found map[string][]discovery.Target, c *model.Config, checks discoveryChecks) (*model.StaticTargetFile, []string, discoveryChecks) {
	now := discoveryChecks{config: c, targets: map[string]discoveryCheck{}}
	if checks.config != c {
		checks.targets = nil
	}
	written := f.Targets[:min(f.Written, len(f.Targets))]
	taken := make(map[string]bool, len(written))
	for _, t := range written {
		taken[t.Name] = true
	}
	writtenNames := maps.Clone(taken)
	out := *f
	out.Targets = slices.Clone(written)
	var skipped []string
	for i := range f.Discovered {
		e := &f.Discovered[i]
		addresses := map[string]bool{}
		for j := range e.Sources {
			s := &e.Sources[j]
			for _, d := range found[discoverySourceKey(e, s)] {
				if addresses[d.Address] {
					continue
				}
				addresses[d.Address] = true
				target := discovery.FromTemplate(e.Template, d.Address)
				name := discovery.Name(e.Name, d.Address)
				if writtenNames[name] {
					name = e.Name + "_" + discovery.HashedAddress(d.Address)
				}
				if taken[name] {
					skipped = append(skipped, fmt.Sprintf("%s entry %q: target %q from %s is named %s, as another target is", DiscoveredTargetsKey, e.Name, target, d.Address, name))
					continue
				}
				node := discoveryTargetNode(e, name, target, discovery.StaticLabels(d.Labels))
				text, _ := yaml.Marshal(node)
				result, known := checks.targets[string(text)]
				if !known {
					t, err := discoveredTarget(e, node)
					if err == nil {
						err = checkTargetsAgainst([]model.StaticTarget{*t}, c)
					}
					result = discoveryCheck{target: t, err: err}
				}
				now.targets[string(text)] = result
				if result.err != nil {
					skipped = append(skipped, fmt.Sprintf("%s entry %q: target %q from %s: %v", DiscoveredTargetsKey, e.Name, target, d.Address, result.err))
					continue
				}
				taken[name] = true
				out.Targets = append(out.Targets, *result.target)
			}
		}
	}
	return &out, skipped, now
}

// ResolvedNode is a copy of n with every alias replaced by a copy of what it
// names, every merge key (<<) replaced by the keys it merges in, and no
// anchors or comments. So a part of a document holds none of the nodes an alias
// could point into elsewhere, and reads as the decoder read the whole.
func ResolvedNode(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	out := &yaml.Node{Kind: n.Kind, Style: n.Style, Tag: n.Tag, Value: n.Value, Line: n.Line, Column: n.Column}
	if n.Kind != yaml.MappingNode {
		for _, child := range n.Content {
			out.Content = append(out.Content, ResolvedNode(child))
		}
		return out
	}
	// A key written out wins over a merged one, and of two merged ones the
	// first, as the decoder has them.
	taken := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if !isMergeKey(n.Content[i]) {
			taken[model.KeyName(n.Content[i])] = true
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if !isMergeKey(key) {
			out.Content = append(out.Content, ResolvedNode(key), ResolvedNode(value))
			continue
		}
		for _, entry := range model.MappingEntries(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{key, value}}) {
			if name := model.KeyName(entry.Key); !taken[name] {
				taken[name] = true
				out.Content = append(out.Content, ResolvedNode(entry.Key), ResolvedNode(entry.Value))
			}
		}
	}
	return out
}

// isMergeKey reports whether key is a merge key, <<, written out.
func isMergeKey(key *yaml.Node) bool {
	return key.Kind == yaml.ScalarNode && key.Tag == "!!merge"
}
