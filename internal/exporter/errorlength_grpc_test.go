//go:build !select_request_types || request_type_grpc

package exporter

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A gRPC call that a server ends with a status message of megabytes fails
// its probe at the grpc stage in 2,000 bytes: the answer to the scraper and
// the log line's error start as the failure did, with the status code and
// the message, and end with the failure's length, and the failure log
// remembers it by 2,000 bytes with the mark for the length. They were the
// message whole, ten megabytes each. With a message half as long the same
// failure is a repeat to the log, and a message of ordinary length is
// answered as it was.
func TestAGRPCStatusMessageOfMegabytesFailsTheProbeWithinTheBound(t *testing.T) {
	var said atomic.Pointer[string]
	upstream := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: func(context.Context, string, string) (string, error) {
		return "", status.Error(codes.FailedPrecondition, *said.Load())
	}})
	testutil.CaptureLogs(t)
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.NewReplacer("VERBOSE", "false", "CACHE", "0s").Replace(grpcExporterConfig)))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
	logs := &bytes.Buffer{}
	server.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	probe := func(message string) (string, map[string]any) {
		t.Helper()
		said.Store(&message)
		logs.Reset()
		answer := probeOnce(t, server, "/probe?collector=queue_stats&param_queue=orders&target="+url.QueryEscape(upstream.Addr), nil)
		var lines []map[string]any
		for _, record := range loggedRecords(t, logs) {
			if record["msg"] == "probe failed" {
				lines = append(lines, record)
			}
		}
		if answer.Code != http.StatusBadGateway || len(lines) != 1 || lines[0]["stage"] != "grpc" || lines[0]["grpc_code"] != "FAILED_PRECONDITION" {
			t.Fatalf("a message of %d bytes: answered %d, %.200q, logged as %.600v", len(message), answer.Code, answer.Body, lines)
		}
		text, _ := lines[0]["error"].(string)
		if answer.Body.String() != "collector queue_stats grpc failed: "+text+"\n" {
			t.Fatalf("a message of %d bytes: answered %d bytes, %.200q, and logged %d", len(message), answer.Body.Len(), answer.Body, len(text))
		}
		for _, kept := range rememberedTexts(server) {
			if len(kept) > model.MaxFailureBytes {
				t.Fatalf("a message of %d bytes: the failure log remembers the failure by %d bytes", len(message), len(kept))
			}
		}
		return text, lines[0]
	}
	size := alloctest.UnlessRaced(10<<20, 1<<20)
	const starts = "gRPC call failed: FAILED_PRECONDITION: sss"
	text, line := probe(strings.Repeat("s", size))
	if mark := fmt.Sprintf("sss... (%d bytes)", size+len("gRPC call failed: FAILED_PRECONDITION: ")); len(text) != model.MaxFailureBytes || !strings.HasPrefix(text, starts) || !strings.HasSuffix(text, mark) || line["repeat"] != nil {
		t.Errorf("a message of %d bytes fails the probe in %d bytes, %.80q ... %q", size, len(text), text, text[max(0, len(text)-40):])
	}
	if kept := rememberedTexts(server); len(kept) != 1 || len(kept[0]) != model.MaxFailureBytes || !strings.HasSuffix(kept[0], "sss... (# bytes)") {
		t.Errorf("the failure is remembered by %.80q", kept)
	}
	if again, line := probe(strings.Repeat("s", size/2)); line["repeat"] != true || line["level"] != "DEBUG" || again == text {
		t.Errorf("a message half as long is logged as a repeat %v at %v, in the same text %t", line["repeat"], line["level"], again == text)
	}
	if short, _ := probe("the queue is paused"); short != "gRPC call failed: FAILED_PRECONDITION: the queue is paused" {
		t.Errorf("a message of ordinary length fails the probe with %q", short)
	}
}
