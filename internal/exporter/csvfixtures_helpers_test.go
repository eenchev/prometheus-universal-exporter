//go:build !select_request_types || request_type_http || request_type_localfile

package exporter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The files of testdata/csv are CSV in the shapes it comes in, each written
// as the tool it stands for writes it (docs/DEVELOPMENT.md lists them;
// internal/decode's csvfixtures tests hold the rows each decodes into, and
// internal/transform's the series of each). The csvfixtures tests here read
// them the whole way, through /probe: served by a stand-in for an http
// collector (csvfixtures_probe_test.go), and as files for a localfile one
// (csvfixtures_localfile_test.go). The collectors are written in YAML and
// loaded as a configuration file is, so what loading decides, the decoder a
// csv transform implies and the defaults of a rule among it, is in force.

func csvFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/csv/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// csvFixtureServer loads the configuration written in document, as a file,
// and serves it, logging to the default logger, which the test captures.
func csvFixtureServer(t *testing.T, document string) *Server {
	t.Helper()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, "", slog.Default()), "python3", slog.Default())
}

// sampleLines is the sample lines of an exposition, and typeLines its TYPE
// lines, each in the order written.
func sampleLines(body string) (samples, types []string) {
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		switch {
		case strings.HasPrefix(line, "# TYPE "):
			types = append(types, line)
		case line != "" && !strings.HasPrefix(line, "#"):
			samples = append(samples, line)
		}
	}
	return samples, types
}

// sameLines reports how the lines got differ from the ones wanted, whatever
// the order of either.
func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	got, want = slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// answersSeries holds a text exposition against the series and the TYPE
// lines wanted: every sample line and every TYPE line of it, each TYPE line
// once. The lines are compared whatever their order.
func answersSeries(t *testing.T, body string, series, types []string) {
	t.Helper()
	samples, declared := sampleLines(body)
	sameLines(t, "series", samples, series)
	sameLines(t, "TYPE lines", declared, types)
}

// ruleFailureLogs reads what a probe logged: the failures of rules, each as
// "<level> <collector> <metric>: <n> failed: <the first error>", with
// " in <file>" after the metric for a file of a directory, and every other
// line as it was written.
func ruleFailureLogs(t *testing.T, logs fmt.Stringer) (failures, others []string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level, Msg, Collector, Metric, Error, File string
			Failures                                   uint64
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %s: %v", line, err)
		}
		if record.Msg != "metric extraction failed" {
			others = append(others, line)
			continue
		}
		where := record.Metric
		if record.File != "" {
			where += " in " + record.File
		}
		failures = append(failures, fmt.Sprintf("%s %s %s: %d failed: %s", record.Level, record.Collector, where, record.Failures, record.Error))
	}
	return failures, others
}

// loggedOnly reports what a probe logged beyond the rule failures wanted.
func loggedOnly(t *testing.T, logs fmt.Stringer, failures ...string) {
	t.Helper()
	got, others := ruleFailureLogs(t, logs)
	sameLines(t, "logged rule failures", got, failures)
	if len(others) > 0 {
		t.Errorf("also logged:\n%s", strings.Join(others, "\n"))
	}
}
