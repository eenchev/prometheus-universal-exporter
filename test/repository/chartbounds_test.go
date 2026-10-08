package repository

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Three kinds of value got past the chart to fail only when Kubernetes was
// handed the Deployment or the PodDisruptionBudget, or failed rendering
// though they were valid: a probe's timing or a rolling update's count of
// 1e30, which the values schema took and toYaml passed through; a budget's
// count given as a string, such as "010" or "200%", which the schema and the
// template took as any digits; and a goMemLimit.ratio below 0.0001, which the
// schema and the exporter take and the template printed as 1e-05 and then
// refused. The tests below hold the chart to refusing the first two where
// Kubernetes would, by the value and naming it, and to rendering the third
// written out.

// budgetCountPattern is what a disruption budget's count given as a string
// may be, as the values schema and the budgetCount helper both check it: a
// whole number from 0 to 2147483647, or a percentage from 0% to 100%, each
// with no zero before another digit.
const budgetCountPattern = `^(0|[1-9][0-9]{0,8}|1[0-9]{9}|20[0-9]{8}|21[0-3][0-9]{7}|214[0-6][0-9]{6}|2147[0-3][0-9]{5}|21474[0-7][0-9]{4}|214748[0-2][0-9]{3}|2147483[0-5][0-9]{2}|21474836[0-3][0-9]|214748364[0-7]|(0|[1-9][0-9]?|100)%)$`

// schemaRefusal reports whether helm's output refuses the value at path, a
// dotted path of the values, on a line that says why with word, such as
// maximum or pattern.
func schemaRefusal(out, path, word string) bool {
	at := "'/" + strings.ReplaceAll(path, ".", "/") + "'"
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, at) && strings.Contains(line, word) {
			return true
		}
	}
	return false
}

