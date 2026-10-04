package decode

import (
	"errors"
	"fmt"
	"math/bits"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// The YAML library (gopkg.in/yaml.v3 v3.0.1, decode.go, mapping) refuses a
// key written twice by comparing every two keys of a mapping, and nothing
// switches that off: a mapping of n keys costs n(n-1)/2 comparisons, 5 s for
// the 40,000 keys of a 600 kB body, and where keys are written twice it
// makes a text for each two of them, 137 MB for a key written 1200 times in
// 6 kB. limits.max_response_bytes bounds the body, not what the library
// makes of it, so a target's answer could stall the exporter or exhaust its
// memory.
//
// A document is therefore looked through once before it is decoded
// (yamlLearnt), and only one the library would handle badly is decoded
// otherwise than by the library as a whole:
//
//   - A document with no key written twice and no mapping of more than
//     yamlLargeMapping keys is decoded by the library, as it always was, and
//     so is one with keys written twice but no such mapping, whose problems
//     the library lists in no more than yamlProblemTextDecoded bytes.
//   - A document with keys written twice otherwise is refused here
//     (yamlRefusal) with the error the library would have made of them, its
//     first problems worded and the rest counted, without making the list.
//   - A document with no key written twice and a large mapping is decoded
//     by yamlWalk, which decodes the large mappings a part at a time and
//     hands everything else to the library.
//
// Either way a document costs time and memory linear in its size, and
// looking through it a stack no deeper than it is nested, which the
// library's parser bounds at 10,000: learn, yamlRefusal.read and
// yamlWalk.learn call themselves for what a node holds and follow no alias,
// and what the problems come to is added up without a call for each node
// (listed). yamlWalk calls itself as the library's own decoding does, for
// each node in another and for what an alias stands for, each such node
// counted against what the library allows the aliases of a document.
//
// The library parses a document within that bound on its nesting, but it
// decodes through the targets of its aliases, so an alias chain is a stack as
// deep as the chain is long, with no bound on it (deepest, yamlDepthLimit). A
// document whose depth through its aliases is over the bound is therefore
// refused before anything decodes it, which refuses a chain of more than
// 10,000 links that the library would otherwise decode; the depth is added up
// like the problems, without a call for each node.

// yamlLargeMapping is how many keys a mapping may have for the library to
// decode it whole, and how many keys of a larger one it is handed at a
// time: of 128 keys the library compares each with 64 others on average,
// which is a small part of what decoding the key and its value costs.
// Measured on a mapping of 200,000 keys, parts of 8 to 128 keys decode in
// the same time, about 1 µs a key; parts of 256 take 1.3 times as long, of
// 512 1.7 times and of 1024 2.5 times. And few documents have a mapping of
// more than 128 keys, so few are decoded otherwise than they were.
const yamlLargeMapping = 128

// yamlProblemTextDecoded is how much text the problems of a document may
// come to for the library to be left to list them, counted as
// yamlProblemText bytes and the key for each: about a thousand problems of
// short keys, which is a key of one or two bytes written 45 times, and one
// of three bytes written 44 times. Past it the list is not made. The text
// is bounded rather than the number of problems because a problem holds its
// key: a thousand problems of a key of 200 kB would be 200 MB.
const (
	yamlProblemTextDecoded = 64 << 10
	yamlProblemText        = 64
)

// yamlKeysCompared is how many keys a mapping may have for its keys to be
// compared two by two, as the library compares them, when it is looked
// through for one written twice: that allocates nothing, and costs less
// than a table of the keys up to here. Measured, 24 keys are compared in
// 0.7 µs and put in a table in 1.0, 32 in 1.5 and 1.2. Most mappings of a
// document are that small.
const yamlKeysCompared = 24

// yamlKeysKept is how many keys the table kept from one mapping to the next
// has room for; a larger mapping is given a table of its own, since a table
// costs its size to empty.
const yamlKeysKept = 1024

// yamlCountless is where a count that could grow with the square of the
// document, or with what its aliases expand to, stops growing.
const yamlCountless = 1 << 62

func yamlPlus(a, b uint64) uint64 {
	if sum := a + b; sum >= a && sum < yamlCountless {
		return sum
	}
	return yamlCountless
}

func yamlTimes(a, b uint64) uint64 {
	if over, product := bits.Mul64(a, b); over == 0 && product < yamlCountless {
		return product
	}
	return yamlCountless
}

// yamlKey is what the library tells two keys of a mapping apart by: the
// kind of node and its text, which is empty for a key that is a sequence or
// a mapping and the anchor's name for an alias.
type yamlKey struct {
	kind  yaml.Kind
	value string
}

// yamlTwice is what a mapping with keys written twice comes to: how many
// problems the library lists for it, one for each two of a key written k
// times, and how much text they are counted as.
type yamlTwice struct {
	problems, text uint64
}

// yamlLearnt is what looking through a document once tells of it: whether
// any mapping is large, how many problems the keys written twice come to,
// and which mappings have them.
//
// The problems are counted as the library lists them where nothing else
// stops it: a mapping with a key written twice is not read further, and
// neither is the value of a key that is such a mapping.
type yamlLearnt struct {
	large    int
	hasLarge bool
	// aliases is whether the document has an alias anywhere.
	aliases bool
	// problems is how many problems the library lists for the document read
	// in its order, an alias not followed.
	problems uint64
	twice    map[*yaml.Node]yamlTwice
	// seen is the table of a mapping's keys, kept for the next mapping.
	seen map[yamlKey]uint32
	// lists is how much text the problems under an anchored node come to.
	lists map[*yaml.Node]uint64
	// depths is how deep the library recurses through an anchored node
	// (deepest).
	depths map[*yaml.Node]uint64
	// compared counts the comparisons of two keys, for the tests.
	compared int
}

// keys is an empty table for the keys of a mapping of so many pairs. It is
// left to grow with the different keys it is given, which may be one.
func (l *yamlLearnt) keys(pairs int) map[yamlKey]uint32 {
	if pairs > yamlKeysKept {
		return map[yamlKey]uint32{}
	}
	if l.seen == nil {
		l.seen = map[yamlKey]uint32{}
	}
	clear(l.seen)
	return l.seen
}

// writtenTwice counts the problems the library lists for mapping n itself.
func (l *yamlLearnt) writtenTwice(n *yaml.Node) yamlTwice {
	var twice yamlTwice
	keys := n.Content
	if len(keys) <= 2*yamlKeysCompared {
		for i := 0; i < len(keys); i += 2 {
			kind, value := keys[i].Kind, keys[i].Value
			for j := i + 2; j < len(keys); j += 2 {
				if keys[j].Kind == kind && keys[j].Value == value {
					twice.problems++
					twice.text += yamlProblemText + uint64(len(value))
				}
			}
		}
		pairs := len(keys) / 2
		l.compared += pairs * (pairs - 1) / 2
		return twice
	}
	seen := l.keys(len(keys) / 2)
	for i := 0; i < len(keys); i += 2 {
		key := yamlKey{keys[i].Kind, keys[i].Value}
		// A key written before as many times is a problem with each of them.
		before := seen[key]
		seen[key] = before + 1
		twice.problems = yamlPlus(twice.problems, uint64(before))
		twice.text = yamlPlus(twice.text, yamlTimes(uint64(before), yamlProblemText+uint64(len(key.value))))
	}
	return twice
}

// learn looks through n and everything under it. counted is whether the
// library, reading the document in its order, reads n at all. A scalar and
// an alias have nothing under them, and are not looked at but as keys.
func (l *yamlLearnt) learn(n *yaml.Node, counted bool) {
	if n.Kind != yaml.MappingNode {
		for _, child := range n.Content {
			switch child.Kind {
			case yaml.ScalarNode:
			case yaml.AliasNode:
				l.aliases = true
			default:
				l.learn(child, counted)
			}
		}
		return
	}
	if len(n.Content)/2 > l.large {
		l.hasLarge = true
	}
	if twice := l.writtenTwice(n); twice.problems > 0 {
		if l.twice == nil {
			l.twice = map[*yaml.Node]yamlTwice{}
		}
		l.twice[n] = twice
		if counted {
			l.problems = yamlPlus(l.problems, twice.problems)
		}
		counted = false
	}
	for i, child := range n.Content {
		switch child.Kind {
		case yaml.ScalarNode:
		case yaml.AliasNode:
			l.aliases = true
		default:
			l.learn(child, counted && (i%2 == 0 || !l.isWrittenTwice(n.Content[i-1])))
		}
	}
}

// isWrittenTwice reports whether n is a mapping with a key written twice.
func (l *yamlLearnt) isWrittenTwice(n *yaml.Node) bool {
	if n.Kind != yaml.MappingNode {
		return false
	}
	_, twice := l.twice[n]
	return twice
}

// listed is how much text the problems come to that the library lists when
// it decodes root: those of every mapping under it, and those of a mapping
// an alias stands for once more at each alias. It is never less than what
// the library makes, which reads no further than the first anchor that
// holds itself.
//
// A node is added up with what it holds and with what its aliases stand
// for, which is as deep as the aliases of the document follow one another
// and not as its nodes are nested: of anchors written in a mapping with a
// key written twice, which is read no further, each may be met first
// through an alias in the one written after it. So the nodes being added
// up are kept in a list, and no call waits for another: a call for each
// would be a stack as deep as there are anchors.
func (l *yamlLearnt) listed(root *yaml.Node) uint64 {
	// The document is added up as the one node a list of itself holds.
	whole := yaml.Node{Content: []*yaml.Node{root}}
	var few [16]yamlListing
	open := append(few[:0], yamlListing{of: &whole})
	for {
		at := &open[len(open)-1]
		if at.next == len(at.of.Content) {
			// Added up: counted for what holds it, or for the alias that
			// stands for it there.
			text := at.text
			if at.of.Anchor != "" {
				l.lists[at.of] = text
			}
			open = open[:len(open)-1]
			if len(open) == 0 {
				return text
			}
			open[len(open)-1].text = yamlPlus(open[len(open)-1].text, text)
			continue
		}
		n := at.of.Content[at.next]
		at.next++
		for n.Kind == yaml.AliasNode && n.Alias != nil {
			n = n.Alias
		}
		switch n.Kind {
		case yaml.ScalarNode, yaml.AliasNode:
			continue
		case yaml.MappingNode:
			if twice, ok := l.twice[n]; ok {
				at.text = yamlPlus(at.text, twice.text)
				continue
			}
		}
		if n.Anchor != "" {
			// Counted once, however many aliases stand for it; and an alias under
			// it that stands for it adds nothing.
			if text, known := l.lists[n]; known {
				at.text = yamlPlus(at.text, text)
				continue
			}
			if l.lists == nil {
				l.lists = map[*yaml.Node]uint64{}
			}
			l.lists[n] = 0
		}
		open = append(open, yamlListing{of: n})
	}
}

// yamlListing is a node being added up by listed: how many of the nodes it
// holds are added, and what they come to.
type yamlListing struct {
	of   *yaml.Node
	next int
	text uint64
}

// yamlDepthLimit is how deep the library's decoding may recurse through a
// document and its aliases. It is the parser's own limit on how deeply a
// document may be nested (scannerc.go, max_indents and max_flow_level, each
// 10,000): a document nested deeper than that cannot be written, and a
// mapping or a sequence of that depth is decoded by a stack of under 10 MB
// (measured at about 720 bytes a level in the library and about 440 in the
// walker, for sequences, mappings, merges and alias links alike). The
// library parses within the limit, but it decodes through the targets of
// aliases, so a chain of anchors that each hold an alias of the one before —
// `a0: &a0 [x]`, `a1: &a1 [*a0]`, ... — expands to a depth that is the length
// of the chain, with no limit on it: 100,000 such links are a 64 MB stack and
// 320,000, which fit in a 10 MiB body, a 256 MB one; past about 100 MiB of
// body the stack would pass Go's own 1 GB limit, which is fatal and caught by
// no recover. So a document whose depth through its aliases is over the limit
// is refused before anything decodes it.
const yamlDepthLimit = 10000

// yamlDepthFrame is a node whose depth deepest is finding: how many of the
// nodes it holds have been looked at, and the deepest of those.
type yamlDepthFrame struct {
	of    *yaml.Node
	next  int
	child uint64
}

// yamlDepthChildren is how many nodes n holds for deepest: those of a
// sequence or a mapping, and the one an alias stands for.
func yamlDepthChildren(n *yaml.Node) int {
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return 0
		}
		return 1
	}
	return len(n.Content)
}

