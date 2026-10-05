package decode

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// The YAML decoder's bound on nesting as it was before every response had
// the one bound, copied from the decoder as it then was: the oracle for what
// a document that is not nested near the bound decodes into, and for what
// was refused then. Only a document with an alias was looked through for its
// depth, and the depth was that of the library's calls: 1 for the document,
// for a scalar and for an alias itself, beside 1 for each sequence and
// mapping. The ways a document is decoded and the walk are the decoder's
// own, which were not changed.

// yamlTooDeepAsItWas is the text of yamlTooDeep as it was.
const yamlTooDeepAsItWas = "yaml: the document is nested more than 10000 deep through its aliases, deeper than a YAML document may be written"

// yamlLearnAsItWas is yamlLearnt.learn as it was, which did not carry how
// deep the document is nested.
func yamlLearnAsItWas(l *yamlLearnt, n *yaml.Node, counted bool) {
	if n.Kind != yaml.MappingNode {
		for _, child := range n.Content {
			switch child.Kind {
			case yaml.ScalarNode:
			case yaml.AliasNode:
				l.aliases = true
			default:
				yamlLearnAsItWas(l, child, counted)
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
			yamlLearnAsItWas(l, child, counted && (i%2 == 0 || !l.isWrittenTwice(n.Content[i-1])))
		}
	}
}

// yamlDeepestAsItWas is yamlLearnt.deepest as it was.
func yamlDeepestAsItWas(l *yamlLearnt, root *yaml.Node) uint64 {
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

// yamlValueAsItWas is yamlReading.value as it was.
func yamlValueAsItWas(r yamlReading, root *yaml.Node) (outcome yamlOutcome) {
	defer func() {
		if failed := recover(); failed != nil {
			outcome = yamlOutcome{failed: failed}
		}
	}()
	learnt := yamlLearnt{large: r.large}
	yamlLearnAsItWas(&learnt, root, true)
	if learnt.aliases && yamlDeepestAsItWas(&learnt, root) > 10000 {
		return yamlOutcome{err: errors.New(yamlTooDeepAsItWas)}
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
			return yamlOutcome{err: yamlFailure(err)}
		}
		return yamlOutcome{value: v}
	case yamlRefused:
		return yamlOutcome{err: learnt.refusal(root)}
	}
	var v any
	if err := root.Decode(&v); err != nil {
		return yamlOutcome{err: yamlFailure(err)}
	}
	return yamlOutcome{value: v}
}

// yamlSurvey is what looking through a document told of it before it told
// how deep the document is nested.
type yamlSurvey struct {
	hasLarge, aliases bool
	problems          uint64
	twice             map[*yaml.Node]yamlTwice
	compared          int
}

func yamlSurveyOf(l *yamlLearnt) yamlSurvey {
	return yamlSurvey{hasLarge: l.hasLarge, aliases: l.aliases, problems: l.problems, twice: l.twice, compared: l.compared}
}

// The one bound on nesting changes nothing for a document that is not nested
// near it. Every document of the differential corpus — the repository's YAML
// files whole and cut off, the error tests' documents, those written for the
// forms of a merge, a key and an alias — and 10,000 drawn at random, each
// with mappings large past 128 keys and past 2, so that the library and the
// walk both decode them, is what it was: the same value with the same types,
// the same error recognised by the same text, the same failing outright.
// Looking through a document tells of its mappings, its aliases and its keys
// written twice what it told, with as many keys compared. And the depth is
// now less than the depth of the library's calls, which a document with an
// alias was held to, by the calls that are no level of nesting — one for the
// document, one for a scalar innermost and one for each alias on the way: by
// two in a document without an alias, or by one where its innermost sequence
// or mapping holds nothing — and is never more than it, so no document with
// an alias that the bound let through then is refused now for being counted
// otherwise.
func TestAYAMLDocumentFarFromTheDepthBoundIsDecodedAsItWas(t *testing.T) {
	documents := append(append(yamlFixtureDocuments(t), hostileYAMLDocuments()...), yamlWrittenDocuments()...)
	maker := yamlMaker{random: rand.New(rand.NewPCG(22, 25))} //nolint:gosec // documents for a test
	generated := 10000
	if raceDetector {
		generated = 1000
	}
	for range generated {
		documents = append(documents, maker.document())
	}
	decoded, refused, aliased := 0, 0, 0
	for i, document := range documents {
		if raceDetector && i%2 != 0 {
			// One in two, of the files, which are cut off after fewer of their
			// lines there (yamlFixtureDocuments), and of those drawn alike: the
			// race detector makes each several times as slow.
			continue
		}
		root, _ := yamlDocumentOf([]byte(document))
		if root == nil {
			continue
		}
		now, was := yamlLearnt{large: yamlLargeMapping}, yamlLearnt{large: yamlLargeMapping}
		now.learn(root, true)
		yamlLearnAsItWas(&was, root, true)
		if !reflect.DeepEqual(yamlSurveyOf(&now), yamlSurveyOf(&was)) {
			t.Fatalf("%q is looked through as %+v, and was as %+v", document, yamlSurveyOf(&now), yamlSurveyOf(&was))
		}
		depth, calls := now.deepest(root), yamlDeepestAsItWas(&was, root)
		if now.aliases {
			aliased++
		}
		// An alias was a call, and a merge and a key are nodes like any other,
		// then and now.
		if depth > calls || !now.aliases && calls-depth != 1 && calls-depth != 2 {
			t.Fatalf("%q is nested %d deep, and was decoded by the library in calls %d deep", document, depth, calls)
		}
		for _, large := range []int{yamlLargeMapping, 2} {
			got, before := yamlWith(root, large, nil), yamlValueAsItWas(yamlReading{large: large}, root)
			if isYAMLTooDeep(got.err) || before.err != nil && before.err.Error() == yamlTooDeepAsItWas {
				t.Fatalf("%q, large past %d keys, is refused for its depth: %s, and before %s", document, large, got.text(), before.text())
			}
			if !got.same(before) {
				t.Fatalf("%q, large past %d keys, is\n%s\nand was\n%s", document, large, got.text(), before.text())
			}
			if got.err != nil || got.failed != nil {
				refused++
			} else {
				decoded++
			}
		}
	}
	least := 1
	if raceDetector {
		least = 10
	}
	if decoded < 5000/least || refused < 1000/least || aliased < 500/least {
		t.Fatalf("%d documents were decoded and %d refused, %d of them with an alias: too few to show anything", decoded, refused, aliased)
	}
}
