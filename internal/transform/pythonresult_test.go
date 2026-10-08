package transform

import (
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// pythonResult reads a worker's answer for a test that has the answer and
// made no run for it (countedPythonResult): how it ended is counted in the
// statistics kept under the collector's name, as a run counts that was
// handed none.
func pythonResult(c *model.Collector, what string, timeout time.Duration, line []byte, err error) (*pythonOutput, error) {
	return countedPythonResult(PythonWorkers().Stats(c.Name), c, what, timeout, line, err)
}