// The probes' timings and the rolling update's counts are handed to toYaml,
// so the chart renders whatever the schema takes, and the schema took any
// integer: periodSeconds: 1e30 reached Kubernetes, which refused the
// Deployment when it was applied. The schema now bounds each as Kubernetes
// validates it — an int32, from 0 for initialDelaySeconds and from 1 for the
// other timings, a liveness probe's successThreshold of 1 alone, and a count
// a whole number or a percentage, never a string of digits alone, which
// toYaml renders as a string and Kubernetes refuses, and at most 100% for
// maxUnavailable — refusing a value past a bound with a message naming it,
// and takes every value at a bound, which renders as written.
func TestTheSchemaBoundsTheProbesAndTheRollingUpdateAsKubernetesDoes(t *testing.T) {
	helm := requireHelm(t)
	type refusal struct{ path, word string }
	for _, tc := range []struct {
		values string
		want   []refusal
	}{
		{
			"livenessProbe: {initialDelaySeconds: 1e30, periodSeconds: 1e30, timeoutSeconds: 1e30, successThreshold: 2, failureThreshold: 1e30, terminationGracePeriodSeconds: 1e30}\n" +
				"readinessProbe: {initialDelaySeconds: 1e30, periodSeconds: 1e30, timeoutSeconds: 1e30, successThreshold: 1e30, failureThreshold: 1e30}\n" +
				"strategy: {rollingUpdate: {maxSurge: 2147483648, maxUnavailable: 2147483648}}\n",
			[]refusal{
				{"livenessProbe.initialDelaySeconds", "maximum"}, {"livenessProbe.periodSeconds", "maximum"}, {"livenessProbe.timeoutSeconds", "maximum"},
				{"livenessProbe.successThreshold", "maximum"}, {"livenessProbe.failureThreshold", "maximum"}, {"livenessProbe.terminationGracePeriodSeconds", "maximum"},
				{"readinessProbe.initialDelaySeconds", "maximum"}, {"readinessProbe.periodSeconds", "maximum"}, {"readinessProbe.timeoutSeconds", "maximum"},
				{"readinessProbe.successThreshold", "maximum"}, {"readinessProbe.failureThreshold", "maximum"},
				{"strategy.rollingUpdate.maxSurge", "maximum"}, {"strategy.rollingUpdate.maxUnavailable", "maximum"},
			},
		},
		{
			"livenessProbe: {periodSeconds: 2147483648, timeoutSeconds: 2.5}\nreadinessProbe: {failureThreshold: 2147483648}\nstrategy: {rollingUpdate: {maxSurge: -1, maxUnavailable: 2.5}}\n",
			[]refusal{{"livenessProbe.periodSeconds", "maximum"}, {"livenessProbe.timeoutSeconds", "integer"}, {"readinessProbe.failureThreshold", "maximum"}, {"strategy.rollingUpdate.maxSurge", "minimum"}, {"strategy.rollingUpdate.maxUnavailable", "integer"}},
		},
		{
			`strategy: {rollingUpdate: {maxSurge: "3", maxUnavailable: "101%"}}` + "\n",
			[]refusal{{"strategy.rollingUpdate.maxSurge", "pattern"}, {"strategy.rollingUpdate.maxUnavailable", "pattern"}},
		},
		{
			`strategy: {rollingUpdate: {maxSurge: "010%", maxUnavailable: "1"}}` + "\n",
			[]refusal{{"strategy.rollingUpdate.maxSurge", "pattern"}, {"strategy.rollingUpdate.maxUnavailable", "pattern"}},
		},
	} {
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, tc.values))
		if ok {
			t.Errorf("%s: rendered, want the schema to refuse it", tc.values)
			continue
		}
		for _, want := range tc.want {
			if !schemaRefusal(out, want.path, want.word) {
				t.Errorf("%s: the schema does not refuse %s for its %s:\n%s", tc.values, want.path, want.word, out)
			}
		}
	}

	out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, `livenessProbe: {initialDelaySeconds: 2147483647, periodSeconds: 2147483647, timeoutSeconds: 2147483647, successThreshold: 1, failureThreshold: 2147483647, terminationGracePeriodSeconds: 2147483647}
readinessProbe: {initialDelaySeconds: 0, periodSeconds: 1, timeoutSeconds: 1, successThreshold: 3, failureThreshold: 1}
strategy: {rollingUpdate: {maxSurge: "200%", maxUnavailable: "100%"}}
`))
	if !ok {
		t.Fatalf("every timing and count at a bound: rendering failed:\n%s", out)
	}
	for _, want := range []string{
		"\n            initialDelaySeconds: 2147483647\n            periodSeconds: 2147483647\n            successThreshold: 1\n            terminationGracePeriodSeconds: 2147483647\n            timeoutSeconds: 2147483647\n",
		"\n            failureThreshold: 1\n            initialDelaySeconds: 0\n            periodSeconds: 1\n            successThreshold: 3\n            timeoutSeconds: 1\n",
		"\n      maxSurge: 200%\n      maxUnavailable: 100%\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the render does not hold %q:\n%s", want, out)
		}
	}
	out, ok = helmTemplate(t, helm, chartDir, "-f", valuesFile(t, "strategy: {rollingUpdate: {maxSurge: 2147483647, maxUnavailable: \"0%\"}}\n"))
	if !ok || !strings.Contains(out, "\n      maxSurge: 2147483647\n      maxUnavailable: 0%\n") {
		t.Errorf("a surge of 2147483647 and 0%% unavailable: ok=%v, want them rendered as written:\n%s", ok, out)
	}
}

