package transform

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/itchyny/gojq"
)

// jq's debug and stderr write nothing in a scrape (expr.jqQuiet). In a
// debug probe, whose context carries the logger of its report
// (WithRuleLogger), each value they are given is a line of that report,
// with the rule it was told in and the expression: what an expression made
// part of the way is then seen without rewriting the rule. The report shows
// the response already, so the values tell its reader nothing they could
// not read there.
//
// A program that calls neither is the scrape's own. One that does is
// compiled anew for the probe, once for each of its expressions, with
// functions that tell this (expr.CompileJQTold).

// The most lines a transform's debug and stderr write, after which one line
// says the rest are left out, and the most bytes of a value a line shows.
const (
	jqToldLines = 100
	jqToldBytes = 1024
)

// jqTold takes what a transform's programs tell. The rules of a transform
// run one after another, so it needs no lock.
type jqTold struct {
	logger   *slog.Logger
	programs map[string]*gojq.Code
	// rule names the rule being evaluated.
	rule  string
	lines int
}

// newJQTold is where the programs of ctx's Transform tell their values:
// nil unless ctx is a debug probe's.
func newJQTold(ctx context.Context) *jqTold {
	logger, ok := ctx.Value(ruleLoggerKey{}).(*slog.Logger)
	if !ok {
		return nil
	}
	return &jqTold{logger: logger, programs: map[string]*gojq.Code{}}
}

// jqToldOf is the jqTold of a Transform's context, nil for a scrape that
// is no debug probe.
func jqToldOf(ctx context.Context) *jqTold {
	vars, _ := ctx.Value(responseVariablesKey{}).(*responseVariables)
	if vars == nil {
		return nil
	}
	return vars.told
}

// at notes the rule whose expressions run next.
func (t *jqTold) at(rule string) {
	if t != nil {
		t.rule = rule
	}
}

// program is the program to run in place of p: p compiled so that its
// debug and stderr tell t. It compiled before, so it does again; were it
// not to, p itself runs.
func (t *jqTold) program(p *expr.JQProgram) *gojq.Code {
	if code := t.programs[p.Expression]; code != nil {
		return code
	}
	// A program tells its own expression: one whose values are taken as
	// they are needed, as a rule's items are, runs on between others.
	code, err := expr.CompileJQTold(p.Expression, func(function string, value any) { t.tell(function, p.Expression, value) })
	if err != nil {
		code = p.Code
	}
	t.programs[p.Expression] = code
	return code
}

// tell writes the line of one value.
func (t *jqTold) tell(function, expression string, value any) {
	t.lines++
	switch {
	case t.lines == jqToldLines+1:
		t.logger.Info("the transform's expressions called debug or stderr more often; the rest is left out", "shown", jqToldLines)
		return
	case t.lines > jqToldLines:
		return
	}
	text := "a value that cannot be written as JSON"
	if written, err := json.Marshal(value); err == nil {
		text = string(written)
	}
	t.logger.Info("jq "+function, "rule", t.rule, "expression", jqToldCut(expression), "value", jqToldCut(text))
}

// jqToldCut is text, cut to its first jqToldBytes, at a character, with
// its length when it is longer.
func jqToldCut(text string) string {
	if len(text) <= jqToldBytes {
		return text
	}
	cut := jqToldBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes)", text[:cut], len(text))
}
