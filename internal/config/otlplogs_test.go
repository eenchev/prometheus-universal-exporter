//go:build !select_request_types || request_type_http

package config

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// otlp.logs goes to the collector the metrics go to, at its standard path
// for logs, unless it names an endpoint of its own; it is checked, and its
// defaults filled in, only while both blocks are switched on.
func TestOTLPLogsSettingsAreCheckedAndFilledIn(t *testing.T) {
	on := func(change func(*model.OTLPLogsConfig)) *model.Config {
		return otlpSettings(func(o *model.OTLPConfig) {
			o.Endpoint = "https://user:pw@collector.invalid:4318/otel/v1/metrics?tenant=a"
			o.Logs.Enabled = true
			change(&o.Logs)
		})
	}
	cfg := on(func(*model.OTLPLogsConfig) {})
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.OTLP.Logs, (model.OTLPLogsConfig{Enabled: true, Endpoint: "https://user:pw@collector.invalid:4318/otel/v1/logs?tenant=a", Level: "info", Interval: model.Duration(5 * time.Second), MaxPendingRecords: 10000}); got != want {
		t.Errorf("otlp.logs is filled in as %+v, want %+v", got, want)
	}
	cfg = on(func(l *model.OTLPLogsConfig) {
		l.Endpoint, l.Level, l.Interval, l.MaxPendingRecords = "http://logs.invalid/ingest", "WARN", model.Duration(time.Second), 7
	})
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.OTLP.Logs; got.Endpoint != "http://logs.invalid/ingest" || got.Level != "warn" || got.Interval != model.Duration(time.Second) || got.MaxPendingRecords != 7 {
		t.Errorf("otlp.logs as written is kept as %+v", got)
	}
	for name, test := range map[string]struct {
		change func(*model.OTLPConfig)
		want   string
	}{
		"no endpoint beside one without /v1/metrics": {func(o *model.OTLPConfig) { o.Endpoint = "http://collector.invalid/ingest" }, "otlp.logs.endpoint is required when otlp.endpoint does not end in /v1/metrics"},
		"an endpoint that is no URL":                 {func(o *model.OTLPConfig) { o.Logs.Endpoint = "collector:4318" }, "otlp.logs.endpoint must be an http or https URL"},
		"a level that is none":                       {func(o *model.OTLPConfig) { o.Logs.Level = "verbose" }, `otlp.logs.level "verbose" is not a level; use debug, info, warn or error`},
		"an interval under a second":                 {func(o *model.OTLPConfig) { o.Logs.Interval = model.Duration(500 * time.Millisecond) }, "otlp.logs.interval 500ms is under the least, 1s"},
		"a negative bound":                           {func(o *model.OTLPConfig) { o.Logs.MaxPendingRecords = -1 }, "otlp.logs.max_pending_records must not be negative"},
	} {
		cfg := otlpSettings(func(o *model.OTLPConfig) { o.Logs.Enabled = true; test.change(o) })
		if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
		// Switched off, either block keeps its settings unchecked.
		cfg = otlpSettings(func(o *model.OTLPConfig) { test.change(o) })
		if err := Validate(cfg); err != nil && !strings.Contains(name, "without /v1/metrics") {
			t.Errorf("%s, with otlp.logs switched off: %v", name, err)
		}
		cfg = otlpSettings(func(o *model.OTLPConfig) { o.Logs.Enabled = true; test.change(o) })
		cfg.OTLP.Enabled = false
		if err := Validate(cfg); err != nil {
			t.Errorf("%s, with otlp switched off: %v", name, err)
		}
	}
}

// A logs block that sets anything says whether it is on, as the otlp block
// does.
func TestOTLPLogsBlockMustSayEnabled(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", "otlp:\n  enabled: true\n  endpoint: http://collector.invalid:4318/v1/metrics\n  logs:\n    level: debug\n"+testutil.MinimalConfig)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "otlp.logs sets level but not enabled") {
		t.Fatalf("a logs block without enabled: %v", err)
	}
}

