//go:build !select_request_types || request_type_http || request_type_grpc || request_type_sql || request_type_tcp || request_type_redis

package exporter

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// requireFreeTextWithheld loads collectors, the collectors list of a
// configuration whose request values hold the credential s3cret, and checks
// that neither the configured view of each nor its Edit a copy, with and
// without --web.debug.dangerous, shows it, each holding fetch.Redacted in
// its place.
func requireFreeTextWithheld(t *testing.T, collectors string) {
	t.Helper()
	cfg, err := config.Load(testutil.WriteFile(t, "config.yaml", "collectors:\n"+collectors))
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Collectors {
		x := &cfg.Collectors[i]
		shown, err := shownCollectorYAML(x)
		if err != nil {
			t.Fatal(err)
		}
		texts := []string{shown}
		for _, dangerous := range []bool{false, true} {
			copied, _, err := copiedCollectorYAML(x, dangerous)
			if err != nil {
				t.Fatal(err)
			}
			texts = append(texts, copied)
		}
		for _, text := range texts {
			if strings.Contains(text, "s3cret") || !strings.Contains(text, fetch.Redacted) {
				t.Errorf("%s is shown with its credential, or without %s in its place:\n%s", x.Name, fetch.Redacted, text)
			}
		}
	}
}