// yamlDepthChild is the ith node n holds, for deepest.
func yamlDepthChild(n *yaml.Node, i int) *yaml.Node {
	if n.Kind == yaml.AliasNode {
		return n.Alias
	}
	return n.Content[i]
}

// deepest is how deep the library's decoding recurses through the document
// under root and the targets of its aliases: the depth of a node is 1 plus
// the deepest of the nodes it holds, a scalar being 1, and an alias counting
// as its target with 1 for the frame of the alias itself, as the library
// decodes one (decode.go, unmarshal, alias). A merge recurses through the
// value of the merge key, which is a node the mapping holds, so it is counted
// with the rest.
//
// It is added up without a call for each node, in a stack no deeper than the
// document is nested: the nodes being added up are kept in a list, as listed
// keeps them, so that an alias chain costs a long list on the heap and not a
// deep stack of the goroutine, which no recover catches when it is too deep.
// The depth of a node with an anchor is memoised, so an alias of it is not
// followed again and the whole is linear in the document; a node met again
// while its depth is still being found is part of a cycle — an anchor that
// holds itself — and counts as 0 there, so the adding up terminates and the
// library is left to refuse the document (yamlHoldsItself).
func (l *yamlLearnt) deepest(root *yaml.Node) uint64 {
	// The document is the one node a list of itself holds; its depth is that
	// node's, without a frame of its own.
	whole := yaml.Node{Content: []*yaml.Node{root}}
	var few [16]yamlDepthFrame
	open := append(few[:0], yamlDepthFrame{of: &whole})
	for {
		at := &open[len(open)-1]
		if at.next == yamlDepthChildren(at.of) {
			depth := at.child
			node := at.of
			if node != &whole {
				depth++
				if node.Anchor != "" {
					l.depths[node] = depth
				}
			}
			open = open[:len(open)-1]
			if len(open) == 0 {
				return depth
			}
			if depth > open[len(open)-1].child {
				open[len(open)-1].child = depth
			}
			continue
		}
		n := yamlDepthChild(at.of, at.next)
		at.next++
		if n == nil {
			continue
		}
		if n.Anchor != "" {
			// Counted once, however many aliases stand for it; a node met while
			// its depth is still open is in a cycle and counts as 0.
			if depth, known := l.depths[n]; known {
				if depth > at.child {
					at.child = depth
				}
				continue
			}
			if l.depths == nil {
				l.depths = map[*yaml.Node]uint64{}
			}
			l.depths[n] = 0
		}
		open = append(open, yamlDepthFrame{of: n})
	}
}

