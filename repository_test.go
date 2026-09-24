package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
	"gopkg.in/yaml.v3"
)

// The released image carries its version.
func TestDockerfileSetsTheVersion(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	for _, want := range []string{"ARG VERSION\n", "-X main.version=${VERSION}"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
	}
}

// collector-file.schema.json is committed, current, printed by its flag, and
// describes collectors exactly as the configuration schema does.
func TestCommittedCollectorFileSchemaIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(collectorFileSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := config.CollectorFileSchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, generated) {
		t.Fatalf("%s is out of date; regenerate it with: go run . --config.collector-file-schema > %s", collectorFileSchemaFile, collectorFileSchemaFile)
	}
	out := runCLI(t, "--config.collector-file-schema")
	if out.code != 0 || out.stdout != string(generated) || out.stderr != "" {
		t.Fatalf("exit=%d stderr=%q", out.code, out.stderr)
	}
	var parsed map[string]any
	if err := json.Unmarshal(generated, &parsed); err != nil {
		t.Fatal(err)
	}
	configSchema := parsedSchema(t, config.SchemaJSON)
	fileItems := parsed["properties"].(map[string]any)["collectors"].(map[string]any)["items"]
	configItems := configSchema["properties"].(map[string]any)["collectors"].(map[string]any)["items"]
	if !reflect.DeepEqual(fileItems, configItems) {
		t.Fatal("the collector file schema describes collectors differently from the configuration schema")
	}
}

func TestCollectorFileSchemaAcceptsCollectorsOnly(t *testing.T) {
	schema := loadSchemaFile(t, collectorFileSchemaFile)
	valid := testutil.CollectorsDocument("demo")
	for name, tc := range map[string]struct {
		document string
		valid    bool
	}{
		"collectors":        {valid, true},
		"another key":       {"web: {}\n" + valid, false},
		"collector_files":   {"collector_files: [x.yaml]\n" + valid, false},
		"no collectors":     {"collectors: []\n", false},
		"nothing":           {"{}\n", false},
		"invalid collector": {strings.Replace(valid, "name: demo", "name: bad-name", 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			var doc any
			if err := yaml.Unmarshal([]byte(tc.document), &doc); err != nil {
				t.Fatal(err)
			}
			errs := validateAgainstSchema(schema, normalizeYAML(doc))
			if tc.valid != (len(errs) == 0) {
				t.Fatalf("valid=%v, errors=%v", tc.valid, errs)
			}
		})
	}
	// The configuration schema accepts a configuration made of collector
	// files alone.
	var doc any
	if err := yaml.Unmarshal([]byte("collector_files: ['collectors.d/*.yaml']\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validateAgainstSchema(loadSchema(t), normalizeYAML(doc)); len(errs) > 0 {
		t.Fatal(errs)
	}
}

// In the chart, every key of config.data is a file beside config.yaml, which is
// how collector files are supplied. The template renders every key and the
// whole ConfigMap is mounted, and the example the chart README documents loads
// as the exporter would load it, beside the scheduled target file the chart
// renders into the same directory.
func TestChartSuppliesCollectorFilesAsConfigMapKeys(t *testing.T) {
	configmap := readChartFile(t, "templates/configmap.yaml")
	if !strings.Contains(configmap, "range $name, $content := .Values.config.data") {
		t.Fatal("the ConfigMap template must render every key of config.data")
	}
	deployment := readChartFile(t, "templates/deployment.yaml")
	const configVolume = "      volumes:\n        - name: config\n"
	_, volume, found := strings.Cut(deployment, configVolume)
	if !found {
		t.Fatal("the deployment has no configuration volume")
	}
	volume, _, _ = strings.Cut(volume, "        - ")
	if !strings.Contains(volume, "configMap:") || strings.Contains(volume, "items:") {
		t.Fatalf("the configuration volume must mount the whole ConfigMap:\n%s", volume)
	}

	readme := readChartFile(t, "README.md")
	start := strings.Index(readme, "### Collector files")
	if start < 0 {
		t.Fatal("the chart README does not document collector files")
	}
	_, block, _ := strings.Cut(readme[start:], "```yaml\n")
	block, _, _ = strings.Cut(block, "```")
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal([]byte(block), &values); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range values.Config.Data {
		testutil.WriteIn(t, dir, name, content)
	}
	testutil.WriteIn(t, dir, "targets.yaml", "targets: []\n")
	c, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectorNames(c); !reflect.DeepEqual(got, []string{"payments_api", "search_api"}) {
		t.Fatalf("collectors=%v", got)
	}
}

// config.schema.json is the JSON Schema of the configuration file, generated
// from the Config struct (config/configschema.go) and printed by --config.schema.

const (
	configSchemaFile        = "config.schema.json"
	collectorFileSchemaFile = "collector-file.schema.json"
)

// The committed schema is exactly what the code generates, so it cannot drift
// from the configuration it describes. Regenerate it with:
//
//	go run . --config.schema > config.schema.json
func TestCommittedConfigSchemaIsCurrent(t *testing.T) {
	committed, err := os.ReadFile(configSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := config.SchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, generated) {
		t.Fatalf("%s is out of date; regenerate it with: go run . --config.schema > %s", configSchemaFile, configSchemaFile)
	}
}

// Every shipped configuration is valid against the schema, so an editor
// pointed at it shows no false errors on the examples.
func TestShippedConfigurationsMatchTheSchema(t *testing.T) {
	schema := loadSchema(t)
	files := []string{"config.example.yaml", "config.otlp.example.yaml"}
	testdata, _ := filepath.Glob("testdata/config.*.yaml")
	files = append(files, testdata...)
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			if errs := validateAgainstSchema(schema, readYAMLDocument(t, file)); len(errs) > 0 {
				t.Fatalf("%s does not match the schema:\n%s", file, strings.Join(errs, "\n"))
			}
		})
	}
}

