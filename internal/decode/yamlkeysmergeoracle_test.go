package decode

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// yamlWalkBefore is yamlWalk as it decoded a document before the pairs of a
// merged mapping were handed to the library a run at a time: every function
// of it that calls itself for what a node holds, as it was to the letter, so
// that the merged pairs are decoded by the function below, one call of the
// library for each key and one for each value. What these functions call
// and that calls none of them back — what is known of a node, the count of
// the aliases, the library, the items of a sequence handed together — is
// yamlWalk's own, which did not change.
type yamlWalkBefore struct {
	yamlWalk
}

// yamlWithTheWalkBefore decodes a parsed document as yamlReading.value does,
// with a mapping large past so many keys, by yamlWalkBefore where it is
// decoded in parts; a document decoded another way is decoded as it is now,
// since no other way changed.
func yamlWithTheWalkBefore(root *yaml.Node, large int) (outcome yamlOutcome) {
	defer func() {
		if failed := recover(); failed != nil {
			outcome = yamlOutcome{failed: failed}
		}
	}()
	learnt := yamlLearnt{large: large}
	learnt.learn(root, true)
	if learnt.tooDeep(root) || learnt.way(root) != yamlByParts {
		return yamlWith(root, large, nil)
	}
	walk := yamlWalkBefore{yamlWalk{large: large, aliases: learnt.aliases, known: map[*yaml.Node]yamlKnown{}, expanding: map[*yaml.Node]bool{}}}
	walk.null = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
	walk.learn(root)
	v, err := walk.value(root)
	if err == nil && len(walk.problems) > 0 {
		err = &yaml.TypeError{Errors: walk.problems}
	}
	if err != nil {
		return yamlOutcome{err: yamlFailure(err)}
	}
	return yamlOutcome{value: v}
}

func (w *yamlWalkBefore) value(n *yaml.Node) (any, error) {
	known := w.of(n)
	if !known.mine {
		if err := w.count(known); err != nil {
			return nil, err
		}
		var v any
		err := w.library(n, &v)
		return v, err
	}
	if err := w.step(); err != nil {
		return nil, err
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, nil
		}
		return w.value(n.Content[0])
	case yaml.AliasNode:
		if w.expanding[n] {
			return nil, yamlHoldsItself(n)
		}
		w.expanding[n] = true
		w.depth++
		v, err := w.value(n.Alias)
		w.depth--
		delete(w.expanding, n)
		return v, err
	case yaml.SequenceNode:
		return w.sequence(n)
	case yaml.MappingNode:
		return w.mapping(n)
	}
	var v any
	err := w.library(n, &v)
	return v, err
}

func (w *yamlWalkBefore) sequence(n *yaml.Node) (any, error) {
	items := make([]any, 0, len(n.Content))
	// held is what is known of the items n.Content[from:i], which the
	// library decodes together.
	from := 0
	var held yamlKnown
	var err error
	for i, item := range n.Content {
		known := w.of(item)
		if known.mine || known.part().aliases() {
			if items, err = w.items(n, from, i, items); err != nil {
				return nil, err
			}
			from, held = i+1, yamlKnown{}
			v, err := w.value(item)
			if err != nil {
				return nil, err
			}
			items = append(items, v)
			continue
		}
		if i-from >= max(w.large, 1) || held.and(known).part().aliases() {
			if items, err = w.items(n, from, i, items); err != nil {
				return nil, err
			}
			from, held = i, yamlKnown{}
		}
		if refused := w.count(known); refused != nil {
			// What the items before it fail with comes first, as it did.
			if _, err = w.items(n, from, i, items); err != nil {
				return nil, err
			}
			return nil, refused
		}
		held = held.and(known)
	}
	if items, err = w.items(n, from, len(n.Content), items); err != nil {
		return nil, err
	}
	return items, nil
}