// yamlTooDeep is the error for a document nested, through its aliases, deeper
// than the parser lets one be written.
func yamlTooDeep() error {
	return fmt.Errorf("yaml: the document is nested more than %d deep through its aliases, deeper than a YAML document may be written", yamlDepthLimit)
}

// The ways a document is decoded.
const (
	yamlByLibrary = iota
	yamlByParts
	yamlRefused
)

// way is how the document under root, looked through, is to be decoded.
func (l *yamlLearnt) way(root *yaml.Node) int {
	switch {
	case l.twice == nil && !l.hasLarge:
		return yamlByLibrary
	case l.twice == nil:
		return yamlByParts
	case !l.hasLarge && l.listed(root) <= yamlProblemTextDecoded:
		return yamlByLibrary
	}
	return yamlRefused
}

// yamlReading decodes a parsed document. large is yamlLargeMapping, and
// less in tests, which are told of each node handed to the library.
type yamlReading struct {
	large int
	hands func(n *yaml.Node)
}

// value is the document under root as the YAML library decodes it into
// plain values, or the error it refuses it with.
func (r yamlReading) value(root *yaml.Node) (any, error) {
	learnt := yamlLearnt{large: r.large}
	learnt.learn(root, true)
	// A document whose aliases expand to a depth over the limit is refused
	// before anything decodes it, on every way, the library's own included:
	// that is where the deepest stack is (deepest, yamlDepthLimit). A document
	// with no alias is nested no deeper than the parser let it be written, so
	// only one with aliases is looked through for its depth.
	if learnt.aliases && learnt.deepest(root) > yamlDepthLimit {
		return nil, yamlTooDeep()
	}
	switch learnt.way(root) {
	case yamlByParts:
		walk := yamlWalk{large: r.large, hands: r.hands, aliases: learnt.aliases, known: map[*yaml.Node]yamlKnown{}, expanding: map[*yaml.Node]bool{}}
		walk.null = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
		walk.learn(root)
		v, err := walk.value(root)
		if err == nil && len(walk.problems) > 0 {
			err = &yaml.TypeError{Errors: walk.problems}
		}
		if err != nil {
			return nil, yamlFailure(err)
		}
		return v, nil
	case yamlRefused:
		return nil, learnt.refusal(root)
	}
	var v any
	if err := root.Decode(&v); err != nil {
		return nil, yamlFailure(err)
	}
	return v, nil
}