// So is the configuration the Helm chart ships by default.
func TestChartDefaultConfigurationMatchesTheSchema(t *testing.T) {
	raw, err := os.ReadFile(chartDir + "values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	document, ok := values.Config.Data["config.yaml"]
	if !ok {
		t.Fatal("the chart ships no config.yaml")
	}
	var doc any
	if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validateAgainstSchema(loadSchema(t), normalizeYAML(doc)); len(errs) > 0 {
		t.Fatalf("the chart's default configuration does not match the schema:\n%s", strings.Join(errs, "\n"))
	}
}

// The schema rejects what startup rejects, where a schema can tell.
func TestConfigSchemaRejectsInvalidConfigurations(t *testing.T) {
	schema := loadSchema(t)
	base := `collectors:
  - name: demo
    request:
      type: http
    transform:
      type: jq
    metrics:
      - name: value
        expression: .value
`
	tests := map[string]string{
		"unknown top-level key":   "colectors: []\n" + base,
		"no collectors":           "collectors: []\n",
		"nothing at all":          "web: {}\n",
		"empty collector_files":   "collector_files: []\n",
		"empty collector file":    "collector_files: ['']\n",
		"collector_files string":  "collector_files: collectors.d/*.yaml\n",
		"localfile without root":  strings.Replace(base, "type: http", "type: localfile", 1),
		"unknown collector key":   strings.Replace(base, "    request:", "    requst: {}\n    request:", 1),
		"missing request.type":    strings.Replace(base, "      type: http\n", "      path: /x\n", 1),
		"unknown request.type":    strings.Replace(base, "type: http", "type: gopher", 1),
		"unknown transform":       strings.Replace(base, "type: jq", "type: jsonpath", 1),
		"bad metric name":         strings.Replace(base, "name: value", "name: bad-name", 1),
		"bad metrics_prefix":      strings.Replace(base, "  - name: demo\n", "  - name: demo\n    metrics_prefix: grafana_\n", 1),
		"bad duration":            strings.Replace(base, "  - name: demo\n", "  - name: demo\n    cache:\n      ttl: five minutes\n", 1),
		"bad error policy":        strings.Replace(base, "  - name: demo\n", "  - name: demo\n    error_handling:\n      on_fetch_error: panic\n", 1),
		"bad label type":          base + "        labels:\n          - name: l\n            type: literal\n",
		"negative limit":          strings.Replace(base, "  - name: demo\n", "  - name: demo\n    limits:\n      max_metrics: -1\n", 1),
		"unsupported library":     strings.Replace(base, "      type: jq\n", "      type: jq\n      libraries: [requests]\n", 1),
		"string for a list":       strings.Replace(base, "  - name: demo\n", "  - name: demo\n    request_list: x\n", 1),
		"boolean for a structure": strings.Replace(base, "    request:\n      type: http\n", "    request: true\n", 1),
	}
	var doc any
	if err := yaml.Unmarshal([]byte(base), &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validateAgainstSchema(schema, normalizeYAML(doc)); len(errs) > 0 {
		t.Fatalf("the base document should be valid: %v", errs)
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			var doc any
			if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
				t.Fatal(err)
			}
			if errs := validateAgainstSchema(schema, normalizeYAML(doc)); len(errs) == 0 {
				t.Fatalf("accepted:\n%s", document)
			}
		})
	}
}