// A budget's count given as a string is rendered bare, so a string of digits
// becomes the number, and one with a zero first YAML's octal number, "010"
// as eight; a count past 2147483647 and a percentage past 100% Kubernetes
// refuses on a budget. The schema took any digits with an optional %, and
// the template any string. Both now refuse what is none of a whole number
// from 0 to 2147483647 and a percentage from 0% to 100%, written with no
// zero before another digit, naming the value; what they take renders bare,
// as it did. A number keeps the last round's rule.
func TestChartHoldsADisruptionBudgetStringToWhatKubernetesTakes(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	refused := []string{"3000000000", "2147483648", "010", "00", "200%", "101%", "050%", "-1", "1.5", "1e3", "1 ", ""}
	// The schema reports every value it refuses, so the two keys are
	// looked at in one render.
	for i := 0; i < len(refused); i += 2 {
		values := fmt.Sprintf("podDisruptionBudget: {enabled: true, minAvailable: %q, maxUnavailable: %q}\n", refused[i], refused[i+1])
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, values))
		if ok || !schemaRefusal(out, "podDisruptionBudget.minAvailable", "pattern") || !schemaRefusal(out, "podDisruptionBudget.maxUnavailable", "pattern") {
			t.Errorf("%s: ok=%v, want the schema to refuse both for its pattern:\n%s", values, ok, out)
		}
	}
	const rule = " must be a whole number from 0 to 2147483647, or a percentage from 0% to 100%, written with no zero before another digit"
	for i, value := range refused {
		key := []string{"minAvailable", "maxUnavailable"}[i%2]
		values := fmt.Sprintf("podDisruptionBudget: {enabled: true, %s: %q}\n", key, value)
		out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, values))
		if want := fmt.Sprintf("podDisruptionBudget.%s %q%s", key, value, rule); ok || !strings.Contains(out, want) {
			t.Errorf("%s with the schema skipped: ok=%v, want an error saying %q:\n%s", values, ok, want, out)
		}
	}
	for _, skip := range []bool{false, true} {
		for _, tc := range []struct{ key, value string }{
			{"minAvailable", "2147483647"}, {"minAvailable", "0"}, {"maxUnavailable", "0%"}, {"maxUnavailable", "100%"},
		} {
			args := []string{"-f", valuesFile(t, fmt.Sprintf("podDisruptionBudget: {enabled: true, %s: %q}\n", tc.key, tc.value))}
			if skip {
				args = append(args, "--skip-schema-validation")
			}
			out, ok := helmTemplate(t, helm, chartDir, args...)
			if want := fmt.Sprintf("\n  %s: %s\n", tc.key, tc.value); !ok || !strings.Contains(out, want) {
				t.Errorf("%s %q (schema skipped: %v): ok=%v, want %q rendered:\n%s", tc.key, tc.value, skip, ok, want, out)
			}
		}
	}
}

// The budget's pattern is one: the schema's for both keys is the helper's,
// and it takes exactly the strings of digits that are 2147483647 or less
// and the percentages that are 100 or less, written as Go writes the number,
// here compared with the number each string is.
func TestTheDisruptionBudgetPatternTakesExactlyTheCountsKubernetesTakes(t *testing.T) {
	if !strings.Contains(readChartFile(t, "templates/_helpers.tpl"), `regexMatch "`+budgetCountPattern+`" (index . 1)`) {
		t.Errorf("the budgetCount helper does not check a string against %s", budgetCountPattern)
	}
	for _, key := range []string{"podDisruptionBudget.minAvailable", "podDisruptionBudget.maxUnavailable"} {
		if got := schemaAt(t, key)["pattern"]; got != budgetCountPattern {
			t.Errorf("the schema's pattern for %s is %v, want the helper's, %s", key, got, budgetCountPattern)
		}
	}
	pattern := regexp.MustCompile(budgetCountPattern)
	largest := big.NewInt(largestWholeNumber)
	check := func(text string) {
		digits, percent := strings.CutSuffix(text, "%")
		most := largest
		if percent {
			most = big.NewInt(100)
		}
		number, ok := new(big.Int).SetString(digits, 10)
		want := ok && number.Sign() >= 0 && number.Cmp(most) <= 0 && number.String() == digits
		if got := pattern.MatchString(text); got != want {
			t.Errorf("the budget pattern takes %q: %v, want %v", text, got, want)
		}
	}
	const edge = "2147483647"
	for at := range edge {
		for digit := byte('0'); digit <= '9'; digit++ {
			changed := []byte(edge)
			changed[at] = digit
			for _, zeros := range []string{"", "0"} {
				check(zeros + string(changed))
				check(zeros + string(changed[:at+1]))
				check(zeros + string(changed) + "0")
				check(zeros + string(changed[:at+1]) + "%")
			}
		}
	}
	for n := range 1001 {
		check(strconv.Itoa(n))
		check(strconv.Itoa(n) + "%")
		check("0" + strconv.Itoa(n) + "%")
	}
	for _, text := range []string{"", "%", "%%", "50%%", "-1", "-1%", "+1", "1.5", "1.5%", "1e3", " 1", "1 ", "1 %", "0x10", "ten", "100%0"} {
		check(text)
	}
	random := rand.New(rand.NewPCG(44, 45))
	for range 4000 {
		check(strconv.FormatUint(random.Uint64N(1e12), 10))
		check(strconv.FormatUint(largestWholeNumber-2000+random.Uint64N(4000), 10))
	}
}