func (w *yamlWalkBefore) mapping(n *yaml.Node) (any, error) {
	m := &yamlMap{}
	if yamlKeysAreText(n) {
		m.text = map[string]any{}
	} else {
		m.general = map[any]any{}
	}
	// held is the pairs n.Content[from:i], which the library decodes
	// together, and what is known of them.
	from := 0
	var held, keys yamlKnown
	var merge *yaml.Node
	hand := func(to int) error {
		if to == from {
			return nil
		}
		part := yaml.Node{Kind: yaml.MappingNode, Style: n.Style, Tag: n.Tag, Line: n.Line, Column: n.Column, Content: n.Content[from:to]}
		err := w.count(held)
		held = yamlKnown{}
		if err != nil {
			return err
		}
		return w.library(&part, m.into())
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		ofKey, ofValue := w.of(key), w.of(value)
		keys = keys.and(ofKey)
		pair := ofKey.and(ofValue)
		isMerge := yamlMerges(key)
		if isMerge || pair.mine || pair.part().aliases() {
			if err := hand(i); err != nil {
				return nil, err
			}
			from = i + 2
			if isMerge {
				merge = value
				continue
			}
			name, good, err := w.key(key, m, n)
			if err != nil {
				return nil, err
			}
			if !good {
				continue
			}
			v, err := w.value(value)
			if err != nil {
				return nil, err
			}
			m.set(name, v)
			continue
		}
		if (i-from)/2 >= max(w.large, 1) || held.and(pair).part().aliases() {
			if err := hand(i); err != nil {
				return nil, err
			}
			from = i
		}
		held = held.and(pair)
	}
	if err := hand(len(n.Content) &^ 1); err != nil {
		return nil, err
	}
	if merge != nil {
		// The library decodes the mapping's keys once more, to know which a
		// merge may not replace: those are the keys of the map, and the
		// merge key's own text.
		if err := w.count(keys); err != nil {
			return nil, err
		}
		if err := w.merge(merge, m); err != nil {
			return nil, err
		}
	}
	return m.value(), nil
}

func (w *yamlWalkBefore) key(key *yaml.Node, m *yamlMap, n *yaml.Node) (name any, good bool, err error) {
	known := w.of(key)
	if !known.mine && !known.part().aliases() {
		if err := w.count(known); err != nil {
			return nil, false, err
		}
		// Decoded as the one key of a mapping, beside a value that is
		// nothing: the map then holds the key, if it is good.
		w.two = [2]*yaml.Node{key, &w.null}
		w.pair = yaml.Node{Kind: yaml.MappingNode, Style: n.Style, Tag: n.Tag, Line: n.Line, Column: n.Column, Content: w.two[:]}
		if m.text != nil {
			if w.oneText == nil {
				w.oneText = map[string]any{}
			}
			err = w.library(&w.pair, &w.oneText)
			for k := range w.oneText {
				name, good = k, true
			}
			clear(w.oneText)
			return name, good, err
		}
		if w.oneGeneral == nil {
			w.oneGeneral = map[any]any{}
		}
		err = w.library(&w.pair, &w.oneGeneral)
		for k := range w.oneGeneral {
			name, good = k, true
		}
		clear(w.oneGeneral)
		return name, good, err
	}
	// A key that holds a large mapping, or so much that the library could
	// refuse the mapping made for it for its aliases, is a sequence or a
	// mapping, which is no key.
	if m.text == nil {
		v, err := w.value(key)
		if err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("yaml: invalid map key: %#v", v)
	}
	// Among text keys the library lists it as a problem without reading
	// what is in it, which a node of its kind, tag and line that holds
	// nothing is listed as too.
	if err := w.step(); err != nil {
		return nil, false, err
	}
	if key.Kind == yaml.AliasNode {
		if w.expanding[key] {
			return nil, false, yamlHoldsItself(key)
		}
		w.depth++
		err := w.step()
		w.depth--
		if err != nil {
			return nil, false, err
		}
		key = key.Alias
	}
	empty := yaml.Node{Kind: key.Kind, Style: key.Style, Tag: key.Tag, Value: key.Value, Line: key.Line, Column: key.Column}
	var text string
	return nil, false, w.library(&empty, &text)
}

func (w *yamlWalkBefore) merge(merge *yaml.Node, m *yamlMap) error {
	mapping := func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode {
			return n.Alias == nil || n.Alias.Kind == yaml.MappingNode
		}
		return n.Kind == yaml.MappingNode
	}
	switch merge.Kind {
	case yaml.MappingNode, yaml.AliasNode:
		if !mapping(merge) {
			return errYAMLMergesMap
		}
		return w.merged(merge, m)
	case yaml.SequenceNode:
		for _, item := range merge.Content {
			if !mapping(item) {
				return errYAMLMergesMap
			}
			if err := w.merged(item, m); err != nil {
				return err
			}
		}
		return nil
	}
	return errYAMLMergesMap
}

func (w *yamlWalkBefore) merged(n *yaml.Node, m *yamlMap) error {
	if err := w.step(); err != nil {
		return err
	}
	if n.Kind == yaml.AliasNode {
		if w.expanding[n] {
			return yamlHoldsItself(n)
		}
		alias := n
		w.expanding[alias] = true
		w.depth++
		defer func() {
			w.depth--
			delete(w.expanding, alias)
		}()
		n = n.Alias
		if err := w.step(); err != nil {
			return err
		}
	}
	var merge *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if yamlMerges(key) {
			merge = value
			continue
		}
		name, good, err := w.key(key, m, n)
		if err != nil {
			return err
		}
		if !good || m.merged(name) {
			continue
		}
		v, err := w.value(value)
		if err != nil {
			return err
		}
		m.set(name, v)
	}
	if merge != nil {
		return w.merge(merge, m)
	}
	return nil
}
