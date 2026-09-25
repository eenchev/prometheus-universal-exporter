package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment expansion is opt-in, through --config.expand-env for the
// configuration and its collector files and --static-targets.expand-env for
// the static target file, because a configuration file is full of characters that look like references and are
// not: a regex metric rule, a jq expression and a Python pre-script can all
// contain a dollar sign, and expanding by default would rewrite them behind the
// operator's back. Off by default, nothing in a configuration file means
// anything but itself.
//
// Only the braced form is a reference. `$VAR` is left exactly as written, which
// is what keeps `expression: '\$([0-9]+)'` and shell-style text in a pre-script
// working with expansion switched on. `$$` escapes a literal dollar, so
// `$${NOT_A_REFERENCE}` survives as `${NOT_A_REFERENCE}`.
var envReference = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvironment substitutes ${NAME} references in the values of a
// configuration document.
//
// A reference is expanded where the document holds it as a value, and the
// value is written back so that YAML reads exactly it: the document is parsed
// first, each scalar — a value, or a key — that holds a reference is expanded,
// and its text in the document is replaced by the expansion, written plain
// when YAML reads it back unchanged and as a double-quoted string otherwise.
// Substituting the text of the file as it stood, as this once did, let a
// value change the document around it: a token holding " #" lost the rest to
// a comment, one starting with "*" or "&" became an alias or an anchor, and a
// reference in a comment had to be set. Now a value is only ever a value, a
// line break in one included, and a reference in a comment is left alone.
// The rewritten scalar keeps its lines, so the document's line numbers, which
// its errors name, are those of the file.
//
// A reference to a variable that is not set is an error rather than an empty
// string. An empty substitution produces a document that parses and is wrong —
// a collector with no target, or credentials that silently become blank — and
// the exporter would serve it. Every missing name is reported at once, because
// finding them one restart at a time is miserable.
//
// flag is the flag that asked for expansion, which the error names.
func expandEnvironment(path string, raw []byte, flag string) ([]byte, error) {
	text := string(raw)
	doc, ok := parseDocument(text)
	// A bare reference in a flow collection, as in [${A}, b] or {k: ${A}},
	// is not YAML until it is expanded: { and } end an unquoted value there.
	// Such references are read as if quoted, and written back as the bare
	// value they stand for (encodeScalar).
	var wrapped map[int]bool
	if !ok && envReference.MatchString(text) {
		text, wrapped = quoteBareReferences(text)
		doc, ok = parseDocument(text)
	}
	if !ok {
		// Not YAML at all: the decoder that reads the file says so, in the
		// file's own terms, so the file is handed to it as it is.
		return raw, nil
	}
	scalars := documentReferences(doc)
	if len(scalars) == 0 {
		return raw, nil
	}

	var missing []string
	seen := map[string]bool{}
	expand := func(text string) string {
		return envReference.ReplaceAllStringFunc(text, func(match string) string {
			if match == "$$" {
				return "$"
			}
			name := match[2 : len(match)-1]
			value, ok := os.LookupEnv(name)
			if !ok {
				if !seen[name] {
					seen[name] = true
					missing = append(missing, name)
				}
				return match
			}
			return value
		})
	}
	values := make([]string, len(scalars))
	for i, ref := range scalars {
		values[i] = expand(ref.node.Value)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%s: %s", path, missingMessage(missing, flag))
	}

	lineStarts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	var edits []textEdit
	for i, ref := range scalars {
		edit, err := scalarEdit(text, lineStarts, ref, values[i], expand, wrapped)
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, ref.node.Line, err)
		}
		edits = append(edits, edit)
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, edit := range edits {
		text = text[:edit.start] + edit.text + text[edit.end:]
	}
	return []byte(text), nil
}

// parseDocument parses text, reporting whether it is YAML.
func parseDocument(text string) (*yaml.Node, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, false
	}
	return &doc, true
}

// bareReference is a reference standing alone as a value or a key, between
// what can come before a flow value — [ { , : or the start of the line — and
// what can come after it.
var bareReference = regexp.MustCompile(`([\[{,:]|^)([ \t]*)(\$\{[A-Za-z_][A-Za-z0-9_]*\})([ \t]*)([,\]}:]|$)`)

