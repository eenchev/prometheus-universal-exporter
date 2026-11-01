package config

import (
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// Each collector loaded from a file keeps the mapping its file writes it as
// (model.Collector.Written), for the debug page to show the operator's own
// text rather than the collector as loading has normalised it. It is taken
// from the file as it is written: its ${NAME} references as references,
// whatever expansion put in their place, so the environment is not shown
// through it; the anchors, aliases and merge keys it uses resolved, as the
// decoder resolves them, so it stands on its own; and its comments left out,
// since they are not configuration. The keys it takes from its file's
// defaults block are added where the collector would write them, from the
// block as it is written, each with a comment that says so. A folded block
// scalar (>, >-), such as a long statement, keeps the lines the file writes
// it over (model.Collector.WrittenBlocks): its value is one line, and the
// page would show it as one.

// fromDefaultsComment marks a key the collector took from its file's
// defaults block.
const fromDefaultsComment = "# from the file's defaults"

// recordWritten gives the collectors decoded from a file, raw being the file
// as it is written, the mappings it writes them as. A file whose text does
// not give a mapping for each collector, which a file that decoded always
// does, leaves them without one.
func recordWritten(raw []byte, collectors []model.Collector) {
	root, ok := sourceRoot(string(raw))
	if !ok {
		return
	}
	nodes := collectorNodes(root)
	if len(nodes) != len(collectors) {
		return
	}
	lines := strings.Split(string(raw), "\n")
	var defaults *yaml.Node
	for _, entry := range model.DecodedEntries(root) {
		if model.KeyName(entry.Key) == defaultsKey {
			defaults = entry.Value
		}
	}
	for i := range collectors {
		w := writtenCopy{lines: lines}
		written := w.standalone(nodes[i], map[*yaml.Node]bool{})
		if written == nil || written.Kind != yaml.MappingNode {
			continue
		}
		if defaults != nil {
			for _, key := range collectors[i].FromDefaults {
				w.addFromDefaults(written, defaults, strings.Split(key, "."))
			}
		}
		collectors[i].Written, collectors[i].WrittenBlocks = written, w.inOrder(written, nil)
	}
}

// writtenCopy copies a collector's nodes out of its file (standalone), and
// keeps the folded block scalars among them as the file's lines write them.
type writtenCopy struct {
	lines  []string
	blocks map[*yaml.Node]model.WrittenBlock
}

// inOrder appends to out the folded block scalars of n, depth first, as the
// file writes them, an empty one for each whose lines blockOf could not
// tell: what model.Collector.WrittenBlocks holds, nil when n has none.
func (w *writtenCopy) inOrder(n *yaml.Node, out []model.WrittenBlock) []model.WrittenBlock {
	if n.Kind == yaml.ScalarNode && n.Style&yaml.FoldedStyle != 0 {
		return append(out, w.blocks[n])
	}
	for _, child := range n.Content {
		out = w.inOrder(child, out)
	}
	return out
}

// blockOf is the folded block scalar n as the file writes it: its header,
// from where the node starts, and the lines after it that are indented
// further than the line it starts on, without the indentation of the
// first; false for a header with an indentation indicator, whose lines
// cannot be indented anew, or a node the file's lines do not hold.
func (w *writtenCopy) blockOf(n *yaml.Node) (model.WrittenBlock, bool) {
	if n.Line < 1 || n.Line > len(w.lines) || n.Column < 1 || n.Column > len(w.lines[n.Line-1]) {
		return model.WrittenBlock{}, false
	}
	start := w.lines[n.Line-1]
	header := strings.Fields(start[n.Column-1:])
	if len(header) == 0 || !strings.HasPrefix(header[0], ">") || strings.ContainsAny(header[0], "123456789") {
		return model.WrittenBlock{}, false
	}
	indentOf := func(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }
	parent, indent := indentOf(start), -1
	block := model.WrittenBlock{Header: header[0]}
	for _, line := range w.lines[n.Line:] {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			block.Lines = append(block.Lines, "")
			continue
		}
		if indentOf(line) <= parent || indent >= 0 && indentOf(line) < indent {
			break
		}
		if indent < 0 {
			indent = indentOf(line)
		}
		block.Lines = append(block.Lines, line[indent:])
	}
	for len(block.Lines) > 0 && block.Lines[len(block.Lines)-1] == "" {
		block.Lines = block.Lines[:len(block.Lines)-1]
	}
	return block, len(block.Lines) > 0
}

// sourceRoot is the root of a document as it is written, which may hold
// references that make it YAML only once they are expanded, as
// expandEnvironment reads it.
func sourceRoot(text string) (*yaml.Node, bool) {
	doc, ok := parseDocument(text)
	if !ok && envReference.MatchString(text) {
		if masked, unmask, masks := maskReferences(text); masks {
			if doc, ok = parseDocument(masked); ok {
				unmaskScalars(doc, unmask)
			}
		}
	}
	if !ok || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false
	}
	return doc.Content[0], true
}

// standalone is a copy of n with its aliases replaced by what they name and
// its merge keys by the entries the decoder takes from them
// (model.DecodedEntries), without anchors or comments. visiting guards
// against an alias of a node that holds it, which the decoder refuses.
func (w *writtenCopy) standalone(n *yaml.Node, visiting map[*yaml.Node]bool) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || visiting[n] {
		return nil
	}
	visiting[n] = true
	defer delete(visiting, n)
	out := &yaml.Node{Kind: n.Kind, Style: n.Style, Tag: n.Tag, Value: n.Value}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Style&yaml.FoldedStyle != 0 {
			if block, ok := w.blockOf(n); ok {
				if w.blocks == nil {
					w.blocks = map[*yaml.Node]model.WrittenBlock{}
				}
				w.blocks[out] = block
			}
		}
	case yaml.MappingNode:
		for _, entry := range model.DecodedEntries(n) {
			key, value := w.standalone(entry.Key, visiting), w.standalone(entry.Value, visiting)
			if key == nil || value == nil {
				continue
			}
			out.Content = append(out.Content, key, value)
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if copied := w.standalone(item, visiting); copied != nil {
				out.Content = append(out.Content, copied)
			}
		}
	}
	return out
}

// addFromDefaults adds to the collector's mapping the value of the defaults
// block at path, where the collector does not write it, marked as taken
// from the block. A block the collector writes takes the keys of the
// defaults' block that it does not, each added on its own path.
func (w *writtenCopy) addFromDefaults(collector, defaults *yaml.Node, path []string) {
	value := lookUp(defaults, path)
	if value == nil {
		return
	}
	at := collector
	for i, key := range path {
		next := lookUp(at, []string{key})
		if i == len(path)-1 {
			if next == nil {
				keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key, LineComment: fromDefaultsComment}
				at.Content = append(at.Content, keyNode, w.standalone(value, map[*yaml.Node]bool{}))
			}
			return
		}
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			at.Content = append(at.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, next)
		}
		if next.Kind != yaml.MappingNode {
			return
		}
		at = next
	}
}

// lookUp is the value at path in a mapping copied by standalone, or in the
// defaults block as it is written, nil when there is none.
func lookUp(n *yaml.Node, path []string) *yaml.Node {
	for _, key := range path {
		for n != nil && n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		entries := model.DecodedEntries(n)
		i := slices.IndexFunc(entries, func(e model.MappingEntry) bool { return model.KeyName(e.Key) == key })
		if i < 0 {
			return nil
		}
		n = entries[i].Value
	}
	return n
}
