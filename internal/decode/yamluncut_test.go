package decode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// The YAML decoder's errors as they were made before any part of one was
// cut, copied from the decoder as it then was: the oracle for what an error
// with no long part reads as, and is recognised by. The looking through a
// document, the ways it is decoded and the walk are the decoder's own, which
// were not changed.

// decodeYAMLUncut is yamlReading.decode as it was.
func decodeYAMLUncut(r yamlReading, body []byte) (v any, err error) {
	defer func() {
		if failed := recover(); failed != nil {
			v, err = nil, yamlPanickedUncut(failed, yamlPanicPlace())
		}
	}()
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var root yaml.Node
	if err := decoder.Decode(&root); errors.Is(err, io.EOF) {
		return nil, nil
	} else if err != nil {
		return nil, yamlFailureUncut(err)
	}
	for {
		var next yaml.Node
		err := decoder.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, yamlFailureUncut(err)
		}
		if !emptyYAMLDocument(&next) {
			return nil, errors.New("the body holds more than one YAML document (separated by ---); multi-document YAML is not supported")
		}
	}
	timestampsAsText(&root, map[*yaml.Node]bool{})
	return yamlValueUncut(r, &root)
}

// yamlValueUncut is yamlReading.value as it was.
func yamlValueUncut(r yamlReading, root *yaml.Node) (any, error) {
	learnt := yamlLearnt{large: r.large}
	learnt.learn(root, true)
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
			return nil, yamlFailureUncut(err)
		}
		return v, nil
	case yamlRefused:
		var refusal yamlRefusalUncut
		refusal.read(&learnt, root)
		return nil, refusal.failure(learnt.problems)
	}
	var v any
	if err := root.Decode(&v); err != nil {
		return nil, yamlFailureUncut(err)
	}
	return v, nil
}

// yamlPanickedUncut is yamlPanicked as it was.
func yamlPanickedUncut(failed any, at yamlPlace) error {
	said := fmt.Sprint(failed)
	_, ours := failed.(runtime.Error)
	if at.library {
		const library = "the YAML library failed on the document: "
		err := errors.New(library + said)
		if ours {
			return model.SameFailureAs(err, fmt.Sprintf("%sruntime error (%s %s)", library, at.file, at.function))
		}
		return err
	}
	const exporter, report = "the exporter failed on the YAML document (%s %s): %s", "; this is a defect of the exporter and not of the document, please report it"
	err := fmt.Errorf(exporter+report, fmt.Sprintf("%s:%d", at.file, at.line), at.function, said)
	if ours {
		return model.SameFailureAs(err, fmt.Sprintf(exporter, at.file, at.function, "runtime error"))
	}
	return model.SameFailureAs(err, fmt.Sprintf(exporter+report, at.file, at.function, said))
}

// yamlFailureUncut is yamlFailure as it was.
func yamlFailureUncut(err error) error {
	if problems, ok := err.(*yaml.TypeError); ok { //nolint:errorlint // as yamlFailure
		return yamlProblemsUncut(problems)
	}
	const library = "yaml: "
	text := err.Error()
	problem, ok := strings.CutPrefix(text, library)
	if !ok {
		return err
	}
	if same := withoutYAMLLines(problem); same != problem {
		return model.SameFailureAs(err, library+strings.TrimPrefix(same, "line "+model.MovingMark+": "))
	}
	return err
}

// yamlProblemsUncut is yamlProblems as it was.
func yamlProblemsUncut(problems *yaml.TypeError) error {
	var err error = problems
	moved := len(problems.Errors) > yamlProblemsShown
	if moved {
		more := "problems"
		if len(problems.Errors) == yamlProblemsShown+1 {
			more = "problem"
		}
		shown := append(slices.Clone(problems.Errors[:yamlProblemsShown]), fmt.Sprintf("... and %d more %s", len(problems.Errors)-yamlProblemsShown, more))
		err = &yaml.TypeError{Errors: shown}
	}
	same := make([]string, 0, min(len(problems.Errors), yamlProblemsShown+1))
	var masked []byte
listed:
	for _, problem := range problems.Errors {
		masked = appendWithoutYAMLLines(masked[:0], problem)
		for _, known := range same {
			if known == string(masked) {
				continue listed
			}
		}
		if len(same) == yamlProblemsShown {
			same = append(same, "... and "+model.MovingMark+" more problems")
			break
		}
		moved = moved || string(masked) != problem
		same = append(same, string(masked))
	}
	if !moved && len(same) == len(problems.Errors) {
		return err
	}
	return model.SameFailureAs(err, (&yaml.TypeError{Errors: same}).Error())
}

// yamlWrittenTwiceUncut is yamlWrittenTwice as it was, the library's own
// format.
const yamlWrittenTwiceUncut = "line %d: mapping key %#v already defined at line %d"

// yamlRefusalUncut is yamlRefusal as it was.
type yamlRefusalUncut struct {
	first []string
	kinds []string
	same  []string
}

func (r *yamlRefusalUncut) done() bool {
	return len(r.first) == yamlProblemsShown && len(r.kinds) > yamlProblemsShown
}

func (r *yamlRefusalUncut) read(learnt *yamlLearnt, n *yaml.Node) {
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

func (r *yamlRefusalUncut) list(n *yaml.Node, left map[yamlKey]uint32) {
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
		for j := i + 2; j < len(keys) && later > 0 && len(r.first) < yamlProblemsShown; j += 2 {
			if keys[j].Kind == key.kind && keys[j].Value == key.value {
				r.first = append(r.first, fmt.Sprintf(yamlWrittenTwiceUncut, keys[j].Line, keys[j].Value, keys[i].Line))
				later--
			}
		}
	}
}

func (r *yamlRefusalUncut) kind(key *yaml.Node) {
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
	r.same = append(r.same, withoutYAMLLines(fmt.Sprintf(yamlWrittenTwiceUncut, key.Line, key.Value, key.Line)))
}

func (r *yamlRefusalUncut) failure(problems uint64) error {
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
