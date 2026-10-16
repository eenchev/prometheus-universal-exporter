package allocator

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Paths are the allocator's input files. Config is required; either target
// file may be left out.
type Paths struct {
	// Config is the main configuration the replicas are served, with the
	// collector files it lists.
	Config string
	// StaticTargets is the static target file, split among the replicas.
	StaticTargets string
	// ProbeTargets is the probe targets file (ProbeFile).
	ProbeTargets string
}

// Inputs are the allocator's input files as one checked whole: what every
// bundle and every service discovery answer is made from until the next
// change to them is accepted.
type Inputs struct {
	// Config is the configuration, as the exporter's loader read and
	// checked it.
	Config *model.Config
	// Targets is the static target file, checked against Config; nil
	// without one.
	Targets *model.StaticTargetFile
	// Probes is the probe targets file, checked against Config; nil
	// without one.
	Probes *ProbeFile
	// configFiles are the files of every bundle: the configuration at
	// allocapi.ConfigFile and its collector files at their paths relative
	// to its directory, each as written.
	configFiles map[string]string
	// collectors are the names of Config's collectors, which service
	// discovery routes a probe job by.
	collectors map[string]bool
	// static is the static target file as written, each target a document
	// of its own to put in a member's share.
	static *staticDocument
	// shardKeys are the static targets' shard keys, in file order, and
	// targetIndex their places by name.
	shardKeys   []string
	targetIndex map[string]int
	// digest identifies the files as read (snapshot.digest).
	digest string
}

// errChanged says the input files changed while they were being read: what
// was checked may not be what would be served, so the attempt is dropped
// and the files are read again at the next check. It is no refusal.
var errChanged = errors.New("the input files changed while they were read; they are read again at the next check")

// LoadInputs reads the input files and checks them as the exporter checks
// its own: the configuration with config.Load, the static target file with
// the checks of --static-targets-file, both target files against the
// configuration. On top of that it refuses a collector_files entry that is
// absolute or leaves the configuration's directory, since the bundle carries
// collector files at their paths relative to it, and one whose files would
// take the place of a bundle's own file on the replicas. The text of every
// file is read before and after the check, and the two must be the same: the
// files served are then exactly the files checked.
func LoadInputs(p Paths) (*Inputs, error) {
	before, err := takeSnapshot(p)
	if err != nil {
		return nil, err
	}
	conf, err := config.Load(p.Config, config.WithoutNamedFiles())
	if err != nil {
		return nil, err
	}
	in := &Inputs{Config: conf, configFiles: before.configFiles, digest: before.digest, collectors: map[string]bool{}}
	for i := range conf.Collectors {
		in.collectors[conf.Collectors[i].Name] = true
	}
	if p.StaticTargets != "" {
		targets, err := config.LoadStaticTargets(p.StaticTargets)
		if err == nil {
			err = config.ValidateStaticTargets(targets)
		}
		if err == nil {
			err = config.ValidateStaticTargetsAgainst(targets, conf)
		}
		if err != nil {
			return nil, fmt.Errorf("static target file %s: %w", p.StaticTargets, err)
		}
		in.Targets = targets
	}
	if p.ProbeTargets != "" {
		probes, err := parseProbeFile(p.ProbeTargets, before.probeTargets)
		if err == nil {
			err = checkProbeFile(probes, conf)
			if err != nil {
				err = fmt.Errorf("probe targets file %s: %w", p.ProbeTargets, err)
			}
		}
		if err != nil {
			return nil, err
		}
		in.Probes = probes
	}
	after, err := takeSnapshot(p)
	if err != nil || after.digest != before.digest || !sameFiles(before.read, conf.LoadedCollectorFiles) {
		return nil, errChanged
	}
	if in.Targets != nil {
		if in.static, err = readStaticDocument(before.staticTargets, in.Targets); err != nil {
			return nil, fmt.Errorf("static target file %s: %w", p.StaticTargets, err)
		}
		in.targetIndex = make(map[string]int, len(in.Targets.Targets))
		for i := range in.Targets.Targets {
			t := &in.Targets.Targets[i]
			key := t.ShardKey
			if key == "" {
				key = t.Name
			}
			in.shardKeys = append(in.shardKeys, key)
			in.targetIndex[t.Name] = i
		}
	}
	return in, nil
}

// sameFiles reports whether the collector files a snapshot read are the
// ones the loader read.
func sameFiles(read, loaded []string) bool {
	if len(read) != len(loaded) {
		return false
	}
	for i := range read {
		a, errA := filepath.Abs(read[i])
		b, errB := filepath.Abs(loaded[i])
		if errA != nil || errB != nil || a != b {
			return false
		}
	}
	return true
}

