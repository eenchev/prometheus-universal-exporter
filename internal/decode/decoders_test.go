package decode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestDecodeJSONAutoDetectionAndMalformedInput(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}}
	r := &fetch.HTTPResponse{Body: []byte(`[{"value":7}]`), Headers: make(http.Header)}
	d, err := Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "json" {
		t.Fatalf("detected kind=%q, want json", d.Kind)
	}
	values, ok := d.Data.([]any)
	if !ok || len(values) != 1 {
		t.Fatalf("decoded JSON array=%#v", d.Data)
	}
	row, ok := values[0].(map[string]any)
	if !ok || row["value"] != float64(7) {
		t.Fatalf("decoded JSON row=%#v", values[0])
	}

	c.Decoder.Type = "json"
	r.Body = []byte(`{"value":`)
	if _, err := Decode(r, &c); err == nil || !strings.Contains(err.Error(), "JSON decode") {
		t.Fatalf("malformed JSON error=%v", err)
	}
}

func TestDecodeCSVQuotedFieldsAndRowsWithoutHeader(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"}, Response: model.ResponseConfig{CSV: model.CSVConfig{Header: boolPtr(true), Delimiter: ";", TrimSpace: true}}}
	r := &fetch.HTTPResponse{Body: []byte("server;note;cpu\n\"web;01\";\"up;ok\"; 72 \n"), Headers: make(http.Header)}
	d, err := Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	rows := d.Data.([]any)
	row := rows[0].(map[string]any)
	if row["server"] != "web;01" || row["note"] != "up;ok" || row["cpu"] != "72" {
		t.Fatalf("decoded CSV row=%#v", row)
	}

	c.Response.CSV.Header = boolPtr(false)
	r.Body = []byte("web01;72\nweb02;31\n")
	d, err = Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	rows = d.Data.([]any)
	first := rows[0].([]any)
	if len(rows) != 2 || first[0] != "web01" || first[1] != "72" {
		t.Fatalf("decoded headerless CSV rows=%#v", rows)
	}
}

func TestDecodePrometheusPreservesTimestamp(t *testing.T) {
	c := model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "prometheus"}}
	r := &fetch.HTTPResponse{Body: []byte("# TYPE vendor_value gauge\nvendor_value 42 1700000000000\n"), Headers: make(http.Header)}
	d, err := Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set := d.Data.(model.MetricSet)
	if len(set.Metrics) != 1 || set.Metrics[0].Timestamp == nil || *set.Metrics[0].Timestamp != 1700000000000 {
		t.Fatalf("decoded Prometheus timestamp=%#v", set.Metrics)
	}
}
