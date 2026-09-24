package config

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

func preScriptCollector(name, script string) model.Collector {
	c := testutil.Collector(name, "text")
	c.Transform = model.TransformConfig{Type: "jq", PreScript: script}
	c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, ErrorMode: "log", Expression: ".value"}}
	c.Limits = model.Limits{ScriptTimeout: model.Duration(5 * time.Second)}
	return c
}

func validatedConfig(t *testing.T, collectors ...model.Collector) *model.Config {
	t.Helper()
	cfg := &model.Config{Collectors: collectors}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A pre-script hands its result back through `data`, so every shape that
// produces one must be accepted: replacing it, mutating it by key or attribute,
// augmenting it, binding it in a loop or a with-statement, or calling a method
// that mutates it.
func TestPreScriptProducingDataIsAccepted(t *testing.T) {
	scripts := map[string]string{
		"replaces data":            `data = {"value": 1}`,
		"replaces from response":   "import json\ndata = json.loads(response.text)",
		"mutates by key":           `data["value"] = 1`,
		"mutates nested key":       `data["a"]["b"] = 1`,
		"mutates by attribute":     "class X: pass\ndata = X()\ndata.value = 1",
		"augments":                 `data += [1]`,
		"annotated assignment":     "data: dict = {}",
		"tuple assignment":         `data, extra = {"value": 1}, 2`,
		"starred assignment":       `first, *data = [1, 2, 3]`,
		"method call mutation":     `data.update({"value": 1})`,
		"append mutation":          `data.append(1)`,
		"loop target":              "for data in [{'value': 1}]:\n    pass",
		"with statement":           "import contextlib\nwith contextlib.suppress(Exception) as data:\n    pass",
		"conditional reassignment": "if response.status_code == 200:\n    data = {'value': 1}\nelse:\n    data = {}",
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			cfg := validatedConfig(t, preScriptCollector("accepted", script))
			if err := transform.ValidatePythonScripts("python3", cfg); err != nil {
				t.Fatalf("a pre-script that produces data was rejected: %v", err)
			}
		})
	}
}

func TestPreScriptWithoutDataIsRejected(t *testing.T) {
	scripts := map[string]string{
		"assigns another name":   `result = {"value": 1}`,
		"computes and discards":  "import json\njson.loads(response.text)",
		"only reads data":        `print(len(data))`,
		"mutates another object": "other = {}\nother[\"value\"] = 1",
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			cfg := validatedConfig(t, preScriptCollector("rejected", script))
			err := transform.ValidatePythonScripts("python3", cfg)
			if err == nil {
				t.Fatal("a pre-script that never produces data was accepted")
			}
			for _, want := range []string{"rejected", "pre_script", "'data'"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q should mention %q", err, want)
				}
			}
		})
	}
}

// The shape that slipped through once: a script that reads `data` thoroughly
// and assigns its result to some other name. Reading is not producing, however
// much of it there is, and a method call is only a mutation when the method
// mutates — `data["rates"].items()` is a read. Accepting it let the exporter
// start and then extract from the untouched response, which surfaces as
// mis-extraction errors on every scrape rather than as the configuration
// mistake it is.
func TestPreScriptThatOnlyReadsDataIsRejected(t *testing.T) {
	misnamed := "from datetime import datetime, timezone\n" +
		"published = datetime.strptime(data[\"date\"], \"%Y-%m-%d\").replace(tzinfo=timezone.utc)\n" +
		"mata = {\n" +
		"    \"base\": data[\"base\"],\n" +
		"    \"observed_at\": published.timestamp(),\n" +
		"    \"rates\": [\n" +
		"        {\"currency\": currency, \"rate\": rate}\n" +
		"        for currency, rate in sorted(data[\"rates\"].items())\n" +
		"    ],\n" +
		"}\n"
	cfg := validatedConfig(t, preScriptCollector("misnamed", misnamed))
	err := transform.ValidatePythonScripts("python3", cfg)
	if err == nil {
		t.Fatal("a pre-script that assigns its result to another name was accepted")
	}
	if !strings.Contains(err.Error(), "'data'") || !strings.Contains(err.Error(), "misnamed") {
		t.Fatalf("error %q should name the collector and the variable", err)
	}
}