// A goMemLimit.ratio below 0.0001 passed the values schema and the
// exporter's flag, which reads it with strconv.ParseFloat, and failed
// rendering as 1e-05, since the template printed the floating-point number
// with an exponent and then checked the text. The number is now written out
// with the digits toJson gives it, however small, and what is no ratio is
// refused by the schema and, with it skipped, by the templates. A ratio
// below 0.1 is now refused too (the test after this one); what renders is
// from 0.1 to 1.
func TestChartRendersASmallMemoryLimitRatioWrittenOut(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	for written, rendered := range map[string]string{
		"1":         "1",
		"1.0":       "1",
		"0.9999999": "0.9999999",
		"1e-1":      "0.1",
		`"0.5"`:     "0.5",
	} {
		for _, skip := range []bool{false, true} {
			args := []string{"-f", valuesFile(t, "goMemLimit:\n  ratio: "+written+"\n")}
			if skip {
				args = append(args, "--skip-schema-validation")
			}
			out, ok := helmTemplate(t, helm, chartDir, args...)
			if want := `"--runtime.memory-limit-ratio=` + rendered + `"`; !ok || !strings.Contains(out, want) {
				t.Errorf("ratio %s (schema skipped: %v): ok=%v, want %s:\n%s", written, skip, ok, want, out)
			}
		}
	}
	for _, written := range []string{"0", "1.5", `"1e-5"`, "-1e-5"} {
		values := "goMemLimit:\n  ratio: " + written + "\n"
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, values))
		if ok || !namesInHelmError(out, "goMemLimit.ratio") {
			t.Errorf("ratio %s: ok=%v, want the schema to refuse it naming goMemLimit.ratio:\n%s", written, ok, out)
		}
		out, ok = helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, values))
		if ok || !strings.Contains(out, " must be more than 0 and at most 1, such as 0.8") || !strings.Contains(out, "goMemLimit.ratio ") {
			t.Errorf("ratio %s with the schema skipped: ok=%v, want the templates to refuse it:\n%s", written, ok, out)
		}
	}
}

// A goMemLimit.ratio above 0 and below 0.1 sets a Go memory limit of next to
// nothing, so the Go runtime would collect garbage all the time, and the
// exporter refuses it. The values schema refuses it naming the key and,
// with it skipped, the templates with a message saying the floor and the
// value written out; 0.1 and above render as before.
func TestChartRefusesAMemoryLimitRatioBelowATenth(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	for written, rendered := range map[string]string{
		"0.1":        "0.1",
		"0.10000001": "0.10000001",
		"0.1000":     "0.1",
		`".1"`:       ".1",
		"1":          "1",
	} {
		for _, skip := range []bool{false, true} {
			args := []string{"-f", valuesFile(t, "goMemLimit:\n  ratio: "+written+"\n")}
			if skip {
				args = append(args, "--skip-schema-validation")
			}
			out, ok := helmTemplate(t, helm, chartDir, args...)
			if want := `"--runtime.memory-limit-ratio=` + rendered + `"`; !ok || !strings.Contains(out, want) {
				t.Errorf("ratio %s (schema skipped: %v): ok=%v, want %s:\n%s", written, skip, ok, want, out)
			}
		}
	}
	for written, shown := range map[string]string{
		"0.0999999": "0.0999999",
		"0.0001":    "0.0001",
		"1e-5":      "0.00001",
		"0.00001":   "0.00001",
		"1e-9":      "0.000000001",
		"1.5e-7":    "0.00000015",
		"1e-300":    "0." + strings.Repeat("0", 299) + "1",
		`"0.05"`:    "0.05",
	} {
		values := "goMemLimit:\n  ratio: " + written + "\n"
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, values))
		if ok || !namesInHelmError(out, "goMemLimit.ratio") {
			t.Errorf("ratio %s: ok=%v, want the schema to refuse it naming goMemLimit.ratio:\n%s", written, ok, out)
		}
		out, ok = helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, values))
		if want := "goMemLimit.ratio " + shown + " must be at least 0.1, since below it the Go memory limit leaves the heap almost nothing"; ok || !strings.Contains(out, want) {
			t.Errorf("ratio %s with the schema skipped: ok=%v, want %q:\n%s", written, ok, want, out)
		}
	}
}

