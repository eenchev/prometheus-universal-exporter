package exporter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A directory read the deadline or the shutdown cut short
// (fetch/localfile_directory.go) answers with what it read, but is neither
// cached, which would serve the files it did not reach as failed, nor, at
// shutdown, published or logged.

// registerDirectoryFixture registers a request type whose every fetch is a
// directory read of a.prom and slow.prom, the second not reached, cut short
// as cutShort says. fetches counts them.
func registerDirectoryFixture(t *testing.T, cutShort bool) *atomic.Int64 {
	t.Helper()
	fetches := &atomic.Int64{}
	fetch.RequestTypes["dirfixture"] = &fetch.RequestType{
		Name:           "dirfixture",
		Validate:       func(*model.Collector) error { return nil },
		OptionalTarget: true,
		Fetch: func(_ context.Context, target string, c *model.Collector, _ fetch.RequestOverrides, _ http.Header) (*fetch.HTTPResponse, error) {
			fetches.Add(1)
			file := &fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("value=1\n"), Target: target, Collector: c.Name}
			read := &fetch.DirectoryRead{Path: "/data", Matched: 2, CutShort: cutShort, Files: []fetch.FileRead{
				{Name: "a.prom", ModTime: time.Unix(1_000_000, 0), Response: file},
				{Name: "slow.prom", Err: context.DeadlineExceeded},
			}}
			return &fetch.HTTPResponse{StatusCode: http.StatusOK, Target: target, Collector: c.Name, Directory: read}, nil
		},
	}
	t.Cleanup(func() { delete(fetch.RequestTypes, "dirfixture") })
	return fetches
}

func directoryFixtureCollector() model.Collector {
	c := cachingCollector("dir", time.Minute)
	c.Request = model.RequestConfig{Type: "dirfixture"}
	return c
}

func TestADirectoryReadCutShortIsNotCached(t *testing.T) {
	for _, cutShort := range []bool{true, false} {
		fetches := registerDirectoryFixture(t, cutShort)
		server, _ := newCacheTestServer(t, directoryFixtureCollector())
		for range 2 {
			if recorder := probeOnce(t, server, "/probe?collector=dir", nil); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `localfile_scrape_error{file="slow.prom"} 1`) {
				t.Fatalf("cut short=%v: %d %s", cutShort, recorder.Code, recorder.Body)
			}
		}
		want := int64(1)
		if cutShort {
			want = 2
		}
		if got := fetches.Load(); got != want {
			t.Errorf("cut short=%v: %d reads for two probes, want %d", cutShort, got, want)
		}
	}
}

func TestADirectoryReadTheShutdownCutShortIsAborted(t *testing.T) {
	registerDirectoryFixture(t, true)
	logs := testutil.CaptureLogs(t)
	server, manager := newCacheTestServer(t, directoryFixtureCollector())
	c := &manager.Get().Collectors[0]
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errShuttingDown)
	result := server.collect(ctx, collectJob{
		collector: c, rec: server.recorderFor(server.statsFor(c.Name), c.Name, "", "READ"),
		log: collectLog{key: "k", failed: "static target scrape failed", attrs: []any{"collector", c.Name}},
	})
	if !result.aborted || !errors.Is(result.err, errShuttingDown) {
		t.Fatalf("result=%+v, want aborted by the shutdown", result)
	}
	if strings.Contains(logs.String(), "file of a directory failed") || strings.Contains(logs.String(), "scrape failed") {
		t.Fatalf("the cut-short read was logged as failing:\n%s", logs)
	}
}
