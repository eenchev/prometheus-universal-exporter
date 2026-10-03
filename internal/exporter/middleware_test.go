package exporter

import (
	"bytes"
	"compress/gzip"
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
	handler := compressed(func(w http.ResponseWriter, _ *http.Request) {
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
	empty := compressed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	recorder = httptest.NewRecorder()
	empty(recorder, request)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Encoding") != "" {
		t.Errorf("204: %d, %d bytes, Content-Encoding %q", recorder.Code, recorder.Body.Len(), recorder.Header().Get("Content-Encoding"))
	}
}
