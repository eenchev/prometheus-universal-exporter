package repository

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocator"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The target allocator's own files have schemas of their own: its static
// target file, which is the exporter's with discovered_targets, and its
// probe targets file. configs/static-targets.schema.json is the exporter's
// and refuses discovered_targets, as the exporter does, so a file of the
// allocator's using it was flagged by an editor pointed at the one schema
// there was, and a probe targets file had none.

const (
	allocatorStaticTargetsSchemaFile = "configs/" + allocator.StaticTargetsSchemaFile
	probeTargetsSchemaFile           = "configs/" + allocator.ProbeTargetsSchemaFile
)

// The committed schemas are exactly what the allocator generates, and each
// says where it is published.
func TestCommittedAllocatorSchemasAreCurrent(t *testing.T) {
	for file, render := range map[string]func() ([]byte, error){
		allocatorStaticTargetsSchemaFile: allocator.StaticTargetsSchemaJSON,
		probeTargetsSchemaFile:           allocator.ProbeTargetsSchemaJSON,
	} {
		committed, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		generated, err := render()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(committed, generated) {
			t.Errorf("%s is out of date; regenerate the schemas with: make schemas", file)
		}
		if id := parsedSchema(t, render)["$id"]; id != "https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/"+file {
			t.Errorf("%s says it is published at %v", file, id)
		}
	}
}

// The allocator's static target schema takes discovered_targets beside
// targets, or in their place, and every file the exporter's schema takes;
// the exporter's refuses discovered_targets still.
func TestTheAllocatorsStaticTargetSchemaTakesDiscoveredTargets(t *testing.T) {
	own, exporters := loadSchemaFile(t, allocatorStaticTargetsSchemaFile), loadSchemaFile(t, staticTargetsSchemaFile)
	const entry = "discovered_targets:\n  - name: node\n    collector: node\n    target_template: http://{{address}}/metrics\n    interval: 30s\n    shard_key: address\n    labels: {team: infra}\n    discovery:\n      - kubernetes: {namespaces: [monitoring], port: metrics}\n        labels: {node: __meta_kubernetes_node_name}\n"
	const written = "interval: 1m\ntargets:\n  - name: legacy\n    collector: node\n    target: http://legacy.internal:9100/metrics\n"
	for name, document := range map[string]string{
		"beside targets":     written + entry,
		"alone":              "interval: 1m\n" + entry,
		"with empty targets": "interval: 1m\ntargets: []\n" + entry,
	} {
		if problems := schemaProblems(t, own, document); len(problems) > 0 {
			t.Errorf("%s: the allocator's schema refuses discovered_targets: %v", name, problems)
		}
		if problems := schemaProblems(t, exporters, document); len(problems) == 0 {
			t.Errorf("%s: the exporter's schema takes discovered_targets, which the exporter refuses", name)
		}
	}
	for _, file := range []string{"configs/static-targets.example.yaml"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if problems := schemaProblems(t, own, string(raw)); len(problems) > 0 {
			t.Errorf("the allocator's schema refuses %s, which the exporter's takes: %v", file, problems)
		}
	}
}

