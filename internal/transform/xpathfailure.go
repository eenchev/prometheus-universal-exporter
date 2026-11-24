package transform

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The XPath engine (antchfx/xpath, as pinned in go.mod) compiles some
// expressions and then panics when it evaluates them, depending on what the
// response holds. Examples:
//   - `sum('abc')` and `sum(string(//a))` over text that is not a number
//   - `//a[contains(@x, 5)]` once an `a` has an `x`
//   - `substring(//a, '1')`, `replace(//a, '(', '')`, `substring(//a, 0 div 0)`
//
// Nothing recovered these panics. The panic left the transform and the
// probe. The probe answered 500 or closed the connection, the series of
// every other rule were lost, and neither the rule's error_mode nor
// on_transform_error was consulted.
//
// Such a panic is now a failure of the rule being evaluated, like any other
// failure of it: of its expression, of one of its labels, or of the nodes of
// a sum() in either. It is recovered once per rule, around the whole
// evaluation (evaluateXPathRule), not at each node or label. A rule that
// does not panic pays for one deferred call. The rule then gives no series
// for the response. Series it made before the engine failed are taken back,
// because which ones they are depends only on the order of the nodes. The
// rule fails by its error_mode, with an error that names the metric, the
// expression being evaluated and what the engine said.
//
// The rule fails once, as a whole. Nodes that failed before the engine did
// (a cell without a value, text that is not a number) were counted as they
// came. On a day the engine did not fail, the first of them is what the rule
// is reported and logged with. Here that would be the same text, suppressed
// by the log as a repeat, for a rule that had stopped giving any series. So
// the engine's failure replaces them (engineFailureAlone): the rule is
// counted once, with no missing value, and reported with what the engine
// said.

// evaluateXPathRule adds the series of one rule to out, as xpathRule does,
// using the rule's own copies of its compiled expression and labels. A panic
// during evaluation becomes a failure of the rule.
//
// Every panic is recovered, including runtime errors in the exporter's own
// code. The engine panics with those too (a slice out of range in
// `substring('abc', 0 div 0)`), and the two cannot be told apart here. The
// message says so; for a runtime error it starts with "runtime error:".
func evaluateXPathRule[N comparable](ctx context.Context, out *model.MetricSet, root N, nodes xpathNodes[N], rule model.MetricRule, c *model.Collector, namespaces map[string]string, program *expr.XPathProgram) (err error) {
	made := len(out.Metrics)
	// How each label is read is decided here, once for the rule, and not
	// at each of its nodes (planXPathLabels).
	expression, plan := program.Get(), planXPathLabels(rule, namespaces, nodes.html)
	defer func() {
		failed := recover()
		if failed == nil {
			return
		}
		// The rule's compiled copies are not returned to the pool. The
		// engine runs an expression in the tree it compiled it to, and a
		// copy left half-way through an evaluation is not known to start
		// the next one like a new copy. The pool compiles another.
		releaseSeries(ctx, len(out.Metrics)-made)
		clear(out.Metrics[made:])
		out.Metrics = out.Metrics[:made]
		failure := xpathEngineFailure(nodes.kind, rule, plan, failed)
		if _, ours := failed.(runtime.Error); ours {
			logEngineStack(ctx, c, rule, failure)
		}
		if handleMetricError(ctx, c, rule, failure) {
			engineFailureAlone(ctx, rule, failure)
			err = nil
			return
		}
		err = ruleFailure(c, rule, failure)
	}()
	err = xpathRule(ctx, out, root, nodes, rule, c, plan, expression, program.Sums())
	releaseXPathLabels(plan)
	program.Put(expression)
	return err
}

// xpathEngineFailure is the failure of a rule the XPath engine panicked on
// with failed. It names the metric and the expression (the label's, with the
// label's name, when a label was being read), and gives the engine's own
// words, without a stack.
//
// The text of a runtime error after "runtime error:" changes with the
// response: `slice bounds out of range [9223372036854775807:2]` for
// `substring(//a, 0 div 0)` over text of two characters, another length at
// the next. So the failure is recognised by its text up to that point
// (model.SameFailureAs), and the log treats it as one failure however the
// numbers change. A failure the engine raised itself, a string or its own
// error, is recognised by its whole text.
func xpathEngineFailure(kind string, rule model.MetricRule, plan []xpathLabel, failed any) error {
	said := fmt.Sprint(failed)
	_, ours := failed.(runtime.Error)
	if ours && !strings.HasPrefix(said, runtimeErrorSaid) {
		// A failed type assertion is a runtime error that does not say so.
		said = runtimeErrorSaid + " " + said
	}
	failure := fmt.Errorf("metric %q %s %q cannot be evaluated: the XPath engine failed on it: %s", rule.Name, kind, rule.Expression, said)
	for i := range plan {
		if plan[i].evaluating {
			failure = fmt.Errorf("metric %q label %q: %s %q cannot be evaluated: the XPath engine failed on it: %s", rule.Name, plan[i].name, kind, rule.Labels[i].Expression, said)
			break
		}
	}
	if ours {
		return model.SameFailureAs(failure, strings.TrimSuffix(failure.Error(), strings.TrimPrefix(said, runtimeErrorSaid)))
	}
	return failure
}

// runtimeErrorSaid is what the text of a runtime error starts with.
const runtimeErrorSaid = "runtime error:"

// engineFailureAlone makes failure, which handleMetricError has just counted
// for rule, the rule's only failure in this transform. It replaces what was
// gathered for the rule before the engine failed. The rule is found the same
// way addCounted finds it. Under fail, or outside a Transform, nothing is
// gathered and there is nothing to replace.
func engineFailureAlone(ctx context.Context, rule model.MetricRule, failure error) {
	failures, gathering := ctx.Value(ruleFailuresKey{}).(*ruleFailures)
	if !gathering {
		return
	}
	for _, known := range failures.rules {
		if known.rule.Name == rule.Name && known.rule.Expression == rule.Expression && known.rule.Items == rule.Items {
			known.first, known.count, known.missing = failure, 1, 0
			return
		}
	}
}

// logEngineStack logs, at debug level, the stack of a runtime error the
// engine failed with, where the rule's failures are logged. A runtime error
// may be the exporter's own fault rather than the engine's. The failure's
// text has no stack, and under error_mode ignore nothing else of it is
// logged, so this is how it can be found. A caller that logs the rules'
// failures itself (LeaveRuleLoggingToCaller) does not log this line, so it
// is written here regardless. It is called while the panic is being
// recovered, so the stack is that of the panic.
func logEngineStack(ctx context.Context, c *model.Collector, rule model.MetricRule, failure error) {
	logger := ruleLogger(ctx)
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	logger.Debug("the XPath engine failed with a runtime error", "collector", collectorName(c), "metric", rule.Name, "error", failure, "stack", string(debug.Stack()))
}
