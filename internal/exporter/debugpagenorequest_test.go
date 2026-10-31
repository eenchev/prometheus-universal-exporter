package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The playground's request block is inert: a playground of any request type
// is read as that type's answer is, and no run makes a request with it — a
// pasted response is decoded as it is, and a target is fetched, or a static
// target scraped, through a configured collector alone. Each type's test
// (debugpagenorequest_<type>_test.go) gives a playground of it whose request
// reaches a listener wherever its type names a host in the request block,
// and requirePlaygroundMakesNoRequest checks that nothing connects to it and
// that every request a run makes is the configured collector's.

// playgroundCase is a playground of one request type.
type playgroundCase struct {
	// collector is a collector of the type named name, as it is written
	// under collectors: without its dash or indentation, whose request
	// reaches addr, a host:port that records what connects to it, wherever
	// its type names a host in the request block. With source set it is
	// the configured collector a target is fetched through, which may name
	// files; otherwise the playground, which may not.
	collector func(name, addr string, source bool) string
	// target is what a probe of the collector names to reach addr, "" for a
	// type that takes none.
	target func(addr string) string
	// body is a response of the type, as the debug page takes one pasted,
	// and series what the playground must make of it.
	body, series string
}

// connRecorder listens on a TCP and a UDP port of one number, on the
// loopback address, and counts what connects to the first and what is sent
// to the second, closing each connection at once.
type connRecorder struct {
	addr string
	mu   sync.Mutex
	hits int
}

func newConnRecorder(t *testing.T) *connRecorder {
	t.Helper()
	for range 20 {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenPacket("udp", tcp.Addr().String())
		if err != nil {
			// The UDP port of that number is taken; try another.
			_ = tcp.Close()
			continue
		}
		r := &connRecorder{addr: tcp.Addr().String()}
		t.Cleanup(func() { _ = tcp.Close(); _ = udp.Close() })
		go func() {
			for {
				conn, err := tcp.Accept()
				if err != nil {
					return
				}
				r.hit()
				_ = conn.Close()
			}
		}()
		go func() {
			buf := make([]byte, 2048)
			for {
				if _, _, err := udp.ReadFrom(buf); err != nil {
					return
				}
				r.hit()
			}
		}()
		return r
	}
	t.Fatal("no port free for TCP and UDP both")
	return nil
}

func (r *connRecorder) hit() {
	r.mu.Lock()
	r.hits++
	r.mu.Unlock()
}

