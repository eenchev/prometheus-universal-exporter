//go:build !select_request_types || request_type_localfile

package exporter

import (
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A localfile probe that carries on past a failed stage is answered as any
// other is (probeTrip, carriedOnAnswer): an empty exposition in the format
// asked for.

// fileFormats are the text format and OpenMetrics as a probe asks for them,
// with the Content-Type of each and what an exposition ends in.
var fileFormats = []struct {
	name, accept, contentType, ending string
}{
	{"text", "", expositionContentType, ""},
	{"openmetrics", prometheus3Accept, openMetricsType1, "# EOF\n"},
}

// A localfile probe that carries on past a file it cannot read, one it
// cannot decode or one its transform cannot read is answered 200 with an
// empty exposition, under log and ignore, in the text format and in
// OpenMetrics, compressed for a scrape that accepts gzip: status, headers
// and body are those of a probe of an empty file, whose rules produce no
// series.
func TestALocalFileProbeThatCarriesOnIsAnsweredWithAnEmptyExposition(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "empty.prom", "")
	testutil.WriteIn(t, root, "broken.prom", "this is { not prometheus\n")
	testutil.WriteIn(t, root, "status.json", `{"value": 1}`)
	for _, tc := range []struct {
		name, counter string
		setup         func(c *model.Collector, policy string)
	}{
		{"fetch", "", func(c *model.Collector, policy string) {
			c.Request.Path = "missing.prom"
			c.ErrorHandling.OnFetchError = policy
		}},
		{"decode", "http_exporter_parse_errors_total", func(c *model.Collector, policy string) {
			c.Request.Path = "broken.prom"
			c.ErrorHandling.OnDecodeError = policy
		}},
		{"transform", "http_exporter_transform_errors_total", func(c *model.Collector, policy string) {
			// An xpath transform whose decoder is left to each file fails
			// as a whole when the file turns out to be JSON.
			c.Request.Path = "status.json"
			c.Transform.Type = "xpath"
			c.Metrics = []model.MetricRule{{Name: "demo_value", Type: model.GaugeMetricType, Expression: "//value"}}
			c.ErrorHandling.OnTransformError = policy
		}},
	} {
		for _, policy := range []string{model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
			t.Run(tc.name+"/"+policy, func(t *testing.T) {
				carried := fileCollector("carried", root, "")
				tc.setup(&carried, policy)
				server := fileServer(t, carried, fileCollector("none", root, "empty.prom"))
				probes := 0
				for _, format := range fileFormats {
					for _, encoding := range []string{"", "gzip"} {
						header := http.Header{}
						if format.accept != "" {
							header.Set("Accept", format.accept)
						}
						if encoding != "" {
							header.Set("Accept-Encoding", encoding)
						}
						got := probeOnce(t, server, "/probe?collector=carried", header)
						probes++
						want := http.Header{"Content-Type": {format.contentType}, "X-Content-Type-Options": {"nosniff"}, "Vary": {"Accept-Encoding", "Accept"}}
						if encoding != "" {
							want.Set("Content-Encoding", encoding)
						}
						if got.Code != http.StatusOK || !reflect.DeepEqual(got.Header(), want) {
							t.Errorf("as %s, encoding %q: answered %d with %v, want 200 with %v", format.name, encoding, got.Code, got.Header(), want)
						}
						if encoding == "" && got.Body.String() != format.ending {
							t.Errorf("as %s: the body is %q, want %q", format.name, got.Body, format.ending)
						}
						twin := probeOnce(t, server, "/probe?collector=none", header)
						if got.Code != twin.Code || !reflect.DeepEqual(got.Header(), twin.Header()) || got.Body.String() != twin.Body.String() {
							t.Errorf("as %s, encoding %q: answered %d %v %q\nbut a probe of an empty file is answered %d %v %q",
								format.name, encoding, got.Code, got.Header(), got.Body, twin.Code, twin.Header(), twin.Body)
						}
					}
				}
				stats := selfMetrics(t, server)
				counted := map[string]float64{
					`http_exporter_scrape_success_total{collector="carried"}`:  float64(probes),
					`http_exporter_metrics_emitted_total{collector="carried"}`: 0,
				}
				if tc.counter != "" {
					counted[tc.counter+`{collector="carried"}`] = float64(probes)
				}
				for series, want := range counted {
					if got := seriesValue(t, stats, series); got != want {
						t.Errorf("%s is %v, want %v", series, got, want)
					}
				}
			})
		}
	}
}

// A file of a directory that fails is left out alone whatever error_handling
// says, fail, log or ignore: the probe does not carry on past a stage, it
// goes through whole, and is answered the other files' series and the
// failed file's localfile_scrape_error 1, in the format asked for. A
// directory that cannot be read at all fails the fetch: under log or ignore
// the probe carries on, and is answered an empty exposition.
func TestADirectoryProbeIsAnsweredInTheFormatAskedForWhateverFails(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "logs/good.prom", "# TYPE ok gauge\nok 1\n")
	testutil.WriteIn(t, root, "logs/broken.prom", "this is { not prometheus\n")
	at := time.Unix(1_700_000_000, 0)
	mtime(t, filepath.Join(root, "logs", "good.prom"), at)
	mtime(t, filepath.Join(root, "logs", "broken.prom"), at)
	series := []string{
		`ok{file="good.prom"} 1`,
		`localfile_mtime_seconds{file="broken.prom"} 1.7e+09`,
		`localfile_mtime_seconds{file="good.prom"} 1.7e+09`,
		`localfile_scrape_error{file="broken.prom"} 1`,
		`localfile_scrape_error{file="good.prom"} 0`,
		`localfile_files_skipped 0`,
	}
	for _, policy := range []string{model.ErrorPolicyFail, model.ErrorPolicyLog, model.ErrorPolicyIgnore} {
		t.Run(policy, func(t *testing.T) {
			dir := dirCollector("dir", root, "*.prom")
			dir.ErrorHandling = model.ErrorHandling{OnFetchError: policy, OnDecodeError: policy, OnTransformError: policy}
			server := fileServer(t, dir)
			for _, format := range fileFormats {
				header := http.Header{}
				if format.accept != "" {
					header.Set("Accept", format.accept)
				}
				got := probeOnce(t, server, "/probe?collector=dir&target=logs", header)
				samples, _ := sampleLines(got.Body.String())
				if got.Code != http.StatusOK || got.Header().Get("Content-Type") != format.contentType || !reflect.DeepEqual(samples, series) || !strings.HasSuffix(got.Body.String(), "\n"+format.ending) {
					t.Errorf("the directory as %s: answered %d as %q\n%s\nwant 200 and the samples\n%s", format.name, got.Code, got.Header().Get("Content-Type"), got.Body, strings.Join(series, "\n"))
				}

				got = probeOnce(t, server, "/probe?collector=dir&target=missing", header)
				if policy == model.ErrorPolicyFail {
					if got.Code != http.StatusBadGateway || got.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(got.Body.String(), "collector dir "+fetch.FetchStage(&dir)+" failed: ") {
						t.Errorf("the missing directory under fail, as %s: answered %d as %q: %s", format.name, got.Code, got.Header().Get("Content-Type"), got.Body)
					}
					continue
				}
				if got.Code != http.StatusOK || got.Header().Get("Content-Type") != format.contentType || got.Body.String() != format.ending {
					t.Errorf("the missing directory under %s, as %s: answered %d as %q: %q, want 200 and an empty exposition", policy, format.name, got.Code, got.Header().Get("Content-Type"), got.Body)
				}
			}
		})
	}
}
