//go:build request_type_s3file

package exporter

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// s3Store is a stand-in for a store that speaks the S3 API, path-style:
// HeadObject, GetObject and ListObjectsV2 of the objects of one bucket,
// metrics, without checking signatures. requests counts the requests it
// answered.
type s3Store struct {
	server   *httptest.Server
	mu       sync.Mutex
	objects  map[string]string
	modified time.Time
	requests int
}

func newS3Store(t *testing.T, objects map[string]string) *s3Store {
	t.Helper()
	s := &s3Store{objects: objects, modified: time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(t.TempDir(), "none"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(t.TempDir(), "none"),
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_ACCESS_KEY_ID": "AKIDTEST", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_REGION": "",
		"AWS_PROFILE": "", "AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "",
	} {
		t.Setenv(key, value)
	}
	t.Cleanup(func() { fetch.FollowConfiguration(nil, nil) })
	return s
}

func (s *s3Store) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != "metrics" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code></Error>`))
		return
	}
	if key == "" && r.URL.Query().Get("list-type") == "2" {
		prefix := r.URL.Query().Get("prefix")
		type object struct {
			Key          string `xml:"Key"`
			LastModified string `xml:"LastModified"`
			ETag         string `xml:"ETag"`
			Size         int    `xml:"Size"`
		}
		var result struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			IsTruncated bool     `xml:"IsTruncated"`
			Contents    []object `xml:"Contents"`
		}
		for k, body := range s.objects {
			if rest, ok := strings.CutPrefix(k, prefix); ok && !strings.Contains(rest, "/") {
				result.Contents = append(result.Contents, object{k, s.modified.Format("2006-01-02T15:04:05.000Z"), `"e1"`, len(body)})
			}
		}
		sort.Slice(result.Contents, func(i, j int) bool { return result.Contents[i].Key < result.Contents[j].Key })
		_ = xml.NewEncoder(w).Encode(result)
		return
	}
	if key == "" {
		return
	}
	body, ok := s.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Last-Modified", s.modified.Format(http.TimeFormat))
	w.Header().Set("ETag", `"e1"`)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(body))
	}
}

// answered is how many requests the store answered.
func (s *s3Store) answered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// s3Request is a collector's request block reading under /exports of the
// store, with keys set beside type, root and the store's settings.
func (s *s3Store) s3Request(keys string) string {
	return `{type: s3file, root: /exports, endpoint: "` + s.server.URL + `", path_style: true, region: eu-west-1, ` + keys + `}`
}

// A probe reads the object and passes its series through, its verbose
// series labelled with its s3:// URL and READ; a debug probe shows the read
// by that URL; the collector has a series of targets refused, as every type
// whose target a probe names.
func TestAnS3FileCollectorProbe(t *testing.T) {
	s := newS3Store(t, map[string]string{"exports/app.prom": "# TYPE app_jobs_total counter\napp_jobs_total 7\n"})
	server := labelServer(t, `web:
  self_metrics:
    verbose: true
collectors:
  - name: objects
    request: `+s.s3Request("path: app.prom")+`
    transform:
      type: prometheus
`)
	recorder := probeOnce(t, server, "/probe?collector=objects&target=s3://metrics", nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "app_jobs_total 7") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	for _, want := range []string{
		`http_exporter_scrapes_total{collector="objects",http_method="READ",url="s3://metrics/exports/app.prom"} 1`,
		`http_exporter_targets_refused_total{collector="objects"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %q:\n%s", want, metrics)
		}
	}
	server.SetProbeDebug(true)
	body := probeOnce(t, server, "/probe?debug=true&collector=objects&target=metrics", nil).Body.String()
	if !strings.Contains(body, "1. READ s3://metrics/exports/app.prom -> read in") || strings.Contains(body, "secret") {
		t.Errorf("the debug report does not show the read, or shows the secret:\n%s", body)
	}
}

// A probe without a target, or with one that is no bucket, is answered 400,
// and one of a bucket the collector's denied_targets lists 403, before
// anything is sent.
func TestS3FileProbeTargets(t *testing.T) {
	s := newS3Store(t, map[string]string{"exports/app.prom": "app 1\n"})
	server := labelServer(t, `collectors:
  - name: objects
    request: `+s.s3Request("path: app.prom, denied_targets: [secret-*]")+`
    transform:
      type: prometheus
`)
	for query, want := range map[string]int{
		"/probe?collector=objects": http.StatusBadRequest,
		"/probe?collector=objects&target=" + url.QueryEscape("s3://metrics/x.prom"): http.StatusBadRequest,
		"/probe?collector=objects&target=Metrics":                                   http.StatusBadRequest,
		"/probe?collector=objects&target=secret-keys":                               http.StatusForbidden,
	} {
		if recorder := probeOnce(t, server, query, nil); recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
	}
	if s.answered() != 0 {
		t.Errorf("the store answered %d requests", s.answered())
	}
}

