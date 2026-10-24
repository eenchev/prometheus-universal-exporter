package repository

import (
	"strings"
	"testing"
)

// webAuthArgs enable the exporter's own Basic Auth from a Secret.
var webAuthArgs = []string{"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth"}

// exposures are the values that make the exporter reachable from outside the
// cluster, each with the setting the refusal names.
var exposures = map[string]struct {
	args    []string
	setting string
	kind    string
}{
	"an Ingress":             {[]string{"--set", "ingress.enabled=true"}, "ingress.enabled", "Ingress"},
	"a LoadBalancer Service": {[]string{"--set", "service.type=LoadBalancer"}, "service.type=LoadBalancer", "Service"},
	"a NodePort Service":     {[]string{"--set", "service.type=NodePort"}, "service.type=NodePort", "Service"},
}

// /probe sends requests on its caller's behalf, so an exporter reachable from
// outside the cluster without authentication is an open proxy to its
// targets. An Ingress, a LoadBalancer and a NodePort Service fail rendering
// unless the chart sees the exporter authenticate, through webAuth, or the
// operator says callers are authenticated elsewhere; the message names the
// setting, the open proxy and both ways out.
func TestChartRefusesToExposeTheExporterWithoutAuthentication(t *testing.T) {
	helm := requireHelm(t)
	for what, c := range exposures {
		out, ok := helmTemplate(t, helm, chartDir, c.args...)
		if ok {
			t.Errorf("%s without webAuth rendered", what)
			continue
		}
		for _, want := range []string{
			c.setting + " exposes the exporter outside the cluster, and webAuth is not enabled",
			"an open proxy to every target allowed_targets and denied_targets let through",
			"Enable webAuth and point the configuration's web.basic_auth at its files",
			"at the ingress controller or by web.basic_auth written into the configuration, set exposure.allowWithoutWebAuth=true",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: the refusal does not say %q:\n%s", what, want, out)
			}
		}
	}
}

// Either way out renders the exposing object; a ClusterIP Service, reached
// from inside the cluster only, needs neither, nor does a Service type on a
// Service that is not rendered; and the opt-out written as false is no
// opt-out.
func TestChartExposesTheExporterWithWebAuthOrTheOptOut(t *testing.T) {
	helm := requireHelm(t)
	for what, c := range exposures {
		for way, extra := range map[string][]string{
			"webAuth":                      webAuthArgs,
			"exposure.allowWithoutWebAuth": {"--set", "exposure.allowWithoutWebAuth=true"},
		} {
			docs := manifests(t, helm, append(append([]string{}, c.args...), extra...)...)
			if _, ok := docs[c.kind+"/test-prometheus-universal-exporter"]; !ok {
				t.Errorf("%s with %s rendered no %s", what, way, c.kind)
			}
		}
	}
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "service.type=ClusterIP"); !ok {
		t.Errorf("a ClusterIP Service without webAuth failed:\n%s", out)
	}
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "service.type=LoadBalancer", "--set", "service.enabled=false"); !ok {
		t.Errorf("a LoadBalancer type on a Service that is not rendered failed:\n%s", out)
	}
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "ingress.enabled=true", "--set", "exposure.allowWithoutWebAuth=false"); ok {
		t.Errorf("exposure.allowWithoutWebAuth=false let an Ingress render:\n%s", out)
	}
}