// refusal is the error for the document under root, which has keys written
// twice and is not given to the library.
func (l *yamlLearnt) refusal(root *yaml.Node) error {
	var refusal yamlRefusal
	refusal.read(l, root)
	return refusal.failure(l.problems)
}

// yamlWrittenTwice is the library's problem with a key written twice
// (decode.go, mapping): the later key's line, its text, the earlier key's
// line.
const yamlWrittenTwice = "line %d: mapping key %#v already defined at line %d"

// yamlRefusal makes the error for a document with keys written twice that
// the library is not given: what yamlProblems makes of the library's list,
// without the list. The library lists the problems of a mapping by the
// earlier key and then by the later, and the mappings in the order it reads
// them; the first yamlProblemsShown are worded and the rest are counted,
// which is arithmetic, and the failure is recognised by one problem for
// each different key, which is all that tells two such problems apart once
// their lines are left out.
//
// The document is read in the order it is written, as the library reads
// one that has nothing but keys written twice wrong with it. Where the
// library leaves that order the error can differ from the one it made,
// which is accepted, since the document is refused either way but in the
// last case:
//
//   - Where the library met an error that ends the decoding, in any part of
//     the document — a value that does not fit its tag, a key that is a
//     sequence, an anchor that holds itself, a merge of something that is no
//     mapping — it reported that one and no key written twice; and a problem
//     of another kind, which only a sequence or a mapping merged as a key
//     into a mapping of text keys is, was listed among them.
//   - It lists the problems of a mapping again at each alias of it, and
//     those of a mapping that is merged after those of the other keys.
//   - It does not read a value a merge leaves out, that of a key the
//     merging mapping has itself, so it lists no key written twice there,
//     and decodes a document that has them nowhere else. Such a document is
//     refused here, when it has a large mapping.
type yamlRefusal struct {
	// first is the first yamlProblemsShown problems, as the library words
	// them.
	first []string
	// kinds is the keys written twice, in the order of their first problem,
	// to one past yamlProblemsShown, and same what each is recognised by.
	kinds []string
	same  []string
}

