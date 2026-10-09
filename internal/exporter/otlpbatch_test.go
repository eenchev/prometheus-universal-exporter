package exporter

// The requests an OTLP export is sent in: within otlp.batch_max_size data
// points and otlp.batch_max_bytes bytes of JSON each (otlpRequestsOf,
// splitOTLP), one after another, each accounted for by its own points when
// it fails (exportOTLPOnce).

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// pushOTLP sends resources as an export sends them, without the exporter's
// own metrics: in the requests otlpRequestsOf makes of them, one after
// another, stopping at the first that fails. It returns the retries of every
// request and the partial success of the last.
func (s *Server) pushOTLP(ctx context.Context, cfg model.OTLPConfig, resources []otlpResourceSet, budget time.Duration) (int, otlpPartialSuccess, error) {
	requests, err := s.otlpRequestsOf(cfg, resources)
	if err != nil {
		return 0, otlpPartialSuccess{}, err
	}
	deadline := time.Now().Add(budget)
	retries := 0
	var partial otlpPartialSuccess
	for _, body := range requests.bodies {
		var tries int
		tries, partial, err = s.deliverOTLP(ctx, cfg, body, deadline)
		retries += tries
		if err != nil {
			return retries, partial, err
		}
	}
	return retries, partial, nil
}

// otlpBodyAsItWas is the body the export sent for resources before it was
// split into requests, uncompressed when compression says so: every resource
// in one request. It is the oracle of the request an export within both
// bounds is sent as.
func (s *Server) otlpBodyAsItWas(cfg model.OTLPConfig, resources []otlpResourceSet) ([]byte, error) {
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	payload := otlpPayload{}
	s.otlpStarts.bound(cfg.MaxPendingPoints)
	for _, resource := range resources {
		if len(resource.Set.Metrics) == 0 {
			continue
		}
		key := resource.Identity.key()
		metrics, clashes := otlpMetricsOf(resource, now, s.otlpStarts.forResource(key))
		s.logOTLPNameClashes(resource.Identity, key, clashes)
		payload.ResourceMetrics = append(payload.ResourceMetrics, otlpResourceMetrics{Resource: otlpResource{Attributes: resource.Identity.attributes()}, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "prometheus-universal-exporter"}, Metrics: metrics}}})
	}
	if len(payload.ResourceMetrics) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if cfg.Compression == model.OTLPCompressionNone {
		return raw, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// batchServer is a server whose OTLP export goes to endpoint, logging to
// log.
func batchServer(endpoint string, change func(*model.OTLPConfig)) (*Server, *bytes.Buffer) {
	cfg := &model.Config{OTLP: otlpConfig(endpoint)}
	if change != nil {
		change(&cfg.OTLP)
	}
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	return NewServer(config.NewManager(cfg, "", logger), "python3", logger), &log
}

// generatedResources are resources of n series each, made of a few names of
// every type, some sharing a name with another type or with the samples of a
// histogram, some counters negative, which makes their family gauges, and
// some series written twice: what makes otlpMetricsOf leave points out, or
// take the shared path, alongside what it makes as it is.
func generatedResources(r *rand.Rand, resources, n int) []otlpResourceSet {
	names := []string{"a", "b", "c", "c_bucket", "c_count", "d"}
	types := []model.MetricType{model.GaugeMetricType, model.CounterMetricType, model.UntypedMetricType, model.HistogramMetricType, model.SummaryMetricType}
	at := int64(1700000000000)
	var out []otlpResourceSet
	for x := range resources {
		identity := otlpResourceIdentity{ServiceName: "svc", Attributes: map[string]string{"site": strconv.Itoa(x)}}
		resource := otlpResourceSet{Identity: identity}
		for i := range n {
			m := model.Metric{Name: names[r.IntN(len(names))], Type: types[r.IntN(len(types))], Value: float64(r.IntN(100)), Timestamp: &at,
				Labels: map[string]string{"k": strconv.Itoa(r.IntN(4))}}
			switch m.Type {
			case model.CounterMetricType:
				if r.IntN(8) == 0 {
					m.Value = -1
				}
			case model.HistogramMetricType:
				m.Histogram = &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 1}, {UpperBound: 5, CumulativeCount: 3}}, Count: 3, Sum: 4.5, NoSum: r.IntN(3) == 0}
			case model.SummaryMetricType:
				m.Summary = &model.Summary{Quantiles: []model.Quantile{{Quantile: 0.5, Value: math.NaN()}}, Count: 2, Sum: 1}
			}
			resource.Set.Metrics = append(resource.Set.Metrics, m)
			resource.seqs = append(resource.seqs, int64(x*n+i+1))
		}
		out = append(out, resource)
	}
	return out
}

