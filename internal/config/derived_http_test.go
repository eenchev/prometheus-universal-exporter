//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

const derivedRules = `      - name: hits_total
        type: counter
        expression: 'hits (\d+)'
      - name: misses_total
        type: counter
        expression: 'misses (\d+)'
`

// A collector's derived list loads as it is written: by: [] is a sum into
// one series, which by left out is not, and an entry without a type is a
// gauge. What could not be computed is refused at load, with the
// collector's other problems where the rules are sound.
func TestDerivedEntriesLoad(t *testing.T) {
	derived := `    derived:
      - name: gets
        hidden: true
        sum: [hits_total, misses_total]
      - name: hit_ratio
        ratio: [hits_total, gets]
      - name: hits_all
        type: counter
        sum: [hits_total]
        by: []
`
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", collectorRules("regex", "", derivedRules)+derived))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Collectors[0].Derived
	if len(got) != 3 || !got[0].Hidden || got[0].By != nil || got[1].Type != model.GaugeMetricType || got[2].By == nil || len(*got[2].By) != 0 || got[2].Type != model.CounterMetricType {
		t.Fatalf("derived %+v", got)
	}

	for name, tc := range map[string]struct{ derived, want string }{
		"an unknown operand": {"      - name: x\n        sum: [hits_total, hits]\n",
			`collector "node" derived "x" sum reads "hits", which is neither a rule of the collector nor an earlier derived entry; write the name of one, as it is written, without metrics_prefix`},
		"a number as an operand": {"      - name: x\n        sum: [hits_total, 1]\n",
			`is written 1, which YAML reads as a number, not as text`},
		"an unknown key": {"      - name: x\n        sum: [hits_total, misses_total]\n        on: [pool]\n",
			`unknown key "on" in a derived entry`},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", collectorRules("regex", "", derivedRules)+"    derived:\n"+tc.derived))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}

	// A python collector's operands are the names of its script's series,
	// which only a scrape shows.
	python := collectorRules("python", "", "      - name: v\n") + "    derived:\n      - name: x\n        ratio: [v, w]\n"
	if _, err := Load(testutil.WriteFile(t, "config.yaml", python)); err != nil {
		t.Fatalf("a python collector's derived entry: %v", err)
	}
}
