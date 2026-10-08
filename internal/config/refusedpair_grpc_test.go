//go:build !select_request_types || request_type_grpc

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A target file read alone was checked, to learn which descriptor files its
// check opens (targetsChecked), against the configuration read and the one
// in force, which with no configuration read are one: it looked through
// that one's collectors twice. It now looks once, and twice only when a
// configuration was read with it, which the check opens files of too.
func TestATargetFileReadAloneLooksThroughTheConfigurationInForceOnce(t *testing.T) {
	p, _, _, _ := descriptorPair(t)
	p.write(t, p.targetsAt, messageTargets("invoices"))
	indexes, checks := countCollectorIndexes(t), countTargetChecks(t)
	p.manager.reloadMu.Lock()
	err := p.manager.apply(reloadTriggerWatch, false, true)
	p.manager.reloadMu.Unlock()
	if err != nil || messageInForce(p) != `{"queue": "invoices"}` {
		t.Fatalf("the target file read alone was not put in force: %v", err)
	}
	if got, want := indexes.Load()-checks.Load(), int64(1); got != want || checks.Load() != 1 {
		t.Errorf("the collectors were gone through %d times beside the %d checks, want %d beside 1", got, checks.Load(), want)
	}

	// Read with the configuration, both are looked through.
	p.write(t, p.targetsAt, messageTargets("orders"))
	p.write(t, p.configPath, mustRead(t, p.configPath)+"\n")
	indexes.Store(0)
	checks.Store(0)
	if err := p.manager.Reload(ReloadTriggerHTTP); err != nil {
		t.Fatal(err)
	}
	if got, want := indexes.Load()-checks.Load(), int64(2); got != want || checks.Load() != 1 {
		t.Errorf("with the configuration read too, the collectors were gone through %d times beside the %d checks, want %d beside 1", got, checks.Load(), want)
	}
}

// mustRead is the content of the file at path.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Over generated sequences of reloads of a grpc collector whose descriptor
// set is replaced, removed and put back, and of a target file whose message
// the set refuses or accepts, a reload that checks each pair once, and that
// looks through the configuration in force once for the files a target
// file's check opens, leaves what the reload before did (oldApply): the
// same files in force, errors, log lines in their order, reload metrics,
// and the same files watched for a refused file, stamped alike.
func TestAReloadOfDescriptorFilesThatChecksEachPairOnceDoesWhatItDid(t *testing.T) {
	sequences := alloctest.UnlessRaced(60, 12)
	outcomes := compareReloads(t, sequences, func(t *testing.T, dir string) []reloadFile {
		set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), false)
		content := mustRead(t, set)
		config := strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset\n      protoset_file: "+set, 1)
		return []reloadFile{
			{path: filepath.Join(dir, "config.yaml"), variants: []string{
				config, strings.Replace(config, "name: queue_stats", "name: renamed", 1), "collectors: [\n",
			}},
			{path: filepath.Join(dir, "targets.yaml"), variants: []string{
				messageTargets("orders"), messageTargets("invoices"),
				strings.Replace(messageTargets("orders"), `{\"queue\": \"orders\"}`, `{\"nope\": 1}`, 1),
				strings.Replace(messageTargets("orders"), "collector: queue_stats", "collector: renamed", 1),
				"targets: [\n",
			}},
			{path: set, variants: []string{content, removed, withoutMethod(t, content, "GetStats")}},
		}
	})
	least := sequences / 10
	wantOutcomes(t, outcomes, least, "file installed", "refused for itself, watch true", "refused for itself, watch false", "refused for the other, watch true",
		"refused for the other, watch false", "one went alone, the other refused for it", "a refused target file's check opened files")
}
