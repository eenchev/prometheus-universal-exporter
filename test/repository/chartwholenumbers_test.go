package repository

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Helm reads the numbers of a values file, and of --set-json, as floating
// point, and a template prints a floating-point number of a million or more
// with an exponent: replicaCount: 2000000 rendered replicas: 2e+06, and
// server.probeMaxConcurrent: 2000000 passed the values schema and then
// failed rendering as no whole number, since the templates' own check looked
// at 2e+06. --set gives an integer, which printed as written, so only a
// values file showed it. The tests below hold the chart to rendering a whole
// number as the whole number it is, however it was written, and to refusing
// what is none by its value. The renders need helm, as those of
// chartrender_test.go do; the first test reads the chart's files and runs
// without it.

// largestWholeNumber is the most a count, a number of replicas or a number
// of seconds of the values may be: what a Kubernetes count holds, and what
// the exporter's count flags take on every platform.
const largestWholeNumber = 2147483647

// countFlagPattern is what a count flag's value may be, a number written out
// or a string, as the templates check it: 0 to 2147483647, with no zero
// before another digit, since the exporter's flag reads 010 as eight and
// refuses 08. A pattern has to spell the range out digit by digit, since it
// is the one bound a schema can put on a string of digits.
const countFlagPattern = `^(0|[1-9][0-9]{0,8}|1[0-9]{9}|20[0-9]{8}|21[0-3][0-9]{7}|214[0-6][0-9]{6}|2147[0-3][0-9]{5}|21474[0-7][0-9]{4}|214748[0-2][0-9]{3}|2147483[0-5][0-9]{2}|21474836[0-3][0-9]|214748364[0-7])$`

// valuesFile writes a values file for one render and returns its path.
func valuesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// requireSchemaSkipping skips a test of the templates' own checks with a helm
// that cannot leave the values schema out.
func requireSchemaSkipping(t *testing.T, helm string) {
	t.Helper()
	if out, _ := helmTemplate(t, helm, chartDir, "--skip-schema-validation"); strings.Contains(out, "unknown flag") {
		t.Skip("this helm cannot skip the values schema")
	}
}

// schemaAt returns the schema found from the root of values.schema.json by
// the keys of path, each a property of the one before it.
func schemaAt(t *testing.T, path string) map[string]any {
	t.Helper()
	var node map[string]any
	if err := json.Unmarshal([]byte(readChartFile(t, "values.schema.json")), &node); err != nil {
		t.Fatal(err)
	}
	for key := range strings.SplitSeq(path, ".") {
		properties, _ := node["properties"].(map[string]any)
		next, ok := properties[key].(map[string]any)
		if !ok {
			t.Fatalf("values.schema.json has no property %s", path)
		}
		node = next
	}
	return node
}

