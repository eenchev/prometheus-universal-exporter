//go:build !select_request_types || request_type_graphite

package fetch

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A clock placeholder fills a Graphite expression with what one may hold,
// and one whose format holds a character an expression may not is refused
// when the configuration loads, naming the clock placeholder.
func TestAClockPlaceholderInAGraphiteExpression(t *testing.T) {
	atClock(t, time.Date(2026, time.November, 9, 8, 0, 0, 0, time.UTC))
	c := graphiteCollector("app.{{date_compact}}.cpu")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	if got := graphiteURL(t, &c, RequestOverrides{}).Query()["target"]; len(got) != 1 || got[0] != "app.20261109.cpu" {
		t.Fatalf("asked for %q", got)
	}
	us := graphiteCollector("app.{{date_us}}.cpu")
	if err := ValidateRequest(&us); err == nil || !strings.Contains(err.Error(), "the clock placeholder writes values such as that one") {
		t.Fatalf("got %v", err)
	}
}

// A static target's parameter that holds a clock placeholder is checked
// as it is written at clockSample, not as the text it is written as, which
// a Graphite expression could not hold.
func TestAStaticTargetsClockParameterInAGraphiteExpression(t *testing.T) {
	c := graphiteCollector("app.{{param_day}}.cpu")
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	target := model.StaticTarget{Name: "t", Params: map[string]string{"param_day": "{{date_compact}}"}}
	if _, err := CheckRequestParams(&c, TargetOverrides(&target)); err != nil {
		t.Fatal(err)
	}
	us := model.StaticTarget{Name: "t", Params: map[string]string{"param_day": "{{date_us}}"}}
	if _, err := CheckRequestParams(&c, TargetOverrides(&us)); err == nil {
		t.Fatal("a date with slashes was taken for a Graphite expression")
	}
}
