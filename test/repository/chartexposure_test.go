package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// webAuthArgs enable the exporter's own Basic Auth from a Secret.
var webAuthArgs = []string{"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth"}

// webAuthConfig is a configuration whose web.basic_auth reads the files
// webAuth mounts at its default mountPath, which the make and CI checks render
// too.
const webAuthConfig = "testdata/chart/web-auth.yaml"

// configFile writes content to a file and returns the options that render it
// as config.yaml: written with --set, a comma would split the value.
func configFile(t *testing.T, content string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--set-file", `config.data.config\.yaml=` + path}
}

// webAuthConfigArgs render webAuthConfig as config.yaml.
var webAuthConfigArgs = []string{"--set-file", `config.data.config\.yaml=` + webAuthConfig}

// withWebAuthConfig is webAuthArgs with config.yaml read from webAuthConfig.
var withWebAuthConfig = append(append([]string{}, webAuthArgs...), webAuthConfigArgs...)

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
			"webAuth":                      withWebAuthConfig,
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

// webAuth only mounts the Secret; a configuration whose web.basic_auth does
// not read its files leaves /probe open all the same. When the chart renders
// the configuration and the exporter is exposed, rendering fails unless
// config.yaml's web.basic_auth is enabled with its password_file in
// webAuth.mountPath, and the message names that path.
func TestChartRefusesToExposeTheExporterWhoseConfigurationIgnoresWebAuth(t *testing.T) {
	helm := requireHelm(t)
	for name, extra := range map[string][]string{
		"no web block":                   nil,
		"web.basic_auth disabled":        configFile(t, `web: {basic_auth: {enabled: false, username_file: /var/run/prometheus-universal-exporter/web-auth/username, password_file: /var/run/prometheus-universal-exporter/web-auth/password}}`),
		"a password written in place":    configFile(t, `web: {basic_auth: {enabled: true, username: u, password: p}}`),
		"a password_file elsewhere":      configFile(t, `web: {basic_auth: {enabled: true, username_file: /secrets/username, password_file: /secrets/password}}`),
		"the files at another mountPath": append([]string{"--set", "webAuth.mountPath=/secrets/web"}, webAuthConfigArgs...),
		"web that is not a mapping":      configFile(t, `web: 3`),
	} {
		for what, c := range exposures {
			out, ok := helmTemplate(t, helm, chartDir, append(append(append([]string{}, c.args...), webAuthArgs...), extra...)...)
			if ok {
				t.Errorf("%s, %s: rendered", what, name)
				continue
			}
			want := c.setting + " exposes the exporter outside the cluster, and webAuth is enabled, but config.data.\"config.yaml\" does not authenticate with its files"
			path := "/var/run/prometheus-universal-exporter/web-auth/password"
			if name == "the files at another mountPath" {
				path = "/secrets/web/password"
			}
			for _, want := range []string{want, "password_file at " + path, "set exposure.allowWithoutWebAuth=true"} {
				if !strings.Contains(out, want) {
					t.Errorf("%s, %s: the refusal does not say %q:\n%s", what, name, want, out)
				}
			}
		}
	}
}

// The configuration that reads the mounted files renders, with the files at
// another mountPath too and a path written with a trailing slash; nothing is
// checked with the opt-out, with a configuration the chart does not render
// (an external ConfigMap, which the operator answers for), with a config.yaml
// that is not YAML, which the exporter refuses, or with nothing exposed.
func TestChartExposesTheExporterWhoseConfigurationReadsWebAuth(t *testing.T) {
	helm := requireHelm(t)
	ingress := append([]string{"--set", "ingress.enabled=true"}, webAuthArgs...)
	for name, args := range map[string][]string{
		"the default mountPath": webAuthConfigArgs,
		"another mountPath": append([]string{"--set", "webAuth.mountPath=/secrets/web/"},
			configFile(t, "web: {basic_auth: {enabled: true, username_file: /secrets/web/username, password_file: /secrets/web//password}}\ncollectors: []\n")...),
		"the opt-out":                    {"--set", "exposure.allowWithoutWebAuth=true"},
		"an external ConfigMap":          {"--set", "config.enabled=false"},
		"a config.yaml that is not YAML": configFile(t, "web: [\n"),
	} {
		if out, ok := helmTemplate(t, helm, chartDir, append(append([]string{}, ingress...), args...)...); !ok {
			t.Errorf("an Ingress with webAuth and %s failed:\n%s", name, out)
		}
	}
	if out, ok := helmTemplate(t, helm, chartDir, webAuthArgs...); !ok {
		t.Errorf("webAuth with a configuration that ignores it, not exposed, failed:\n%s", out)
	}
}