func (r *connRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

// indented is a collector's YAML as an entry of the collectors list.
func indented(collector string) string {
	lines := strings.Split(strings.TrimRight(collector, "\n"), "\n")
	for i, line := range lines {
		prefix := "    "
		if i == 0 {
			prefix = "  - "
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n") + "\n"
}

// requirePlaygroundMakesNoRequest runs a playground of c's type on a pasted
// response, on a target fetched through a configured collector of the type,
// source, and on a static target scraped through source, at full. The paste
// must give c.series and make no request; the fetch and the scrape must make
// source's request alone, whatever they answer; and nothing must ever
// connect to what the playground's request block reaches.
func requirePlaygroundMakesNoRequest(t *testing.T, c playgroundCase) {
	t.Helper()
	reached, sourced := newConnRecorder(t), newConnRecorder(t)
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "collectors:\n"+indented(c.collector("source", sourced.addr, true))))
	if err != nil {
		t.Fatal(err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(2 * time.Second), Targets: []model.StaticTarget{
		{Name: "st", Collector: "source", Target: c.target(sourced.addr)},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	var mu sync.Mutex
	var fetched []string
	made := server.fetchCollector
	server.fetchCollector = func(ctx context.Context, target string, x *model.Collector, overrides fetch.RequestOverrides, forwarded http.Header) (*fetch.HTTPResponse, error) {
		mu.Lock()
		fetched = append(fetched, x.Name)
		mu.Unlock()
		return made(ctx, target, x, overrides, forwarded)
	}
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := fetched
		fetched = nil
		return out
	}
	playground := c.collector("playground", reached.addr, false)
	run := func(what string, request map[string]any) debugAnswer {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Debug-Run", "run")
		r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", "2")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, r)
		var answer debugAnswer
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", what, recorder.Code, recorder.Body)
		}
		return answer
	}
	answer := run("the paste", map[string]any{"collector": playground, "body": c.body})
	if !answer.OK || !strings.Contains(answer.Series, c.series) {
		t.Errorf("the paste gave %q, want %q: %s", answer.Series, c.series, answer.Report)
	}
	if got := requests(); len(got) != 0 {
		t.Errorf("the paste made the requests of %v", got)
	}
	run("the fetch", map[string]any{"collector": playground, "request_from": "source", "target": c.target(sourced.addr)})
	if got := requests(); len(got) == 0 || slices.ContainsFunc(got, func(name string) bool { return name != "source" }) {
		t.Errorf("the fetch made the requests of %v, want source's alone", got)
	}
	run("the scrape", map[string]any{"collector": playground, "static_target": "st"})
	if got := requests(); len(got) == 0 || slices.ContainsFunc(got, func(name string) bool { return name != "source" }) {
		t.Errorf("the scrape made the requests of %v, want source's alone", got)
	}
	if n := reached.count(); n != 0 {
		t.Errorf("what the playground's request reaches was connected to %d times", n)
	}
}

// requirePlaygroundRefusesFiles runs, for each key of a request type ending
// in _file or _files, a playground of the type that sets it, which must be
// refused before it is checked, naming the key: a playground reads no file
// of the server's, whatever its type. The keys are those of the common part
// the type takes (fetch.RequestType.Fields) and those of its own
// (fetch.RequestType.Config), and the file keys of the mappings among them,
// such as tls.ca_file.
func requirePlaygroundRefusesFiles(t *testing.T, requestType string) {
	t.Helper()
	rt := fetch.RequestTypes[requestType]
	if rt == nil {
		t.Fatalf("no request type %s in this build", requestType)
	}
	// No configured collector is needed: each run is refused.
	server := NewServer(config.NewManager(&model.Config{}, "", testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	server.SetDebug(DebugFull)
	keys := fileKeysOf(reflect.TypeOf(model.RequestConfig{}), "", rt.Fields)
	if rt.Config != nil {
		keys = append(keys, fileKeysOf(reflect.TypeOf(rt.Config), "", nil)...)
	}
	for _, key := range keys {
		name, value, _ := strings.Cut(key, "=")
		path := strings.Split(name, ".")
		var yaml strings.Builder
		yaml.WriteString("name: x\nrequest:\n  type: " + requestType + "\n")
		for i, part := range path {
			yaml.WriteString(strings.Repeat("  ", i+1) + part + ":")
			if i < len(path)-1 {
				yaml.WriteString("\n")
			}
		}
		yaml.WriteString(" " + value + "\ntransform: {type: jq}\nmetrics: [{name: up, expression: '1'}]\n")
		// A login reads its secrets from files, and is named as a whole.
		named := name
		if path[0] == "login" {
			named = "login"
		}
		body, _ := json.Marshal(map[string]any{"collector": yaml.String(), "body": "{}"})
		r := httptest.NewRequest(http.MethodPost, "/debug/run", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Debug-Run", "run")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, r)
		if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "sets request."+named+", which names a file on the server") {
			t.Errorf("%s request.%s: %d %s", requestType, name, recorder.Code, recorder.Body)
		}
	}
}

// fileKeysOf are the keys of the struct typ, by their yaml names after
// prefix, that end in _file or _files, each with a value of its kind as
// YAML (key=value), and those of the structs it holds; at the top, only
// those of the keys in only, when it is set.
func fileKeysOf(typ reflect.Type, prefix string, only []string) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var out []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if !field.IsExported() || key == "" || key == "-" || only != nil && !slices.Contains(only, key) {
			continue
		}
		inner := field.Type
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		switch {
		case inner.Kind() == reflect.Int:
			// max_files, a count.
		case strings.HasSuffix(key, "_file") || strings.HasSuffix(key, "_files"):
			out = append(out, prefix+key+"="+yamlSampleOf(inner))
		case inner.Kind() == reflect.Struct:
			out = append(out, fileKeysOf(inner, prefix+key+".", nil)...)
		}
	}
	return out
}

// yamlSampleOf is a value of typ as YAML, naming a file wherever it holds a
// string.
func yamlSampleOf(typ reflect.Type) string {
	const file = "/nonexistent/secret"
	switch typ.Kind() {
	case reflect.Slice:
		return "[" + file + "]"
	case reflect.Map:
		return "{a: " + file + "}"
	case reflect.Struct:
		var pairs []string
		for i := range typ.NumField() {
			if key, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ","); key != "" && key != "-" && typ.Field(i).Type.Kind() == reflect.String {
				pairs = append(pairs, key+": "+file)
			}
		}
		return "{" + strings.Join(pairs, ", ") + "}"
	}
	return file
}