// What the allocator refuses at load, the schema refuses where it can tell.
func TestTheAllocatorsSchemasRefuseWhatTheAllocatorRefuses(t *testing.T) {
	static := loadSchemaFile(t, allocatorStaticTargetsSchemaFile)
	entry := func(rest string) string {
		return "interval: 1m\ndiscovered_targets:\n  - name: node\n    collector: node\n" + rest
	}
	const source = "    discovery:\n      - file_sd: {files: [sd.json]}\n"
	if problems := schemaProblems(t, static, entry(source)); len(problems) > 0 {
		t.Fatalf("the base entry is refused: %v", problems)
	}
	for _, dns := range []string{"{names: [_metrics._tcp.example.com], server: 10.0.0.53}", "{names: [a.example], type: SRV}", "{names: [a.example], type: A, port: 9100, refresh_interval: 1m, timeout: 5s}"} {
		if problems := schemaProblems(t, static, entry("    discovery:\n      - dns_sd: "+dns+"\n")); len(problems) > 0 {
			t.Errorf("the dns_sd source %s is refused: %v", dns, problems)
		}
	}
	for name, document := range map[string]string{
		"neither targets nor entries": "interval: 1m\n",
		"empty targets alone":         "interval: 1m\ntargets: []\n",
		"no entries":                  "interval: 1m\ndiscovered_targets: []\n",
		"no interval":                 strings.Replace(entry(source), "interval: 1m\n", "", 1),
		"no name":                     strings.Replace(entry(source), "  - name: node\n    collector", "  - collector", 1),
		"a bad name":                  strings.Replace(entry(source), "name: node", "name: node-exporter", 1),
		"no collector":                strings.Replace(entry(source), "    collector: node\n", "", 1),
		"no discovery":                entry(""),
		"empty discovery":             entry("    discovery: []\n"),
		"a target":                    entry(source + "    target: http://x/\n"),
		"a shard_key of text":         entry(source + "    shard_key: db\n"),
		"a template without address":  entry(source + "    target_template: http://x/\n"),
		"an unknown entry key":        entry(source + "    bogus: 1\n"),
		"two kinds":                   entry("    discovery:\n      - file_sd: {files: [sd.json]}\n        http_sd: {url: 'https://a/'}\n"),
		"no kind":                     entry("    discovery:\n      - name: a\n"),
		"an unknown source key":       entry("    discovery:\n      - file_sd: {files: [sd.json]}\n        bogus: 1\n"),
		"a file of no format":         entry("    discovery:\n      - file_sd: {files: [sd.txt]}\n"),
		"no files":                    entry("    discovery:\n      - file_sd: {files: []}\n"),
		"an ftp URL":                  entry("    discovery:\n      - http_sd: {url: 'ftp://x/'}\n"),
		"credentials in the URL":      entry("    discovery:\n      - http_sd: {url: 'https://u:p@x/'}\n"),
		"a certificate without a key": entry("    discovery:\n      - http_sd: {url: 'https://x/', tls: {cert_file: c.pem}}\n"),
		"no port":                     entry("    discovery:\n      - kubernetes: {}\n"),
		"a port out of range":         entry("    discovery:\n      - kubernetes: {port: 70000}\n"),
		"a bad namespace":             entry("    discovery:\n      - kubernetes: {port: m, namespaces: [Monitoring]}\n"),
		"namespaces and all":          entry("    discovery:\n      - kubernetes: {port: m, namespaces: [a], all_namespaces: true}\n"),
		"a reserved label":            entry("    discovery:\n      - file_sd: {files: [sd.json]}\n        labels: {job: __meta_x}\n"),
		"a meta label set":            entry("    discovery:\n      - file_sd: {files: [sd.json]}\n        labels: {__x: zone}\n"),
		"a bad source name":           entry("    discovery:\n      - name: -a\n        file_sd: {files: [sd.json]}\n"),
		"a negative max_targets":      entry("    discovery:\n      - file_sd: {files: [sd.json]}\n        max_targets: -1\n"),
		"a bad refresh_interval":      entry("    discovery:\n      - file_sd: {files: [sd.json], refresh_interval: often}\n"),
		"dns_sd without names":        entry("    discovery:\n      - dns_sd: {names: []}\n"),
		"a dns_sd name twice":         entry("    discovery:\n      - dns_sd: {names: [a.example, a.example]}\n"),
		"a dns_sd name with a port":   entry("    discovery:\n      - dns_sd: {names: ['a.example:53']}\n"),
		"an MX dns_sd":                entry("    discovery:\n      - dns_sd: {names: [a.example], type: MX}\n"),
		"a port for SRV":              entry("    discovery:\n      - dns_sd: {names: [a.example], port: 9100}\n"),
		"an A dns_sd without a port":  entry("    discovery:\n      - dns_sd: {names: [a.example], type: A}\n"),
		"a dns_sd port out of range":  entry("    discovery:\n      - dns_sd: {names: [a.example], type: AAAA, port: 70000}\n"),
		"an unknown dns_sd key":       entry("    discovery:\n      - dns_sd: {names: [a.example], transport: tcp}\n"),
	} {
		if problems := schemaProblems(t, static, document); len(problems) == 0 {
			t.Errorf("%s: the allocator's static target schema takes\n%s", name, document)
		}
	}

	probes := loadSchemaFile(t, probeTargetsSchemaFile)
	job := func(rest string) string {
		return "probe_jobs:\n  - name: websites\n    collector: html_status\n" + rest
	}
	if problems := schemaProblems(t, probes, job("    targets: [https://shop.example]\n")+"x-common: {a: 1}\n"); len(problems) > 0 {
		t.Fatalf("the base job is refused: %v", problems)
	}
	if problems := schemaProblems(t, probes, job(source)); len(problems) > 0 {
		t.Fatalf("a job of discovery alone is refused: %v", problems)
	}
	for name, document := range map[string]string{
		"no jobs":                    "probe_jobs: []\n",
		"nothing":                    "{}\n",
		"another top-level key":      "jobs: []\n" + job(source),
		"no name":                    "probe_jobs:\n  - collector: html_status\n" + source,
		"a bad name":                 strings.Replace(job(source), "name: websites", "name: web/sites", 1),
		"no collector":               "probe_jobs:\n  - name: websites\n" + source,
		"neither targets nor source": job(""),
		"empty targets":              job("    targets: []\n"),
		"an empty target":            job("    targets: ['']\n"),
		"a bad shard_key":            job(source + "    shard_key: host\n"),
		"a param of the allocator":   job(source + "    params: {target: x}\n"),
		"debug":                      job(source + "    params: {debug: 'true'}\n"),
		"a label instance":           job(source + "    labels: {instance: x}\n"),
		"a template without address": job(source + "    target_template: https://x/\n"),
		"an unknown job key":         job(source + "    target: https://x/\n"),
		"a bad source":               job("    discovery:\n      - kubernetes: {port: m, bogus: 1}\n"),
	} {
		if problems := schemaProblems(t, probes, document); len(problems) == 0 {
			t.Errorf("%s: the probe targets schema takes\n%s", name, document)
		}
	}
}

