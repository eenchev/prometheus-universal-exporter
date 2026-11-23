//go:build !select_request_types || request_type_http

package exporter

import (
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a script printed before it failed is written with its failure, in
// the failure's own line: once, the first time it fails so, and not at
// every scrape after, as a line of its own was. A script that fails having
// printed nothing has none.
func TestWhatAFailedScriptPrintedIsLoggedWithItsFailureAndNotAtEveryScrape(t *testing.T) {
	requirePython(t)
	out := testutil.CaptureLogs(t)
	target := textTarget(t, "value=42\n")
	server := modeServer(t,
		pythonCollector("printing", "print('shard 4 answered', 503)\nraise ValueError('bad data')\n"),
		pythonCollector("silent", "raise ValueError('bad data')\n"))
	for range 4 {
		for _, collector := range []string{"printing", "silent"} {
			probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(target.URL), nil)
		}
	}
	printing, silent, lines := 0, 0, 0
	for _, record := range logRecords(t, out) {
		if strings.Contains(out.String(), "printed before it failed") {
			t.Fatalf("what a failed script printed is a line of its own:\n%s", out)
		}
		printed, has := record["printed"]
		switch record["collector"] {
		case "printing":
			if lines++; has {
				if printing++; printed != "shard 4 answered 503\n" {
					t.Errorf("the failure is logged with printed=%q", printed)
				}
			}
		case "silent":
			if has {
				silent++
			}
		}
	}
	if printing != 1 || silent != 0 || lines == 0 {
		t.Fatalf("what the script printed is in %d of its %d lines, want 1, and in %d of the lines of a script that printed nothing:\n%s", printing, lines, silent, out)
	}
}
