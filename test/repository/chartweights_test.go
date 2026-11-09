package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// server.allocatorCapacity renders --allocator.capacity on the exporter pods
// with the allocator, as a number or a string, and nothing when empty; set
// without the allocator, or to what the exporter refuses, it fails
// rendering, with the values schema and with it skipped.
func TestChartSetsTheAllocatorCapacity(t *testing.T) {
	tier.Slow(t)
	helm := requireHelm(t)
	allocator := []string{"--set", "allocator.enabled=true"}
	for value, want := range map[string]string{"2": "2", "0.1": "0.1", "100": "100", "1.25": "1.25", `"2.5"`: "2.5"} {
		out, ok := helmTemplate(t, helm, chartDir, append(allocator, "--set-json", "server.allocatorCapacity="+value)...)
		if !ok || !strings.Contains(out, `- "--allocator.capacity=`+want+`"`+"\n") {
			t.Errorf("%s: rendered %v\n%s", value, ok, testutil.FirstLines(out, 5))
		}
	}
	for _, args := range [][]string{allocator, append(allocator, "--set-string", "server.allocatorCapacity="), append(allocator, "--set", "server.allocatorCapacity=null")} {
		if out, ok := helmTemplate(t, helm, chartDir, args...); !ok || strings.Contains(out, "--allocator.capacity") {
			t.Errorf("%v: rendered %v, with the flag: %v", args, ok, strings.Contains(out, "--allocator.capacity"))
		}
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"without the allocator": {[]string{"--set", "server.allocatorCapacity=2"}, "server.allocatorCapacity is set without allocator.enabled"},
		"below 0.1":             {append(allocator, "--set", "server.allocatorCapacity=0.05"), "server.allocatorCapacity"},
		"above 100":             {append(allocator, "--set", "server.allocatorCapacity=100.5"), "server.allocatorCapacity"},
		"four decimals":         {append(allocator, "--set-json", "server.allocatorCapacity=1.0001"), "allocatorCapacity"},
		"a word":                {append(allocator, "--set-string", "server.allocatorCapacity=lots"), "allocatorCapacity"},
		"as an extra":           {append(allocator, "--set", "extraArgs[0]=--allocator.capacity=2"), "server.allocatorCapacity"},
	} {
		for _, skip := range []bool{false, true} {
			args := c.args
			if skip {
				args = append([]string{"--skip-schema-validation"}, args...)
			}
			if out, ok := helmTemplate(t, helm, chartDir, args...); ok || !strings.Contains(out, c.want) {
				t.Errorf("%s (schema skipped %v): rendered %v, want a failure naming %q:\n%s", name, skip, ok, c.want, testutil.FirstLines(out, 5))
			}
		}
	}
}

// A probe job's weight and weight_label reach the allocator's probe
// targets file as written.
func TestChartProbeJobsTakeWeights(t *testing.T) {
	helm := requireHelm(t)
	out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true", "--set-json",
		`allocator.probeJobs=[{"name":"web","collector":"example","targets":["https://a.example"],"weight":2.5},{"name":"found","collector":"example","weight_label":"__meta_pue_weight","discovery":[{"file_sd":{"files":["sd.json"]}}]}]`)
	if !ok || !strings.Contains(out, "weight: 2.5\n") || !strings.Contains(out, "weight_label: __meta_pue_weight\n") {
		t.Errorf("rendered %v:\n%s", ok, testutil.FirstLines(out, 10))
	}
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "allocator.enabled=true", "--set-json", `allocator.probeJobs=[{"name":"web","collector":"example","targets":["https://a.example"],"weight":0}]`); ok || !strings.Contains(out, "weight") {
		t.Errorf("a weight of 0 rendered %v:\n%s", ok, testutil.FirstLines(out, 5))
	}
}

// allocator.balanceFactor renders --allocation.balance-factor on the
// allocator's pods, as a number or a string, and nothing when empty or
// null; what the allocator refuses fails rendering, with the values schema
// and with it skipped.
func TestChartSetsTheBalanceFactor(t *testing.T) {
	tier.Slow(t)
	helm := requireHelm(t)
	allocator := []string{"--set", "allocator.enabled=true"}
	for value, want := range map[string]string{"1": "1", "1.5": "1.5", "10": "10", "2.125": "2.125", `"3"`: "3", `"1.25"`: "1.25"} {
		out, ok := helmTemplate(t, helm, chartDir, append(allocator, "--set-json", "allocator.balanceFactor="+value)...)
		if !ok || !strings.Contains(out, `- "--allocation.balance-factor=`+want+`"`+"\n") {
			t.Errorf("%s: rendered %v\n%s", value, ok, testutil.FirstLines(out, 5))
		}
	}
	for _, args := range [][]string{allocator, append(allocator, "--set-string", "allocator.balanceFactor="), append(allocator, "--set", "allocator.balanceFactor=null")} {
		if out, ok := helmTemplate(t, helm, chartDir, args...); !ok || strings.Contains(out, "--allocation.balance-factor") {
			t.Errorf("%v: rendered %v, with the flag: %v", args, ok, strings.Contains(out, "--allocation.balance-factor"))
		}
	}
	for name, value := range map[string]string{
		"below 1": "0.99", "above 10": "10.5", "eleven": "11", "four decimals": "1.0001", "a word": `"even"`, "zero": "0", "negative": "-2",
	} {
		for _, skip := range []bool{false, true} {
			args := append(allocator, "--set-json", "allocator.balanceFactor="+value)
			if skip {
				args = append([]string{"--skip-schema-validation"}, args...)
			}
			if out, ok := helmTemplate(t, helm, chartDir, args...); ok || !strings.Contains(out, "balanceFactor") {
				t.Errorf("%s (schema skipped %v): rendered %v, want a failure naming allocator.balanceFactor:\n%s", name, skip, ok, testutil.FirstLines(out, 5))
			}
		}
	}
}