// The examples point editors at the published schema.
func TestExamplesReferenceTheSchema(t *testing.T) {
	modeline := "# yaml-language-server: $schema=" + parsedSchema(t, config.SchemaJSON)["$id"].(string)
	for _, file := range []string{"config.example.yaml", "config.otlp.example.yaml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(raw), modeline+"\n") {
			t.Errorf("%s does not start with %q", file, modeline)
		}
	}
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	return loadSchemaFile(t, configSchemaFile)
}

func loadSchemaFile(t *testing.T, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func readYAMLDocument(t *testing.T, file string) any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return normalizeYAML(doc)
}

// normalizeYAML turns yaml.v3's map[string]any and ints into the shapes a
// JSON decoder produces, which is what a schema validator sees.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, value := range x {
			out[k] = normalizeYAML(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = normalizeYAML(value)
		}
		return out
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return v
}

// validateAgainstSchema checks the part of JSON Schema the generated schema
// uses: type, properties, additionalProperties, required, items, minItems,
// enum, pattern and minimum. It is enough to test the schema without a
// third-party validator.
func validateAgainstSchema(schema map[string]any, value any) []string {
	var errs []string
	var check func(schema map[string]any, value any, path string)
	check = func(schema map[string]any, value any, path string) {
		if types, ok := schema["type"]; ok && !schemaTypeMatches(types, value) {
			errs = append(errs, fmt.Sprintf("%s: %T is not %v", path, value, types))
			return
		}
		if enum, ok := schema["enum"].([]any); ok {
			found := false
			for _, allowed := range enum {
				if allowed == value {
					found = true
				}
			}
			if !found {
				errs = append(errs, fmt.Sprintf("%s: %v is not one of %v", path, value, enum))
			}
		}
		if pattern, ok := schema["pattern"].(string); ok {
			if s, ok := value.(string); ok && !regexp.MustCompile(pattern).MatchString(s) {
				errs = append(errs, fmt.Sprintf("%s: %q does not match %s", path, s, pattern))
			}
		}
		if minLength, ok := schema["minLength"].(float64); ok {
			if s, ok := value.(string); ok && float64(len(s)) < minLength {
				errs = append(errs, fmt.Sprintf("%s: %q is shorter than %v", path, s, minLength))
			}
		}
		if constant, ok := schema["const"]; ok && constant != value {
			errs = append(errs, fmt.Sprintf("%s: %v is not %v", path, value, constant))
		}
		if allOf, ok := schema["allOf"].([]any); ok {
			for _, part := range allOf {
				errs = append(errs, validateAgainstSchema(part.(map[string]any), value)...)
			}
		}
		if condition, ok := schema["if"].(map[string]any); ok && len(validateAgainstSchema(condition, value)) == 0 {
			if then, ok := schema["then"].(map[string]any); ok {
				errs = append(errs, validateAgainstSchema(then, value)...)
			}
		}
		if anyOf, ok := schema["anyOf"].([]any); ok {
			matched := false
			for _, alternative := range anyOf {
				if len(validateAgainstSchema(alternative.(map[string]any), value)) == 0 {
					matched = true
				}
			}
			if !matched {
				errs = append(errs, path+": matches none of anyOf")
			}
		}
		if minimum, ok := schema["minimum"].(float64); ok {
			if n, ok := value.(float64); ok && n < minimum {
				errs = append(errs, fmt.Sprintf("%s: %v is below %v", path, n, minimum))
			}
		}
		switch x := value.(type) {
		case map[string]any:
			properties, _ := schema["properties"].(map[string]any)
			for _, key := range toStrings(schema["required"]) {
				if _, ok := x[key]; !ok {
					errs = append(errs, fmt.Sprintf("%s: %s is required", path, key))
				}
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if sub, ok := properties[key].(map[string]any); ok {
					check(sub, x[key], path+"."+key)
					continue
				}
				switch additional := schema["additionalProperties"].(type) {
				case bool:
					if !additional {
						errs = append(errs, fmt.Sprintf("%s: unknown key %q", path, key))
					}
				case map[string]any:
					check(additional, x[key], path+"."+key)
				}
			}
		case []any:
			if minItems, ok := schema["minItems"].(float64); ok && float64(len(x)) < minItems {
				errs = append(errs, fmt.Sprintf("%s: %d items, want at least %v", path, len(x), minItems))
			}
			if items, ok := schema["items"].(map[string]any); ok {
				for i, item := range x {
					check(items, item, fmt.Sprintf("%s[%d]", path, i))
				}
			}
		}
	}
	check(schema, value, "")
	return errs
}

