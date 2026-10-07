//go:build !select_request_types || request_type_graphite

package fetch

import (
	"slices"
	"strings"
	"testing"
)

// A Graphite expression lists alternatives in braces, and a placeholder may
// be the first of them: in app.{{{param_host}},db}.cpu the first brace opens
// the list and the two after it the placeholder. The expression is checked
// with a value in the placeholder's place and sent with the probe's: it was
// sent as it is written, three braces and the placeholder's name, by a
// collector that refused the parameter as unused. A value is held to what
// an expression may hold, and a list left open by the placeholder is
// refused when the configuration loads.
func TestAGraphiteListMayStartWithAPlaceholder(t *testing.T) {
	c := graphiteCollector("app.{{{param_host}},db}.cpu", "{{{param_env:prod}},{{param_other:test}}}.up")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	overrides := RequestOverrides{Params: map[string]string{"param_host": "web01"}}
	if err := CheckPathParams(&c, overrides); err != nil {
		t.Fatal(err)
	}
	if got, want := graphiteURL(t, &c, overrides).Query()["target"], []string{"app.{web01,db}.cpu", "{prod,test}.up"}; !slices.Equal(got, want) {
		t.Errorf("the expressions asked for: %q, want %q", got, want)
	}
	if err := CheckPathParams(&c, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "request.targets[0] needs param_host, which the probe did not supply and which has no default") {
		t.Errorf("a probe without the parameter: %v", err)
	}
	if err := CheckPathParams(&c, RequestOverrides{Params: map[string]string{"param_host": "web*"}}); err == nil || !strings.Contains(err.Error(), "request.targets[0]: the value of param_host") {
		t.Errorf("a value an expression cannot hold: %v", err)
	}
	for expression, want := range map[string]string{
		"app.{{{param_host}}.cpu":      "has a { that is never closed",
		"app.{{{ param_host}},db}.cpu": "request.targets[0] has a placeholder with a space after {{",
		"app.{{{param_host:a*}},db}":   "that value is the placeholder's default",
	} {
		open := graphiteCollector(expression)
		if err := ValidateRequest(&open); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", expression, err, want)
		}
	}
}