// Reading methods must never be mistaken for mutations, and mutating ones must
// still be recognised, including on a nested part of data.
func TestOnlyMutatingMethodsCountAsProducingData(t *testing.T) {
	reads := map[string]string{
		"items":      `for k, v in data.items(): pass`,
		"keys":       `names = list(data.keys())`,
		"values":     `total = sum(data.values())`,
		"get":        `base = data.get("base")`,
		"copy":       `other = data.copy()`,
		"nested get": `rates = data["rates"].get("USD")`,
		"index":      `first = data["rates"].index(1)`,
	}
	for name, script := range reads {
		t.Run("reads/"+name, func(t *testing.T) {
			cfg := validatedConfig(t, preScriptCollector("reader", script))
			if err := transform.ValidatePythonScripts("python3", cfg); err == nil {
				t.Fatalf("%q only reads data and must be rejected", script)
			}
		})
	}
	mutations := map[string]string{
		"update":        `data.update({"value": 1})`,
		"append":        `data.append(1)`,
		"nested append": `data["rates"].append(1)`,
		"setdefault":    `data.setdefault("value", 1)`,
		"clear":         `data.clear()`,
		"sort":          `data["rates"].sort()`,
	}
	for name, script := range mutations {
		t.Run("mutates/"+name, func(t *testing.T) {
			cfg := validatedConfig(t, preScriptCollector("mutator", script))
			if err := transform.ValidatePythonScripts("python3", cfg); err != nil {
				t.Fatalf("%q mutates data in place and must be accepted: %v", script, err)
			}
		})
	}
}

func TestPreScriptSyntaxErrorIsRejected(t *testing.T) {
	cfg := validatedConfig(t, preScriptCollector("broken", "data = {'value': 1"))
	err := transform.ValidatePythonScripts("python3", cfg)
	if err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("error=%v, want a syntax error for the pre-script", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error %q should name the collector", err)
	}
}

// A Python transform emits through metric(...) rather than data, so it is held
// to syntax only.
func TestPythonTransformScriptIsCheckedForSyntaxOnly(t *testing.T) {
	valid := testutil.Collector("emitting", "text")
	valid.Transform = model.TransformConfig{Type: "python", Script: `metric(name="demo_value", value=1)`}
	valid.Metrics = nil
	if err := transform.ValidatePythonScripts("python3", validatedConfig(t, valid)); err != nil {
		t.Fatalf("a transform script without data was rejected: %v", err)
	}

	broken := testutil.Collector("emitting", "text")
	broken.Transform = model.TransformConfig{Type: "python", Script: `metric(name="demo_value", value=`}
	broken.Metrics = nil
	err := transform.ValidatePythonScripts("python3", validatedConfig(t, broken))
	if err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("error=%v, want a syntax error for the transform script", err)
	}
}

func TestEveryFaultyScriptIsReported(t *testing.T) {
	cfg := validatedConfig(t,
		preScriptCollector("first", `result = 1`),
		preScriptCollector("second", `data = {`),
		preScriptCollector("third", `data = {"value": 1}`),
	)
	err := transform.ValidatePythonScripts("python3", cfg)
	if err == nil {
		t.Fatal("expected the faulty scripts to be rejected")
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should report collector %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "third") {
		t.Fatalf("the valid collector should not be reported: %q", err)
	}
}

// A configuration with no Python at all must not need an interpreter, so a
// deployment that uses none is unaffected by this check.
func TestConfigurationWithoutPythonNeedsNoInterpreter(t *testing.T) {
	cfg := validatedConfig(t, testutil.Collector("plain", "text"))
	if err := transform.ValidatePythonScripts("/nonexistent/python", cfg); err != nil {
		t.Fatalf("a configuration without Python scripts must not run an interpreter: %v", err)
	}
}

func TestMissingInterpreterIsReportedWhenScriptsExist(t *testing.T) {
	cfg := validatedConfig(t, preScriptCollector("scripted", `data = {"value": 1}`))
	err := transform.ValidatePythonScripts("/nonexistent/python", cfg)
	if err == nil || !strings.Contains(err.Error(), "working interpreter") {
		t.Fatalf("error=%v, want a clear interpreter error", err)
	}
}

func TestConfigReloadRejectsAPreScriptThatDoesNotProduceData(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	valid := "collectors:\n  - name: reloaded\n    request:\n      type: http\n    transform:\n      type: jq\n      pre_script: |\n" +
		"        data = {\"value\": 1}\n    metrics:\n      - name: demo_value\n        expression: .value\n"
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := transform.ValidatePythonScripts("python3", cfg); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, path, slog.Default())
	manager.SetPythonPath("python3")

	// Reads data, produces nothing: the shape a reload must refuse just as
	// startup does.
	broken := strings.Replace(valid, "        data = {\"value\": 1}\n", "        mata = {\"value\": data[\"value\"]}\n", 1)
	if err := os.WriteFile(path, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	manager.lastMod = time.Time{}
	manager.reloadConfig()
	if got := manager.Get().Collectors[0].Transform.PreScript; !strings.Contains(got, "data =") {
		t.Fatalf("a reload whose pre-script stops producing data must be rejected; active script is %q", got)
	}

	fixed := strings.Replace(valid, "        data = {\"value\": 1}\n", "        data = {\"value\": 2}\n", 1)
	if err := os.WriteFile(path, []byte(fixed), 0600); err != nil {
		t.Fatal(err)
	}
	manager.lastMod = time.Time{}
	manager.reloadConfig()
	if got := manager.Get().Collectors[0].Transform.PreScript; !strings.Contains(got, `"value": 2`) {
		t.Fatalf("a valid reload should take effect, got %q", got)
	}
}

