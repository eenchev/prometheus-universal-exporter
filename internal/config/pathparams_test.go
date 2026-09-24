package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestMalformedPlaceholdersAreRejectedAtStartup(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/api/{{param_tenant", "unclosed placeholder"},
		{"/api/{{tenant}}", "not a path parameter"},
		{"/api/{{param_}}", "not a path parameter"},
		{"/api/{{param_a-b}}", "not a path parameter"},
		{"/api/{{ param_tenant }}", "not a path parameter"},
		{"/api/{{param_tenant:${DEFAULT_TENANT}}}", "--config.export-env"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			cfg := &model.Config{Collectors: []model.Collector{pathCollector(test.path)}}
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("%q should be rejected", test.path)
			}
			if !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), `"tenants"`) {
				t.Fatalf("error %q should mention %q and name the collector", err, test.want)
			}
		})
	}
}

func TestWellFormedPlaceholdersAreAccepted(t *testing.T) {
	for _, path := range []string{
		"/api/{{param_tenant}}",
		"/api/{{param_tenant:acme}}",
		"/api/{{param_suffix:}}",
		"/{{param_a}}/{{param_b:x}}/{{param_a}}",
		"/api/{{param_Region_2}}",
		"/metrics}}", // a stray closing pair is ordinary text
	} {
		cfg := &model.Config{Collectors: []model.Collector{pathCollector(path)}}
		if err := Validate(cfg); err != nil {
			t.Errorf("%q: %v", path, err)
		}
	}
}
