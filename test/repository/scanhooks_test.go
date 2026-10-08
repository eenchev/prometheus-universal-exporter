package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// That a collector or a static target is found by its name without going
// through the collectors or the targets is held by counting the times they
// are gone through, which the code tells through a hook: collectorsScannedHook
// and targetsScannedHook in internal/exporter, collectorsIndexedHook and
// targetsValidatedHook (the checks of a static target file a reload makes)
// in internal/config. That a read of the verbose self-metrics goes through
// the tracked requests at most once, however many static targets are left
// over past the tracker's limit, is held the same way, by
// requestsScannedHook in internal/exporter; and that the check of a static
// target file finds a collector's placeholders once for the check and reads
// the yaml keys of a request block once for its type, not once for each
// target, by placeholdersParsedHook and yamlKeysReadHook in internal/fetch.
// Only a test sets one. The package declares it, as a pointer nothing is
// stored in, and reads it in one place, where a test's function is called
// when there is one; so the exporter itself calls nothing there, and pays
// one load of the pointer for each time it goes through them, never one for
// each collector, target or request.
func TestOnlyTestsCountTheTimesTheCollectorsAndTheTargetsAreGoneThrough(t *testing.T) {
	for _, hook := range []struct{ name, dir string }{
		{"collectorsScannedHook", "internal/exporter"},
		{"targetsScannedHook", "internal/exporter"},
		{"requestsScannedHook", "internal/exporter"},
		{"collectorsIndexedHook", "internal/config"},
		{"targetsValidatedHook", "internal/config"},
		{"placeholdersParsedHook", "internal/fetch"},
		{"yamlKeysReadHook", "internal/fetch"},
	} {
		files, err := filepath.Glob(filepath.Join(hook.dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		declared, read, set := 0, 0, 0
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				switch {
				case !strings.Contains(line, hook.name), strings.HasPrefix(line, "//"):
				case strings.HasSuffix(file, "_test.go"):
					if strings.Contains(line, hook.name+".Store(&") {
						set++
					}
				case line == "var "+hook.name+" atomic.Pointer[func()]":
					declared++
				case line == "if hook := "+hook.name+".Load(); hook != nil {":
					read++
				default:
					t.Errorf("%s: %q: only a test may set %s or use it otherwise than to call the function a test stored in it", file, line, hook.name)
				}
			}
		}
		if declared != 1 || read != 1 || set == 0 {
			t.Errorf("%s is declared %d times in %s, read %d times and set by the tests %d times; want one declaration, one place that reads it and a test that sets it: the test looks for a name that is gone", hook.name, declared, hook.dir, read, set)
		}
	}
}
