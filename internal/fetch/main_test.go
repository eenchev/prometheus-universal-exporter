package fetch

import (
	"os"
	"testing"
	"time"
)

// testsTLSHandshakeTimeout is what a TLS handshake has in the tests, where
// the exporter gives it ten seconds, and testsGRPCConnectTimeout what an
// attempt to connect to a grpc target has, where the exporter gives it
// twenty.
const (
	testsTLSHandshakeTimeout = 30 * time.Second
	testsGRPCConnectTimeout  = 30 * time.Second
)

// TestMain leaves every TLS handshake the tests make with a target half a
// minute, the bound of a hang: the exporter's ten seconds are a limit a
// handshake of a few milliseconds has to keep to on a machine with every CPU
// busy elsewhere, under the race detector, and a test that is not about the
// limit would fail by it (SetTLSHandshakeTimeout). A test of the limit sets a
// short one on pools of its own. An attempt to connect to a grpc target has
// the half minute too, where the exporter gives it twenty seconds, which
// every test that calls a grpc server runs under (SetGRPCConnectTimeout).
func TestMain(m *testing.M) {
	SetTLSHandshakeTimeout(testsTLSHandshakeTimeout)
	SetGRPCConnectTimeout(testsGRPCConnectTimeout)
	os.Exit(m.Run())
}