// An s3file collector's label values take the probe's parameters as its
// path does; one no placeholder names is refused as unused, and a collector
// reading a prefix answers each object's series labelled with its name,
// beside the series a directory read adds, under localfile's names.
func TestAnS3FileCollectorsLabelsTakeAProbeParameter(t *testing.T) {
	s := newS3Store(t, map[string]string{
		"exports/app.prom":     "# TYPE app_jobs_total counter\napp_jobs_total 7\n",
		"exports/billing.prom": "# TYPE app_jobs_total counter\napp_jobs_total 3\n",
	})
	server := labelServer(t, `collectors:
  - name: apps
    request: `+s.s3Request(`path: "{{param_app:app}}.prom"`)+`
    transform:
      type: prometheus
      labels:
        app: "{{param_app:app}}"
  - name: dir
    request: `+s.s3Request(`files: ["*.prom"]`)+`
    transform:
      type: prometheus
`)
	for query, want := range map[string][]string{
		"collector=apps":                   {`app_jobs_total{app="app"} 7`},
		"collector=apps&param_app=billing": {`app_jobs_total{app="billing"} 3`},
		"collector=dir":                    {`app_jobs_total{file="app.prom"} 7`, `app_jobs_total{file="billing.prom"} 3`, `localfile_files_skipped 0`, `localfile_mtime_seconds{file="app.prom"} 1.792476e+09`, `localfile_mtime_seconds{file="billing.prom"} 1.792476e+09`, `localfile_scrape_error{file="app.prom"} 0`, `localfile_scrape_error{file="billing.prom"} 0`},
	} {
		recorder := probeOnce(t, server, "/probe?"+query+"&target=metrics", nil)
		got := seriesLines(recorder.Body.String())
		if recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: %d %v, want %v", query, recorder.Code, got, want)
		}
	}
	for query, want := range map[string]string{
		"collector=apps&param_ap=x":   `probe parameters param_ap are not used by collector "apps": no placeholder in its request.path ("{{param_app:app}}.prom") or its label values names them`,
		"collector=dir&path=app.prom": `reads every object under request.root that request.files matches, so there is no file for it to name`,
	} {
		recorder := probeOnce(t, server, "/probe?"+query+"&target=metrics", nil)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%s: %d %s, want 400 saying %q", query, recorder.Code, recorder.Body, want)
		}
	}
}

// A Python transform sees an object as localfile's file, with its entity
// tag besides: no status, and the content type from the extension, the
// length and the ETag as its headers.
func TestAnS3FileCollectorWithPython(t *testing.T) {
	requirePython(t)
	s := newS3Store(t, map[string]string{"exports/status.json": `{"temperature": 41.5}`})
	server := labelServer(t, `collectors:
  - name: status
    request: `+s.s3Request("path: status.json")+`
    transform:
      type: python
      script: |
        metric(name="no_status", value=1 if response.status_code is None else 0)
        metric(name="json_file", value=1 if response.header("content-type") == "application/json" else 0)
        metric(name="file_bytes", value=float(response.header("content-length")))
        metric(name="tagged", value=1 if response.header("etag") == '"e1"' else 0)
        metric(name="temperature", value=data["temperature"])
`)
	recorder := probeOnce(t, server, "/probe?collector=status&target=metrics", nil)
	if got, want := seriesLines(recorder.Body.String()), []string{"file_bytes 21", "json_file 1", "no_status 1", "tagged 1", "temperature 41.5"}; recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v", recorder.Code, got, want)
	}
}

// A static target of an s3file collector names its bucket, and may name
// another object with its path.
func TestS3FileStaticTargets(t *testing.T) {
	s := newS3Store(t, map[string]string{"exports/app.prom": "# TYPE app_jobs_total counter\napp_jobs_total 7\n", "exports/other.prom": "# TYPE app_jobs_total counter\napp_jobs_total 2\n"})
	cfg := loadS3Config(t, `collectors:
  - name: objects
    request: `+s.s3Request("path: app.prom")+`
    transform:
      type: prometheus
`)
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "app", Collector: "objects", Target: "s3://metrics"},
		{Name: "other", Collector: "objects", Target: "metrics", Request: model.TargetRequestConfig{Path: "other.prom", PathSet: true}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(t.Context(), 0)
	recorder := probeOnce(t, server, "/static-targets", nil)
	for _, want := range []string{`app_jobs_total{static_target="app"} 7`, `app_jobs_total{static_target="other"} 2`} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("missing %q:\n%s", want, recorder.Body)
		}
	}
}

// loadS3Config loads a configuration written as a user writes one.
func loadS3Config(t *testing.T, document string) *model.Config {
	t.Helper()
	cfg, err := config.Load(testutil.WriteIn(t, t.TempDir(), "config.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The collectors page hints an s3file collector's target as a bucket, which
// it requires.
func TestTheCollectorsPageHintsAnS3FileTarget(t *testing.T) {
	s := newS3Store(t, nil)
	server := labelServer(t, `collectors:
  - name: objects
    request: `+s.s3Request("path: app.prom")+`
    transform:
      type: prometheus
`)
	if page := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(page, `placeholder="the bucket, or s3://bucket"`) {
		t.Errorf("the page does not hint a bucket:\n%s", page)
	}
}
