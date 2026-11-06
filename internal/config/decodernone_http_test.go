//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// decoder.type none is taken with the transforms that read null, jq, yq and
// python, and refused at load with each that reads a body, as a decoder a
// transform cannot read is, and with response.charset, which it would
// ignore; it is a value the error names among the decoders.
func TestDecoderNoneIsTakenByTheTransformsThatReadNull(t *testing.T) {
	collector := func(transform, rule, response string) string {
		return "collectors:\n  - name: up\n    request:\n      type: http\n      method: HEAD\n    decoder: {type: none}\n    transform: {type: " + transform + "}\n" + response + "    metrics:\n      - name: m\n        expression: '" + rule + "'\n"
	}
	for _, taken := range []struct{ transform, rule string }{{"jq", "$status"}, {"yq", "$status"}, {"python", "x"}} {
		document := collector(taken.transform, taken.rule, "")
		if taken.transform == "python" {
			document = strings.Replace(document, "{type: python}", "{type: python, script: 'metric(\"m\", value=1)'}", 1)
		}
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err != nil || cfg.Collectors[0].Decoder.Type != "none" {
			t.Errorf("%s: %v", taken.transform, err)
		}
	}
	for transform, reads := range map[string]string{"regex": "text", "css": "html", "xpath": "xml or html", "csv": "csv", "prometheus": "prometheus"} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", collector(transform, "x", "")))
		if want := `collector "up" decodes with none, which its ` + transform + ` transform cannot read: it reads ` + reads + `;`; err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "use a transform that reads none: jq, yq or python") {
			t.Errorf("%s: %v, want %q", transform, err, want)
		}
	}
	_, err := Load(testutil.WriteFile(t, "config.yaml", collector("jq", "$status", "    response: {charset: windows-1252}\n")))
	if want := `collector "up" sets response.charset, which names the encoding a decoder reads the body in, but its decoder is none, which reads no body`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("response.charset: %v", err)
	}
	_, err = Load(testutil.WriteFile(t, "config.yaml", strings.Replace(collector("jq", "$status", ""), "type: none", "type: nothing", 1)))
	if err == nil || !strings.Contains(err.Error(), "ndjson, logfmt, toml, ini, binary, none, in lower case") {
		t.Errorf("an unknown decoder: %v", err)
	}
}