// An export within otlp.batch_max_size and otlp.batch_max_bytes is the one
// request it was before exports were split, byte for byte, gzipped and not:
// the body is the old one's, made by the old code as an oracle, over
// generated resources of every kind of point and name clash, and it costs no
// more allocations than the old one, but the one of the list of requests.
// Told what series its points are of, the conversion of a resource makes
// the same metrics, and as many sources as points.
func TestAnOTLPExportWithinTheBatchBoundsIsTheOneRequestItWas(t *testing.T) {
	r := rand.New(rand.NewPCG(54, 1))
	for _, compression := range []string{model.OTLPCompressionGzip, model.OTLPCompressionNone} {
		for round := range alloctest.UnlessRaced(40, 12) {
			resources := generatedResources(r, 1+round%3, 1+round*3)
			old, _ := batchServer("http://collector.invalid/v1/metrics", nil)
			now, _ := batchServer("http://collector.invalid/v1/metrics", nil)
			cfg := old.manager.Get().OTLP
			cfg.Compression = compression
			want, err := old.otlpBodyAsItWas(cfg, resources)
			if err != nil {
				t.Fatal(err)
			}
			requests, err := now.otlpRequestsOf(cfg, resources)
			if err != nil {
				t.Fatal(err)
			}
			if len(requests.bodies) != 1 || requests.last != nil {
				t.Fatalf("%s, round %d: %d requests, want 1", compression, round, len(requests.bodies))
			}
			if !bytes.Equal(requests.bodies[0], want) {
				t.Fatalf("%s, round %d: the request is not the export it was\nwant %q\ngot  %q", compression, round, want, requests.bodies[0])
			}
			for _, resource := range resources {
				metrics, _ := otlpMetricsOf(resource, "1", nil)
				sources := otlpPointSources{index: map[string]int{}}
				told, _ := otlpMetricsWith(resource, "1", nil, &sources)
				if !slices.EqualFunc(metrics, told, func(a, b otlpMetric) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }) {
					t.Fatalf("round %d: told what series its points are of, the conversion made other metrics", round)
				}
				if len(sources.of) != len(metrics) {
					t.Fatalf("round %d: sources of %d metrics for %d", round, len(sources.of), len(metrics))
				}
				for m := range metrics {
					if len(sources.of[m]) != otlpMetricPoints(metrics[m]) {
						t.Fatalf("round %d: %s has %d points and %d sources", round, metrics[m].Name, otlpMetricPoints(metrics[m]), len(sources.of[m]))
					}
				}
			}
		}
	}
	resources := generatedResources(r, 2, 200)
	server, _ := batchServer("http://collector.invalid/v1/metrics", nil)
	cfg := server.manager.Get().OTLP
	was, _ := alloctest.Allocations(5, func() { _, _ = server.otlpBodyAsItWas(cfg, resources) })
	// The one allocation more is the list of bodies. The count of the same
	// call is not fixed: the conversion goes through maps, whose order
	// differs from run to run and with it how often a slice grows, so the
	// least of five has come out a few apart (6506 against 6508 in CI's
	// single-type runs) and under the race detector up to twenty apart (6875
	// against 6895). So a hundredth of the count is allowed beside it: far
	// less than a second conversion of the points, thousands, would cost.
	most := was + 1 + was/100
	is := alloctest.AllocsAtMost(5, most, func() { _, _ = server.otlpRequestsOf(cfg, resources) })
	if is > most {
		t.Errorf("an export of one request costs %v allocations, and cost %v", is, was)
	}
	t.Logf("an export of one request costs %v allocations, and cost %v", is, was)
}