// quoteBareReferences quotes each reference standing alone as a flow value,
// line by line, so the document parses. It returns the text and the offsets,
// in it, of the quotes it added. The lines stay as they were.
func quoteBareReferences(text string) (string, map[int]bool) {
	wrapped := map[int]bool{}
	var out strings.Builder
	for i, line := range strings.SplitAfter(text, "\n") {
		if i > 0 && line == "" {
			break
		}
		body := strings.TrimRight(line, "\r\n")
		rest := line[len(body):]
		for {
			match := bareReference.FindStringSubmatchIndex(body)
			if match == nil || strings.ContainsAny(body[:match[6]], `"'#`) {
				break
			}
			wrapped[out.Len()+match[6]] = true
			quoted := body[:match[6]] + `"` + body[match[6]:match[7]] + `"`
			out.WriteString(quoted)
			body = body[match[7]:]
		}
		out.WriteString(body)
		out.WriteString(rest)
	}
	return out.String(), wrapped
}

// scalarRef is a scalar that holds a reference, and where it stands.
type scalarRef struct {
	node *yaml.Node
	scalarContext
}

// scalarContext is where a scalar stands: as a mapping key, and within a flow
// collection, [...] or {...}. Both limit how a value may be written back.
type scalarContext struct{ key, flow bool }

// collectReferences gathers the scalars of n, keys included, that hold a
// reference or a $$. An alias is the node it names, gathered where that is.
func collectReferences(n *yaml.Node, where scalarContext, out *[]scalarRef) {
	switch n.Kind {
	case yaml.ScalarNode:
		if envReference.MatchString(n.Value) {
			*out = append(*out, scalarRef{n, where})
		}
	case yaml.DocumentNode, yaml.SequenceNode, yaml.MappingNode:
		flow := where.flow || n.Style&yaml.FlowStyle != 0
		for i, child := range n.Content {
			collectReferences(child, scalarContext{key: n.Kind == yaml.MappingNode && i%2 == 0, flow: flow}, out)
		}
	}
}

// documentReferences gathers the references the document uses: those of
// every scalar, keys included, except within a top-level x- entry, which the
// exporter ignores (yamlerrors.go), unless an alias elsewhere uses what an
// anchor there defines. A reference in an x- block nothing uses is left as it
// is, so a variable only such a block names need not be set.
func documentReferences(doc *yaml.Node) []scalarRef {
	var all []scalarRef
	collectReferences(doc, scalarContext{}, &all)
	where := make(map[*yaml.Node]scalarContext, len(all))
	for _, ref := range all {
		where[ref.node] = ref.scalarContext
	}
	var root *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		root = doc.Content[0]
	}
	var out []scalarRef
	seen := map[*yaml.Node]bool{}
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		switch n.Kind {
		case yaml.ScalarNode:
			if ctx, ok := where[n]; ok {
				out = append(out, scalarRef{n, ctx})
			}
		case yaml.AliasNode:
			visit(n.Alias)
		default:
			for i := 0; i < len(n.Content); i++ {
				if n == root && i%2 == 0 && isExtensionKey(n.Content[i].Value) {
					i++
					continue
				}
				visit(n.Content[i])
			}
		}
	}
	visit(doc)
	return out
}

// textEdit replaces text[start:end].
type textEdit struct {
	start, end int
	text       string
}

// scalarEdit is the edit that writes value in place of scalar n's text, on as
// many lines as it took. expand expands the text of a block scalar's lines.
func scalarEdit(text string, lineStarts []int, ref scalarRef, value string, expand func(string) string, wrapped map[int]bool) (textEdit, error) {
	n := ref.node
	if n.Line < 1 || n.Line > len(lineStarts) {
		return textEdit{}, errors.New("an environment reference could not be placed")
	}
	lineStart := lineStarts[n.Line-1]
	lineEnd := len(text)
	if n.Line < len(lineStarts) {
		lineEnd = lineStarts[n.Line] - 1
	}
	line := text[lineStart:lineEnd]
	start := lineStart + byteOffset(line, n.Column-1)
	// An anchored value is placed at its anchor: the value follows it.
	if n.Anchor != "" && start < len(text) && text[start] == '&' {
		start += 1 + len(n.Anchor)
		for start < len(text) && (text[start] == ' ' || text[start] == '\t') {
			start++
		}
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return textEdit{}, errors.New("an environment reference cannot be in a value with an explicit tag; remove the tag")
	}
	switch {
	case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return blockEdit(text, lineStarts, n, expand)
	case n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0:
		end, ok := quotedEnd(text, start)
		if !ok {
			return textEdit{}, errors.New("an environment reference is in a quoted value whose end could not be found")
		}
		// A reference quoted only to be read (quoteBareReferences) was bare
		// in the file, and is written back as a bare value would be.
		return textEdit{start, end, encodeScalar(value, wrapped[start], ref.scalarContext) + strings.Repeat("\n", strings.Count(text[start:end], "\n"))}, nil
	default:
		// A plain scalar on one line is its own text; one folded over
		// several lines is not, and cannot be rewritten in place.
		if !strings.HasPrefix(text[start:], n.Value) || strings.Contains(text[start:start+len(n.Value)], "\n") {
			return textEdit{}, errors.New("an environment reference is in an unquoted value that spans lines; write it on one line or quote it")
		}
		return textEdit{start, start + len(n.Value), encodeScalar(value, true, ref.scalarContext)}, nil
	}
}

