package decode

import (
	"net/http"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// The decoder a response's Content-Type names, with the decoder left to the
// response, as docs/CONFIGURATION.md tabulates it: the type is read without
// its parameters and in any case, a type ending in +json is JSON, and only
// version=0.0.4 of text/plain says Prometheus text. Nothing but text/csv
// says CSV: application/csv, text/tab-separated-values and every other type
// leave the body to be read by its content, and a CSV body's content is
// text.
func TestTheDecoderAContentTypeNames(t *testing.T) {
	const csvBody = "host,used\nweb01,72\n"
	for contentType, want := range map[string]string{
		"application/json":                "json",
		"application/problem+json":        "json",
		"APPLICATION/JSON; charset=utf-8": "json",
		"application/yaml":                "yaml",
		"application/x-yaml":              "yaml",
		"text/yaml":                       "yaml",
		"application/xml":                 "xml",
		"text/xml; charset=utf-8":         "xml",
		"text/csv":                        "csv",
		"Text/CSV; header=present":        "csv",
		"text/html":                       "html",
		"text/html; charset=windows-1251": "html",
		"application/openmetrics-text; version=1.0.0; charset=utf-8": "prometheus",
		"text/plain; version=0.0.4":                                  "prometheus",
		fetch.GraphiteContentType:                                    "graphite",

		"text/plain":                  "text",
		"application/csv":             "text",
		"text/tab-separated-values":   "text",
		"application/vnd.ms-excel":    "text",
		"text/comma-separated-values": "text",
		"application/octet-stream":    "text",
		"application/xhtml+xml":       "text",
		"":                            "text",
	} {
		headers := http.Header{}
		if contentType != "" {
			headers.Set("Content-Type", contentType)
		}
		if got := detectFormat(&fetch.HTTPResponse{StatusCode: http.StatusOK, Headers: headers, Body: []byte(csvBody)}); got != want {
			t.Errorf("a CSV body sent as %q is decoded as %s, want %s", contentType, got, want)
		}
	}
}
