package exporter

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                           false,
		"gzip":                       true,
		"x-gzip":                     true,
		"*":                          true,
		"GZip":                       true,
		"identity":                   false,
		"br":                         false,
		"br, gzip;q=0.5":             true,
		"gzip, deflate, br":          true,
		"gzip;q=0":                   false,
		"gzip; q=0.0":                false,
		"gzip;q=0, *;q=0":            false,
		"identity;q=1, gzip;Q=0.001": true,
		// gzip named overrides *, either way.
		"gzip;q=0, *":   false,
		"*, gzip;q=0":   false,
		"*;q=0, gzip":   true,
		"*;q=0, x-gzip": true,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func gunzip(t *testing.T, body []byte) string {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

// A handler that writes without a Content-Type gets it sniffed from the plain
// bytes, not from the compressed ones; a 204 is passed on without a body.
func TestGzipWriterSniffsAndPassesBodylessAnswers(t *testing.T) {
	handler := newAnswerCompression(DefaultGzipLevel).wrap(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>hi</body></html>"))
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type %q, want text/html", got)
	}
	if gunzip(t, recorder.Body.Bytes()) != "<html><body>hi</body></html>" {
		t.Error("the body did not round-trip")
	}
	empty := newAnswerCompression(DefaultGzipLevel).wrap(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	recorder = httptest.NewRecorder()
	empty(recorder, request)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Encoding") != "" {
		t.Errorf("204: %d, %d bytes, Content-Encoding %q", recorder.Code, recorder.Body.Len(), recorder.Header().Get("Content-Encoding"))
	}
}

// variedExposition is an answer of the size a passed-through node_exporter's
// is, varied enough that the gzip levels make answers of different sizes.
func variedExposition() string {
	var b strings.Builder
	b.WriteString("# HELP node_cpu_seconds_total Seconds the CPUs spent in each mode.\n# TYPE node_cpu_seconds_total counter\n")
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "node_cpu_seconds_total{cpu=\"%d\",mode=\"m%d\",host=\"h%d\"} %d.%d\n", i/8, i%8, (i*7919)%1013, i*31337%100003, i%97)
	}
	return b.String()
}

// zippedAt answers plain through the compression of one --web.gzip-level, to
// a client that accepts gzip.
func zippedAt(level int, plain string) *httptest.ResponseRecorder {
	handler := newAnswerCompression(level).wrap(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, plain)
	})
	request := httptest.NewRequest(http.MethodGet, "/probe", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

// Every level from 1 to 9 decompresses to the answer, byte for byte, and
// the levels are gzip's own: 9 makes a smaller answer than 1 of a large
// exposition, and each is the size gzip makes at that level.
func TestEachGzipLevelDecompressesToTheSameAnswer(t *testing.T) {
	plain := variedExposition()
	sizes := map[int]int{}
	for level := 1; level <= 9; level++ {
		recorder := zippedAt(level, plain)
		if recorder.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("level %d: Content-Encoding %q", level, recorder.Header().Get("Content-Encoding"))
		}
		if gunzip(t, recorder.Body.Bytes()) != plain {
			t.Fatalf("level %d does not decompress to the answer", level)
		}
		var want bytes.Buffer
		gz, _ := gzip.NewWriterLevel(&want, level)
		_, _ = io.WriteString(gz, plain)
		_ = gz.Close()
		if recorder.Body.Len() != want.Len() {
			t.Errorf("level %d: %d bytes, gzip makes %d at that level", level, recorder.Body.Len(), want.Len())
		}
		sizes[level] = recorder.Body.Len()
	}
	if sizes[9] >= sizes[1] {
		t.Errorf("level 9 made %d bytes and level 1 %d of a %d-byte exposition", sizes[9], sizes[1], len(plain))
	}
}

// A writer taken from the pool again, after Reset, compresses at the level
// it was made with: the second answer is the size of the first.
func TestAPooledGzipWriterKeepsItsLevel(t *testing.T) {
	plain := variedExposition()
	compression := newAnswerCompression(9)
	answer := func() int {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/probe", nil)
		request.Header.Set("Accept-Encoding", "gzip")
		compression.wrap(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, plain) })(recorder, request)
		return recorder.Body.Len()
	}
	if first, second := answer(), answer(); first != second {
		t.Errorf("the first answer was %d bytes and the second %d", first, second)
	}
}

func TestValidateGzipLevel(t *testing.T) {
	for level := 0; level <= 9; level++ {
		if err := ValidateGzipLevel(level); err != nil {
			t.Errorf("level %d: %v", level, err)
		}
	}
	for _, level := range []int{-1, 10, 100} {
		if err := ValidateGzipLevel(level); err == nil || !strings.Contains(err.Error(), "--web.gzip-level must be from 0") {
			t.Errorf("level %d: %v", level, err)
		}
	}
}
