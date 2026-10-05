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
// itself runs on a pool of its own (holdScriptsToTheirTimeout).
func TestMain(m *testing.M) {
	PythonWorkers().SetLeastScriptTimeout(time.Minute)
	os.Exit(m.Run())
}
