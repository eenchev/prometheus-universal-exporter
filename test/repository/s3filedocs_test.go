//go:build request_type_s3file

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/S3FILE.md load as written in a build with the type:
// the backup report, the tenant's usage with its placeholder, MinIO's
// prefix with static credentials and the textfile prefix; and its static
// targets load against them.
func TestS3FileDocumentationExamples(t *testing.T) {
	doc := read(t, "docs/S3FILE.md")
	var blocks []string
	for _, part := range strings.Split(doc, "```yaml\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	find := func(prefix string) string {
		for _, block := range blocks {
			if strings.HasPrefix(block, prefix) {
				return block
			}
		}
		t.Fatalf("docs/S3FILE.md has no example starting %q", prefix)
		return ""
	}
	dir := t.TempDir()
	conf := find("collectors:\n") + find("  - name: tenant_usage\n") + find("  - name: minio_textfiles\n") + find("  - name: textfiles\n")
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	if len(cfg.Collectors) != 4 {
		t.Fatalf("%d collectors loaded, want the four examples", len(cfg.Collectors))
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", find("interval: 5m\ntargets:\n")))
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
}
