package exporter

import (
	"os"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// TestMain leaves every Python script the tests run on the process's own
// worker pool at least a minute: on a machine with every CPU busy elsewhere a
// script that takes a millisecond has overrun the default
// limits.script_timeout, 100ms, and a test that is not about the timeout
// failed by it (transform.PythonPool.SetLeastScriptTimeout). usePythonPool does
// the same on the pool it gives a test, and a test of the timeout itself calls
// holdScriptsToTheirTimeout on that pool. An interpreter has a minute to start
// in for the same reason, on both pools: one start has taken more than the
// exporter's ten seconds (transform.PythonPool.SetStartTimeout).
func TestMain(m *testing.M) {
	transform.PythonWorkers().SetLeastScriptTimeout(time.Minute)
	transform.PythonWorkers().SetStartTimeout(time.Minute)
	os.Exit(m.Run())
}