// requestPoints are the points of an uncompressed request, a line each in
// the order the request has them: its resource's attributes, its scope, its
// metric's name, kind and description, and the point.
func requestPoints(t *testing.T, body []byte) []string {
	t.Helper()
	var payload otlpPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the request is not an export: %v", err)
	}
	var lines []string
	for _, rm := range payload.ResourceMetrics {
		resource, _ := json.Marshal(rm.Resource)
		for _, scope := range rm.ScopeMetrics {
			if scope.Scope.Name != "prometheus-universal-exporter" {
				t.Errorf("a request's scope is %q", scope.Scope.Name)
			}
			for _, m := range scope.Metrics {
				for j := range otlpMetricPoints(m) {
					point, _ := json.Marshal(otlpMetricPoint(m, j))
					lines = append(lines, fmt.Sprintf("%s %s %d %q %s", resource, m.Name, otlpKind(m), m.Description, point))
				}
			}
		}
	}
	return lines
}

// manyResources are two resources, of the default identity and another, of
// n gauges each of three names, and a histogram, with a long label of size
// bytes.
func manyResources(cfg model.OTLPConfig, n, size int) []otlpResourceSet {
	at := int64(1700000000000)
	pad := strings.Repeat("x", size)
	var out []otlpResourceSet
	for x, identity := range []otlpResourceIdentity{defaultResourceIdentity(cfg), {ServiceName: "other", Attributes: map[string]string{"site": "b"}}} {
		resource := otlpResourceSet{Identity: identity}
		for i := range n {
			m := model.Metric{Name: []string{"app_a", "app_b", "app_c"}[i%3], Help: "Some help.", Type: model.GaugeMetricType, Value: float64(i), Timestamp: &at, Labels: map[string]string{"i": fmt.Sprintf("%04d", i), "pad": pad}}
			if i == n/2 {
				m.Name, m.Type, m.Histogram = "app_h", model.HistogramMetricType, &model.Histogram{Buckets: []model.Bucket{{UpperBound: 1, CumulativeCount: 2}}, Count: 2, Sum: 1}
			}
			resource.Set.Metrics = append(resource.Set.Metrics, m)
			resource.seqs = append(resource.seqs, int64(x*n+i+1))
		}
		out = append(out, resource)
	}
	return out
}

