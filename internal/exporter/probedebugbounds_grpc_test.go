//go:build !select_request_types || request_type_grpc

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A gRPC status the collector accepts is its answer, and the status message
// is the answer's grpc-message header, which the server makes as long as it
// likes: of a megabyte, the report shows its first 1,024 bytes and its
// length, where it had the megabyte in one line, and says the status and
// what a probe would have answered as it did. Under the race detector the
// message is 64 kB.
func TestAGRPCStatusMessageOfAMegabyteIsShownByItsStartInTheReport(t *testing.T) {
	size := alloctest.UnlessRaced(1<<20, 1<<16)
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		return "", status.Error(codes.FailedPrecondition, strings.Repeat("s", size))
	}})
	testutil.CaptureLogs(t)
	yaml := strings.NewReplacer("VERBOSE", "false", "CACHE", "0s", "      descriptors: reflection\n", "      descriptors: reflection\n      accept_codes: [FAILED_PRECONDITION]\n").Replace(grpcExporterConfig)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", yaml))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	trip := debugTripOf(t, server, "queue_stats", upstream.Addr, url.Values{"param_queue": {"orders"}})
	report, old := trip.report(), trip.old()
	assertContains(t, report,
		"-> grpc FAILED_PRECONDITION in ",
		"\n  gRPC status 9\n",
		"\n    Grpc-Message: "+strings.Repeat("s", debugHeaderValueLimit)+fmt.Sprintf("... (%d bytes)\n", size),
		"\n    Grpc-Status: 9\n",
		"A probe would have answered 200 with ",
	)
	if !strings.Contains(old, "\n    Grpc-Message: "+strings.Repeat("s", size)+"\n") || len(old) < size {
		t.Errorf("the report, of %d bytes, did not have the message whole before", len(old))
	}
	if longest, line := longestLine(report); longest > 2*debugHeaderValueLimit || len(report) > 8<<10 {
		t.Errorf("the report is %d bytes and its longest line %d, %.80q", len(report), longest, line)
	}
}