// snapshot is the text of the input files at one moment.
type snapshot struct {
	// configFiles are the bundle's shared files (Inputs.configFiles).
	configFiles map[string]string
	// read are the collector files read, as the loader names them.
	read                        []string
	staticTargets, probeTargets []byte
	// digest identifies all of it: the same files give the same digest.
	digest string
}

// takeSnapshot reads the input files. The collector files are found as the
// loader finds them (config's resolveCollectorFiles): each collector_files
// entry against the configuration's directory, a pattern's files in name
// order, each file once and never the configuration itself. A file that
// cannot be found is left out, for the loader to report.
func takeSnapshot(p Paths) (*snapshot, error) {
	raw, err := os.ReadFile(p.Config)
	if err != nil {
		return nil, fmt.Errorf("reading configuration file: %w", err)
	}
	s := &snapshot{configFiles: map[string]string{allocapi.ConfigFile: string(raw)}}
	var head struct {
		CollectorFiles []string `yaml:"collector_files"`
	}
	// A configuration that cannot be read this way is one the loader
	// refuses, with a better message than this would give.
	_ = yaml.Unmarshal(raw, &head)
	if err := checkCollectorEntries(head.CollectorFiles); err != nil {
		return nil, err
	}
	base := filepath.Dir(p.Config)
	self, _ := filepath.Abs(p.Config)
	seen := map[string]bool{}
	for _, entry := range head.CollectorFiles {
		joined := filepath.Join(base, entry)
		var matches []string
		if strings.ContainsAny(entry, "*?[") {
			found, err := filepath.Glob(joined)
			if err != nil {
				continue
			}
			sort.Strings(found)
			matches = found
		} else {
			matches = []string{joined}
		}
		for _, match := range matches {
			st, err := os.Stat(match)
			if err != nil || st.IsDir() {
				continue
			}
			abs, err := filepath.Abs(match)
			if err != nil {
				abs = match
			}
			if abs == self || seen[abs] {
				continue
			}
			seen[abs] = true
			rel, err := filepath.Rel(base, match)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("collector file %s is outside the configuration's directory %s; the bundle carries a collector file at its path relative to it, so keep the file inside it", match, base)
			}
			name := filepath.ToSlash(rel)
			if name == allocapi.ConfigFile || name == allocapi.StaticTargetsFile {
				return nil, fmt.Errorf("collector file %s would be %s in the bundle, which is the bundle's own file of that name; rename the collector file", match, name)
			}
			if top, _, _ := strings.Cut(name, "/"); strings.HasPrefix(top, "..") {
				return nil, fmt.Errorf("collector file %s would be %s in the bundle, but on the replicas the names beginning with \"..\" are the cache directory's own, and a bundle naming one is refused; rename it", match, name)
			}
			body, err := os.ReadFile(match) // #nosec G304 -- a collector file the configuration lists
			if err != nil {
				return nil, fmt.Errorf("reading collector file: %w", err)
			}
			s.configFiles[name] = string(body)
			s.read = append(s.read, match)
		}
	}
	if p.StaticTargets != "" {
		if s.staticTargets, err = os.ReadFile(p.StaticTargets); err != nil {
			return nil, fmt.Errorf("reading static target file: %w", err)
		}
	}
	if p.ProbeTargets != "" {
		if s.probeTargets, err = os.ReadFile(p.ProbeTargets); err != nil {
			return nil, fmt.Errorf("reading probe targets file: %w", err)
		}
	}
	digest := map[string]string{"static": string(s.staticTargets), "probe": string(s.probeTargets)}
	for name, body := range s.configFiles {
		digest["bundle/"+name] = body
	}
	s.digest = allocapi.Generation(digest)
	return s, nil
}