// An export over otlp.batch_max_size or otlp.batch_max_bytes is split into
// requests each within both: every point of the export is in one of them,
// in the order of the one request it would be, each request carries the
// resource, the scope and the metric of every point it has, written again
// where a resource or a metric goes on in the next, and the requests are the
// same every time they are made. A request is as full as the bounds let it
// be: one of a point more would be past one of them. A point larger alone
// than otlp.batch_max_bytes is sent in a request of its own.
func TestAnOTLPExportOverTheBatchBoundsIsSplitWithinThem(t *testing.T) {
	cases := []struct {
		name               string
		points, size, pad  int
		maxPoints, maxSize int
	}{
		{name: "by count", points: 25, maxPoints: 7, maxSize: 1 << 20},
		{name: "by bytes", points: 60, pad: 1000, maxPoints: 8192, maxSize: 64 << 10},
		{name: "by both", points: 60, pad: 300, maxPoints: 9, maxSize: 3000},
		{name: "one point past the bytes", points: 6, pad: 70 << 10, maxPoints: 8192, maxSize: 64 << 10},
	}
	for _, c := range cases {
		server, _ := batchServer("http://collector.invalid/v1/metrics", func(o *model.OTLPConfig) {
			o.Compression, o.BatchMaxSize, o.BatchMaxBytes = model.OTLPCompressionNone, c.maxPoints, model.ByteSize(c.maxSize)
		})
		cfg := server.manager.Get().OTLP
		resources := manyResources(cfg, c.points, c.pad)
		whole := cfg
		whole.BatchMaxSize, whole.BatchMaxBytes = math.MaxInt32, math.MaxInt32
		one, err := server.otlpRequestsOf(whole, resources)
		if err != nil {
			t.Fatal(err)
		}
		want := requestPoints(t, one.bodies[0])
		requests, err := server.otlpRequestsOf(cfg, resources)
		if err != nil {
			t.Fatal(err)
		}
		if len(requests.bodies) < 2 {
			t.Fatalf("%s: %d request(s)", c.name, len(requests.bodies))
		}
		var got []string
		for k, body := range requests.bodies {
			points := requestPoints(t, body)
			switch {
			case len(points) > c.maxPoints:
				t.Errorf("%s: request %d has %d points, over %d", c.name, k+1, len(points), c.maxPoints)
			case len(body) > c.maxSize && len(points) > 1:
				t.Errorf("%s: request %d of %d points is %d bytes, over %d", c.name, k+1, len(points), len(body), c.maxSize)
			}
			got = append(got, points...)
			// As full as the bounds let it be.
			if k+1 < len(requests.bodies) {
				next := requestPoints(t, requests.bodies[k+1])[0]
				fuller := append(slices.Clone(points), next)
				if len(fuller) <= c.maxPoints && len(otlpRequestOfLines(t, fuller)) <= c.maxSize {
					t.Errorf("%s: request %d of %d points, %d bytes, had room for the next point", c.name, k+1, len(points), len(body))
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the requests carry other points than the export, or in another order:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		again, _ := server.otlpRequestsOf(cfg, resources)
		if !slices.EqualFunc(requests.bodies, again.bodies, bytes.Equal) {
			t.Errorf("%s: the requests are not the same when made again", c.name)
		}
	}
}

// otlpRequestOfLines is the JSON of a request of the points requestPoints
// gave the lines of, in their order: each line's resource, metric and point,
// grouped as splitOTLP groups them.
func otlpRequestOfLines(t *testing.T, lines []string) []byte {
	t.Helper()
	payload := otlpPayload{}
	lastResource, lastMetric := "", ""
	for _, line := range lines {
		var resource otlpResource
		decoder := json.NewDecoder(strings.NewReader(line))
		if err := decoder.Decode(&resource); err != nil {
			t.Fatal(err)
		}
		var name, description string
		var kind int
		rest := line[decoder.InputOffset()+1:]
		if _, err := fmt.Sscanf(rest, "%s %d %q", &name, &kind, &description); err != nil {
			t.Fatalf("%v: %s", err, rest)
		}
		point := rest[strings.LastIndex(rest, "\" ")+2:]
		resourceKey := line[:decoder.InputOffset()]
		if resourceKey != lastResource {
			payload.ResourceMetrics = append(payload.ResourceMetrics, otlpResourceMetrics{Resource: resource, ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: "prometheus-universal-exporter"}}}})
			lastResource, lastMetric = resourceKey, ""
		}
		scope := &payload.ResourceMetrics[len(payload.ResourceMetrics)-1].ScopeMetrics[0]
		if metricKey := name + " " + strconv.Itoa(kind); metricKey != lastMetric {
			scope.Metrics = append(scope.Metrics, newOTLPMetric(name, description, kind))
			lastMetric = metricKey
		}
		m := &scope.Metrics[len(scope.Metrics)-1]
		var err error
		switch kind {
		case otlpKindHistogram:
			var p otlpHistogramDataPoint
			err = json.Unmarshal([]byte(point), &p)
			m.Histogram.DataPoints = append(m.Histogram.DataPoints, p)
		default:
			var p otlpNumberDataPoint
			err = json.Unmarshal([]byte(point), &p)
			m.Gauge.DataPoints = append(m.Gauge.DataPoints, p)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// splitOTLP measures a request exactly: a payload whose JSON is n bytes is
// one request within n bytes and two within one byte less.
func TestSplitOTLPMeasuresARequestExactly(t *testing.T) {
	server, _ := batchServer("http://collector.invalid/v1/metrics", nil)
	cfg := server.manager.Get().OTLP
	payload, _ := server.otlpPayloadOf(cfg, manyResources(cfg, 40, 10))
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for bound, want := range map[int]int{len(raw): 1, len(raw) - 1: 2} {
		requests, err := splitOTLP(payload, math.MaxInt32, bound)
		if err != nil {
			t.Fatal(err)
		}
		if len(requests) != want {
			t.Errorf("within %d bytes, a payload of %d bytes is %d requests, want %d", bound, len(raw), len(requests), want)
		}
	}
}

// batchEndpoint is an OTLP endpoint that answers the n'th request it gets,
// counting from 1, with what answer says, and keeps the queue label of each
// point of app_queue_depth in each request it accepts.
type batchEndpoint struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests int
	sizes    []int
	accepted map[string]int
}

func newBatchEndpoint(t *testing.T, answer func(n int, body []byte, w http.ResponseWriter, r *http.Request) bool) *batchEndpoint {
	t.Helper()
	e := &batchEndpoint{accepted: map[string]int{}}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		body, err := io.ReadAll(zr)
		if err != nil {
			t.Error(err)
			return
		}
		e.mu.Lock()
		e.requests++
		n := e.requests
		e.sizes = append(e.sizes, len(body))
		e.mu.Unlock()
		if answer != nil && !answer(n, body, w, r) {
			return
		}
		var payload otlpPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		e.mu.Lock()
		for _, rm := range payload.ResourceMetrics {
			for _, m := range rm.ScopeMetrics[0].Metrics {
				if m.Name == "app_queue_depth" {
					for _, p := range m.Gauge.DataPoints {
						for _, a := range p.Attributes {
							if a.Key == "queue" {
								e.accepted[a.Value.StringValue]++
							}
						}
					}
				}
			}
		}
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(e.server.Close)
	return e
}

// queueDepths queues n gauges of app_queue_depth, queue q00000 to q<n-1>,
// which an export sends in that order, before the exporter's own metrics:
// their instance, which is before the queue among the labels, is in the
// same order.
func queueDepths(server *Server, n int) {
	set := model.MetricSet{}
	for i := range n {
		set.Metrics = append(set.Metrics, model.Metric{Name: "app_queue_depth", Type: model.GaugeMetricType, Value: float64(i),
			Labels: map[string]string{"queue": fmt.Sprintf("q%05d", i), "region": "eu-west-1", "instance": fmt.Sprintf("10.0.%03d.%03d:8080", i/250, i%250)}})
	}
	server.queueOTLPResource(set, defaultResourceIdentity(server.manager.Get().OTLP), scrapeTime{})
}

func sumOf(sizes []int) int {
	total := 0
	for _, n := range sizes {
		total += n
	}
	return total
}

// queues are the queue labels from q<from> up to q<to>.
func queues(from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("q%05d", i))
	}
	return out
}

