//go:build !select_request_types || (request_type_http && request_type_dns)

package config

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// request.proxy_url and request.proxy_basic_auth_file (fetch/proxy.go).

// defaults may give the collectors of a file their proxy and its
// credentials' files: those whose type takes them, http here and not dns,
// and not one that names its own; one that goes direct leaves the
// credentials out.
func TestDefaultsGiveTheCollectorsTheirProxy(t *testing.T) {
	c, err := Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request:\n    proxy_url: http://proxy.corp.example:3128\n    proxy_basic_auth_file: {username: /run/secrets/proxy-user, password: /run/secrets/proxy-password}\ncollectors:\n"+
		defaultsCollector("web", "http")+defaultsCollector("names", "dns")+
		"  - name: inside\n    request: {type: http, proxy_url: direct}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	web, names := collectorNamed(t, c, "web"), collectorNamed(t, c, "names")
	if web.Request.ProxyURL != "http://proxy.corp.example:3128" || web.Request.ProxyBasicAuthFile == nil || web.Request.ProxyBasicAuthFile.Password != "/run/secrets/proxy-password" {
		t.Errorf("web request = %+v", web.Request)
	}
	if want := []string{"request.proxy_url", "request.proxy_basic_auth_file"}; !slices.Equal(web.FromDefaults, want) {
		t.Errorf("web took %q, want %q", web.FromDefaults, want)
	}
	if names.Request.ProxyURL != "" || names.Request.ProxyBasicAuthFile != nil {
		t.Errorf("names request = %+v, want no proxy", names.Request)
	}
	inside := collectorNamed(t, c, "inside")
	if inside.Request.ProxyURL != model.ProxyDirect || inside.Request.ProxyBasicAuthFile != nil || slices.Contains(inside.FromDefaults, "request.proxy_basic_auth_file") {
		t.Errorf("inside request = %+v, took %q; want direct, without the credentials meant for the proxy", inside.Request, inside.FromDefaults)
	}
	// Credentials the collector writes itself with no proxy to send them
	// to are refused, and so are the defaults' when its proxy is the
	// environment's, which carries its own in its URL.
	for name, document := range map[string]string{
		"written":           "collectors:\n  - name: inside\n    request: {type: http, proxy_url: direct, proxy_basic_auth_file: {username: /u, password: /p}}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n",
		"from the defaults": "defaults:\n  request:\n    proxy_basic_auth_file: {username: /u, password: /p}\ncollectors:\n" + defaultsCollector("inside", "http"),
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err == nil || !strings.Contains(err.Error(), `collector "inside" request.proxy_basic_auth_file is set without a request.proxy_url`) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A proxy_url defaults cannot give any collector is refused once, naming
// the block; one a collector writes is refused naming the collector, and
// neither quotes credentials written in the URL.
func TestAProxyURLIsRefusedWhenTheConfigurationLoads(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request:\n    proxy_url: ftp://proxy.example\ncollectors:\n"+defaultsCollector("a", "http")+defaultsCollector("b", "http")))
	if err == nil || !strings.HasPrefix(err.Error(), `defaults.request.proxy_url ftp://proxy.example has the scheme "ftp"`) || strings.Contains(err.Error(), `collector "`) {
		t.Errorf("err = %v, want it refused once, naming the block", err)
	}
	_, err = Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: web\n    request: {type: http, proxy_url: 'http://scraper:s3cret@proxy.example:3128'}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n"))
	if err == nil || !strings.Contains(err.Error(), `collector "web" request.proxy_url http://proxy.example:3128 carries credentials`) || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("err = %v", err)
	}
	_, err = Load(testutil.WriteFile(t, "config.yaml", "collectors:\n  - name: names\n    request: {type: dns, proxy_url: 'http://proxy.example:3128'}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n"))
	if err == nil || !strings.Contains(err.Error(), "proxy_url") {
		t.Errorf("a dns collector's proxy_url: err = %v", err)
	}
}

// The configuration's schema has request.proxy_url, of a collector and of
// defaults, described as http and graphite's, whose pattern takes what the
// exporter takes and refuses credentials and a scheme no proxy speaks; and
// proxy_basic_auth_file, which requires both its files.
func TestTheSchemasHaveTheProxy(t *testing.T) {
	at := func(schema map[string]any, path ...string) map[string]any {
		for _, key := range path {
			if items, ok := schema["items"].(map[string]any); ok {
				schema = items
			}
			schema, _ = schema["properties"].(map[string]any)[key].(map[string]any)
			if schema == nil {
				t.Fatalf("no %s in %v", key, path)
			}
		}
		return schema
	}
	for name, key := range map[string]map[string]any{
		"collectors": at(configSchema(), "collectors", "request", "proxy_url"),
		"defaults":   at(configSchema(), "defaults", "request", "proxy_url"),
	} {
		if !strings.HasPrefix(key["description"].(string), "http and graphite: ") || key["pattern"] != fetch.ProxyURLPattern {
			t.Errorf("%s: %v", name, key)
		}
	}
	if files := at(configSchema(), "collectors", "request", "proxy_basic_auth_file"); !slices.Equal(files["required"].([]string), []string{"username", "password"}) {
		t.Errorf("proxy_basic_auth_file: %v", files)
	}
	pattern := regexp.MustCompile(fetch.ProxyURLPattern)
	for _, value := range []string{"", model.ProxyDirect, "http://proxy.example:3128", "https://proxy.example", "socks5://10.0.0.5:1080", "socks5h://proxy.example:1080/", "http://[fd00::5]:3128", "HTTP://proxy.example:3128", "Socks5H://proxy.example:1080"} {
		if !pattern.MatchString(value) {
			t.Errorf("the pattern refuses %q", value)
		}
	}
	for _, value := range []string{"DIRECT", "ftp://proxy.example", "FTP://proxy.example", "proxy.example:3128", "http://user:pw@proxy.example", "http://proxy.example/path", "http://proxy.example?x=1", "http://{{param_p}}:1"} {
		if pattern.MatchString(value) {
			t.Errorf("the pattern takes %q", value)
		}
	}
}

// The credentials of a defaults block that names the proxy are for that
// proxy alone: a collector naming a proxy of its own takes neither, and
// --dry-run (Collector.FromDefaults) does not list them as taken; given
// without a proxy, they are for the collectors that name their own. Taken
// by no collector, they are refused, saying why.
func TestDefaultsProxyCredentialsGoOnlyToTheDefaultsProxy(t *testing.T) {
	own := func(name, proxyURL string) string {
		return "  - name: " + name + "\n    request: {type: http, proxy_url: '" + proxyURL + "'}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n"
	}
	files := "    proxy_basic_auth_file: {username: /run/secrets/proxy-user, password: /run/secrets/proxy-password}\n"
	c, err := Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request:\n    proxy_url: http://proxy.corp.example:3128\n"+files+"collectors:\n"+
		defaultsCollector("web", "http")+own("partner", "socks5://partner-proxy.example:1080")+own("other", "https://other-proxy.example:3128")))
	if err != nil {
		t.Fatal(err)
	}
	if web := collectorNamed(t, c, "web"); web.Request.ProxyBasicAuthFile == nil {
		t.Errorf("web request = %+v, want the defaults' credentials", web.Request)
	}
	for _, name := range []string{"partner", "other"} {
		x := collectorNamed(t, c, name)
		if x.Request.ProxyBasicAuthFile != nil || len(x.FromDefaults) != 0 {
			t.Errorf("%s request = %+v, took %q; want its own proxy without the defaults' credentials", name, x.Request, x.FromDefaults)
		}
	}
	c, err = Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request:\n"+files+"collectors:\n"+own("partner", "socks5://partner-proxy.example:1080")))
	if err != nil {
		t.Fatal(err)
	}
	if partner := collectorNamed(t, c, "partner"); partner.Request.ProxyBasicAuthFile == nil || !slices.Equal(partner.FromDefaults, []string{"request.proxy_basic_auth_file"}) {
		t.Errorf("partner request = %+v, took %q; want the credentials given without a proxy", partner.Request, partner.FromDefaults)
	}
	_, err = Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request:\n    proxy_url: http://proxy.corp.example:3128\n"+files+"collectors:\n"+own("partner", "socks5://partner-proxy.example:1080")))
	if err == nil || !strings.Contains(err.Error(), "defaults.request.proxy_basic_auth_file applies to no collector of the file: each collector it would apply to sets request.proxy_basic_auth_file itself, or names a request.proxy_url of its own, and the credentials are for defaults.request.proxy_url alone") {
		t.Errorf("err = %v", err)
	}
	_, err = Load(testutil.WriteFile(t, "config.yaml", "defaults:\n  request:\n"+files+"collectors:\n"+own("inside", "direct")))
	if err == nil || !strings.Contains(err.Error(), "defaults.request.proxy_basic_auth_file applies to no collector of the file: each collector it would apply to sets request.proxy_basic_auth_file itself, or goes direct with request.proxy_url: direct") {
		t.Errorf("err = %v", err)
	}
}