// checkCollectorEntries refuses a collector_files entry the bundle cannot
// carry: an absolute path, one that leaves the configuration's directory,
// a pattern that would match the bundle's static target file on the
// replicas, where the bundle's files lie side by side, and an entry of a
// directory and more whose directory could be one of the names beginning
// with ".." that the replica's cache directory keeps for itself, where
// ..data/config.yaml, say, would be read as a collector file.
func checkCollectorEntries(entries []string) error {
	for _, entry := range entries {
		if strings.TrimSpace(entry) == "" {
			// The loader refuses it, in its own words.
			continue
		}
		if filepath.IsAbs(entry) || strings.HasPrefix(entry, "/") {
			return fmt.Errorf("collector_files entry %q is an absolute path; the allocator serves collector files in the bundle at their paths relative to the configuration's directory, so write the entry relative to it", entry)
		}
		clean := filepath.ToSlash(filepath.Clean(entry))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("collector_files entry %q leaves the configuration's directory; the allocator serves collector files in the bundle at their paths relative to that directory, so keep them inside it", entry)
		}
		if matched, err := path.Match(clean, allocapi.StaticTargetsFile); err == nil && matched {
			return fmt.Errorf("collector_files entry %q would also match %s, the replica's share of the static targets, which lies beside the configuration in the bundle; narrow the entry, for example to a directory of collector files", entry, allocapi.StaticTargetsFile)
		}
		if top, rest, nested := strings.Cut(clean, "/"); nested && rest != "" && mayMatchDotDot(top) {
			return fmt.Errorf("collector_files entry %q begins with %q, which on the replicas could also match the cache directory's own names beginning with \"..\"; begin the entry with a directory's name, such as collectors/*.yaml", entry, top)
		}
	}
	return nil
}

// mayMatchDotDot reports whether the path element elem, a name or a
// pattern, could match a name beginning with "..": it begins with "..", or
// with a pattern character, or with a dot and one.
func mayMatchDotDot(elem string) bool {
	const special = "*?[\\"
	switch {
	case strings.HasPrefix(elem, ".."), elem != "" && strings.ContainsRune(special, rune(elem[0])):
		return true
	case len(elem) > 1 && elem[0] == '.' && strings.ContainsRune(special, rune(elem[1])):
		return true
	}
	return false
}

// staticDocument is the static target file as written, to be split into
// the members' shares: each a static target file of the same interval,
// concurrency and other top-level keys, with the targets of the share.
type staticDocument struct {
	// head are the top-level keys and their values, other than targets and
	// the file's x- keys.
	head []*yaml.Node
	// targets are the targets, in file order, each with its name written
	// out: a name left to its default, <collector>_<index>, would change
	// with the target's place in a share.
	targets []*yaml.Node
}

// readStaticDocument reads the static target file's text, whose loaded and
// checked form is f, into its parts. Every alias is replaced by what it
// names, so a share carries none that points into a target it does not
// hold, nor the x- keys that held the anchors.
func readStaticDocument(raw []byte, f *model.StaticTargetFile) (*staticDocument, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the file is not a mapping")
	}
	root := doc.Content[0]
	d := &staticDocument{}
	var targets *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		switch {
		case key.Value == "targets":
			targets = resolved(value)
		case strings.HasPrefix(key.Value, "x-"):
		default:
			d.head = append(d.head, resolved(key), resolved(value))
		}
	}
	if targets == nil || targets.Kind != yaml.SequenceNode || len(targets.Content) != len(f.Targets) {
		return nil, errors.New("its targets could not be told apart for the replicas' shares")
	}
	for i, target := range targets.Content {
		if target.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("target %d is not a mapping", i)
		}
		named := false
		for j := 0; j+1 < len(target.Content); j += 2 {
			named = named || target.Content[j].Value == "name"
		}
		if !named {
			target.Content = append([]*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.Targets[i].Name},
			}, target.Content...)
		}
		d.targets = append(d.targets, target)
	}
	return d, nil
}

// resolved is a copy of n with every alias replaced by a copy of what it
// names, every merge key (<<) by the keys it merges in, and no anchors or
// comments: a share holds none of the targets an alias could point into, and
// a replica reads it as the decoder read the file.
func resolved(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	out := &yaml.Node{Kind: n.Kind, Style: n.Style, Tag: n.Tag, Value: n.Value}
	if n.Kind != yaml.MappingNode {
		for _, child := range n.Content {
			out.Content = append(out.Content, resolved(child))
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
			out.Content = append(out.Content, resolved(key), resolved(value))
			continue
		}
		for _, entry := range model.MappingEntries(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{key, value}}) {
			if name := model.KeyName(entry.Key); !taken[name] {
				taken[name] = true
				out.Content = append(out.Content, resolved(entry.Key), resolved(entry.Value))
			}
		}
	}
	return out
}

// isMergeKey reports whether key is a merge key, <<, written out.
func isMergeKey(key *yaml.Node) bool {
	return key.Kind == yaml.ScalarNode && key.Tag == "!!merge"
}

// render is the static target file of the targets at indices, in the order
// given.
func (d *staticDocument) render(indices []int) (string, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, i := range indices {
		seq.Content = append(seq.Content, d.targets[i])
	}
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: slices.Clone(d.head)}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "targets"}, seq)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}