func schemaTypeMatches(types, value any) bool {
	for _, name := range toStrings(types) {
		switch name {
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "number":
			if _, ok := value.(float64); ok {
				return true
			}
		case "integer":
			if n, ok := value.(float64); ok && n == float64(int64(n)) {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		}
	}
	return value == nil
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case []string:
		return x
	}
	return nil
}

// The shipped configurations document the key; the reference example uses it.
func TestTheExampleConfigurationShowsMetricsPrefix(t *testing.T) {
	cfg, err := config.Load("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfg.Collectors {
		if c.MetricsPrefix != "" {
			return
		}
	}
	raw, _ := yaml.Marshal(cfg.Collectors[0])
	t.Fatalf("config.example.yaml has no collector with metrics_prefix; first collector:\n%s", raw)
}

// The decoder no longer pulls in the Prometheus client libraries; this keeps
// them, and the protobuf runtime they bring, from coming back unnoticed.
func TestGoModDoesNotNeedThePrometheusClientLibraries(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"github.com/prometheus/common", "github.com/prometheus/client_model", "google.golang.org/protobuf", "github.com/munnerz/goautoneg"} {
		if strings.Contains(string(raw), module+" ") {
			t.Errorf("go.mod requires %s again; the prometheus decoder parses the text format itself (promparse.go)", module)
		}
	}
}

// The image carries no BeautifulSoup and no pip, and its lxml is at or above
// 6.1.0, the first release without CVE-2026-41066.
func TestDockerfileImageContents(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	dockerfile := string(raw)
	if strings.Contains(strings.ToLower(dockerfile), "beautifulsoup") {
		t.Error("the Dockerfile still installs BeautifulSoup")
	}
	if !strings.Contains(dockerfile, "pip uninstall -y pip") {
		t.Error("the Dockerfile no longer removes pip from the runtime image")
	}
	if !strings.Contains(dockerfile, "apt-get upgrade") {
		t.Error("the Dockerfile no longer applies Debian security updates at build time")
	}
	match := regexp.MustCompile(`(?m)^ARG LXML_VERSION=(\d+)\.(\d+)`).FindStringSubmatch(dockerfile)
	if match == nil {
		t.Fatal("the Dockerfile no longer pins LXML_VERSION")
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < 6 || major == 6 && minor < 1 {
		t.Errorf("LXML_VERSION %s.%s is older than 6.1.0, which fixes CVE-2026-41066", match[1], match[2])
	}
}

// usePythonPool swaps the shared Python worker pool for the length of a test,
// which is only safe while no test runs in parallel with another, in any
// package.
func TestNoTestRunsInParallel(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	call := "t." + "Parallel("
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), call) {
			t.Errorf("%s calls %s), but usePythonPool swaps the shared worker pool per test", file, call)
		}
	}
}

// The configuration the chart ships by default has to start, so it declares
// its request type like any other.
func TestTheChartsDefaultConfigurationIsValid(t *testing.T) {
	var values struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	raw, err := os.ReadFile("charts/prometheus-universal-exporter/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	path := testutil.WriteFile(t, "config.yaml", values.Config.Data["config.yaml"])
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the chart's default configuration does not load: %v", err)
	}
}

