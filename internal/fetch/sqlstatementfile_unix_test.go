//go:build (!select_request_types || request_type_sql) && unix

package fetch

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A named pipe at the statement file's path is refused without being
// opened, which would hold the load until something wrote to it.
func TestAStatementFileThatIsANamedPipeIsRefusedUnopened(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "jobs.sql")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no named pipe here: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- statementFileLoadError(fifo) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "is no regular file (p---------); name a file that holds the statement") {
			t.Errorf("%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the load waited on the named pipe")
	}
}