// Kubernetes refuses a rolling update whose maxSurge and maxUnavailable are
// both 0, as a number or 0%, when the Deployment is applied. The chart now
// refuses it at rendering, by the schema at strategy and, with it skipped,
// by the templates naming both keys, and only where both are rendered: a
// count left out takes Kubernetes' default, 25%, and Recreate renders no
// rollingUpdate. The repository's own schema validator gives helm's verdict
// on each strategy as helm merges it over the defaults.
func TestChartRefusesARollingUpdateOfNoSurgeAndNoUnavailable(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	schema := schemaAt(t, "strategy")
	for _, tc := range []struct {
		values, merged string
		refused        bool
	}{
		{"{rollingUpdate: {maxSurge: 0}}", `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 0, "maxUnavailable": 0}}`, true},
		{`{rollingUpdate: {maxSurge: "0%"}}`, `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": "0%", "maxUnavailable": 0}}`, true},
		{`{rollingUpdate: {maxSurge: "0%", maxUnavailable: "0%"}}`, `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": "0%", "maxUnavailable": "0%"}}`, true},
		{`{rollingUpdate: {maxSurge: 0, maxUnavailable: "0%"}}`, `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 0, "maxUnavailable": "0%"}}`, true},
		{"{type: null, rollingUpdate: {maxSurge: 0}}", `{"rollingUpdate": {"maxSurge": 0, "maxUnavailable": 0}}`, true},
		{"{rollingUpdate: {maxSurge: null}}", `{"type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 0}}`, false},
		{"{rollingUpdate: {maxSurge: 0, maxUnavailable: null}}", `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 0}}`, false},
		{"{rollingUpdate: {maxSurge: 0, maxUnavailable: 1}}", `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 0, "maxUnavailable": 1}}`, false},
		{`{rollingUpdate: {maxSurge: "1%", maxUnavailable: 0}}`, `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": "1%", "maxUnavailable": 0}}`, false},
		{"{type: Recreate, rollingUpdate: {maxSurge: 0}}", `{"type": "Recreate", "rollingUpdate": {"maxSurge": 0, "maxUnavailable": 0}}`, false},
		{"{}", `{"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 1, "maxUnavailable": 0}}`, false},
	} {
		values := valuesFile(t, "strategy: "+tc.values+"\n")
		out, ok := helmTemplate(t, helm, chartDir, "-f", values)
		if ok == tc.refused || (tc.refused && !schemaRefusal(out, "strategy", "not")) {
			t.Errorf("%s: ok=%v, want refused %v by the schema at strategy:\n%s", tc.values, ok, tc.refused, out)
		}
		skipped, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", values)
		named := strings.Contains(skipped, "strategy.rollingUpdate.maxSurge ") && strings.Contains(skipped, " and strategy.rollingUpdate.maxUnavailable ") && strings.Contains(skipped, " are both 0, which Kubernetes refuses")
		if ok == tc.refused || tc.refused != named {
			t.Errorf("%s with the schema skipped: ok=%v, want refused %v naming both keys:\n%s", tc.values, ok, tc.refused, skipped)
		}
		if !tc.refused && out != skipped {
			t.Errorf("%s: the render differs with the schema skipped", tc.values)
		}
		var merged any
		if err := json.Unmarshal([]byte(tc.merged), &merged); err != nil {
			t.Fatal(err)
		}
		if errs := validateAgainstSchema(schema, merged); (len(errs) != 0) != tc.refused {
			t.Errorf("%s: the repository's validator says %v, want refused %v", tc.merged, errs, tc.refused)
		}
	}
	out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, "strategy: {rollingUpdate: {maxSurge: null}}\n"))
	if !ok || !strings.Contains(out, "\n    rollingUpdate:\n      maxUnavailable: 0\n  selector:") {
		t.Errorf("a surge left out: ok=%v, want maxUnavailable alone rendered:\n%s", ok, out)
	}
}