// tools/request-type-tags.sh turns REQUEST_TYPES into build tags for the
// Dockerfile and the Makefile.
func TestRequestTypeTagsScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := func(list string) (string, string, error) {
		cmd := exec.Command("sh", "tools/request-type-tags.sh", list)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return strings.TrimSpace(stdout.String()), stderr.String(), err
	}
	for list, want := range map[string]string{
		"":               "",
		"http":           "select_request_types,request_type_http",
		"http,http":      "select_request_types,request_type_http",
		" http , ":       "select_request_types,request_type_http",
		"http,http ":     "select_request_types,request_type_http",
		"localfile":      "select_request_types,request_type_localfile",
		"localfile,http": "select_request_types,request_type_localfile,request_type_http",
	} {
		got, stderr, err := run(list)
		if err != nil || got != want {
			t.Errorf("%q: got %q err=%v %s, want %q", list, got, err, stderr, want)
		}
	}
	for _, list := range []string{"htp", "http,grpcc", "none", "test"} {
		if got, stderr, err := run(list); err == nil || !strings.Contains(stderr, "no request type") {
			t.Errorf("%q: got %q err=%v stderr=%q, want it refused", list, got, err, stderr)
		}
	}
}

func TestDockerfileBuildsTheSelectedRequestTypes(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Skipf("no Dockerfile to check: %v", err)
	}
	for _, want := range []string{"ARG REQUEST_TYPES\n", `sh tools/request-type-tags.sh "${REQUEST_TYPES}"`, `-tags "${tags}"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the Dockerfile is missing %q", want)
		}
	}
}

// The shipped examples must stay loadable and consistent with each other.
func TestShippedExampleFilesLoadTogether(t *testing.T) {
	cfg, err := config.Load("config.otlp.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OTLP.Enabled {
		t.Fatal("config.otlp.example.yaml must enable OTLP export")
	}
	file, err := config.LoadTargets("targets.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateTargets(file); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateTargetsAgainst(file, cfg); err != nil {
		t.Fatalf("targets.example.yaml does not match config.otlp.example.yaml: %v", err)
	}

	plain, err := config.Load("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateTargetsAgainst(file, plain); err == nil {
		t.Fatal("config.example.yaml leaves OTLP disabled, so the target file must be rejected against it")
	}
}

// The configurations the repository ships must satisfy the contract they
// demonstrate — the examples an operator copies from, and the fixture the
// different file format demos runs against, which is the only one of them carrying a
// pre-script.
func TestShippedExampleScriptsSatisfyTheContract(t *testing.T) {
	for _, path := range []string{
		"config.example.yaml",
		"config.otlp.example.yaml",
		"testdata/config.frankfurter.json-test.yaml",
		"testdata/config.usgs.csv-test.yaml",
		"testdata/config.k8sguestbook.yaml-test.yaml",
		"testdata/config.scrapethissite.html-test.yaml",
		"testdata/config.grafanastatus.json-test.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := transform.ValidatePythonScripts("python3", cfg); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		})
	}
}

// internalLayers is the order of the internal packages: each may import only
// the ones before it (docs/SPECIFICATION-EXPORTER.md, section 7.1).
var internalLayers = []string{"model", "expr", "fetch", "decode", "transform", "config", "exporter"}

// The internal packages stay layered, and testutil stays out of the binary.
func TestInternalPackagesAreLayered(t *testing.T) {
	const prefix = "github.com/eenchev/prometheus-universal-exporter/internal/"
	rank := map[string]int{}
	for i, name := range internalLayers {
		rank[name] = i
	}
	dirs, err := os.ReadDir("internal")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		name := dir.Name()
		own, layered := rank[name]
		if !layered && name != "testutil" {
			t.Errorf("internal/%s is not in internalLayers; add it where it belongs", name)
			continue
		}
		files, err := filepath.Glob(filepath.Join("internal", name, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			test := strings.HasSuffix(file, "_test.go")
			for _, spec := range parsed.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				imported, internal := strings.CutPrefix(path, prefix)
				if !internal {
					continue
				}
				if imported == "testutil" {
					if !test {
						t.Errorf("%s imports internal/testutil, which only tests may", file)
					}
					continue
				}
				if name == "testutil" {
					if imported != "model" {
						t.Errorf("%s imports internal/%s; testutil may import only internal/model, so every package's tests can use it", file, imported)
					}
					continue
				}
				if rank[imported] >= own {
					t.Errorf("%s imports internal/%s, which comes after internal/%s in internalLayers", file, imported, name)
				}
			}
		}
	}
	for _, file := range []string{"main.go", "check.go"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			if strings.HasSuffix(spec.Path.Value, `/internal/testutil"`) {
				t.Errorf("%s imports internal/testutil, which only tests may", file)
			}
		}
	}
}

// parsedSchema is a schema as render prints it, parsed.
func parsedSchema(t *testing.T, render func() ([]byte, error)) map[string]any {
	t.Helper()
	raw, err := render()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}
