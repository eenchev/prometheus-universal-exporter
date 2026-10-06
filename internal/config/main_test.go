package config

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
// failed by it (transform.PythonPool.SetLeastScriptTimeout). An interpreter
// has a minute to start in for the same reason: one start has taken more
// than the exporter's ten seconds (transform.PythonPool.SetStartTimeout).
func TestMain(m *testing.M) {
	transform.PythonWorkers().SetLeastScriptTimeout(time.Minute)
	transform.PythonWorkers().SetStartTimeout(time.Minute)
	os.Exit(m.Run())
}
