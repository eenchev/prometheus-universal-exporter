package exporter

import (
	"os"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
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
//
// Two more of the exporter's ten seconds are half a minute in the tests, the
// bound of a hang: the TLS handshake with a target, which every test of an
// https target makes (fetch.SetTLSHandshakeTimeout), and the time a client
// has to send its request's headers in, which every test that asks an HTTP
// server of the exporter's over a real connection runs under
// (SetReadHeaderTimeout). A test of either limit sets a short one, against
// something that never ends. So are the twenty seconds an attempt to connect
// to a grpc target has, which every test that probes a grpc server runs
// under (fetch.SetGRPCConnectTimeout).
func TestMain(m *testing.M) {
	transform.PythonWorkers().SetLeastScriptTimeout(time.Minute)
	transform.PythonWorkers().SetStartTimeout(time.Minute)
	fetch.SetTLSHandshakeTimeout(testsHandshakeAndHeaderTimeout)
	fetch.SetGRPCConnectTimeout(testsHandshakeAndHeaderTimeout)
	SetReadHeaderTimeout(testsHandshakeAndHeaderTimeout)
	os.Exit(m.Run())
}

// testsHandshakeAndHeaderTimeout is what TestMain gives a TLS handshake with
// a target, an attempt to connect to a grpc target and the headers of a
// request to the exporter.
const testsHandshakeAndHeaderTimeout = 30 * time.Second

// exportersReadHeaderTimeout is what the exporter gives a request's headers,
// read before TestMain sets another.
var exportersReadHeaderTimeout = httpReadHeaderTimeout