// blockEdit expands the references in the lines of a literal or folded block
// scalar, whose text is its value line by line. A value with a line break
// would need the block's indentation on each new line, and more lines, so it
// is refused there; a quoted value takes one.
func blockEdit(text string, lineStarts []int, n *yaml.Node, expand func(string) string) (textEdit, error) {
	first := n.Line // the block's first content line, 0-based
	if first >= len(lineStarts) {
		return textEdit{}, errors.New("an environment reference could not be placed")
	}
	lineAt := func(i int) string {
		end := len(text)
		if i+1 < len(lineStarts) {
			end = lineStarts[i+1] - 1
		}
		return text[lineStarts[i]:end]
	}
	indent := -1
	last := first - 1
	for i := first; i < len(lineStarts); i++ {
		line := strings.TrimRight(lineAt(i), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		width := len(line) - len(strings.TrimLeft(line, " "))
		if indent < 0 {
			indent = width
		}
		if width < indent || indent == 0 {
			break
		}
		last = i
	}
	if last < first {
		return textEdit{}, errors.New("an environment reference could not be placed")
	}
	start := lineStarts[first]
	end := len(text)
	if last+1 < len(lineStarts) {
		end = lineStarts[last+1] - 1
	}
	var lineBreak error
	expanded := envReference.ReplaceAllStringFunc(text[start:end], func(match string) string {
		value := expand(match)
		if strings.ContainsAny(value, "\n\r") {
			lineBreak = errors.New("an environment variable with a line break cannot go into a block value (| or >); quote the reference instead, as in \"${NAME}\"")
		}
		return value
	})
	if lineBreak != nil {
		return textEdit{}, lineBreak
	}
	return textEdit{start, end, expanded}, nil
}

// quotedEnd is the offset just past the closing quote of the quoted scalar
// starting at start.
func quotedEnd(text string, start int) (int, bool) {
	if start >= len(text) {
		return 0, false
	}
	quote := text[start]
	for i := start + 1; i < len(text); i++ {
		switch {
		case quote == '"' && text[i] == '\\':
			i++
		case text[i] == quote && quote == '\'' && i+1 < len(text) && text[i+1] == '\'':
			i++
		case text[i] == quote:
			return i + 1, true
		}
	}
	return 0, false
}

// plainSafe is a value YAML reads back as itself when written unquoted: no
// space, no character that starts or ends anything, not empty. What it reads
// as — a number, a boolean, a string — is what the value would have been
// written there by hand, as the reference was.
var plainSafe = regexp.MustCompile(`^[A-Za-z0-9_./+-][A-Za-z0-9_./+=:@?&%~-]*$`)

// encodeScalar writes value as YAML reads it back. A value in place of an
// unquoted one is written unquoted when that is safe, so a number or a
// boolean stays one; an empty one is written as nothing, as the file would
// hold it by hand, where nothing is a value — a block mapping's; everything
// else is a double-quoted string, JSON's escapes being YAML's. A key is
// always quoted: a key is a name, and a plain one at the start of a line may
// be taken for something else, as --- is.
func encodeScalar(value string, plain bool, where scalarContext) string {
	if plain && !where.key {
		switch {
		case value == "":
			if !where.flow {
				return value
			}
		case value == "-":
			// A lone dash starts a sequence entry.
		case plainSafe.MatchString(value) && !strings.HasSuffix(value, ":"):
			return value
		}
	}
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(b.String(), "\n")
}

// byteOffset is the byte offset of the column'th character of line.
func byteOffset(line string, column int) int {
	for i := range line {
		if column == 0 {
			return i
		}
		column--
	}
	return len(line)
}

func missingMessage(names []string, flag string) string {
	return fmt.Sprintf("%s %s not set; %s requires every ${NAME} it finds to be defined",
		plural(len(names), "environment variable", "environment variables"), quoteAll(names), flag)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func quoteAll(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return strings.Join(quoted, ", ")
}
