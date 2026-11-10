package repository

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
	"gopkg.in/yaml.v3"
)

// The line every template of the chart begins with.
const requireBlocksLine = `{{- include "prometheus-universal-exporter.requireBlocks" . -}}` + "\n"

// Helm reads a null as "delete this key and its defaults", and an empty
// `ingress:` in a values file, its keys commented out, is a null: the
// templates then failed with "nil pointer evaluating interface {}.enabled",
// which names no key. Every template begins with the check that names the
// block, so the message is the same whichever Helm renders first.
func TestEveryChartTemplateChecksTheBlocksOfValuesFirst(t *testing.T) {
	templates, err := filepath.Glob(chartDir + "/templates/*")
	if err != nil || len(templates) < 10 {
		t.Fatalf("found %d templates: %v", len(templates), err)
	}
	for _, path := range templates {
		if strings.HasSuffix(path, "_helpers.tpl") {
			continue
		}
		if !strings.HasPrefix(read(t, path), requireBlocksLine) {
			t.Errorf("%s does not begin with %q", path, strings.TrimSpace(requireBlocksLine))
		}
	}
}

// A block of values written as null is refused naming it, with what to do,
// or renders, as a feature's block does that is off when null; no block of
// values.yaml, at any depth, fails with an error of the templates' own
// that names none.
func TestChartNamesABlockOfValuesThatIsNull(t *testing.T) {
	tier.Slow(t)
	helm := requireHelm(t)
	for _, c := range []struct{ values, block, key string }{
		{"ingress:\n#  enabled: true\n", "ingress", "ingress"},
		{"autoscaling: null\n", "autoscaling", "autoscaling"},
		{"image:\n", "image", "image"},
		{"server: ~\n", "server", "server"},
		{"staticTargets:\n  monitor:\n", "staticTargets.monitor", "monitor"},
		{"allocator:\n  enabled: true\n  pdb:\n", "allocator.pdb", "pdb"},
	} {
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, c.values))
		want := c.block + " is null, which removes every default the chart has under it: an empty `" + c.key + ":` in a values file, its keys commented out, or --set " + c.block + "=null. Delete the key to keep the defaults, or write the keys you mean under it"
		if ok || !strings.Contains(out, want) {
			t.Errorf("%q: ok=%v, want %q:\n%s", c.values, ok, want, out)
		}
	}
	for _, values := range []string{"allocator:\n", "webAuth:\n", "kubernetesAccess:\n", "healthProbes:\n", "resources:\n", "podSecurityContext:\nsecurityContext:\n"} {
		if out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, values)); !ok {
			t.Errorf("%q does not render:\n%s", values, out)
		}
	}

	var defaults yaml.Node
	if err := yaml.Unmarshal([]byte(read(t, chartDir+"/values.yaml")), &defaults); err != nil {
		t.Fatal(err)
	}
	var blocks [][]string
	var walk func(node *yaml.Node, path []string)
	walk = func(node *yaml.Node, path []string) {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if value := node.Content[i+1]; value.Kind == yaml.MappingNode {
				at := append(append([]string{}, path...), node.Content[i].Value)
				blocks = append(blocks, at)
				walk(value, at)
			}
		}
	}
	walk(defaults.Content[0], nil)
	if len(blocks) < 40 {
		t.Fatalf("found %d blocks in values.yaml; it has moved", len(blocks))
	}
	// One render of the chart for each block: side by side, they take a
	// fraction of the time they take in turn.
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, runtime.GOMAXPROCS(0))
	for _, block := range blocks {
		var values strings.Builder
		for depth, key := range block {
			values.WriteString(strings.Repeat("  ", depth))
			values.WriteString(key)
			values.WriteString(":\n")
		}
		// A block under the allocator's or the static targets' is read when
		// they are on.
		if len(block) > 1 && (block[0] == "allocator" || block[0] == "staticTargets") {
			values.WriteString("  enabled: true\n")
		}
		file := valuesFile(t, values.String())
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			out, ok := helmTemplate(t, helm, chartDir, "-f", file)
			if ok {
				return
			}
			for _, unnamed := range []string{"nil pointer", "error calling", "wrong type", "interface conversion", "can't evaluate"} {
				if strings.Contains(out, unnamed) {
					t.Errorf("%s null fails without naming it (%s):\n%s", strings.Join(block, "."), unnamed, out)
					break
				}
			}
		})
	}
}
