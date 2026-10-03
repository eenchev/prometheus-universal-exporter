package repository

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// A monitor's `port` has to hold a letter. The chart's README and the
// description of `port` in its values schema said a name is so "as a Service
// port is named", which presented the letter as Kubernetes' rule. It is not:
// Kubernetes names a Service port by a DNS label, which digits alone are
// (`name: "8080"`), and asks for a letter in a container port's name only.
// For a Service the letter is the chart's rule, made so that a number written
// as a name is not rendered as a port name that matches nothing, and it has a
// cost the documentation did not state: a Service whose port is named in
// digits alone cannot be monitored through `port` until the port is renamed.
// The README, the schema's description and the specification now say both,
// and the pattern that enforces the rule is as it was.
func TestTheChartSaysTheLetterInAServicePortsNameIsItsOwnRule(t *testing.T) {
	var schema struct {
		Properties struct {
			Monitors struct {
				Items struct {
					Properties struct {
						Port struct {
							Description string `json:"description"`
							Pattern     string `json:"pattern"`
						} `json:"port"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"monitors"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(readChartFile(t, "values.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	port := schema.Properties.Monitors.Items.Properties.Port
	specification, err := os.ReadFile("docs/SPECIFICATION-CHART.md")
	if err != nil {
		t.Fatal(err)
	}
	// One paragraph of each text, its line breaks made spaces.
	paragraph := func(text, holding string) string {
		for _, candidate := range strings.Split(text, "\n\n") {
			if candidate = strings.Join(strings.Fields(candidate), " "); strings.Contains(candidate, holding) {
				return candidate
			}
		}
		return ""
	}
	for name, tc := range map[string]struct {
		text string
		says []string
	}{
		"the chart README": {
			paragraph(readChartFile(t, "README.md"), "`port` is a port's name, never its number"),
			[]string{
				"for `type: service` it is the chart's own rule, made so that a number written as a name (`\"9115\"`) is not rendered as a port name that matches nothing",
				"Kubernetes itself allows a Service port to be named in digits alone",
				"a Service with such a port cannot be monitored through `port` until its port has a name with a letter",
			},
		},
		"the description of port in values.schema.json": {
			port.Description,
			[]string{
				"for type service it is the chart's own rule",
				"Kubernetes itself allows a Service port to be named in digits alone",
				"such a Service cannot be monitored through this value until its port has a name with a letter",
			},
		},
		"docs/SPECIFICATION-CHART.md": {
			paragraph(string(specification), "A `port` is a port's name and never its number"),
			[]string{
				"for `type: service` the letter is the chart's own rule, made so that a number written as a name (`\"9115\"`) is not rendered as a port name that matches nothing",
				"Kubernetes itself allows a Service port to be named in digits alone",
				"a Service whose port is named in digits alone cannot be monitored through this value until its port has a name with a letter",
				"MUST say that the letter is the chart's rule for a Service port and not Kubernetes'",
			},
		},
	} {
		if tc.text == "" {
			t.Errorf("%s no longer has the text on a monitor's port", name)
			continue
		}
		for _, sentence := range tc.says {
			if !strings.Contains(tc.text, sentence) {
				t.Errorf("%s does not say %q:\n%s", name, sentence, tc.text)
			}
		}
		for _, claim := range []string{"as a Service port is named", "as Kubernetes names a Service port"} {
			if strings.Contains(tc.text, claim) {
				t.Errorf("%s still says a port's name has a letter %q, which Kubernetes does not ask of a Service port:\n%s", name, claim, tc.text)
			}
		}
	}

	// The rule itself: what the documentation says is refused is refused,
	// by the pattern the schema has had.
	const pattern = `^([0-9]+-+)*[0-9]*[a-z]([-a-z0-9]*[a-z0-9])?$`
	if port.Pattern != pattern {
		t.Fatalf("the pattern of a monitor's port is %s, want %s", port.Pattern, pattern)
	}
	names := regexp.MustCompile(port.Pattern)
	for name, taken := range map[string]bool{"8080": false, "9115": false, "80-80": false, "http": true, "8080-tcp": true, "9-a": true} {
		if names.MatchString(name) != taken {
			t.Errorf("the pattern takes the port name %q: %v, want %v", name, !taken, taken)
		}
	}
}
