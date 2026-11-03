//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// The committed schemas and the exporter agree on request.accept_status:
// [any]: taken alone, of a collector in the configuration and in a collector
// file and of a static target, and refused beside another entry and in
// another case, while retry.statuses takes no "any".
func TestTheSchemasAndTheLoaderAgreeOnAcceptStatusAny(t *testing.T) {
	collector := func(request string) string {
		return "collectors:\n  - name: a\n    request:\n      type: http\n" + request + "    decoder: {type: json}\n    transform: {type: jq}\n    metrics:\n      - name: m\n        expression: .x\n"
	}
	targets := func(request string) string {
		return "interval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {" + request + "}\n"
	}
	schemaTakes := func(file, document string) bool {
		var doc any
		if err := yaml.Unmarshal([]byte(document), &doc); err != nil {
			t.Fatal(err)
		}
		return len(validateAgainstSchema(loadSchemaFile(t, file), normalizeYAML(doc))) == 0
	}
	for request, taken := range map[string]bool{
		"accept_status: [any]":                                true,
		"accept_status: ['any']":                              true,
		"accept_status: [any, 2xx]":                           false,
		"accept_status: [503, any]":                           false,
		"accept_status: [ANY]":                                false,
		"accept_status: [any, any]":                           false,
		"retry: {statuses: [any]}":                            false,
		"accept_status: [any], retry: {statuses: [500]}":      false,
		"accept_status: [2xx, 503], retry: {statuses: [500]}": true,
	} {
		lines := "      " + strings.ReplaceAll(request, "], ", "]\n      ") + "\n"
		document := collector(lines)
		_, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
		if (err == nil) != taken {
			t.Errorf("the configuration, %s: the exporter says %v, want taken %v", request, err, taken)
		}
		// retry.statuses accepted whole is the exporter's to refuse: the
		// schemas cannot compare two lists.
		retriedAccepted := strings.Contains(request, "statuses: [500]")
		for _, file := range []string{configSchemaFile, collectorFileSchemaFile} {
			if got := schemaTakes(file, document); got != (taken || retriedAccepted) {
				t.Errorf("%s, %s: the schema takes it %v, want %v", file, request, got, taken)
			}
		}
		plain, err := config.Load(testutil.WriteFile(t, "config.yaml", collector("")))
		if err != nil {
			t.Fatal(err)
		}
		file, err := config.LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targets(request)))
		if err == nil {
			if err = config.ValidateStaticTargets(file); err == nil {
				err = config.ValidateStaticTargetsAgainst(file, plain)
			}
		}
		if (err == nil) != taken {
			t.Errorf("the static target file, %s: the exporter says %v, want taken %v", request, err, taken)
		}
		for _, schema := range []string{staticTargetsSchemaFile, "configs/allocator-static-targets.schema.json"} {
			if got := schemaTakes(schema, targets(request)); got != (taken || retriedAccepted) {
				t.Errorf("%s, %s: the schema takes it %v, want %v", schema, request, got, taken)
			}
		}
	}
}
