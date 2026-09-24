package transform

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func scriptLimits() model.Limits { return model.Limits{ScriptTimeout: model.Duration(5 * time.Second)} }

func TestPreScriptPromotesOnlyForStructuredTransforms(t *testing.T) {
	decodedFor := func(kind string) *decode.Decoded {
		switch kind {
		case "csv":
			return &decode.Decoded{Kind: "csv", Data: []any{}, Raw: []byte("cpu\n42\n")}
		case "html":
			return &decode.Decoded{Kind: "html", Raw: []byte("<span>42</span>")}
		case "xml":
			return &decode.Decoded{Kind: "xml", Raw: []byte("<status><cpu>42</cpu></status>")}
		default:
			return &decode.Decoded{Kind: kind, Data: "Worker a CPU: 42%", Raw: []byte("Worker a CPU: 42%")}
		}
	}
	const structured = `data = {"workers": [{"name": "a", "cpu": 42}]}`
	// CSS and XPath consume the markup a pre-script returns, so they are asked
	// for the string result their contract defines.
	const markup = `data = data`
	tests := []struct {
		transform string
		kind      string
		script    string
		want      string
	}{
		{transform: "", kind: "text", script: structured, want: "json"},
		{transform: "none", kind: "text", script: structured, want: "json"},
		{transform: "jq", kind: "text", script: structured, want: "json"},
		{transform: "yq", kind: "text", script: structured, want: "json"},
		{transform: "jq", kind: "csv", script: structured, want: "json"},
		{transform: "jq", kind: "html", script: structured, want: "json"},
		{transform: "jq", kind: "xml", script: structured, want: "json"},
		{transform: "jq", kind: "json", script: structured, want: "json"},
		{transform: "csv", kind: "csv", script: structured, want: "csv"},
		{transform: "regex", kind: "text", script: structured, want: "text"},
		{transform: "python", kind: "text", script: structured, want: "text"},
		{transform: "css", kind: "html", script: markup, want: "html"},
		{transform: "xpath", kind: "xml", script: markup, want: "xml"},
	}
	for _, test := range tests {
		t.Run(test.transform+"/"+test.kind, func(t *testing.T) {
			c := &model.Collector{Request: model.RequestConfig{Type: fetch.RequestTypeHTTP},
				Name:      "promotion",
				Transform: model.TransformConfig{Type: test.transform, PreScript: test.script},
				Limits:    scriptLimits(),
			}
			decoded := decodedFor(test.kind)
			response := &fetch.HTTPResponse{Body: decoded.Raw, Headers: make(http.Header)}
			out, err := applyPreScript(context.Background(), decoded, response, c, "python3")
			if err != nil {
				t.Fatal(err)
			}
			if out.Kind != test.want {
				t.Fatalf("kind=%q, want %q", out.Kind, test.want)
			}
			if test.want == "json" {
				if _, ok := out.Data.(map[string]any); !ok {
					t.Fatalf("promoted data is %T, want a structured value", out.Data)
				}
			}
		})
	}
}