// pendingQueues are the queue labels of the points waiting for export.
func pendingQueues(server *Server) []string {
	server.otlpMu.Lock()
	defer server.otlpMu.Unlock()
	var out []string
	for _, batch := range server.otlpPending {
		for _, pending := range batch.metrics {
			out = append(out, pending.metric.Labels["queue"])
		}
	}
	slices.Sort(out)
	return out
}

// exportCounts are the export self-metrics' successes, failures and points
// dropped.
func exportCounts(server *Server) (successes, failures uint64, dropped int) {
	server.otlp.mu.Lock()
	defer server.otlp.mu.Unlock()
	return server.otlp.successes, server.otlp.failures, server.otlp.dropped
}

// 90,000 points are about 20.5MiB of JSON, more than the OpenTelemetry
// Collector's OTLP/HTTP receiver takes in one request, 20MiB, which it
// refuses with 413: sent as one request, the export was refused and every
// point dropped. With the default bounds it is sent in requests within
// them, which a receiver of that bound takes, and every point is delivered
// once. Under the race detector the export is 9,000 points, past a
// receiver's bound of 2MiB, with otlp.batch_max_bytes 1MiB.
func TestAnOTLPExportOfNinetyThousandPointsIsDeliveredWithinAReceiversBound(t *testing.T) {
	size := alloctest.UnlessRaced(struct{ points, bound, maxBytes int }{90000, 20 << 20, 0}, struct{ points, bound, maxBytes int }{9000, 2 << 20, 1 << 20})
	endpoint := newBatchEndpoint(t, func(_ int, body []byte, w http.ResponseWriter, _ *http.Request) bool {
		if len(body) > size.bound {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return false
		}
		return true
	})
	server, log := batchServer(endpoint.server.URL, func(o *model.OTLPConfig) {
		o.MaxPendingPoints, o.Compression, o.BatchMaxBytes = model.DefaultOTLPMaxPendingPoints, model.OTLPCompressionGzip, model.ByteSize(size.maxBytes)
	})
	queueDepths(server, size.points)
	server.exportOTLP(t.Context(), time.Minute)
	if len(endpoint.accepted) != size.points {
		t.Errorf("the endpoint took %d of the %d points; sizes of the requests: %v\n%s", len(endpoint.accepted), size.points, endpoint.sizes, log)
	}
	for queue, n := range endpoint.accepted {
		if n != 1 {
			t.Fatalf("%s was delivered %d times", queue, n)
		}
	}
	if successes, failures, dropped := exportCounts(server); successes != 1 || failures != 0 || dropped != 0 {
		t.Errorf("the export counted %d successes, %d failures, %d points dropped", successes, failures, dropped)
	}
	if len(endpoint.sizes) < 2 || len(pendingQueues(server)) != 0 {
		t.Errorf("%d requests, %d points left pending", len(endpoint.sizes), len(pendingQueues(server)))
	}
	// The export is past the receiver's bound, as one request.
	if total := sumOf(endpoint.sizes); total <= size.bound {
		t.Errorf("the export is %d bytes of JSON, within the receiver's %d", total, size.bound)
	}
}