// oldMemoryLimitRatio is the memoryLimitRatio helper as it was before a
// number was written out, the oracle of the test below.
const oldMemoryLimitRatio = `{{- define "old.memoryLimitRatio" -}}
{{- if .Values.goMemLimit.enabled -}}
{{- $ratio := .Values.goMemLimit.ratio | toString -}}
{{- if not (regexMatch "^(0?\\.[0-9]*[1-9][0-9]*|1(\\.0*)?)$" $ratio) -}}
{{- fail (printf "goMemLimit.ratio %s must be more than 0 and at most 1, such as 0.8" $ratio) -}}
{{- end -}}
{{- $ratio -}}
{{- end -}}
{{- end }}
`

// What the fix leaves alone, shown against the helper as it was: a chart of
// the helpers alone renders every ratio of a table that the old helper
// rendered — a number printed without an exponent, given as a values file
// and as --set give it, and a string — through both, and the two are the
// same text, so no render of such a ratio changed. The table holds every
// ratio of up to four decimals from 0.1, the floor, numbers of many digits,
// written with an exponent and without, and generated ones.
func TestTheMemoryLimitRatioRendersAsBeforeWhereItRendered(t *testing.T) {
	helm := requireHelm(t)
	dir := filepath.Join(t.TempDir(), "ratios")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	const table = `kind: Table
ratios:
{{- range .Values.ratios }}
{{- $context := dict "Values" (dict "goMemLimit" (dict "enabled" true "ratio" .)) }}
  - [{{ include "old.memoryLimitRatio" $context | quote }}, {{ include "prometheus-universal-exporter.memoryLimitRatio" $context | quote }}]
{{- end }}
`
	for name, content := range map[string]string{
		"Chart.yaml":             "apiVersion: v2\nname: ratios\nversion: 0.1.0\n",
		"templates/_helpers.tpl": readChartFile(t, "templates/_helpers.tpl"),
		"templates/_old.tpl":     oldMemoryLimitRatio,
		"templates/table.yaml":   table,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var ratios []string
	for n := 1000; n <= 10000; n++ {
		ratios = append(ratios, strconv.FormatFloat(float64(n)/10000, 'f', -1, 64))
	}
	random := rand.New(rand.NewPCG(46, 47))
	for range 2000 {
		ratio := 0.1 + random.Float64()*0.9
		ratios = append(ratios, strconv.FormatFloat(ratio, 'f', -1, 64), strconv.FormatFloat(ratio, 'e', -1, 64))
	}
	ratios = append(ratios, "1e0", "1e-1", "10e-2", "0.10000001", "1.0", "1.00", ".5", "0.12345678901234567", "0.99999999999999999", `"0.8"`, `".5"`, `"1."`, `"1.000"`, `"0.1"`, `".1"`)
	var values strings.Builder
	values.WriteString("ratios:\n")
	for _, ratio := range ratios {
		fmt.Fprintf(&values, "  - %s\n", ratio)
	}
	set := []string{"0.8", "1", "0.5", "0.1", "0.123"}
	for rows, args := range map[int][]string{
		len(ratios): {"-f", valuesFile(t, values.String())},
		len(set):    {"--set", "ratios={" + strings.Join(set, ",") + "}"},
	} {
		out, ok := helmTemplate(t, helm, dir, args...)
		if !ok {
			t.Fatalf("rendering failed:\n%s", out)
		}
		var rendered struct {
			Ratios [][]string `yaml:"ratios"`
		}
		if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
			t.Fatalf("the table does not parse: %v\n%s", err, out)
		}
		if len(rendered.Ratios) != rows {
			t.Fatalf("the table has %d ratios, want %d", len(rendered.Ratios), rows)
		}
		for _, row := range rendered.Ratios {
			if row[0] != row[1] {
				t.Errorf("the old helper renders %q and the new one %q; they must be the same", row[0], row[1])
			}
		}
	}
}
