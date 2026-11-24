package transform

import (
	"context"
	"strconv"
	"strings"

	"github.com/antchfx/xpath"
	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The XPath engine's sum() reads each node's text with strconv.ParseFloat
// and silently skips every node it cannot read: `1,234`, `n/a`, and a
// number with blanks around it, as in the cells of pretty-printed markup.
// So `sum(//td[@class='bytes'])` over a table gave a wrong number, often 0,
// and no error, while XPath says the sum of non-numeric text is NaN. The
// engine is not ours to change, so the nodes of a sum() are read here. This
// covers the calls expr.XPathSums found when the expression compiled: those
// in the expression's own context, outside every predicate.
//
// An expression that is a single sum() and nothing else is added up here,
// not by the engine. Each node's text is trimmed of blanks and read as the
// engine reads a number, in the engine's order. So a sum whose nodes are
// all numbers is the same float the engine gives, bit for bit, and numbers
// with surrounding blanks are summed too. A node that is not a number
// leaves the sum without a value.
//
// A sum() inside a larger expression is computed by the engine, with the
// rest. Its nodes are read first, as the engine will read them, blanks
// included. A node the engine would skip leaves the expression without a
// value. A blank counts there, since the engine cannot be made to trim.
//
// Without a value, a rule is missing its value, like one that computes
// NaN. A label has no such mode, so it fails its series like a label that
// cannot be read. An argument that is not a node-set (a number or a
// string) is left to the engine as before, and so is every sum() inside a
// predicate. The nodes are read whether or not the engine would reach the
// call. If reading them makes the engine panic, the call is left to the
// engine (addXPathSum).

// xpathSumFault is why a sum() gives its expression no value: how many of
// the nodes it adds up are no numbers, and the text of the first.
type xpathSumFault struct {
	call *expr.XPathSum
	// whole says the call is the whole expression, which the exporter adds
	// up, and not a part of one the engine does.
	whole      bool
	text       string
	bad, nodes int
}

// reason describes the fault and what to do about it, to follow the text
// that names the expression. The node count and the count of non-numbers
// are measured from the response and are not part of the failure's
// identity in the log (model.SameFailureText): a table that grows by a row
// fails the same way.
func (f *xpathSumFault) reason() error {
	if f.whole {
		return model.Errorf("cannot be computed: it adds up text that is not a number, first %s (%d of %d nodes); leave such nodes out with a predicate, as in sum(//td[number(.) = number(.)]), or select the nodes with a rule of their own, where value_map or a pre_script makes numbers of the text, and add the series up in PromQL", model.QuoteValue(f.text), model.Size(f.bad), model.Size(f.nodes))
	}
	return model.Errorf("cannot be computed: sum(%s) leaves out text it cannot read as a number, first %s (%d of %d nodes), and blanks around a number count; XPath 1.0 cannot trim each node, so select the nodes with a rule of their own and add the series up in PromQL, or rewrite the text in a pre_script", f.call.Argument, model.QuoteValue(f.text), model.Size(f.bad), model.Size(f.nodes))
}

// sumNavigator is where the argument of a sum() is evaluated from for node:
// where xpathValue evaluates the expression the call stands in, so that the
// nodes read here are those the engine adds up.
func sumNavigator[N comparable](nodes xpathNodes[N], node N) xpath.NodeNavigator {
	return nodes.navigate(node)
}

// readXPathSums reads, at node, the nodes of every sum() of an expression.
// own says the expression is a single sum() over nodes, and sum is their
// total. Otherwise the engine computes the expression, unless fault says a
// sum() in it has a node that is not a number.
func readXPathSums[N comparable](nodes xpathNodes[N], node N, sums *expr.XPathSums) (sum float64, own bool, fault *xpathSumFault) {
	if sums.Whole != nil {
		if sum, own, fault = addXPathSum(nodes, node, sums.Whole, true); fault != nil {
			return 0, false, fault
		}
	}
	for i := range sums.Parts {
		if _, _, fault = addXPathSum(nodes, node, &sums.Parts[i], false); fault != nil {
			return 0, false, fault
		}
	}
	return sum, own, nil
}

// addXPathSum adds up the nodes the argument of one sum() selects at node,
// as the engine does: in the order it gives them, a node twice if it gives
// it twice, each text read with strconv.ParseFloat. whole first trims the
// blanks around each text. nodeSet is false for an argument that computes
// a value, which is not read here.
//
// The engine may panic on the argument, as on `//a[contains(@x, 5)]` once
// an `a` has an `x`. That alone is not a failure of the rule. The nodes
// are read here whether or not the engine would reach the call, and on the
// right of an `or` whose left is true it never does. So the sum is then
// left to the engine, like a sum not read here. The engine either gives
// the expression its value without the call, or panics on it in turn,
// which is the rule's failure (evaluateXPathRule). The copy the engine
// panicked on is not returned to its pool.
func addXPathSum[N comparable](nodes xpathNodes[N], node N, call *expr.XPathSum, whole bool) (sum float64, nodeSet bool, fault *xpathSumFault) {
	argument := call.Program.Get()
	defer func() {
		if recover() != nil {
			sum, nodeSet, fault = 0, false, nil
			return
		}
		call.Program.Put(argument)
	}()
	selected, nodeSet := argument.Evaluate(sumNavigator(nodes, node)).(*xpath.NodeIterator)
	if !nodeSet {
		return 0, false, nil
	}
	count, bad, first := 0, 0, ""
	for selected.MoveNext() {
		count++
		text := selected.Current().Value()
		if whole {
			text = strings.TrimSpace(text)
		}
		number, err := strconv.ParseFloat(text, 64)
		if err != nil {
			if bad == 0 {
				first = text
			}
			bad++
			continue
		}
		sum += number
	}
	if bad > 0 {
		return 0, true, &xpathSumFault{call: call, whole: whole, text: first, bad: bad, nodes: count}
	}
	return sum, true, nil
}

// xpathSumRule makes the series of a rule whose expression has a sum() in
// its own context, when the engine is not to compute it. That is the
// series of the sum the exporter added up, or none (the rule's missing
// value) for a sum() with a node that is not a number. done is false when
// the engine is to evaluate the expression, because every node of its sums
// is a number.
func xpathSumRule[N comparable](ctx context.Context, out *model.MetricSet, nodes xpathNodes[N], root N, rule model.MetricRule, c *model.Collector, plan []xpathLabel, sums *expr.XPathSums) (done bool, err error) {
	sum, own, fault := readXPathSums(nodes, root, sums)
	switch {
	case fault != nil:
		missing := model.MarkError(model.Errorf("metric %q %s %q %w", rule.Name, nodes.kind, rule.Expression, fault.reason()), model.ErrMissingValue)
		if !requiredRule(rule, c) || handleMetricError(ctx, c, rule, missing) {
			return true, nil
		}
		return true, ruleFailure(c, rule, missing)
	case own:
		return true, addComputedXPathSeries(ctx, out, nodes, root, rule, c, plan, sum)
	}
	return false, nil
}

// summedXPathLabels returns the labels of a rule, by index in plan, whose
// expression has a sum() in its own context. It is nil for a rule without
// one, as nearly every rule is, which then pays nothing more at its nodes.
// Only a label the engine evaluates can have one: a label read by walking
// the tree calls no function.
func summedXPathLabels(plan []xpathLabel) []int {
	var summed []int
	for i := range plan {
		if plan[i].kind == xpathLabelEngine && plan[i].program.Sums() != nil {
			summed = append(summed, i)
		}
	}
	return summed
}

// readSummedXPathLabels reads, at node, the nodes of the sums in the labels
// listed in summed, after xpathLabels has read the labels. A label that is
// a single sum() gets the sum the exporter adds up, instead of the
// engine's. A label with a node that is not a number is the series'
// failure. It names the metric, the node by its index among the rule's
// nodes (none for the single series of a computed value, at -1), and the
// label.
func readSummedXPathLabels[N comparable](nodes xpathNodes[N], node N, rule model.MetricRule, plan []xpathLabel, summed []int, index int, labels map[string]string) error {
	for _, at := range summed {
		label := &plan[at]
		// From where the engine evaluates the label: the document is the
		// root for one with an absolute path in it (engineXPathLabel).
		from := nodes
		if label.rooted {
			from = rootedXPathNodes(nodes)
		}
		sum, own, fault := labelXPathSums(from, node, label)
		switch {
		case fault != nil && index < 0:
			return model.Errorf("metric %q label %q: %s %q %w", rule.Name, label.name, nodes.kind, rule.Labels[at].Expression, fault.reason())
		case fault != nil:
			return model.Errorf("metric %q node %d label %q: %s %q %w", rule.Name, model.Position(index), label.name, nodes.kind, rule.Labels[at].Expression, fault.reason())
		case own:
			labels[label.name] = summedLabelText(label, sum)
		}
	}
	return nil
}

// summedLabelText is the text of a label that is one sum(): written once for
// a label that cannot depend on the node, which has the same sum at every
// node, rather than formatted again at each.
func summedLabelText(label *xpathLabel, sum float64) string {
	once := label.constant
	if once == nil {
		return xpathText(sum)
	}
	if !once.sumWritten {
		once.sumText, once.sumWritten = xpathText(sum), true
	}
	return once.sumText
}
