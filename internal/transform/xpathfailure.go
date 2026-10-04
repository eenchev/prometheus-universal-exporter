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

// The XPath engine (antchfx/xpath, as pinned in go.mod) compiles expressions
// it then panics on when it evaluates them, by what the response holds:
// `sum('abc')` and `sum(string(//a))` over text that is no number,
// `//a[contains(@x, 5)]` once an `a` has an `x`, `substring(//a, '1')`,
// `replace(//a, '(', '')`, `//a = true()`. Nothing recovered: the panic left
// the transform, and with it the probe, which answered 500 or closed the
// connection, lost the series of every other rule, and asked neither the
// rule's error_mode nor on_transform_error.
//
// Such a panic is the failure of the rule that was being evaluated, like
// any other failure of it: of its expression, of a label of it, or of the
// nodes of a sum() in either. It is recovered once for the rule, around all
// of its evaluation (evaluateXPathRule), and not at each node or label, so
// a rule that does not panic pays for one deferred call. The rule then
// gives no series for the response — those it made before the engine
// failed are taken back, since which they are is an accident of the order
// of the nodes — and fails by its error_mode, with an error that names the
// metric, the expression that was being evaluated and what the engine said.
//
// It fails once, as a whole. The nodes of the rule that failed before the
// engine did — a cell without a value, text that is no number — were counted
// as they came, and the first of them would be what the rule is reported and
// logged with, as on a day the engine did not fail: the same text, held back
// by the log as a repeat, over a rule that had stopped giving any series. So
// the engine's failure takes their place (engineFailureAlone): the rule is
// counted once, with no missing value, and reported with what the engine
// said.

// evaluateXPathRule adds the series of one rule to out, as xpathRule does,
// with the rule's own copies of its compiled expression and labels, and
// makes a failure of the rule of a panic while it is evaluated.
//
// Every panic is recovered, a runtime error of the exporter's own code
// among them — the engine panics with those too, a nil dereference in
// `//a = true()`, a slice out of range in `substring('abc', 0 div 0)`, and
// the two cannot be told apart from here — and says so in its message,
// which for a runtime error starts with "runtime error:".
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
		// The compiled copies the rule took are not handed back: the
		// engine runs an expression in the tree it compiled it to, and
		// one left half-way through an evaluation is not known to start
		// the next one as a new copy does. The pool compiles another.
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
// with failed: it names the metric and the expression — the label's, with
// the label's name, when a label was being read — and gives the engine's
// own words, without a stack.
//
// What a runtime error says after "runtime error:" moves with the response
// — `slice bounds out of range [:3] with length 2` for `substring(//a, 2,
// 10)` over text of two characters, and another length at the next — so the
// failure is recognised by its text up to there (model.SameFailureAs), and
// is one failure to the log however the numbers change. What the engine
// raised itself, a string or an error of its own, is recognised by all of
// its text.
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
// for rule, the rule's one failure of this transform: what was gathered for
// the rule before the engine failed is replaced by it. The rule is found as
// addCounted finds it. Nothing gathers under fail, or outside a Transform,
// and there is nothing to replace.
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
// may be the exporter's own fault and not the engine's, the failure's text
// has no stack, and under error_mode ignore nothing else is logged of it:
// this is how it can be found. The caller that logs the rules' failures
// itself (LeaveRuleLoggingToCaller) does not log this line, so it is written
// here whatever the caller logs. It is called while the panic is being
// recovered, so the stack is that of the panic.
func logEngineStack(ctx context.Context, c *model.Collector, rule model.MetricRule, failure error) {
	logger := ruleLogger(ctx)
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	logger.Debug("the XPath engine failed with a runtime error", "collector", collectorName(c), "metric", rule.Name, "error", failure, "stack", string(debug.Stack()))
}