// The endpoint refuses the second of an export's requests outright: its
// points, and only its, are dropped and counted, the warning says so with
// their number, once, and the export goes on to deliver the requests after
// it. It is a failed export, as a refused one is.
func TestARefusedOTLPRequestDropsOnlyItsOwnPoints(t *testing.T) {
	endpoint := newBatchEndpoint(t, func(n int, _ []byte, w http.ResponseWriter, _ *http.Request) bool {
		if n == 2 {
			w.WriteHeader(http.StatusBadRequest)
			return false
		}
		return true
	})
	server, log := batchServer(endpoint.server.URL, func(o *model.OTLPConfig) { o.BatchMaxSize = 10 })
	queueDepths(server, 30)
	server.exportOTLP(t.Context(), time.Minute)
	var got []string
	for queue := range endpoint.accepted {
		got = append(got, queue)
	}
	slices.Sort(got)
	if want := append(queues(0, 10), queues(20, 30)...); !slices.Equal(got, want) {
		t.Errorf("the endpoint took %v, want %v", got, want)
	}
	if endpoint.requests < 4 {
		t.Errorf("the export stopped after %d requests", endpoint.requests)
	}
	if successes, failures, dropped := exportCounts(server); successes != 0 || failures != 1 || dropped != 10 {
		t.Errorf("the export counted %d successes, %d failures, %d points dropped, want 0, 1, 10", successes, failures, dropped)
	}
	if lines := strings.Count(log.String(), "refused an export"); lines != 1 || !strings.Contains(log.String(), "dropped_points=10 retries=0 request=2") {
		t.Errorf("%d refusals logged:\n%s", lines, log)
	}
	if pending := pendingQueues(server); len(pending) != 0 {
		t.Errorf("%v left pending", pending)
	}
}

