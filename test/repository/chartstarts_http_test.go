//go:build !select_request_types || request_type_http

package repository

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// podFiles renders the chart with args and lays out, under a temporary root,
// the files the exporter container sees: each ConfigMap volume's files at its
// mountPath. It returns the root and the container's arguments.
func podFiles(t *testing.T, helm string, args ...string) (root string, containerArgs []string) {
	t.Helper()
	docs := renderedDocuments(t, helm, args...)
	configMaps := map[string]map[string]string{}
	var deployment *yaml.Node
	for _, doc := range docs {
		switch child(doc, "kind").Value {
		case "Deployment":
			deployment = doc
		case "ConfigMap":
			files := map[string]string{}
			if data := child(doc, "data"); data != nil {
				for i := 0; i+1 < len(data.Content); i += 2 {
					files[data.Content[i].Value] = data.Content[i+1].Value
				}
			}
			if child(doc, "binaryData") != nil {
				t.Fatal("a file rendered under binaryData; this layout writes text files only")
			}
			configMaps[path(doc, "metadata", "name").Value] = files
		}
	}
	if deployment == nil {
		t.Fatal("no Deployment rendered")
	}
	volumes := map[string]string{}
	for _, volume := range path(deployment, "spec", "template", "spec", "volumes").Content {
		if name := path(volume, "configMap", "name"); name != nil {
			volumes[child(volume, "name").Value] = name.Value
		}
	}
	container := path(deployment, "spec", "template", "spec", "containers", "0")
	root = t.TempDir()
	for _, mount := range child(container, "volumeMounts").Content {
		configMap, ok := volumes[child(mount, "name").Value]
		if !ok {
			continue
		}
		for name, content := range configMaps[configMap] {
			testutil.WriteIn(t, filepath.Join(root, child(mount, "mountPath").Value), name, content)
		}
	}
	for _, arg := range child(container, "args").Content {
		containerArgs = append(containerArgs, arg.Value)
	}
	return root, containerArgs
}

// flagValue returns the value of the --name=value argument, or "".
func flagValue(args []string, name string) string {
	for _, arg := range args {
		if value, found := strings.CutPrefix(arg, "--"+name+"="); found {
			return value
		}
	}
	return ""
}

// A collector_files pattern such as *.yaml in the chart-managed configuration
// matched the static target document, which the chart rendered into the same
// directory, and the exporter refused to start, reading it as a collector
// file. Laid out as the pod sees them, the rendered files now load as the
// exporter loads them at startup: the configuration with the collector file
// the pattern matches and nothing else, and the static target document from
// the path --static-targets-file names, checked against that configuration.
func TestTheExporterStartsWithACollectorPatternBesideStaticTargets(t *testing.T) {
	helm := requireHelm(t)
	values := `config:
  data:
    config.yaml: |
      collector_files: ["*.yaml"]
      collectors:
        - name: example
          request: {type: http, path: /}
          transform: {type: regex}
          metrics: [{name: m, type: gauge, expression: 'x(\d+)'}]
    collectors-more.yaml: |
      collectors:
        - name: more
          request: {type: http, path: /}
          transform: {type: regex}
          metrics: [{name: n, type: gauge, expression: 'y(\d+)'}]
staticTargets:
  enabled: true
  monitor: {enabled: false}
  data: |
    interval: 1m
    targets:
      - collector: more
        target: http://x
`
	root, args := podFiles(t, helm, "-f", valuesFile(t, values))
	configFile, targetFile := flagValue(args, "config.file"), flagValue(args, "static-targets-file")
	if configFile == "" || targetFile == "" {
		t.Fatalf("the container has no --config.file or --static-targets-file: %q", args)
	}
	if filepath.Dir(targetFile) == filepath.Dir(configFile) {
		t.Errorf("--static-targets-file %s is in the configuration directory", targetFile)
	}
	c, err := config.Load(filepath.Join(root, configFile))
	if err != nil {
		t.Fatalf("the exporter refuses the configuration: %v", err)
	}
	if got := strings.Join(testutil.CollectorNames(c), ","); got != "example,more" {
		t.Errorf("collectors %s, want example,more", got)
	}
	targets, err := config.LoadStaticTargets(filepath.Join(root, targetFile))
	if err == nil {
		err = config.ValidateStaticTargets(targets)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(targets, c)
	}
	if err != nil {
		t.Fatalf("the exporter refuses the static target document: %v", err)
	}
}