// done reports whether nothing more of the document is needed.
func (r *yamlRefusal) done() bool {
	return len(r.first) == yamlProblemsShown && len(r.kinds) > yamlProblemsShown
}

// read goes through what the library reads of n, as learnt counted it.
func (r *yamlRefusal) read(learnt *yamlLearnt, n *yaml.Node) {
	if r.done() {
		return
	}
	if learnt.isWrittenTwice(n) {
		r.list(n, learnt.keys(len(n.Content)/2))
		return
	}
	for i, child := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 1 && learnt.isWrittenTwice(n.Content[i-1]) {
			continue
		}
		r.read(learnt, child)
	}
}

// list takes the problems of mapping n in the library's order: for each
// key, those with every later key that is the same. left is an empty table
// of keys, for how many times a key is written from here on.
func (r *yamlRefusal) list(n *yaml.Node, left map[yamlKey]uint32) {
	keys := n.Content
	for i := 0; i < len(keys); i += 2 {
		left[yamlKey{keys[i].Kind, keys[i].Value}]++
	}
	for i := 0; i < len(keys) && !r.done(); i += 2 {
		key := yamlKey{keys[i].Kind, keys[i].Value}
		later := left[key] - 1
		left[key] = later
		if later == 0 {
			continue
		}
		r.kind(keys[i])
		// Each key looked for from here is found, so the first problems cost
		// no more than as many passes over the mapping.
		for j := i + 2; j < len(keys) && later > 0 && len(r.first) < yamlProblemsShown; j += 2 {
			if keys[j].Kind == key.kind && keys[j].Value == key.value {
				r.first = append(r.first, fmt.Sprintf(yamlWrittenTwice, keys[j].Line, keys[j].Value, keys[i].Line))
				later--
			}
		}
	}
}

// kind takes a key written twice as one more problem to recognise the
// failure by, unless one of the same text was taken.
func (r *yamlRefusal) kind(key *yaml.Node) {
	if len(r.kinds) > yamlProblemsShown {
		return
	}
	for _, known := range r.kinds {
		if known == key.Value {
			return
		}
	}
	r.kinds = append(r.kinds, key.Value)
	if len(r.kinds) > yamlProblemsShown {
		r.same = append(r.same, "... and "+model.MovingMark+" more problems")
		return
	}
	// The problem as the library words it, read for its lines as any other.
	r.same = append(r.same, withoutYAMLLines(fmt.Sprintf(yamlWrittenTwice, key.Line, key.Value, key.Line)))
}

// failure is the error of the problems read, of which there are so many.
func (r *yamlRefusal) failure(problems uint64) error {
	shown := r.first
	if problems > yamlProblemsShown {
		more := "problems"
		if problems == yamlProblemsShown+1 {
			more = "problem"
		}
		shown = append(shown, fmt.Sprintf("... and %d more %s", problems-yamlProblemsShown, more))
	}
	return model.SameFailureAs(&yaml.TypeError{Errors: shown}, (&yaml.TypeError{Errors: r.same}).Error())
}

// yamlKnown is what yamlWalk knows of a node before it decodes it.
type yamlKnown struct {
	// mine is whether the walk decodes the node itself: it holds a large
	// mapping, or an alias of something it is in, which the walk tells as
	// the library does, or so much aliasing that the library, handed the
	// node alone, could refuse for it a document it decodes as a whole.
	mine bool
	// open is whether the node is still being looked through.
	open bool
	// steps is how many nodes the library decodes for it, each alias's as
	// many times as it is written, and aliased how many of those an alias
	// stands for.
	steps, aliased uint64
}