// otlp.traces goes where otlp.logs goes: to the collector the metrics go
// to, at its standard path for traces, unless it names an endpoint of its
// own; it is checked, and its defaults filled in, only while both blocks
// are switched on.
func TestOTLPTracesSettingsAreCheckedAndFilledIn(t *testing.T) {
	cfg := otlpSettings(func(o *model.OTLPConfig) {
		o.Endpoint = "https://user:pw@collector.invalid:4318/otel/v1/metrics?tenant=a"
		o.Traces.Enabled = true
	})
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.OTLP.Traces, (model.OTLPTracesConfig{Enabled: true, Endpoint: "https://user:pw@collector.invalid:4318/otel/v1/traces?tenant=a", Threshold: model.Duration(time.Second), Interval: model.Duration(5 * time.Second), MaxPendingSpans: 10000}); got != want {
		t.Errorf("otlp.traces is filled in as %+v, want %+v", got, want)
	}
	cfg = otlpSettings(func(o *model.OTLPConfig) {
		o.Traces = model.OTLPTracesConfig{Enabled: true, Endpoint: "http://traces.invalid/ingest", Threshold: model.Duration(time.Millisecond), Errors: true, ParentBased: true, SampleRatio: 1, MaxPerSecond: 3, Interval: model.Duration(time.Second), MaxPendingSpans: 7}
	})
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.OTLP.Traces; got.Endpoint != "http://traces.invalid/ingest" || got.Threshold != model.Duration(time.Millisecond) || !got.Errors || got.Interval != model.Duration(time.Second) || got.MaxPendingSpans != 7 || !got.ParentBased || got.SampleRatio != 1 || got.MaxPerSecond != 3 {
		t.Errorf("otlp.traces as written is kept as %+v", got)
	}
	for name, test := range map[string]struct {
		change func(*model.OTLPConfig)
		want   string
	}{
		"no endpoint beside one without /v1/metrics": {func(o *model.OTLPConfig) { o.Endpoint = "http://collector.invalid/ingest" }, "otlp.traces.endpoint is required when otlp.endpoint does not end in /v1/metrics"},
		"an endpoint that is no URL":                 {func(o *model.OTLPConfig) { o.Traces.Endpoint = "collector:4318" }, "otlp.traces.endpoint must be an http or https URL"},
		"a threshold under a millisecond":            {func(o *model.OTLPConfig) { o.Traces.Threshold = model.Duration(500 * time.Microsecond) }, "otlp.traces.threshold 500µs is under the least, 1ms"},
		"a negative threshold":                       {func(o *model.OTLPConfig) { o.Traces.Threshold = model.Duration(-time.Second) }, "otlp.traces.threshold -1s is under the least, 1ms"},
		"an interval under a second":                 {func(o *model.OTLPConfig) { o.Traces.Interval = model.Duration(500 * time.Millisecond) }, "otlp.traces.interval 500ms is under the least, 1s"},
		"a negative bound":                           {func(o *model.OTLPConfig) { o.Traces.MaxPendingSpans = -1 }, "otlp.traces.max_pending_spans must not be negative"},
		"a ratio over 1":                             {func(o *model.OTLPConfig) { o.Traces.SampleRatio = 1.5 }, "otlp.traces.sample_ratio 1.5 is not between 0 and 1"},
		"a negative ratio":                           {func(o *model.OTLPConfig) { o.Traces.SampleRatio = -0.25 }, "otlp.traces.sample_ratio -0.25 is not between 0 and 1"},
		"a ratio that is no number":                  {func(o *model.OTLPConfig) { o.Traces.SampleRatio = math.NaN() }, "otlp.traces.sample_ratio NaN is not between 0 and 1"},
		"a negative rate":                            {func(o *model.OTLPConfig) { o.Traces.MaxPerSecond = -1 }, "otlp.traces.max_per_second must not be negative"},
	} {
		cfg := otlpSettings(func(o *model.OTLPConfig) { o.Traces.Enabled = true; test.change(o) })
		if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
		// Switched off, either block keeps its settings unchecked.
		cfg = otlpSettings(func(o *model.OTLPConfig) { test.change(o) })
		if err := Validate(cfg); err != nil && !strings.Contains(name, "without /v1/metrics") {
			t.Errorf("%s, with otlp.traces switched off: %v", name, err)
		}
		cfg = otlpSettings(func(o *model.OTLPConfig) { o.Traces.Enabled = true; test.change(o) })
		cfg.OTLP.Enabled = false
		if err := Validate(cfg); err != nil {
			t.Errorf("%s, with otlp switched off: %v", name, err)
		}
	}
}

// A traces block that sets anything says whether it is on, as the otlp
// block does.
func TestOTLPTracesBlockMustSayEnabled(t *testing.T) {
	path := testutil.WriteFile(t, "config.yaml", "otlp:\n  enabled: true\n  endpoint: http://collector.invalid:4318/v1/metrics\n  traces:\n    threshold: 1s\n"+testutil.MinimalConfig)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "otlp.traces sets threshold but not enabled") {
		t.Fatalf("a traces block without enabled: %v", err)
	}
}
