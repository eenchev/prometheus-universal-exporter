//go:build !select_request_types || request_type_grpc

package config

import (
	"os"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// A descriptor set renamed over by a prepared file of the same modification
// time, size and permissions, whose method is renamed to one of the same
// length, changes only which file the path leads to, its inode: the
// collector's call reads the new file, and the watch, which stamps the
// file's identity as the call does, reloads at the next tick and checks the
// configuration against it, which no longer has the method, so the reload
// is refused, logged, and last_reload_successful is 0.
func TestADescriptorSetRenamedOverWithTheSameTimeAndSizeReloads(t *testing.T) {
	p, set, content, logs := descriptorPair(t)
	st, err := os.Stat(set)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.ReplaceAll(content, "GetStats", "GetStatz")
	if len(renamed) != len(content) || renamed == content {
		t.Fatal("no rename of the method keeps the size")
	}
	next := set + ".new"
	if err := os.WriteFile(next, []byte(renamed), st.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, set); err != nil {
		t.Fatal(err)
	}
	c := p.manager.Get().Collectors[0]
	if err := fetch.ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), "has no method GetStats; it has GetStatz") {
		t.Fatalf("the collector's descriptors as a call reads them: %v", err)
	}
	p.manager.reloadChanged()
	successes, failures := p.reloads(reloadFileConfig)
	if successes != 0 || failures != 1 || lastReloadSuccessful(t, p.manager, reloadFileConfig) != 0 {
		t.Errorf("after the rename over: %d reloads accepted, %d refused, last_reload_successful %v", successes, failures, lastReloadSuccessful(t, p.manager, reloadFileConfig))
	}
	if !strings.Contains(logs.String(), "GetStats") {
		t.Errorf("the refused reload logged %q, without the method it lacks", logs)
	}
	logs.Reset()
	p.manager.reloadChanged()
	if logs.Len() != 0 {
		t.Errorf("the tick after the refused reload logged %q", logs)
	}
}