func (k yamlKnown) and(other yamlKnown) yamlKnown {
	return yamlKnown{mine: k.mine || other.mine, steps: yamlPlus(k.steps, other.steps), aliased: yamlPlus(k.aliased, other.aliased)}
}

// alias is what is known of an alias of what k is known of: every node of
// that is decoded for the alias.
func (k yamlKnown) alias() yamlKnown {
	alias := yamlKnown{mine: k.mine, steps: yamlPlus(k.steps, 1), aliased: k.steps}
	alias.mine = alias.mine || alias.aliases()
	return alias
}

// The library refuses a document whose aliases stand for too much of what
// it decodes (decode.go, unmarshal and allowedAliasRatio): past
// yamlAliasedFree nodes decoded for an alias and yamlDecodedFree nodes
// decoded, the share of the first may not be over 99%, falling to 10% from
// 400,000 nodes decoded to 4,000,000.
const (
	yamlAliasedFree = 100
	yamlDecodedFree = 1000
)

func yamlAliasedAllowed(decoded uint64) float64 {
	const low, high = 400000, 4000000
	switch {
	case decoded <= low:
		return 0.99
	case decoded >= high:
		return 0.10
	}
	return 0.99 - 0.89*(float64(decoded-low)/float64(high-low))
}

// aliases reports whether the library, decoding so much apart from the rest
// of the document, could refuse it for its aliases, which the document as a
// whole might not be refused for.
func (k yamlKnown) aliases() bool {
	return k.steps > yamlDecodedFree && k.aliased > yamlAliasedFree
}

// part is what the library counts of so much handed to it as what a node
// made for the purpose holds, a mapping of some pairs or a sequence of some
// items: it decodes that node too, and one node more can be the one its
// count is over at.
func (k yamlKnown) part() yamlKnown {
	k.steps = yamlPlus(k.steps, 1)
	return k
}

// The library's errors that the walk meets itself (decode.go: alias,
// unmarshal, merge, mapping).
var (
	errYAMLAliasing  = errors.New("yaml: document contains excessive aliasing")
	errYAMLMergesMap = errors.New("yaml: map merge requires map or sequence of maps as the value")
)

func yamlHoldsItself(alias *yaml.Node) error {
	return fmt.Errorf("yaml: anchor '%s' value contains itself", alias.Value)
}

// yamlMerges reports whether a key is a merge key, as the library tells it
// (decode.go, isMerge): `<<` written plainly or tagged !!merge, not "<<".
func yamlMerges(key *yaml.Node) bool {
	return key.Kind == yaml.ScalarNode && key.Value == "<<" && (key.Tag == "" || key.Tag == "!" || key.ShortTag() == "!!merge")
}

// yamlKeysAreText reports whether the library decodes mapping n into a
// map[string]any, which it does when every key is text or a merge key
// (decode.go, isStringMap), and into a map[any]any otherwise.
func yamlKeysAreText(n *yaml.Node) bool {
	for i := 0; i < len(n.Content); i += 2 {
		if tag := n.Content[i].ShortTag(); tag != "!!str" && tag != "!!merge" {
			return false
		}
	}
	return true
}

// yamlMap is the map a mapping is decoded into, of the one type or the
// other.
type yamlMap struct {
	text    map[string]any
	general map[any]any
}

func (m *yamlMap) value() any {
	if m.text != nil {
		return m.text
	}
	return m.general
}

// into is what the library is given to fill the map.
func (m *yamlMap) into() any {
	if m.text != nil {
		return &m.text
	}
	return &m.general
}

func (m *yamlMap) set(key, value any) {
	if m.text != nil {
		m.text[key.(string)] = value //nolint:forcetypeassert // the library decoded the key into this map's key type
		return
	}
	m.general[key] = value
}

// merged reports whether a merge leaves key out: the mapping has it, of
// itself or of an earlier merge, or it is the text of the merge key, which
// the library counts among the mapping's own keys.
func (m *yamlMap) merged(key any) bool {
	if key == "<<" {
		return true
	}
	if m.text != nil {
		_, has := m.text[key.(string)] //nolint:forcetypeassert // as in set
		return has
	}
	_, has := m.general[key]
	return has
}

