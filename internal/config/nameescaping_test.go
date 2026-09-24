package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

func TestNameEscapingIsValidated(t *testing.T) {
	c := testutil.Collector("text", "text")
	cfg := &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err != nil || cfg.Collectors[0].NameEscaping != transform.NameEscapingFail {
		t.Fatalf("default: %v %q", err, cfg.Collectors[0].NameEscaping)
	}
	c.NameEscaping = "dots"
	cfg = &model.Config{Collectors: []model.Collector{c}}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), `name_escaping must be fail, underscores or values, not "dots"`) {
		t.Fatalf("dots: %v", err)
	}
}
