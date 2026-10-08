//go:build !select_request_types || request_type_grpc

package fetch

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// The limit the documentation states: a filesystem that gave an unchanged
// file another inode number at every stat would make every call compile
// its files again, since the identity is what the filesystem reports and
// nothing else is looked at to tell an unchanged file. An identity that
// stays, as every local filesystem's does, compiles nothing after the first
// call.
func TestAnIdentityThatChangesAtEveryLookCompilesAtEveryCall(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, importingService, serviceSource)
	writeProto(t, dir, importedTypes, typesSource)
	writeProto(t, dir, importedBase, baseSource)
	c := importingCollector(dir, dir)
	if _, err := staticMethod(c); err != nil {
		t.Fatal(err)
	}
	if n := compiles(func() {
		for range 10 {
			_, _ = staticMethod(c)
		}
	}); n != 0 {
		t.Fatalf("10 calls of unchanged files with a stable identity compiled %d times", n)
	}
	if _, _, ok := FileIdentity(statOf(t, dir)); !ok {
		t.Skip("the system gives no file identity here; the links are resolved instead")
	}
	inner := statIdentity
	var next atomic.Uint64
	statIdentity = func(st os.FileInfo) (uint64, uint64, bool) {
		dev, _, ok := inner(st)
		return dev, next.Add(1), ok
	}
	t.Cleanup(func() { statIdentity = inner })
	if n := compiles(func() {
		for range 10 {
			if _, err := staticMethod(c); err != nil {
				t.Fatal(err)
			}
		}
	}); n != 10 {
		t.Errorf("10 calls of unchanged files whose inode number changes at every look compiled %d times, want 10", n)
	}
}

func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// What a chmod does, literally: a file missing from an earlier import path
// is looked for in the next, but one there that cannot be read fails the
// call with permission denied, and a later import path's file of that name
// is not read in its place. Run as root, which reads a file whatever its
// permissions, the chmod only makes the next call compile the files once
// more, from the file in the earlier import path, and it succeeds; run as
// another user (or as root without CAP_DAC_OVERRIDE and CAP_DAC_READ_SEARCH,
// under setpriv --bounding-set=-dac_override,-dac_read_search) the test sees
// the call fail.
func TestAnUnreadableFileInAnEarlierImportPathFailsTheCallWhereAMissingOneIsLookedForInTheNext(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeProto(t, first, importingService, serviceSource)
	writeProto(t, first, importedTypes, typesSource)
	writeProto(t, second, importedBase, baseSource)
	c := importingCollector(first, first, second)
	if _, err := staticMethod(c); err != nil {
		t.Fatalf("with base.proto only in the second import path: %v", err)
	}
	early := writeProto(t, first, importedBase, baseSource)
	if err := os.Chmod(early, 0); err != nil {
		t.Fatal(err)
	}
	readable := false
	if f, err := os.Open(early); err == nil {
		readable = true
		_ = f.Close()
	}
	var err error
	if n := compiles(func() { _, err = staticMethod(c) }); n != 1 {
		t.Errorf("the call after an unreadable base.proto appeared in the first import path compiled %d times, want once", n)
	}
	switch {
	case readable && err != nil:
		t.Errorf("run with the file still readable, the call gave %v", err)
	case !readable && (err == nil || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), early)):
		t.Errorf("the call with base.proto unreadable in the first import path gave %v, want permission denied naming it", err)
	}
	if readable {
		t.Log("the file stays readable after chmod 0 (root with CAP_DAC_OVERRIDE): only the compile once more is seen")
	}
}
