package allocator

import (
	"bytes"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// A target's weight says how much of a member it takes, against the other
// targets: 1 unless its file says otherwise. The allocator reads the
// weights of the static target file, weight on a target and on a
// discovered_targets entry and default_weight at the top, and takes them
// out of the file before the exporter's loader reads it, so a member's
// share is a static target file the exporter takes as it is. A discovered
// target may take its weight from a label of its source (weight_label), and
// its entry's or job's weight when the label does not give one.

// weightRule is what a weight must be, for the messages that refuse one.
const weightRule = "a weight is a number above 0 and at most 1000, with at most three decimals, such as 2 or 0.5"

// The keys of weights.
const (
	weightKey        = "weight"
	defaultWeightKey = "default_weight"
	weightLabelKey   = "weight_label"
)

// weightMilli is f as a weight in thousandths, and whether it is one: above
// 0, finite, at most MaxWeight, and of at most weightDecimals decimals.
func weightMilli(f float64) (int64, string) {
	switch {
	case math.IsNaN(f):
		return 0, fallbackNotANumber
	case math.IsInf(f, 0) || f <= 0 || f > MaxWeight:
		return 0, fallbackOutOfRange
	}
	m := f * milli
	r := math.Round(m)
	// A decimal of three places is a float a little off its thousandths:
	// 1.005 is 1004.9999999999999 of them.
	if math.Abs(m-r) > 1e-6 || r < 1 {
		return 0, fallbackDecimals
	}
	return int64(r), ""
}

// The reasons a discovered weight is not taken, the reason label of
// pue_allocator_weight_label_fallbacks_total.
const (
	fallbackMissing    = "missing"
	fallbackNotANumber = "not_a_number"
	fallbackOutOfRange = "out_of_range"
	fallbackDecimals   = "too_many_decimals"
)

// labelWeight is the weight a label's value gives, in thousandths, or why
// it gives none.
func labelWeight(value string, present bool) (int64, string) {
	if !present || value == "" {
		return 0, fallbackMissing
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fallbackNotANumber
	}
	return weightMilli(f)
}

// nodeWeight reads a weight a file writes: a YAML number, refused with a
// message naming where unless it is a weight.
func nodeWeight(n *yaml.Node, where string) (int64, error) {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	var f float64
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!float") || n.Decode(&f) != nil {
		return 0, fmt.Errorf("%s %q is not a number; %s", where, n.Value, weightRule)
	}
	w, reason := weightMilli(f)
	if reason != "" {
		return 0, fmt.Errorf("%s %s is not a weight; %s", where, n.Value, weightRule)
	}
	return w, nil
}

// staticWeights are the weights of a static target file: its
// default_weight, milli when it sets none, and each written target's, by
// its place in targets, the default for one that sets none.
type staticWeights struct {
	defaultWeight int64
	targets       []int64
}

