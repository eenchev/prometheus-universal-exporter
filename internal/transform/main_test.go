package transform

import (
	"os"
	"testing"
	"time"
)

// TestMain leaves every Python script the tests run on the process's own
// worker pool at least a minute, as usePythonPool does on the pool it gives a
// test: on a machine with every CPU busy elsewhere a script that takes a
// millisecond has overrun the default limits.script_timeout, 100ms, and a
// test that is not about the timeout failed by it. A test of the timeout
// itself runs on a pool of its own (holdScriptsToTheirTimeout). An
// interpreter has a minute to start in for the same reason: one start has
// taken more than the exporter's ten seconds.
func TestMain(m *testing.M) {
	PythonWorkers().SetLeastScriptTimeout(time.Minute)
	PythonWorkers().SetStartTimeout(time.Minute)
	os.Exit(m.Run())
}