// The second request of an export fails for a reason worth retrying, until
// its budget runs out: its points and those of every request after it are
// kept for the next export, and none of the first request's, which the
// endpoint took. At the last export before exiting the same points are
// dropped instead, counted, and the warning says how many.
func TestATransientlyFailedOTLPRequestKeepsItsPointsAndThoseAfterIt(t *testing.T) {
	previous := otlpRetryBackoff
	otlpRetryBackoff = 10 * time.Millisecond
	t.Cleanup(func() { otlpRetryBackoff = previous })
	for _, last := range []bool{false, true} {
		endpoint := newBatchEndpoint(t, func(n int, _ []byte, w http.ResponseWriter, _ *http.Request) bool {
			if n >= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return false
			}
			return true
		})
		server, log := batchServer(endpoint.server.URL, func(o *model.OTLPConfig) {
			o.BatchMaxSize, o.Timeout = 10, model.Duration(300*time.Millisecond)
		})
		queueDepths(server, 30)
		if last {
			server.FlushOTLP()
		} else {
			server.exportOTLP(t.Context(), 300*time.Millisecond)
		}
		if got := slices.Sorted(func(yield func(string) bool) {
			for queue := range endpoint.accepted {
				if !yield(queue) {
					return
				}
			}
		}); !slices.Equal(got, queues(0, 10)) {
			t.Errorf("last %v: the endpoint took %v", last, got)
		}
		_, failures, dropped := exportCounts(server)
		pending := pendingQueues(server)
		switch {
		case failures != 1:
			t.Errorf("last %v: %d failed exports counted", last, failures)
		case !last && (!slices.Equal(pending, queues(10, 30)) || dropped != 0):
			t.Errorf("kept %v and dropped %d, want q00010 to q00029 kept\n%s", pending, dropped, log)
		case !last && !strings.Contains(log.String(), "kept for the next export"):
			t.Errorf("logged:\n%s", log)
		case last && (len(pending) != 0 || dropped != 20):
			t.Errorf("the last export kept %v and dropped %d, want none kept and 20 dropped\n%s", pending, dropped, log)
		case last && !strings.Contains(log.String(), `msg="the last OTLP export before exiting failed; its data points are dropped"`) || last && !strings.Contains(log.String(), "dropped_points=20"):
			t.Errorf("the last export logged:\n%s", log)
		}
	}
}

// Shutdown cuts an export short while its second request is being sent:
// that request's points and those after it are kept for the last export,
// the first request's are not, and the export is not counted.
func TestAnOTLPExportCutShortKeepsWhatWasNotDelivered(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	endpoint := newBatchEndpoint(t, func(n int, _ []byte, _ http.ResponseWriter, r *http.Request) bool {
		if n == 2 {
			cancel()
			<-r.Context().Done()
			return false
		}
		return true
	})
	server, _ := batchServer(endpoint.server.URL, func(o *model.OTLPConfig) { o.BatchMaxSize = 10 })
	queueDepths(server, 30)
	server.exportOTLP(ctx, time.Minute)
	if pending := pendingQueues(server); !slices.Equal(pending, queues(10, 30)) {
		t.Errorf("kept %v, want q00010 to q00029", pending)
	}
	if successes, failures, dropped := exportCounts(server); successes+failures != 0 || dropped != 0 {
		t.Errorf("counted %d successes, %d failures, %d dropped", successes, failures, dropped)
	}
}

// Each request's partialSuccess is counted: the points two requests
// rejected are dropped together.
func TestThePartialSuccessOfEachOTLPRequestIsCounted(t *testing.T) {
	endpoint := newBatchEndpoint(t, func(n int, _ []byte, w http.ResponseWriter, _ *http.Request) bool {
		if n <= 2 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedDataPoints":"3","errorMessage":"too old"}}`))
			return false
		}
		return true
	})
	server, log := batchServer(endpoint.server.URL, func(o *model.OTLPConfig) { o.BatchMaxSize = 10 })
	queueDepths(server, 30)
	server.exportOTLP(t.Context(), time.Minute)
	if successes, failures, dropped := exportCounts(server); successes != 1 || failures != 0 || dropped != 6 {
		t.Errorf("counted %d successes, %d failures, %d dropped, want 1, 0, 6", successes, failures, dropped)
	}
	if lines := strings.Count(log.String(), "rejected some of its data points"); lines != 2 {
		t.Errorf("%d partial successes logged:\n%s", lines, log)
	}
}