// required is for labels an expression fills: a fixed value is always there,
// and a python transform's labels come from its script.
func TestRequiredLabelsNeedAnExpression(t *testing.T) {
	fixed := testutil.Collector("fixed", "text")
	fixed.Metrics[0].Labels = []model.LabelRule{{Name: "env", Value: "prod", Required: true}}
	if err := Validate(&model.Config{Collectors: []model.Collector{fixed}}); err == nil || !strings.Contains(err.Error(), `label "env" has a static value, so it cannot be required`) {
		t.Fatalf("err=%v", err)
	}

	script := testutil.Collector("script", "text")
	script.Transform = model.TransformConfig{Type: "python", Script: `metric(name="v", value=1)`}
	script.Metrics = []model.MetricRule{{Name: "v", Type: model.GaugeMetricType, Labels: []model.LabelRule{{Name: "who", Expression: "who", Required: true}}}}
	if err := Validate(&model.Config{Collectors: []model.Collector{script}}); err == nil || !strings.Contains(err.Error(), "a python transform's labels come from its script") {
		t.Fatalf("err=%v", err)
	}

	expression := testutil.Collector("expression", "text")
	expression.Metrics[0].Expression = `v=(\d+) (?P<who>\S+)`
	expression.Metrics[0].Labels = []model.LabelRule{{Name: "who", Expression: "who", Required: true}}
	if err := Validate(&model.Config{Collectors: []model.Collector{expression}}); err != nil {
		t.Fatal(err)
	}
}

// A collector that sets no decoder, and whose transform implies none, decodes
// each response by what it says it is: warned about at load, with how.
func TestAnUnsetDecoderIsWarnedAbout(t *testing.T) {
	collector := func(name, request, transformType string) model.Collector {
		c := testutil.Collector(name, "text")
		c.Decoder = model.DecoderConfig{}
		c.Request = model.RequestConfig{Type: request}
		if request == fetch.RequestTypeLocalFile {
			c.Request.Root = t.TempDir()
			c.Request.Path = "status.json"
		}
		c.Transform = model.TransformConfig{Type: transformType}
		c.Metrics[0].Expression = ".value"
		return c
	}
	pinned := collector("pinned", fetch.RequestTypeHTTP, "jq")
	pinned.Decoder.Type = "json"
	explicit := collector("explicit", fetch.RequestTypeHTTP, "jq")
	explicit.Decoder.Type = "auto"
	implied := testutil.Collector("implied", "text") // regex, which implies text
	implied.Decoder = model.DecoderConfig{}
	cfg := &model.Config{Collectors: []model.Collector{
		collector("by_header", fetch.RequestTypeHTTP, "jq"),
		collector("by_extension", fetch.RequestTypeLocalFile, "jq"),
		pinned, explicit, implied,
	}}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`collector "by_header" sets no decoder.type, so it decodes by the Content-Type header of each response`,
		`collector "by_extension" sets no decoder.type, so it decodes by the extension of each file`,
	}
	if len(cfg.Warnings) != len(want) {
		t.Fatalf("warnings=%q", cfg.Warnings)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(cfg.Warnings[i], prefix) {
			t.Errorf("warning %d = %q, want it to start %q", i, cfg.Warnings[i], prefix)
		}
	}
}

// Warnings are logged with the deprecations on every start and reload.
func TestWarningsAreLogged(t *testing.T) {
	out := testutil.CaptureLogs(t)
	LogDeprecations(slog.Default(), "config.yaml", &model.Config{Warnings: []string{"collector \"x\" sets no decoder.type"}})
	records := testutil.AssertJSONLines(t, out, 1)
	if records[0]["msg"] != "configuration warning" || records[0]["level"] != "WARN" || records[0]["warning"] != "collector \"x\" sets no decoder.type" || records[0]["file"] != "config.yaml" {
		t.Fatalf("record=%v", records[0])
	}
}
