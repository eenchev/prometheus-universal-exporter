package repository

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Go garbage collector's target is a chart value, goGC.percent, rendered
// as the GOGC environment variable of the exporter container, which the Go
// runtime reads itself. The render tests need helm, as those of
// chartrender_test.go do; the first test reads the chart's files and runs
// without it.

// goGCPercentPattern is what a goGC.percent that is set may be, written as
// the templates check it: a whole number from 1 to 10000, or off.
const goGCPercentPattern = `^([1-9][0-9]{0,3}|10000|off)$`

// goGC.percent defaults to empty, so a chart nobody set it in renders what it
// rendered before, and the Deployment takes GOGC from the helper that checks
// the value. The values schema and that helper accept the same strings: one
// pattern, which the schema writes with an alternative for the empty default.
func TestTheGarbageCollectorTargetIsAValue(t *testing.T) {
	if !strings.Contains(readChartFile(t, "values.yaml"), "\ngoGC:\n  percent: \"\"\n") {
		t.Error("goGC.percent must default to empty in values.yaml")
	}
	deployment := readChartFile(t, "templates/deployment.yaml")
	if !strings.Contains(deployment, `{{- $goGC := include "prometheus-universal-exporter.goGC" . }}`) {
		t.Error("the Deployment does not render GOGC from goGC.percent through its helper")
	}
	if !strings.Contains(readChartFile(t, "templates/_helpers.tpl"), `regexMatch "`+goGCPercentPattern+`" $text`) {
		t.Errorf("the goGC helper does not check the value against %s", goGCPercentPattern)
	}

	var schema struct {
		Properties struct {
			GoGC struct {
				AdditionalProperties *bool `json:"additionalProperties"`
				Properties           struct {
					Percent struct {
						Type    []string `json:"type"`
						Minimum *int     `json:"minimum"`
						Maximum *int     `json:"maximum"`
						Pattern string   `json:"pattern"`
					} `json:"percent"`
				} `json:"properties"`
			} `json:"goGC"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(readChartFile(t, "values.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	goGC := schema.Properties.GoGC
	if goGC.AdditionalProperties == nil || *goGC.AdditionalProperties {
		t.Error("the schema must refuse a key of goGC other than percent, or a misspelled one renders nothing")
	}
	percent := goGC.Properties.Percent
	if types := slices.Sorted(slices.Values(percent.Type)); !slices.Equal(types, []string{"integer", "null", "string"}) {
		t.Errorf("the schema takes goGC.percent as %v, want an integer, a string or null", percent.Type)
	}
	if percent.Minimum == nil || *percent.Minimum != 1 || percent.Maximum == nil || *percent.Maximum != 10000 {
		t.Error("the schema must bound a goGC.percent number from 1 to 10000")
	}
	if want := "^$|" + goGCPercentPattern; percent.Pattern != want {
		t.Errorf("the schema's pattern for goGC.percent is %s, want %s: empty, or what the helper accepts", percent.Pattern, want)
	}
}

// exporterEnvironment renders the chart with args and returns the env of the
// Deployment's exporter container as NAME=value, in the order rendered, with
// how many times the render names GOGC anywhere. A value that is not a
// quoted or plain string, as Kubernetes requires of an env value, fails the
// test; an entry with valueFrom has an empty value here.
func exporterEnvironment(t *testing.T, helm string, args ...string) (env []string, mentions int) {
	t.Helper()
	out, ok := helmTemplate(t, helm, chartDir, args...)
	if !ok {
		t.Fatalf("rendering failed:\n%s", out)
	}
	decoder := yaml.NewDecoder(strings.NewReader(out))
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			t.Fatalf("no Deployment is rendered:\n%s", out)
		}
		if err != nil {
			t.Fatalf("the rendered chart does not parse: %v\n%s", err, out)
		}
		if len(doc.Content) == 0 || path(doc.Content[0], "kind") == nil || path(doc.Content[0], "kind").Value != "Deployment" {
			continue
		}
		container := path(doc.Content[0], "spec", "template", "spec", "containers", "0")
		if name := path(container, "name"); name == nil || name.Value != "exporter" {
			t.Fatalf("the Deployment's first container is not the exporter:\n%s", out)
		}
		if list := path(container, "env"); list != nil {
			if list.Kind != yaml.SequenceNode || len(list.Content) == 0 {
				t.Fatalf("env is rendered and is not a list of entries:\n%s", out)
			}
			for _, entry := range list.Content {
				name, value := path(entry, "name"), path(entry, "value")
				if name == nil {
					t.Fatalf("an env entry has no name:\n%s", out)
				}
				text := ""
				if value != nil {
					if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
						t.Errorf("the value of %s is rendered as %s, not as a string, which Kubernetes refuses", name.Value, value.Tag)
					}
					text = value.Value
				}
				env = append(env, name.Value+"="+text)
			}
		}
		return env, strings.Count(out, "GOGC")
	}
}

// Empty, the default, renders no GOGC and no env of the chart's own. Set, a
// number or off, it renders GOGC once, as a string, first in the container's
// env, before what env holds and with envFrom untouched. off needs a Go
// memory limit in force, which the default values give it: goMemLimit is on
// and resources carries a memory limit; GOMEMLIMIT in env is the operator's
// own limit and counts as one. A number needs none. GOGC in env with no
// percent set is rendered as the user wrote it.
func TestChartRendersTheGarbageCollectorTarget(t *testing.T) {
	helm := requireHelm(t)
	quotedOff := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(quotedOff, []byte("goGC:\n  percent: \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wholeNumber := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(wholeNumber, []byte("goGC:\n  percent: 250\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want []string
	}{
		"the default":                 {nil, nil},
		"an empty percent":            {[]string{"--set", "goGC.percent="}, nil},
		"a null percent":              {[]string{"--set-json", "goGC.percent=null"}, nil},
		"a number":                    {[]string{"--set", "goGC.percent=400"}, []string{"GOGC=400"}},
		"a number in a values file":   {[]string{"-f", wholeNumber}, []string{"GOGC=250"}},
		"a number given as a string":  {[]string{"--set-string", "goGC.percent=400"}, []string{"GOGC=400"}},
		"the least number":            {[]string{"--set", "goGC.percent=1"}, []string{"GOGC=1"}},
		"the greatest number":         {[]string{"--set", "goGC.percent=10000"}, []string{"GOGC=10000"}},
		"off with the default limit":  {[]string{"--set", "goGC.percent=off"}, []string{"GOGC=off"}},
		"off quoted in a values file": {[]string{"-f", quotedOff}, []string{"GOGC=off"}},
		"off with another ratio":      {[]string{"--set", "goGC.percent=off", "--set", "goMemLimit.ratio=0.5", "--set", "resources.limits.memory=1Gi"}, []string{"GOGC=off"}},
		"off with the operator's limit": {[]string{"--set", "goGC.percent=off", "--set", "goMemLimit.enabled=false", "--set", "resources.limits.memory=null", "--set-json", `env=[{"name":"GOMEMLIMIT","value":"400MiB"}]`},
			[]string{"GOGC=off", "GOMEMLIMIT=400MiB"}},
		"off with the operator's limit from a Secret": {[]string{"--set", "goGC.percent=off", "--set", "resources.limits.memory=null", "--set-json", `env=[{"name":"GOMEMLIMIT","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}]`},
			[]string{"GOGC=off", "GOMEMLIMIT="}},
		"a number without a memory limit": {[]string{"--set", "goGC.percent=300", "--set", "resources.limits.memory=null"}, []string{"GOGC=300"}},
		"a number without the ratio":      {[]string{"--set", "goGC.percent=300", "--set", "goMemLimit.enabled=false"}, []string{"GOGC=300"}},
		"before the user's variables": {[]string{"--set", "goGC.percent=200", "--set-json", `env=[{"name":"HTTPS_PROXY","value":"http://proxy:3128"},{"name":"API_TOKEN","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}]`, "--set-json", `envFrom=[{"secretRef":{"name":"exporter-secrets"}}]`},
			[]string{"GOGC=200", "HTTPS_PROXY=http://proxy:3128", "API_TOKEN="}},
		"the user's own GOGC, with no percent": {[]string{"--set-json", `env=[{"name":"DEMO","value":"x"},{"name":"GOGC","value":"50"}]`}, []string{"DEMO=x", "GOGC=50"}},
	} {
		t.Run(name, func(t *testing.T) {
			env, mentions := exporterEnvironment(t, helm, tc.args...)
			if !slices.Equal(env, tc.want) {
				t.Errorf("the exporter container's env is %q, want %q", env, tc.want)
			}
			want := 0
			for _, entry := range tc.want {
				if strings.HasPrefix(entry, "GOGC=") {
					want++
				}
			}
			if mentions != want {
				t.Errorf("the render names GOGC %d times, want %d: once on the exporter container when it is set, and nowhere else", mentions, want)
			}
		})
	}

	// envFrom stays where it was, after env.
	out, ok := helmTemplate(t, helm, chartDir, "--set", "goGC.percent=200", "--set-json", `envFrom=[{"secretRef":{"name":"exporter-secrets"}}]`)
	if want := "          env:\n            - name: GOGC\n              value: \"200\"\n          envFrom:\n            - secretRef:\n                name: exporter-secrets\n          ports:\n"; !ok || !strings.Contains(out, want) {
		t.Errorf("ok=%v, want GOGC in env, quoted, and envFrom after it:\n%s", ok, out)
	}
}

// A render with the default values is the same text whether goGC is left
// alone, emptied or removed, so the value costs a release that does not use
// it nothing, and one with a percent differs in the Deployment's pod template,
// which is what rolls the pods: no annotation has to say so.
func TestChartWithoutAGarbageCollectorTargetRendersAsBefore(t *testing.T) {
	helm := requireHelm(t)
	plain, ok := helmTemplate(t, helm, chartDir)
	if !ok {
		t.Fatalf("rendering failed:\n%s", plain)
	}
	for _, args := range [][]string{{"--set", "goGC.percent="}, {"--set-json", "goGC=null"}, {"--set-json", "goGC={}"}} {
		if out, ok := helmTemplate(t, helm, chartDir, args...); !ok || out != plain {
			t.Errorf("%v renders something other than the default values do (ok=%v)", args, ok)
		}
	}
	set, ok := helmTemplate(t, helm, chartDir, "--set", "goGC.percent=400")
	if !ok {
		t.Fatalf("rendering failed:\n%s", set)
	}
	if want := strings.Replace(plain, "          ports:\n", "          env:\n            - name: GOGC\n              value: \"400\"\n          ports:\n", 1); set != want {
		t.Error("goGC.percent=400 changes more of the render than the exporter container's env")
	}
}

// What is not a whole number from 1 to 10000 or off is refused by the values
// schema, naming the value; a bare off in a values file is among it, since
// YAML reads it as the boolean false, which could as well mean "leave it".
// The templates refuse the rest, each naming both values at odds: a GOGC
// entry in env beside a percent, which would give the container the variable
// twice, and off with no Go memory limit in force — goMemLimit off, no
// memory limit in resources, or GOMEMLIMIT=off in env — where nothing would
// ever collect.
func TestChartRefusesAnInvalidGarbageCollectorTarget(t *testing.T) {
	helm := requireHelm(t)
	bareOff := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(bareOff, []byte("goGC:\n  percent: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fraction := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(fraction, []byte("goGC:\n  percent: 1.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const noLimit = "so no Go memory limit is in force and the heap would grow without bound"
	for name, tc := range map[string]struct {
		args []string
		want []string
	}{
		"zero":                        {[]string{"--set", "goGC.percent=0"}, []string{"goGC.percent"}},
		"a negative number":           {[]string{"--set", "goGC.percent=-1"}, []string{"goGC.percent"}},
		"a number over 10000":         {[]string{"--set", "goGC.percent=10001"}, []string{"goGC.percent"}},
		"a fraction":                  {[]string{"--set", "goGC.percent=1.5"}, []string{"goGC.percent"}},
		"a fraction in a values file": {[]string{"-f", fraction}, []string{"goGC.percent"}},
		"a word":                      {[]string{"--set", "goGC.percent=fast"}, []string{"goGC.percent"}},
		"a string of zero":            {[]string{"--set-string", "goGC.percent=0"}, []string{"goGC.percent"}},
		"a string over 10000":         {[]string{"--set-string", "goGC.percent=10001"}, []string{"goGC.percent"}},
		"a string with a leading 0":   {[]string{"--set-string", "goGC.percent=0400"}, []string{"goGC.percent"}},
		"a percent sign":              {[]string{"--set", "goGC.percent=400%"}, []string{"goGC.percent"}},
		"a boolean":                   {[]string{"--set", "goGC.percent=true"}, []string{"goGC.percent"}},
		"a bare off in a values file": {[]string{"-f", bareOff}, []string{"goGC.percent"}},
		"another key":                 {[]string{"--set", "goGC.percentage=400"}, []string{"percentage"}},

		"a number beside GOGC in env":               {[]string{"--set", "goGC.percent=200", "--set-json", `env=[{"name":"GOGC","value":"50"}]`}, []string{"goGC.percent is 200 and env has a GOGC entry too"}},
		"off beside GOGC in env":                    {[]string{"--set", "goGC.percent=off", "--set-json", `env=[{"name":"A","value":"b"},{"name":"GOGC","value":"off"}]`}, []string{"goGC.percent is off and env has a GOGC entry too"}},
		"a number beside GOGC from a key":           {[]string{"--set", "goGC.percent=200", "--set-json", `env=[{"name":"GOGC","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}]`}, []string{"goGC.percent is 200 and env has a GOGC entry too"}},
		"off without the ratio":                     {[]string{"--set", "goGC.percent=off", "--set", "goMemLimit.enabled=false"}, []string{"goGC.percent is off", "goMemLimit.enabled is false", noLimit}},
		"off without a memory limit":                {[]string{"--set", "goGC.percent=off", "--set", "resources.limits.memory=null"}, []string{"goGC.percent is off", "resources.limits.memory is not set", noLimit}},
		"off without limits":                        {[]string{"--set", "goGC.percent=off", "--set-json", "resources.limits=null"}, []string{"goGC.percent is off", "resources.limits.memory is not set", noLimit}},
		"off without resources":                     {[]string{"--set", "goGC.percent=off", "--set-json", "resources=null"}, []string{"goGC.percent is off", "resources.limits.memory is not set", noLimit}},
		"off with neither":                          {[]string{"--set", "goGC.percent=off", "--set", "goMemLimit.enabled=false", "--set", "resources.limits.memory=null"}, []string{"goGC.percent is off", "goMemLimit.enabled is false", noLimit}},
		"off with the limit turned off":             {[]string{"--set", "goGC.percent=off", "--set-json", `env=[{"name":"GOMEMLIMIT","value":"off"}]`}, []string{"goGC.percent is off", "env sets GOMEMLIMIT to off", noLimit}},
		"off with an empty limit of the operator's": {[]string{"--set", "goGC.percent=off", "--set", "goMemLimit.enabled=false", "--set-json", `env=[{"name":"GOMEMLIMIT","value":""}]`}, []string{"goGC.percent is off", "goMemLimit.enabled is false", noLimit}},
		"off with a ratio of 0":                     {[]string{"--set", "goGC.percent=off", "--set", "goMemLimit.ratio=0"}, []string{"goMemLimit.ratio"}},
		"off as a string without the ratio":         {[]string{"--set-string", "goGC.percent=off", "--set", "goMemLimit.enabled=false"}, []string{"goGC.percent is off", "goMemLimit.enabled is false"}},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, tc.args...)
			if ok {
				t.Fatalf("rendering succeeded, want an error naming %q", tc.want)
			}
			for _, want := range tc.want {
				if !namesInHelmError(out, want) {
					t.Errorf("the error does not name %q:\n%s", want, out)
				}
			}
		})
	}
}

// The templates refuse a value of the wrong shape themselves, as well as the
// values schema does, with a message that says what it may be: with the
// schema skipped, GOGC=1.5 or GOGC=false would reach the pod, and the Go
// runtime would ignore what it cannot read and keep its default.
func TestChartTemplatesRefuseAnInvalidGarbageCollectorTarget(t *testing.T) {
	helm := requireHelm(t)
	if out, _ := helmTemplate(t, helm, chartDir, "--skip-schema-validation"); strings.Contains(out, "unknown flag") {
		t.Skip("this helm cannot skip the values schema")
	}
	for _, value := range []string{"0", "-1", "10001", "1.5", `"fast"`, `"0400"`, "false", "true"} {
		out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "--set-json", "goGC.percent="+value)
		want := "goGC.percent " + `"` + strings.Trim(value, `"`) + `"` + " must be a whole number from 1 to 10000"
		if ok || !strings.Contains(out, want) {
			t.Errorf("goGC.percent=%s: ok=%v, want an error saying %q:\n%s", value, ok, want, out)
		}
	}
	for _, value := range []string{"400", `"400"`, `"off"`, `""`, "null"} {
		if out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "--set-json", "goGC.percent="+value); !ok {
			t.Errorf("goGC.percent=%s fails rendering with the schema skipped:\n%s", value, out)
		}
	}
}