// yamlWalk decodes a document that has a large mapping and no key written
// twice anywhere, in time linear in it, into what the library decodes it
// into. It decodes only the nodes that hold a large mapping, and the few
// the library cannot be handed apart from the rest of the document
// (yamlKnown.mine), and hands every other to the library, so every scalar,
// every tag, every small collection and every error of those is the
// library's own:
//
//   - a sequence is the list of its items, those the walk does not decode
//     handed to the library yamlLargeMapping at a time, as a sequence of
//     them (sequence), and an alias is what it stands for, refused as the
//     library refuses it when that holds the alias itself or when the
//     aliases of the document expand to too much;
//   - a mapping is a map of the type the library makes of its keys. Its
//     pairs are handed to the library yamlLargeMapping at a time, as a
//     mapping of those pairs decoded into the same map, so that the keys,
//     the values and what an empty value is are the library's and its
//     comparing of every two keys is of no more than that many; a pair whose
//     value the walk decodes has its key decoded by the library alone;
//   - a merge key is applied after the other pairs, as the library applies
//     it: the mappings merged in their order, each key only if the mapping
//     does not have it yet, a merged mapping's own merge after its keys.
//
// No mapping has a key written twice here, so the library's comparing would
// have found nothing, and the result is the library's, with two things
// that are not:
//
//   - For a merge the library decodes the mapping's keys once more, each
//     into any value, and holds them in a table. A key that is a sequence
//     or a mapping, of the mapping or of a merged one, is then read, which
//     it was not among text keys, and the library fails with what is wrong
//     in it (`invalid map key`, `cannot decode ... as a !!int`, `anchor ...
//     value contains itself`), or outright (`runtime error: hash of
//     unhashable type`) when it puts it in the table. The walk counts the
//     keys once more and decodes them once: where it decodes that mapping
//     it refuses the document with the error it meets in its own order —
//     the key's, as the library refuses it without a merge (`invalid map
//     key`, or `cannot unmarshal !!seq into string` among text keys), a
//     later value's, or the merge's (`map merge requires map or sequence of
//     maps as the value`). The document is refused either way, with an
//     error that may be another; in a part handed to the library, the
//     library fails as it did.
//   - The library counts the nodes it decodes one at a time, and refuses the
//     document at the node where its aliases stand for too much; the walk
//     counts a part handed to the library at once, by what yamlKnown says
//     of it, which for a mapping with a merge key is no less than the
//     library counts: the library decodes the keys once more of a mapping
//     with a merge that is not itself merged, and yamlKnown says so of
//     every one. A document is refused for its aliases at the same share
//     of them, but one within a part of that share may be refused where
//     the library decoded it, or decoded where the library refused it.
type yamlWalk struct {
	large int
	hands func(n *yaml.Node)
	// aliases is whether the document has an alias anywhere. In one that
	// has none nothing is counted, since nothing is decoded for an alias, and
	// known holds only the nodes the walk decodes: an entry for every
	// sequence and mapping of a document cost more than a third of what
	// decoding one of small mappings does.
	aliases bool
	known   map[*yaml.Node]yamlKnown
	// expanding is the aliases being decoded, as the library keeps them, to
	// tell an anchor that holds itself.
	expanding map[*yaml.Node]bool
	// decoded, aliased and depth are the library's own count of the nodes
	// it decodes, of those decoded for an alias, and of the aliases it is
	// in.
	decoded, aliased uint64
	depth            int
	// problems is the library's list of problems, which do not end the
	// decoding.
	problems []string
	// part is the sequence made of the items handed to the library
	// together.
	part yaml.Node
	// null is the value a key is decoded beside, pair the mapping of the
	// two, and oneText and oneGeneral the maps it is decoded into.
	null       yaml.Node
	pair       yaml.Node
	two        [2]*yaml.Node
	oneText    map[string]any
	oneGeneral map[any]any
}

// learn fills known for n and everything under it. An alias is written
// after what it stands for, so that is known already, unless the alias is
// inside it.
func (w *yamlWalk) learn(n *yaml.Node) yamlKnown {
	switch n.Kind {
	case yaml.ScalarNode:
		return yamlKnown{steps: 1}
	case yaml.AliasNode:
		if n.Alias == nil {
			return yamlKnown{steps: 1}
		}
		if n.Alias.Kind == yaml.ScalarNode {
			return yamlKnown{steps: 2, aliased: 1}
		}
		to, read := w.known[n.Alias]
		if !read || to.open {
			// An anchor that holds itself is told by the walk, which every
			// node on the way to this alias is then decoded by.
			return yamlKnown{mine: true, steps: 1}
		}
		return to.alias()
	}
	if w.aliases {
		w.known[n] = yamlKnown{open: true}
	}
	all := yamlKnown{steps: 1}
	var keys yamlKnown
	merges := false
	for i, child := range n.Content {
		known := w.learn(child)
		all = all.and(known)
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			keys = keys.and(known)
			merges = merges || yamlMerges(child)
		}
	}
	if n.Kind == yaml.MappingNode {
		if len(n.Content)/2 > w.large {
			all.mine = true
		}
		if merges {
			// The library decodes the keys of a mapping with a merge once
			// more.
			all.steps, all.aliased = yamlPlus(all.steps, keys.steps), yamlPlus(all.aliased, keys.aliased)
		}
	}
	all.mine = all.mine || all.aliases()
	if all.mine || w.aliases {
		w.known[n] = all
	}
	return all
}

