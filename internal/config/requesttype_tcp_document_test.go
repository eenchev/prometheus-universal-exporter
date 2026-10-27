//go:build request_type_tcp

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A tcp collector with request.document is answered with a JSON document:
// its decoder left out is json, with no warning of a decoder left to each
// response, json written out is kept, and any other decoder, or a
// transform that does not read JSON, is refused at load, as for a type
// whose every answer is JSON.
func TestATCPDocumentIsDecodedAsJSON(t *testing.T) {
	collector := func(decoder, transform string) string {
		document := "collectors:\n  - name: smtp\n    request:\n      type: tcp\n      greeting: '^220 '\n      document: true\n"
		if decoder != "" {
			document += "    decoder:\n      type: " + decoder + "\n"
		}
		return document + "    transform:\n      type: " + transform + "\n    metrics:\n      - name: up\n        expression: '(1)'\n"
	}
	for _, decoder := range []string{"", "json", "auto"} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", collector(decoder, "jq")))
		if err != nil {
			t.Fatalf("decoder %q: %v", decoder, err)
		}
		if got := cfg.Collectors[0].Decoder.Type; got != "json" || len(cfg.Warnings) != 0 {
			t.Errorf("decoder %q: %q, warnings %v", decoder, got, cfg.Warnings)
		}
	}
	for _, tc := range []struct{ decoder, transform, want string }{
		{"text", "jq", `collector "smtp" decodes with text, but it is answered with the JSON document request.document makes of what a tcp server says; leave decoder.type out, or set json`},
		{"yaml", "yq", `collector "smtp" decodes with yaml, but it is answered with the JSON document`},
		{"", "regex", `collector "smtp" transforms with regex, which cannot read the JSON document request.document makes of what a tcp server says; use jq, yq or python`},
		{"", "csv", `collector "smtp" transforms with csv, which cannot read the JSON document`},
	} {
		if _, err := Load(testutil.WriteFile(t, "config.yaml", collector(tc.decoder, tc.transform))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("decoder %q, transform %s: %v, want %q", tc.decoder, tc.transform, err, tc.want)
		}
	}
	// Without request.document the answer is the server's text, which a
	// regex rule reads.
	if _, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(collector("", "regex"), "      document: true\n", "", 1))); err != nil {
		t.Error(err)
	}
}
