// Package tier splits the tests into two tiers: the quick one, which
// `make test-quick` runs (go test -short) while a change is being written,
// and the whole suite, which `make test` and CI run. It is imported only by
// tests, and imports nothing of the module, so that every package's tests
// may use it.
package tier

import "testing"

// Slow leaves t out of the quick tier: a test that takes a second or more,
// as the tests of an oracle over many generated inputs, of the chart
// rendered by helm, or of a schedule run on the real clock do. It is not
// skipped anywhere else, so `make test` and CI run it on every change.
func Slow(t testing.TB) {
	t.Helper()
	if testing.Short() {
		t.Skip("slow: make test-quick (go test -short) leaves it out; make test and CI run it")
	}
}
