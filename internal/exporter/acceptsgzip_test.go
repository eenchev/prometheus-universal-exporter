package exporter

import (
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// splitAcceptsGzip is acceptsGzip as it was, splitting the header and its
// parameters into slices and lower-casing each coding: what the reading in
// place must say of every header.
func splitAcceptsGzip(header string) bool {
	named, wildcard := -1.0, -1.0
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding != "gzip" && coding != "x-gzip" && coding != "*" {
			continue
		}
		quality := 1.0
		for _, param := range strings.Split(params, ";") {
			name, value, found := strings.Cut(strings.TrimSpace(param), "=")
			if found && strings.EqualFold(strings.TrimSpace(name), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = q
				}
			}
		}
		if coding == "*" {
			wildcard = max(wildcard, quality)
		} else {
			named = max(named, quality)
		}
	}
	if named >= 0 {
		return named > 0
	}
	return wildcard > 0
}

// acceptEncodingParts are the pieces the headers acceptEncodingHeaders
// joins are made of: codings in several cases, with and without blanks and
// a quality, qualities of zero, of numbers written in other ways and of no
// number, parameters that are not q, and empty parts.
var acceptEncodingParts = []string{
	"gzip", "GZIP", "x-gzip", "X-Gzip", "*", "identity", "br", "deflate", "", " ",
	" gzip ", "\tgzip\t", "gzip;q=0", "gzip; q=0.0", "gzip;Q=0.001", "gzip;q=1",
	"gzip;q=", "gzip;q=x", "gzip;q = 0", "gzip ;q=0", "gzip;level=1;q=0", "gzip;q=0;q=1",
	"*;q=0", "* ; q=0.5", "*;q=-1", "x-gzip;q=0", "gzip;q=0e0", "gzip;q=inf", "gzip;q=NaN",
	"gzip;q=0x0p0", "gzip;=0", ";q=0", "gzip;;q=0", "gzi p", "gzip ", " *;q=0",
	"ＧＺＩＰ", "gzıp", "gſip",
}

// acceptEncodingHeaders are every header of one, two and three parts of
// acceptEncodingParts joined by commas, with and without blanks after them.
func acceptEncodingHeaders() []string {
	var headers []string
	for _, a := range acceptEncodingParts {
		headers = append(headers, a)
		for _, b := range acceptEncodingParts {
			headers = append(headers, a+","+b, a+", "+b)
			for _, c := range acceptEncodingParts[:12] {
				headers = append(headers, a+","+b+" ,"+c)
			}
		}
	}
	return append(headers, ",", ",,", ";", "gzip,", ",gzip", strings.Repeat("br, ", 40)+"gzip;q=0.1")
}

// Reading Accept-Encoding where it lies says of every header what
// splitting it did: tens of thousands of headers made of codings in any
// case, with blanks and qualities of every kind, * and identity, all agree.
func TestAcceptsGzipSaysWhatSplittingTheHeaderSaid(t *testing.T) {
	headers := acceptEncodingHeaders()
	for _, header := range headers {
		if got, want := acceptsGzip(header), splitAcceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, splitting said %v", header, got, want)
		}
	}
	t.Logf("%d headers", len(headers))
}

func FuzzAcceptsGzipSaysWhatSplittingTheHeaderSaid(f *testing.F) {
	for _, header := range acceptEncodingParts {
		f.Add(header)
	}
	f.Add("identity;q=1, gzip;Q=0.001")
	f.Add("gzip;q=0, *")
	f.Fuzz(func(t *testing.T, header string) {
		if got, want := acceptsGzip(header), splitAcceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, splitting said %v", header, got, want)
		}
	})
}

// Reading Accept-Encoding allocates nothing, whatever the header holds:
// several codings and parameters, codings in upper case, *, identity and
// qualities. Splitting it allocated a slice of its parts on every request,
// and one of each coding's parameters.
func TestReadingAcceptEncodingAllocatesNothing(t *testing.T) {
	for _, header := range []string{
		"", "gzip", "GZIP", "br, gzip;q=0.5", "gzip, deflate, br, zstd",
		"identity;q=1, X-GZIP;Q=0.001, *;q=0", "deflate;level=9;q=0.5, br;q=0.4, *;q=0.1",
	} {
		if got := alloctest.AllocsAtMost(100, 0, func() { _ = acceptsGzip(header) }); got != 0 {
			t.Errorf("acceptsGzip(%q) allocates %v times, want none", header, got)
		}
	}
}