// keyTree is the keys of t as the allocator's decoder reads them, by their
// yaml names, each with the keys below it; nil for a key of no keys, such as
// a scalar, a duration or a map of names of the file's own.
func keyTree(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	keys := map[string]any{}
	for i := range t.NumField() {
		field := t.Field(i)
		key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if key == "" || key == "-" || !field.IsExported() {
			continue
		}
		keys[key] = keyTree(field.Type)
	}
	return keys
}

// schemaKeyTree is the keys a schema declares, as keyTree has them; a $ref
// to the schema's own $defs is followed.
func schemaKeyTree(root, schema map[string]any) map[string]any {
	if ref, ok := schema["$ref"].(string); ok {
		name, found := strings.CutPrefix(ref, "#/$defs/")
		if !found {
			return nil
		}
		schema, _ = root["$defs"].(map[string]any)[name].(map[string]any)
	}
	if items, ok := schema["items"].(map[string]any); ok {
		return schemaKeyTree(root, items)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	keys := map[string]any{}
	for key, value := range properties {
		sub, _ := value.(map[string]any)
		keys[key] = schemaKeyTree(root, sub)
	}
	return keys
}

// at is the schema at the keys of path, each a property, or the items, [],
// of the one before it.
func at(t *testing.T, schema map[string]any, path ...string) map[string]any {
	t.Helper()
	node := schema
	for _, key := range path {
		var next map[string]any
		if key == "[]" {
			next, _ = node["items"].(map[string]any)
		} else {
			next, _ = node["properties"].(map[string]any)[key].(map[string]any)
		}
		if next == nil {
			t.Fatalf("the schema has nothing at %v", path)
		}
		node = next
	}
	return node
}

// Each schema declares the keys the allocator decodes, and no other: a
// probe targets file probe_jobs, a job the keys of ProbeJob, a discovery
// source, of a probe job and of a discovered_targets entry, those of
// DiscoveryConfig with each kind's own, and a discovered_targets entry the
// keys of a static target but target, with name, discovery, target_template
// and shard_key, which the entry reads itself (that the decoder takes each
// is schemaallocator_http_test.go's). The values schema of the chart
// declares a probe job's and a source's keys alike.
func TestTheAllocatorsSchemasDeclareTheDecodersKeys(t *testing.T) {
	probes := loadSchemaFile(t, probeTargetsSchemaFile)
	static := loadSchemaFile(t, allocatorStaticTargetsSchemaFile)
	file := keyTree(reflect.TypeFor[allocator.ProbeFile]())
	if len(file) != 1 || len(file["probe_jobs"].(map[string]any)) < 8 {
		t.Fatalf("read %v from allocator.ProbeFile; the reader has stopped finding its keys", file)
	}
	if got := schemaKeyTree(probes, probes); !reflect.DeepEqual(got, file) {
		t.Errorf("the probe targets schema declares the keys\n%v\nbut the allocator decodes\n%v", got, file)
	}
	source := keyTree(reflect.TypeFor[allocator.DiscoveryConfig]())
	for where, schema := range map[string]map[string]any{
		"a probe job's":                at(t, probes, "probe_jobs", "[]", "discovery"),
		"a discovered_targets entry's": at(t, static, "discovered_targets", "[]", "discovery"),
	} {
		if got := schemaKeyTree(schema, schema); !reflect.DeepEqual(got, source) {
			t.Errorf("%s discovery source takes the keys\n%v\nbut the allocator's DiscoveryConfig decodes\n%v", where, got, source)
		}
	}

	want := keyTree(reflect.TypeFor[model.StaticTarget]())
	delete(want, "target")
	var leaf map[string]any
	want["name"], want["shard_key"], want["target_template"], want["discovery"] = leaf, leaf, leaf, source
	entry := at(t, static, "discovered_targets", "[]")
	if got := schemaKeyTree(entry, entry); !reflect.DeepEqual(got, want) {
		t.Errorf("a discovered_targets entry takes the keys\n%v\nwant a static target's but target, with name, shard_key, target_template and discovery:\n%v", got, want)
	}
	if entry["additionalProperties"] != false || at(t, static, "discovered_targets", "[]", "discovery", "[]")["additionalProperties"] != false {
		t.Error("a discovered_targets entry or its discovery source takes keys of any name, which the allocator refuses")
	}

	var chart map[string]any
	if err := json.Unmarshal([]byte(readChartFile(t, "values.schema.json")), &chart); err != nil {
		t.Fatal(err)
	}
	chartJob := at(t, chart, "allocator", "probeJobs", "[]")
	if got, want := schemaKeyTree(chart, chartJob), schemaKeyTree(probes, at(t, probes, "probe_jobs", "[]")); !reflect.DeepEqual(got, want) {
		t.Errorf("values.schema.json declares a probe job's keys\n%v\nand configs/probe-targets.schema.json\n%v", got, want)
	}
}

// The examples of docs/ALLOCATOR.md of a probe targets file or a static
// target file with discovered_targets are valid against their schemas, and
// each file's section says where its schema is published.
func TestTheAllocatorsExamplesMatchItsSchemas(t *testing.T) {
	raw, err := os.ReadFile("docs/ALLOCATOR.md")
	if err != nil {
		t.Fatal(err)
	}
	probes, static := loadSchemaFile(t, probeTargetsSchemaFile), loadSchemaFile(t, allocatorStaticTargetsSchemaFile)
	counts := map[string]int{}
	for _, block := range regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(raw), -1) {
		document := block[1]
		var schema map[string]any
		switch {
		case strings.Contains(document, "\nprobe_jobs:") || strings.HasPrefix(document, "probe_jobs:"):
			schema = probes
			counts["probe"]++
		case strings.HasPrefix(document, "interval:"):
			schema = static
			counts["static"]++
		default:
			continue
		}
		if problems := schemaProblems(t, schema, document); len(problems) > 0 {
			t.Errorf("an example of docs/ALLOCATOR.md does not match its schema: %v\n%s", problems, document)
		}
	}
	if counts["probe"] < 2 || counts["static"] < 2 {
		t.Errorf("found %d probe targets and %d static target examples in docs/ALLOCATOR.md, want at least two of each", counts["probe"], counts["static"])
	}
	for _, file := range []string{allocatorStaticTargetsSchemaFile, probeTargetsSchemaFile} {
		if !strings.Contains(string(raw), "# yaml-language-server: $schema=https://raw.githubusercontent.com/eenchev/prometheus-universal-exporter/main/"+file) {
			t.Errorf("docs/ALLOCATOR.md does not show the modeline of %s", file)
		}
	}
}
