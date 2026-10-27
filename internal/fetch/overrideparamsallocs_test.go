package fetch

import (
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// Checking a probe's parameters allocates nothing when they are all in
// order: the target, the collector and a parameter of a monitor's own, which
// no request type takes. Every probe checks them, and each check made a list
// of every request type's parameters, a few allocations, for the one
// question whether a parameter is one of them.
func TestCheckingAProbesParametersAllocatesNothing(t *testing.T) {
	c := &model.Collector{Name: "checked", Request: model.RequestConfig{Type: BuiltRequestTypes()[0]}}
	values := url.Values{"target": {"x"}, "collector": {"checked"}, "monitor_own": {"1"}}
	if err := CheckOverrideParams(c, values); err != nil {
		t.Fatal(err)
	}
	if allocations := alloctest.AllocsAtMost(100, 0, func() { _ = CheckOverrideParams(c, values) }); allocations != 0 {
		t.Errorf("checking a probe's parameters allocated %v times", allocations)
	}
}

// A type registered after the first probe, as a test registers a fixture,
// has its parameters known at once, and forgotten once it is taken out: a
// parameter only it takes is refused for a collector of another type while
// it is there, and ignored after.
func TestAParameterOfATypeRegisteredLaterIsKnown(t *testing.T) {
	c := &model.Collector{Name: "checked", Request: model.RequestConfig{Type: BuiltRequestTypes()[0]}}
	values := url.Values{"target": {"x"}, "fixture_only": {"1"}}
	if err := CheckOverrideParams(c, values); err != nil {
		t.Fatal(err)
	}
	RequestTypes["fixture"] = &RequestType{Name: "fixture", Overrides: []string{"fixture_only"}}
	err := CheckOverrideParams(c, values)
	delete(RequestTypes, "fixture")
	if err == nil || !strings.Contains(err.Error(), "probe parameters fixture_only do not apply to collector \"checked\"") {
		t.Fatalf("with the fixture registered: %v", err)
	}
	if err := CheckOverrideParams(c, values); err != nil {
		t.Fatalf("with the fixture taken out: %v", err)
	}
}