// of is what is known of n: of a sequence or a mapping the walk does not
// decode, in a document without an alias, nothing, which nothing is asked
// of.
func (w *yamlWalk) of(n *yaml.Node) yamlKnown {
	switch n.Kind {
	case yaml.ScalarNode:
		return yamlKnown{steps: 1}
	case yaml.AliasNode:
		if n.Alias == nil || n.Alias.Kind == yaml.AliasNode {
			return yamlKnown{steps: 1}
		}
		return w.of(n.Alias).alias()
	}
	return w.known[n]
}

// count takes the nodes the library decodes for a part of the document into
// its count, and refuses the document as the library does once its aliases
// stand for too much.
func (w *yamlWalk) count(known yamlKnown) error {
	w.decoded = yamlPlus(w.decoded, known.steps)
	if w.depth > 0 {
		w.aliased = yamlPlus(w.aliased, known.steps)
	} else {
		w.aliased = yamlPlus(w.aliased, known.aliased)
	}
	if w.aliased > yamlAliasedFree && w.decoded > yamlDecodedFree && float64(w.aliased)/float64(w.decoded) > yamlAliasedAllowed(w.decoded) {
		return errYAMLAliasing
	}
	return nil
}

func (w *yamlWalk) step() error {
	return w.count(yamlKnown{steps: 1})
}

// library has the library decode n into what out points at. A list of
// problems is kept and the decoding goes on, as the library's does; any
// other error ends it.
func (w *yamlWalk) library(n *yaml.Node, out any) error {
	if w.hands != nil {
		w.hands(n)
	}
	err := n.Decode(out)
	if problems, ok := err.(*yaml.TypeError); ok { //nolint:errorlint // the library returns its list of problems as it is, wrapped in nothing
		w.problems = append(w.problems, problems.Errors...)
		return nil
	}
	return err
}

// value decodes n.
func (w *yamlWalk) value(n *yaml.Node) (any, error) {
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

// sequence decodes a sequence the walk decodes itself. An item the walk
// decodes is decoded alone; the items between two of those are handed to
// the library together, yamlLargeMapping at a time, as a sequence of them
// (items): a call of the library for each item, a scalar among them, cost
// several times what the item does.
//
// The items are counted one by one as they are taken, as they were when
// each was handed alone, and what the library counts of a part stays under
// what it could refuse the part for (yamlKnown.aliases). An item that alone
// comes to that with the sequence made for it is handed as the node it is.
func (w *yamlWalk) sequence(n *yaml.Node) (any, error) {
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

// items has the library decode the items n.Content[from:to] of sequence n,
// as a sequence of them, and adds them to into. The library makes a list of
// a sequence's items whatever its tag is, and leaves no item out that is
// decoded into any value.
func (w *yamlWalk) items(n *yaml.Node, from, to int, into []any) ([]any, error) {
	if to == from {
		return into, nil
	}
	w.part = yaml.Node{Kind: yaml.SequenceNode, Style: n.Style, Tag: n.Tag, Line: n.Line, Column: n.Column, Content: n.Content[from:to]}
	var decoded []any
	err := w.library(&w.part, &decoded)
	w.part.Content = nil
	return append(into, decoded...), err
}

// mapping decodes a mapping that is not merged into another.
func (w *yamlWalk) mapping(n *yaml.Node) (any, error) {
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

// key decodes the key of a pair of mapping n into the key type of m, which
// the library does: a key it does not decode, as a null among text keys, is
// not good, and its pair is left out.
func (w *yamlWalk) key(key *yaml.Node, m *yamlMap, n *yaml.Node) (name any, good bool, err error) {
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

// merge merges what a merge key's value names into m (decode.go, merge).
func (w *yamlWalk) merge(merge *yaml.Node, m *yamlMap) error {
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

// merged decodes mapping n, or the mapping alias n stands for, into m as a
// mapping merged into it: a key m has is left out, and its value is not
// decoded.
func (w *yamlWalk) merged(n *yaml.Node, m *yamlMap) error {
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
