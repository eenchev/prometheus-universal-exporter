//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The OTLP export's own settings are checked when the configuration loads,
// as a collector's request is: what Go would refuse to send or connect with
// loaded before, and then failed every export.

func otlpSettings(change func(*model.OTLPConfig)) *model.Config {
	otlp := model.OTLPConfig{Enabled: true, Endpoint: "http://collector.invalid:4318/v1/metrics"}
	change(&otlp)
	return &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}, OTLP: otlp}
}

// otlp.headers get the checks of a collector's request.headers, and otlp.tls
// those of its request.tls; the tls files must be readable too, since the
// export needs them from the start.
func TestOTLPHeadersAndTLSAreCheckedAtLoad(t *testing.T) {
	missing := t.TempDir() + "/missing.pem"
	notPEM := testutil.WriteFile(t, "ca.pem", "not a certificate\n")
	for name, test := range map[string]struct {
		change func(*model.OTLPConfig)
		want   string
	}{
		"a header name with a space":      {func(o *model.OTLPConfig) { o.Headers = map[string]string{"Bad Header": "x"} }, `otlp.headers "Bad Header" is not a header name`},
		"a header name in braces":         {func(o *model.OTLPConfig) { o.Headers = map[string]string{"{{param_x}}": "x"} }, `otlp.headers "{{param_x}}" is not a header name`},
		"a header value over lines":       {func(o *model.OTLPConfig) { o.Headers = map[string]string{"X-A": "a\nb"} }, `otlp.headers X-A has the control character '\n' in its value`},
		"one header in two cases":         {func(o *model.OTLPConfig) { o.Headers = map[string]string{"X-Tenant": "a", "x-tenant": "b"} }, `otlp.headers "X-Tenant" and "x-tenant" are the same header`},
		"a certificate without a key":     {func(o *model.OTLPConfig) { o.TLS.CertFile = "/etc/tls/client.crt" }, "otlp.tls sets only one of cert_file and key_file; a client certificate needs both"},
		"a key without a certificate":     {func(o *model.OTLPConfig) { o.TLS.KeyFile = "/etc/tls/client.key" }, "otlp.tls sets only one of cert_file and key_file; a client certificate needs both"},
		"a ca_file that is not there":     {func(o *model.OTLPConfig) { o.TLS.CAFile = missing }, "otlp.tls cannot be used: open " + missing},
		"a ca_file without a certificate": {func(o *model.OTLPConfig) { o.TLS.CAFile = notPEM }, "otlp.tls cannot be used: no certificates found in " + notPEM},
		"a certificate that is not there": {func(o *model.OTLPConfig) { o.TLS.CertFile, o.TLS.KeyFile = missing, missing }, "otlp.tls cannot be used: open " + missing},
	} {
		if err := Validate(otlpSettings(test.change)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
		// Switched off, the settings are kept without being used or checked.
		off := otlpSettings(test.change)
		off.OTLP.Enabled = false
		if err := Validate(off); err != nil {
			t.Errorf("%s, with enabled: false: %v", name, err)
		}
	}
	valid := otlpSettings(func(o *model.OTLPConfig) {
		o.Headers = map[string]string{"Authorization": "Bearer abc", "X-Scope-OrgID": "tenant\t1"}
	})
	if err := Validate(valid); err != nil {
		t.Fatalf("valid headers: %v", err)
	}
}

// otlp.interval is at least 1s and otlp.timeout not negative; left out, they
// are 30s and 5s.
func TestOTLPIntervalHasAMinimum(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*model.OTLPConfig)
		want   string
	}{
		"a nanosecond":       {func(o *model.OTLPConfig) { o.Interval = 1 }, "otlp.interval 1ns is under the least, 1s; leave it out for the default, 30s"},
		"just under":         {func(o *model.OTLPConfig) { o.Interval = model.Duration(999 * time.Millisecond) }, "otlp.interval 999ms is under the least, 1s"},
		"negative":           {func(o *model.OTLPConfig) { o.Interval = model.Duration(-time.Minute) }, "otlp.interval -1m0s is under the least, 1s"},
		"a negative timeout": {func(o *model.OTLPConfig) { o.Timeout = model.Duration(-time.Second) }, "otlp.timeout must not be negative; got -1s"},
	} {
		if err := Validate(otlpSettings(test.change)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q", name, err, test.want)
		}
	}
	least := otlpSettings(func(o *model.OTLPConfig) { o.Interval = model.Duration(time.Second) })
	unset := otlpSettings(func(*model.OTLPConfig) {})
	if err := model.JoinProblems(Validate(least), Validate(unset)); err != nil {
		t.Fatal(err)
	}
	if time.Duration(least.OTLP.Interval) != time.Second || time.Duration(unset.OTLP.Interval) != 30*time.Second || time.Duration(unset.OTLP.Timeout) != 5*time.Second {
		t.Fatalf("intervals %s and %s, timeout %s", time.Duration(least.OTLP.Interval), time.Duration(unset.OTLP.Interval), time.Duration(unset.OTLP.Timeout))
	}
}

// service.name among the resource attributes would be exported beside the
// one service_name sets, so it is refused in the otlp block and in a static
// target's, pointing at service_name.
func TestServiceNameIsNotAResourceAttribute(t *testing.T) {
	err := Validate(otlpSettings(func(o *model.OTLPConfig) {
		o.ResourceAttributes = map[string]string{"service.name": "billing", "deployment.environment": "prod"}
	}))
	if err == nil || !strings.Contains(err.Error(), "otlp.resource_attributes sets service.name, which otlp.service_name sets; write the name as service_name") {
		t.Fatalf("the otlp block: %v", err)
	}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{
		Name: "billing", Collector: "text", Target: "http://billing.invalid", ExportViaOTLP: true,
		OTLP: model.TargetOTLPConfig{ResourceAttributes: map[string]string{"service.name": "billing"}},
	}}}
	if err := ValidateStaticTargets(file); err == nil || !strings.Contains(err.Error(), `target "billing" otlp.resource_attributes sets service.name, which otlp.service_name sets; write the name as the target's otlp.service_name`) {
		t.Fatalf("a static target: %v", err)
	}
	file.Targets[0].OTLP = model.TargetOTLPConfig{ServiceName: "billing", ResourceAttributes: map[string]string{"deployment.environment": "prod"}}
	if err := ValidateStaticTargets(file); err != nil {
		t.Fatalf("service_name and another attribute: %v", err)
	}
}
