package decode

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// csvWideHeaderShortRows is a header of k named columns over k rows of one
// field each: an answer of a few bytes a row that the decoder once made k
// times k cells of.
func csvWideHeaderShortRows(k int) []byte {
	var b strings.Builder
	for i := range k {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "c%d", i)
	}
	b.WriteByte('\n')
	b.WriteString(strings.Repeat("1\n", k))
	return []byte(b.String())
}

// csvOneColumn is a CSV of one short column, a header and rows of "1", of
// about size bytes.
func csvOneColumn(size int) []byte {
	return []byte("v\n" + strings.Repeat("1\n", size/2))
}

// csvTenColumns is a CSV of ten columns, a host name, a time and eight
// numbers, as an export of a monitoring system is, of about size bytes.
func csvTenColumns(size int) []byte {
	var b strings.Builder
	b.WriteString("host,time,cpu,mem,disk,net_in,net_out,load1,load5,load15\n")
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "web-%d.example.org,2026-10-08T09:00:00Z,%d.5,%d,%d,%d,%d,0.25,0.5,1.75\n", i%500, i%100, i%97, i%89, i*7, i*3)
	}
	return []byte(b.String())
}

// decodeCSVBody decodes body as the csv decoder does for a collector with
// the defaults.
func decodeCSVBody(body []byte) (*Decoded, error) {
	c := &model.Collector{Name: "c", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Decoder: model.DecoderConfig{Type: "csv"}}
	return Decode(&fetch.HTTPResponse{StatusCode: http.StatusOK, Body: body, Headers: http.Header{}}, c)
}

// BenchmarkCSVDecode decodes the shapes docs/DEVELOPMENT.md measures the
// csv decoder by, and reports beside what each decode allocates what the
// decoded rows hold once it is done (held-B/op), a collection later.
func BenchmarkCSVDecode(b *testing.B) {
	for _, bc := range []struct {
		name string
		body []byte
	}{
		{"wide_header_short_rows/k=2000", csvWideHeaderShortRows(2000)},
		{"one_column/10MiB", csvOneColumn(10 << 20)},
		{"ten_columns/1MiB", csvTenColumns(1 << 20)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(bc.body)))
			var held uint64
			for b.Loop() {
				var before, after runtime.MemStats
				b.StopTimer()
				runtime.GC()
				runtime.ReadMemStats(&before)
				b.StartTimer()
				d, err := decodeCSVBody(bc.body)
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				runtime.GC()
				runtime.ReadMemStats(&after)
				runtime.KeepAlive(d)
				if after.HeapAlloc > before.HeapAlloc {
					held = after.HeapAlloc - before.HeapAlloc
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(held), "held-B/op")
		})
	}
}
