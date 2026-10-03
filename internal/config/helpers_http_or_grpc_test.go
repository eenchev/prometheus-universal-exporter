//go:build !select_request_types || request_type_http || request_type_grpc

package config

import (
	"os"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

type pair struct {
	manager               *Manager
	configPath, targetsAt string
}

func newPair(t *testing.T, config, targets string) *pair {
	t.Helper()
	dir := t.TempDir()
	p := &pair{configPath: testutil.WriteIn(t, dir, "config.yaml", config), targetsAt: testutil.WriteIn(t, dir, "targets.yaml", targets)}
	cfg, err := Load(p.configPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := LoadStaticTargets(p.targetsAt)
	if err == nil {
		err = ValidateStaticTargets(file)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.manager = NewManager(cfg, p.configPath, testutil.QuietLogger(t))
	p.manager.SetTargets(p.targetsAt, file)
	return p
}

// write rewrites a file with a modification time later than any read so far,
// as a watch tick would find it.
func (p *pair) write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Duration(len(body)) * time.Millisecond).Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func (p *pair) reloads(file string) (successes, failures uint64) {
	st := p.manager.Reloads.files[file]
	if st == nil {
		return 0, 0
	}
	return st.successes, st.failures
}