// stripStaticWeights takes default_weight and every target's weight out of
// a static target file's text, discovered_targets already taken out
// (splitDiscovered), and checks them. Text that has neither is given back
// as it is, so the exporter's loader reads the file as written and says
// what it says of it; otherwise the targets, and the top level, are
// written with every alias and merge key replaced (resolved), so a weight
// merged in is taken out too. Text that is not a mapping, or whose targets
// are not a list of mappings, is given back as it is, for the loader to
// refuse.
func stripStaticWeights(raw []byte) ([]byte, staticWeights, error) {
	w := staticWeights{defaultWeight: milli}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return raw, w, nil //nolint:nilerr // the loader refuses the text, in its own words
	}
	root := resolved(doc.Content[0])
	found := false
	var targets *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case defaultWeightKey:
			found = true
			v, err := nodeWeight(root.Content[i+1], defaultWeightKey)
			if err != nil {
				return nil, w, err
			}
			w.defaultWeight = v
		case "targets":
			targets = root.Content[i+1]
		}
	}
	if targets != nil && targets.Kind == yaml.SequenceNode {
		for _, t := range targets.Content {
			if t.Kind != yaml.MappingNode {
				continue
			}
			for i := 0; i+1 < len(t.Content); i += 2 {
				found = found || t.Content[i].Value == weightKey || t.Content[i].Value == weightLabelKey
			}
		}
	}
	if !found {
		return raw, w, nil
	}
	root.Content = withoutKey(root.Content, defaultWeightKey)
	if targets != nil && targets.Kind == yaml.SequenceNode {
		for n, t := range targets.Content {
			own := w.defaultWeight
			if t.Kind == yaml.MappingNode {
				name := fmt.Sprintf("target %d", n)
				for i := 0; i+1 < len(t.Content); i += 2 {
					if t.Content[i].Value == "name" {
						name = fmt.Sprintf("target %d (%s)", n, t.Content[i+1].Value)
					}
				}
				for i := 0; i+1 < len(t.Content); i += 2 {
					switch t.Content[i].Value {
					case weightKey:
						v, err := nodeWeight(t.Content[i+1], name+" weight")
						if err != nil {
							return nil, w, err
						}
						own = v
					case weightLabelKey:
						return nil, w, fmt.Errorf("%s sets weight_label, which a discovered_targets entry reads from what its discovery finds; a target written out takes weight", name)
					}
				}
				t.Content = withoutKey(t.Content, weightKey)
			}
			w.targets = append(w.targets, own)
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil || enc.Close() != nil {
		return raw, w, nil //nolint:nilerr // nodes just read write back but for a bug; the loader then reads the text as written
	}
	return b.Bytes(), w, nil
}

// withoutKey is the keys and values of a mapping's content but key's.
func withoutKey(content []*yaml.Node, key string) []*yaml.Node {
	out := content[:0:0]
	for i := 0; i+1 < len(content); i += 2 {
		if content[i].Value != key {
			out = append(out, content[i], content[i+1])
		}
	}
	return out
}

// fallbackKey is a series of pue_allocator_weight_label_fallbacks_total.
type fallbackKey struct{ kind, owner, reason string }

// noteFallbacks takes the discovered targets that fall back on their
// owner's weight now: it logs each source and reason once while any of its
// targets falls back for it, and returns, by series, the targets that have
// begun to since the last expansion. Called under reloadMu.
func (a *Allocator) noteFallbacks(list []weightFallback) map[fallbackKey]uint64 {
	now := make(map[weightFallback]bool, len(list))
	logged := map[string]bool{}
	type group struct {
		f       weightFallback
		targets []string
	}
	var groups []*group
	byKey := map[string]*group{}
	counts := map[fallbackKey]uint64{}
	for _, f := range list {
		now[f] = true
		if !a.fallingBack[f] {
			counts[fallbackKey{f.kind, f.owner, f.reason}]++
		}
		key := f.kind + "\x00" + f.owner + "\x00" + f.source + "\x00" + f.reason
		logged[key] = true
		if a.fallbackLogged[key] {
			continue
		}
		g := byKey[key]
		if g == nil {
			g = &group{f: f}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.targets = append(g.targets, f.target)
	}
	for _, g := range groups {
		shown := g.targets
		if len(shown) > 5 {
			shown = shown[:5]
		}
		a.logger.Warn("discovered targets give no weight in their weight_label and take their owner's weight", "kind", g.f.kind, "owner", g.f.owner, "source", g.f.source, "reason", g.f.reason, "count", len(g.targets), "targets", shown)
	}
	a.fallingBack, a.fallbackLogged = now, logged
	return counts
}

// weightHysteresis is how far, as a fraction, a discovered weight must move
// from the weight in force for a target to take the new one: a label that
// wavers a little moves no target, and changes no bundle.
const weightHysteresis = 0.2

// holdWeights is found, a source's new answer, with the weight label of
// each target whose weight moved less than weightHysteresis from the one
// in force, its value in before, the answer taken last, put back to that
// value: what the source holds, which a follower takes from the leader's
// snapshot, carries the weights in force, so every instance expands it
// alike. A value that gives no weight, before or now, is taken as it is.
// before is not changed, nor are the label maps of found.
func holdWeights(before, found []discovered, label string) []discovered {
	if label == "" || len(before) == 0 {
		return found
	}
	held := make(map[string]string, len(before))
	for _, d := range before {
		if value, ok := d.Labels[label]; ok {
			if _, seen := held[d.Address]; !seen {
				held[d.Address] = value
			}
		}
	}
	var out []discovered
	for i, d := range found {
		old, ok := held[d.Address]
		value, present := d.Labels[label]
		if !ok || !present || old == value {
			continue
		}
		was, r1 := labelWeight(old, true)
		now, r2 := labelWeight(value, true)
		if r1 != "" || r2 != "" || math.Abs(float64(now-was)) >= weightHysteresis*float64(was) {
			continue
		}
		if out == nil {
			out = slices.Clone(found)
		}
		labels := maps.Clone(d.Labels)
		labels[label] = old
		out[i].Labels = labels
	}
	if out == nil {
		return found
	}
	return out
}