// Every whole number the templates print themselves has one range, which the
// values schema and the templates both hold: the schema carries the maximum
// the helper is called with, so a number past it is refused with the
// schema's message and, with the schema skipped, with the templates'. The
// count flags take a string of digits too, which a schema can bound by a
// pattern alone: the schema's is the helper's with an alternative for the
// empty default, and the pattern takes exactly the strings of digits that
// are 2147483647 or less and are written as Go writes the number, with no
// zero before another digit, here compared with the number each string is.
func TestTheChartsWholeNumbersHaveOneRangeInTheSchemaAndTheTemplates(t *testing.T) {
	templates := map[string]string{}
	for _, name := range []string{"_helpers.tpl", "deployment.yaml", "service.yaml", "ingress.yaml", "horizontalpodautoscaler.yaml", "poddisruptionbudget.yaml"} {
		templates[name] = readChartFile(t, "templates/"+name)
	}
	const whole = `include "prometheus-universal-exporter.wholeNumber" `
	for _, tc := range []struct {
		value, template, call string
		least                 float64
	}{
		{"replicaCount", "deployment.yaml", whole + `(list "replicaCount" .Values.replicaCount 0 2147483647)`, 0},
		{"terminationGracePeriodSeconds", "_helpers.tpl", whole + `(list "terminationGracePeriodSeconds" $explicit 0 2147483647)`, 0},
		{"autoscaling.minReplicas", "horizontalpodautoscaler.yaml", whole + `(list "autoscaling.minReplicas" $scaling.minReplicas 1 2147483647)`, 1},
		{"autoscaling.maxReplicas", "horizontalpodautoscaler.yaml", whole + `(list "autoscaling.maxReplicas" $scaling.maxReplicas 1 2147483647)`, 1},
		{"autoscaling.targetCPUUtilizationPercentage", "horizontalpodautoscaler.yaml", whole + `(list "autoscaling.targetCPUUtilizationPercentage" . 1 2147483647)`, 1},
		{"autoscaling.targetMemoryUtilizationPercentage", "horizontalpodautoscaler.yaml", whole + `(list "autoscaling.targetMemoryUtilizationPercentage" . 1 2147483647)`, 1},
		{"podDisruptionBudget.minAvailable", "poddisruptionbudget.yaml", `include "prometheus-universal-exporter.budgetCount" (list "podDisruptionBudget.minAvailable" $pdb.minAvailable)`, 0},
		{"podDisruptionBudget.maxUnavailable", "poddisruptionbudget.yaml", `include "prometheus-universal-exporter.budgetCount" (list "podDisruptionBudget.maxUnavailable" $pdb.maxUnavailable)`, 0},
		{"server.probeMaxConcurrent", "deployment.yaml", `include "prometheus-universal-exporter.countFlag" (list "server.probeMaxConcurrent" .Values.server.probeMaxConcurrent)`, 0},
		{"server.pythonMaxWorkers", "deployment.yaml", `include "prometheus-universal-exporter.countFlag" (list "server.pythonMaxWorkers" .Values.server.pythonMaxWorkers)`, 0},
	} {
		if !strings.Contains(templates[tc.template], tc.call) {
			t.Errorf("templates/%s does not render %s through the helper that checks it: %s", tc.template, tc.value, tc.call)
		}
		schema := schemaAt(t, tc.value)
		if schema["minimum"] != tc.least || schema["maximum"] != float64(largestWholeNumber) {
			t.Errorf("the schema bounds %s from %v to %v, want %v to %d, as the templates do", tc.value, schema["minimum"], schema["maximum"], tc.least, largestWholeNumber)
		}
	}
	helpers := templates["_helpers.tpl"]
	if !strings.Contains(helpers, `(list (index . 0) (index . 1) 0 2147483647)`) {
		t.Error("the budgetCount helper does not hold a number to 0 to 2147483647")
	}
	// The Service's port is rendered in three places, each through the one
	// helper that holds it to a TCP port's range.
	if !strings.Contains(helpers, whole+`(list "service.port" .Values.service.port 1 65535)`) {
		t.Error("the servicePort helper does not hold service.port to 1 to 65535")
	}
	for name, call := range map[string]string{
		"service.yaml": `port: {{ include "prometheus-universal-exporter.servicePort" . }}`,
		"ingress.yaml": `number: {{ include "prometheus-universal-exporter.servicePort" $ }}`,
		"_helpers.tpl": `(include "prometheus-universal-exporter.servicePort" . | default "<nil>") -}}`,
	} {
		if !strings.Contains(templates[name], call) {
			t.Errorf("templates/%s does not render service.port through its helper: %s", name, call)
		}
	}
	var definitions struct {
		Defs struct {
			Port struct {
				Minimum, Maximum float64
			} `json:"port"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal([]byte(readChartFile(t, "values.schema.json")), &definitions); err != nil {
		t.Fatal(err)
	}
	if port := definitions.Defs.Port; port.Minimum != 1 || port.Maximum != 65535 || schemaAt(t, "service.port")["$ref"] != "#/$defs/port" {
		t.Error("the schema does not bound service.port from 1 to 65535, as the templates do")
	}

	// No integer of the schema is left without a maximum by oversight, the
	// timings of a probe and the counts of the rolling update, which their
	// sub-trees hand to toYaml, included: the schema bounds them as
	// Kubernetes does.
	var root any
	if err := json.Unmarshal([]byte(readChartFile(t, "values.schema.json")), &root); err != nil {
		t.Fatal(err)
	}
	unbounded := map[string]bool{}
	var walk func(path string, node any)
	walk = func(path string, node any) {
		object, ok := node.(map[string]any)
		if !ok {
			return
		}
		integer := object["type"] == "integer"
		if kinds, ok := object["type"].([]any); ok {
			integer = slices.Contains(kinds, any("integer"))
		}
		if _, bounded := object["maximum"]; integer && !bounded {
			unbounded[path] = true
		}
		for key, child := range object {
			walk(strings.TrimPrefix(path+"."+key, "."), child)
		}
	}
	walk("", root)
	if len(unbounded) != 0 {
		t.Errorf("the integers of the schema without a maximum are %v, want none: a template that prints a number itself renders it through the wholeNumber helper, and the schema carries the helper's maximum, and the probes' timings and the rolling update's counts, which a sub-tree hands to toYaml, carry Kubernetes' own", slices.Sorted(maps.Keys(unbounded)))
	}

	if !strings.Contains(helpers, `regexMatch "`+countFlagPattern+`" $text`) {
		t.Errorf("the countFlag helper does not check the value against %s", countFlagPattern)
	}
	for _, value := range []string{"server.probeMaxConcurrent", "server.pythonMaxWorkers"} {
		if got, want := schemaAt(t, value)["pattern"], "^$|"+countFlagPattern; got != want {
			t.Errorf("the schema's pattern for %s is %v, want %s: empty, or what the helper accepts", value, got, want)
		}
	}
	pattern := regexp.MustCompile(countFlagPattern)
	largest := big.NewInt(largestWholeNumber)
	check := func(digits string) {
		number, ok := new(big.Int).SetString(digits, 10)
		want := ok && number.Sign() >= 0 && number.Cmp(largest) <= 0 && number.String() == digits
		if got := pattern.MatchString(digits); got != want {
			t.Errorf("the count pattern takes %q: %v, want %v", digits, got, want)
		}
	}
	// Every number that differs from the largest in one digit, the lengths
	// around it, each with zeros before it, which the pattern refuses, what
	// is no string of digits, and generated numbers of up to twelve digits.
	const edge = "2147483647"
	for at := range edge {
		for digit := byte('0'); digit <= '9'; digit++ {
			changed := []byte(edge)
			changed[at] = digit
			for _, zeros := range []string{"", "0", "0000000000000"} {
				check(zeros + string(changed))
				check(zeros + string(changed[:at+1]))
				check(zeros + string(changed) + "0")
			}
		}
	}
	for _, digits := range []string{"", "0", "00", "7", "007", "010", "08", "2147483648", "9999999999", "99999999999999999999", "-1", "+1", "1.5", "2e6", "2e+06", "1_000", " 1", "1 ", "0x10", "ten"} {
		check(digits)
	}
	random := rand.New(rand.NewPCG(41, 43))
	for range 4000 {
		check(strconv.FormatUint(random.Uint64N(1e12), 10))
		check(strconv.FormatUint(largestWholeNumber-2000+random.Uint64N(4000), 10))
	}
}

// wholeNumberValues is a values file that sets every whole number the chart
// renders into the Deployment and the PodDisruptionBudget to written, the
// ones the templates print themselves and, of those a sub-tree hands to
// toYaml, a user and group ID, a probe's seconds, a resource and a number
// inside a monitors entry.
func wholeNumberValues(written string) string {
	return strings.ReplaceAll(`replicaCount: N
server:
  probeMaxConcurrent: N
  pythonMaxWorkers: N
terminationGracePeriodSeconds: N
podDisruptionBudget:
  enabled: true
  minAvailable: N
podSecurityContext:
  runAsUser: N
  fsGroup: N
securityContext:
  runAsUser: N
livenessProbe:
  periodSeconds: N
resources:
  limits:
    cpu: N
monitors:
  - name: apps
    enabled: true
    type: service
    collector: example
    relabelings:
      - action: hashmod
        sourceLabels: [__address__]
        modulus: N
        targetLabel: shard
`, "N", written)
}

// scalingNumberValues is a values file that sets the whole numbers of the
// HorizontalPodAutoscaler, with which the Deployment renders no replicas,
// and the budget's other count, to written.
func scalingNumberValues(written string) string {
	return strings.ReplaceAll(`autoscaling:
  enabled: true
  minReplicas: N
  maxReplicas: N
  targetCPUUtilizationPercentage: N
  targetMemoryUtilizationPercentage: N
podDisruptionBudget:
  enabled: true
  maxUnavailable: N
`, "N", written)
}

// wholeNumberLines is what the two values files above render for the whole
// number whole: each line with how many times it is rendered.
func wholeNumberLines(whole string) (deployment, scaling map[string]int) {
	deployment, scaling = map[string]int{}, map[string]int{}
	for line, count := range map[string]int{
		"\n  replicas: N\n": 1,
		"\n            - \"--probe.max-concurrent=N\"\n": 1,
		"\n            - \"--python.max-workers=N\"\n":   1,
		"\n      terminationGracePeriodSeconds: N\n":     1,
		"\n  minAvailable: N\n":                          1,
		"\n        runAsUser: N\n":                       1,
		"\n        fsGroup: N\n":                         1,
		"\n            runAsUser: N\n":                   1,
		"\n            periodSeconds: N\n":               1,
		"\n              cpu: N\n":                       1,
		"\n          modulus: N\n":                       1,
	} {
		deployment[strings.ReplaceAll(line, "N", whole)] = count
	}
	for line, count := range map[string]int{
		"\n  minReplicas: N\n":                1,
		"\n  maxReplicas: N\n":                1,
		"\n          averageUtilization: N\n": 2,
		"\n  maxUnavailable: N\n":             1,
		// The autoscaler sets the replica count itself.
		"\n  replicas:": 0,
	} {
		scaling[strings.ReplaceAll(line, "N", whole)] = count
	}
	return deployment, scaling
}

// exponent finds a number printed with an exponent, as a template prints a
// floating-point number of a million or more.
var exponent = regexp.MustCompile(`[0-9]e\+[0-9]`)

// nestedValue writes a values file that sets the one value named by a dotted
// path.
func nestedValue(path, written string) string {
	keys := strings.Split(path, ".")
	var values strings.Builder
	for depth, key := range keys {
		fmt.Fprintf(&values, "%s%s:", strings.Repeat("  ", depth), key)
		if depth == len(keys)-1 {
			fmt.Fprintf(&values, " %s", written)
		}
		values.WriteString("\n")
	}
	return values.String()
}

// A whole number of a values file is rendered as the whole number it is, in
// every place the chart renders one, whether it was written out, with an
// exponent or with a decimal point: two million, a number as large as an
// OpenShift user ID, the largest a count may be, and a million, the first a
// template prints with an exponent. Each line is looked for by itself, so a
// place that prints the number as a template does by itself is named. No
// number of the render is printed with an exponent.
func TestChartRendersAWholeNumberOfAValuesFileAsWritten(t *testing.T) {
	helm := requireHelm(t)
	for _, tc := range []struct {
		written, whole string
		scaling        bool
	}{
		{"2000000", "2000000", false},
		{"2e6", "2000000", false},
		{"2000000.0", "2000000", true},
		{"1000680000", "1000680000", false},
		{"2147483647", "2147483647", true},
		{"1000000", "1000000", false},
	} {
		deployment, scaling := wholeNumberLines(tc.whole)
		renders := map[string]map[string]int{wholeNumberValues(tc.written): deployment}
		if tc.scaling {
			renders[scalingNumberValues(tc.written)] = scaling
		}
		for values, lines := range renders {
			out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, values))
			if !ok {
				t.Errorf("%s: rendering failed:\n%s\nwith the values:\n%s", tc.written, out, values)
				continue
			}
			for line, count := range lines {
				if got := strings.Count(out, line); got != count {
					t.Errorf("%s: %q is rendered %d times, want %d", tc.written, line, got, count)
				}
			}
			if found := exponent.FindString(out); found != "" {
				t.Errorf("%s: the render prints a number with an exponent, %q", tc.written, found)
			}
		}
	}
}

// The Service's port is rendered as the whole number it is in the Service,
// in the Ingress and in the address a probe monitor sends Prometheus to,
// written with an exponent too. A port is below a million, so it rendered
// so before as well; the three places now go through one helper.
func TestChartRendersTheServicePortAsAWholeNumber(t *testing.T) {
	helm := requireHelm(t)
	out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, `service:
  port: 9.115e3
ingress:
  enabled: true
  hosts:
    - host: exporter.example
      paths:
        - path: /
          pathType: Prefix
monitors:
  - name: apps
    enabled: true
    type: service
    collector: example
`))
	if !ok {
		t.Fatalf("rendering failed:\n%s", out)
	}
	for _, want := range []string{"\n      port: 9115\n", "\n                  number: 9115\n", "\n          replacement: test-prometheus-universal-exporter.default.svc:9115\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("service.port 9.115e3 does not render %q:\n%s", want, out)
		}
	}
}

// A number past what a count may be, 2147483647, is refused by the values
// schema, naming the value and its maximum, where a template printed it with
// an exponent or, for the grace period, rendered it; the first test of this
// file holds every such value's maximum in the schema. So is a number too
// large for a template to hold as a whole one, a fraction past a million,
// and a count flag's string of digits past the same maximum, which the
// exporter's flag would refuse or take for no limit worth setting, or with
// a zero before another digit, which the flag reads as an octal number, 010
// as eight, or refuses, 08, and the chart took.
func TestChartRefusesAWholeNumberOutOfRange(t *testing.T) {
	helm := requireHelm(t)
	for _, tc := range []struct{ value, number string }{
		{"replicaCount", "2147483648"},
		{"terminationGracePeriodSeconds", "2147483648"},
		{"autoscaling.maxReplicas", "3e9"},
		{"server.pythonMaxWorkers", "1e30"},
	} {
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, nestedValue(tc.value, tc.number)))
		if ok || !namesInHelmError(out, tc.value) || !strings.Contains(out, "maximum") {
			t.Errorf("%s: %s: ok=%v, want the schema to refuse it for its maximum, naming %s:\n%s", tc.value, tc.number, ok, tc.value, out)
		}
	}
	for name, tc := range map[string]struct {
		values, want string
	}{
		"a fraction past a million":           {"replicaCount: 2000000.5\n", "replicaCount"},
		"a limit of workers as long digits":   {"server:\n  pythonMaxWorkers: \"99999999999999999999\"\n", "server.pythonMaxWorkers"},
		"a limit of trips one past as digits": {"server:\n  probeMaxConcurrent: \"2147483648\"\n", "server.probeMaxConcurrent"},
		"a limit of trips read as octal":      {"server:\n  probeMaxConcurrent: \"010\"\n", "server.probeMaxConcurrent"},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, tc.values))
			if ok || !namesInHelmError(out, tc.want) {
				t.Fatalf("ok=%v, want an error naming %q:\n%s", ok, tc.want, out)
			}
		})
	}
	// The largest count and zero, as strings of digits, are rendered as they
	// are written.
	out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, "server:\n  probeMaxConcurrent: \"2147483647\"\n  pythonMaxWorkers: \"0\"\n"))
	if !ok || !strings.Contains(out, `"--probe.max-concurrent=2147483647"`) || !strings.Contains(out, `"--python.max-workers=0"`) {
		t.Errorf("count flags of \"2147483647\" and \"0\": ok=%v, want each rendered as written:\n%s", ok, out)
	}
}

// The templates decide what a whole number is by the value, as the values
// schema does, and say what it may be: with the schema skipped a fraction
// was rendered as a fraction, or cut to the whole number below it, a number
// past the range was printed with an exponent, and one past what a template
// holds as a whole number became another number altogether. The message
// writes the number out, and a number is refused by the range of its own
// value: a port by a port's, a count that may not be 0 by 1.
func TestChartTemplatesRefuseWhatIsNoWholeNumberByItsValue(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	const count = " must be a whole number from 0 to 2147483647"
	const flag = count + ", 0 for no limit, or empty to keep the exporter's default"
	for _, tc := range []struct {
		values, want string
	}{
		{"replicaCount: 1.5", "replicaCount 1.5" + count},
		{"replicaCount: 2000000.5", "replicaCount 2000000.5" + count},
		{"replicaCount: 2147483648", "replicaCount 2147483648" + count},
		{"replicaCount: 3e9", "replicaCount 3000000000" + count},
		{"replicaCount: 1e30", "replicaCount 1e+30" + count},
		{"replicaCount: 100000000000000000000", "replicaCount 100000000000000000000" + count},
		{"replicaCount: -1", "replicaCount -1" + count},
		{"replicaCount: many", "replicaCount many" + count},
		{"terminationGracePeriodSeconds: 2000000.5", "terminationGracePeriodSeconds 2000000.5" + count},
		{"terminationGracePeriodSeconds: 1e30", "terminationGracePeriodSeconds 1e+30" + count},
		{"service: {port: 2000000}", "service.port 2000000 must be a whole number from 1 to 65535"},
		{"ingress: {enabled: true, hosts: [{host: h, paths: [{path: /, pathType: Prefix}]}]}\nservice: {port: 65536}", "service.port 65536 must be a whole number from 1 to 65535"},
		{"autoscaling: {enabled: true, minReplicas: 0}", "autoscaling.minReplicas 0 must be a whole number from 1 to 2147483647"},
		{"autoscaling: {enabled: true, targetCPUUtilizationPercentage: 3000000000}", "autoscaling.targetCPUUtilizationPercentage 3000000000 must be a whole number from 1 to 2147483647"},
		{"podDisruptionBudget: {enabled: true, minAvailable: 1.5}", "podDisruptionBudget.minAvailable 1.5" + count},
		{"server: {probeMaxConcurrent: 3000000000}", `server.probeMaxConcurrent "3000000000"` + flag},
		{"server: {probeMaxConcurrent: 1e30}", `server.probeMaxConcurrent "1e+30"` + flag},
		{"server: {pythonMaxWorkers: 1.5}", `server.pythonMaxWorkers "1.5"` + flag},
		{`server: {pythonMaxWorkers: "2147483648"}`, `server.pythonMaxWorkers "2147483648"` + flag},
		{`server: {probeMaxConcurrent: "010"}`, `server.probeMaxConcurrent "010"` + flag},
		// A number that is no value of the kind is named as the number it
		// is: a monitor's port given as its number, in a monitors entry, and
		// the garbage collector's target.
		{"monitors: [{name: apps, enabled: true, type: service, collector: example, port: 2000000}]", `monitors entry "apps" has port 2000000, a port number; a monitor of type service takes the name of a Service port`},
		{"monitors: [{name: apps, enabled: true, type: pod, collector: example, port: 2e6}]", `monitors entry "apps" has port 2000000, a port number; a monitor of type pod takes the name of a container port`},
		{"goGC: {percent: 2000000}", `goGC.percent "2000000" must be a whole number from 1 to 10000`},
	} {
		out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, tc.values+"\n"))
		if ok || !strings.Contains(out, tc.want) {
			t.Errorf("%s: ok=%v, want an error saying %q:\n%s", tc.values, ok, tc.want, out)
		}
	}
	// What the schema takes renders with the schema skipped as with it.
	deployment, scaling := wholeNumberLines("2147483647")
	for values, lines := range map[string]map[string]int{wholeNumberValues("2147483647"): deployment, scalingNumberValues("2147483647e0"): scaling} {
		out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, values))
		if !ok {
			t.Fatalf("rendering failed with the schema skipped:\n%s", out)
		}
		for line, times := range lines {
			if got := strings.Count(out, line); got != times {
				t.Errorf("with the schema skipped %q is rendered %d times, want %d", line, got, times)
			}
		}
	}
}

// A grace period too short for the shutdown is refused with the seconds
// written out, where a million of them was named 1e+06.
func TestChartNamesAShortGracePeriodAsTheWholeNumberItIs(t *testing.T) {
	helm := requireHelm(t)
	out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, "terminationGracePeriodSeconds: 1000000\nserver:\n  shutdownDelay: 5s\n  shutdownTimeout: 300h\n"))
	if want := "terminationGracePeriodSeconds 1000000 is shorter than the 1080015 seconds a stopping pod needs"; ok || !strings.Contains(out, want) {
		t.Errorf("ok=%v, want an error saying %q:\n%s", ok, want, out)
	}
	out, ok = helmTemplate(t, helm, chartDir, "-f", valuesFile(t, "terminationGracePeriodSeconds: 1080015\nserver:\n  shutdownDelay: 5s\n  shutdownTimeout: 300h\n"))
	if !ok || !strings.Contains(out, "\n      terminationGracePeriodSeconds: 1080015\n") {
		t.Errorf("a grace period of exactly what the shutdown needs: ok=%v, want it rendered:\n%s", ok, out)
	}
}

// The notes compare a budget's minAvailable with the replica count as the
// numbers they are: a count of a million or more from a values file was read
// as no number, so a budget of every replica went without its warning.
func TestChartNotesReadALargeBudgetAsTheWholeNumberItIs(t *testing.T) {
	helm := requireHelm(t)
	dir := chartWithNotesAsAManifest(t)
	for values, warned := range map[string]bool{
		"replicaCount: 3\npodDisruptionBudget:\n  enabled: true\n  minAvailable: 2000000\n":       true,
		"replicaCount: 2000000\npodDisruptionBudget:\n  enabled: true\n  minAvailable: 2e6\n":     true,
		"replicaCount: 2000001\npodDisruptionBudget:\n  enabled: true\n  minAvailable: 2000000\n": false,
	} {
		out, ok := helmTemplate(t, helm, dir, "-f", valuesFile(t, values), "--show-only", "templates/notes.yaml")
		if !ok {
			t.Fatalf("rendering failed:\n%s", out)
		}
		if got := strings.Contains(out, "WARNING: podDisruptionBudget.minAvailable is"); got != warned {
			t.Errorf("%q: warned %v, want %v:\n%s", values, got, warned, out)
		}
		if warned && !strings.Contains(out, "podDisruptionBudget.minAvailable is 2000000 with replicaCount") {
			t.Errorf("%q: the warning does not write the budget out as 2000000:\n%s", values, out)
		}
	}
}

// What the fix leaves alone, shown against the way a number was printed
// before it: a chart of the helpers alone prints each number of a table as a
// template does by itself, which is how the chart printed it, and through
// the helpers. Below a million, where a template prints no exponent, the
// two are the same text for every whole number, given as a values file gives
// it and as --set does, so no render of such a number changed; and what is
// no floating-point number — a string, a boolean, an integer — goes through
// the text helper as it prints. A fraction a template prints without an
// exponent is the same text too.
func TestTheWholeNumberHelpersPrintASmallNumberAsATemplateDoes(t *testing.T) {
	helm := requireHelm(t)
	dir := filepath.Join(t.TempDir(), "numbers")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	const table = `kind: Table
whole:
{{- range .Values.whole }}
  - [{{ printf "%v" . | quote }}, {{ toString . | quote }}, {{ include "prometheus-universal-exporter.text" . | quote }}, {{ include "prometheus-universal-exporter.wholeNumber" (list "number" . -999999 999999) | quote }}]
{{- end }}
{{- range .Values.integers }}
  - [{{ printf "%v" . | quote }}, {{ toString . | quote }}, {{ include "prometheus-universal-exporter.text" . | quote }}, {{ include "prometheus-universal-exporter.wholeNumber" (list "number" . -999999 999999) | quote }}]
{{- end }}
other:
{{- range .Values.other }}
  - [{{ printf "%v" . | quote }}, {{ toString . | quote }}, {{ include "prometheus-universal-exporter.text" . | quote }}]
{{- end }}
`
	for name, content := range map[string]string{
		"Chart.yaml":             "apiVersion: v2\nname: numbers\nversion: 0.1.0\n",
		"templates/_helpers.tpl": readChartFile(t, "templates/_helpers.tpl"),
		"templates/table.yaml":   table,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Every whole number up to a thousand either side of zero, the powers
	// of ten and their neighbours, the last below a million, and generated
	// ones; some written with a decimal point or an exponent.
	var whole []int
	for n := -1000; n <= 1000; n++ {
		whole = append(whole, n)
	}
	for power := 1000; power <= 100000; power *= 10 {
		whole = append(whole, power-1, power, power+1, -power, 1-power)
	}
	whole = append(whole, 65535, 65532, 999998, 999999, -999999)
	random := rand.New(rand.NewPCG(41, 43))
	for range 1000 {
		whole = append(whole, random.IntN(1999999)-999999)
	}
	var values strings.Builder
	values.WriteString("whole:\n")
	for i, n := range whole {
		switch i % 3 {
		case 0:
			fmt.Fprintf(&values, "  - %d\n", n)
		case 1:
			fmt.Fprintf(&values, "  - %d.0\n", n)
		default:
			fmt.Fprintf(&values, "  - %de0\n", n)
		}
	}
	other := []string{`""`, `"abc"`, `"50%"`, `"007"`, `"2e6"`, `"2000000"`, `"off"`, `"1.5"`, "true", "false", "0.5", "1.5", "0.8", "-2.25", "0.001", "123456.75", "999999.5"}
	values.WriteString("other:\n")
	for _, value := range other {
		fmt.Fprintf(&values, "  - %s\n", value)
	}
	integers := []string{"0", "1", "-1", "7", "80", "8080", "65535", "999999", "-999999", "123456"}
	out, ok := helmTemplate(t, helm, dir, "-f", valuesFile(t, values.String()), "--set", "integers={"+strings.Join(integers, ",")+"}")
	if !ok {
		t.Fatalf("rendering failed:\n%s", out)
	}
	var rendered struct {
		Whole [][]string `yaml:"whole"`
		Other [][]string `yaml:"other"`
	}
	if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("the table does not parse: %v\n%s", err, out)
	}
	if len(rendered.Whole) != len(whole)+len(integers) || len(rendered.Other) != len(other) {
		t.Fatalf("the table has %d whole numbers and %d other values, want %d and %d", len(rendered.Whole), len(rendered.Other), len(whole)+len(integers), len(other))
	}
	for i, row := range rendered.Whole {
		want := ""
		if i < len(whole) {
			want = strconv.Itoa(whole[i])
		} else {
			want = integers[i-len(whole)]
		}
		for _, text := range row {
			if text != want {
				t.Errorf("the whole number %s is printed %q by a template, by toString, by the text helper and by the wholeNumber helper; all four must be %s", want, row, want)
				break
			}
		}
	}
	for i, row := range rendered.Other {
		if row[0] != row[1] || row[1] != row[2] {
			t.Errorf("%s is printed %q by a template, by toString and by the text helper; the three must be the same", other[i], row)
		}
	}
}
