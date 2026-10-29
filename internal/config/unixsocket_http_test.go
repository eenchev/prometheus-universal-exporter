//go:build !select_request_types || (request_type_http && request_type_dns)

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// request.unix_socket (fetch/unixsocket.go).

// socketCollector is an http collector whose request block writes more.
func socketCollector(name, request string) string {
	return "  - name: " + name + "\n    request: {type: http, " + request + "}\n    transform: {type: regex}\n    metrics:\n      - name: " + name + "_value\n        expression: 'value=(\\d+)'\n"
}

// An http collector's unix_socket loads; written empty, relative or beside
// a proxy, on a collector of another type or in defaults, it is refused.
func TestAUnixSocketIsRefusedWhenTheConfigurationLoads(t *testing.T) {
	c, err := Load(testutil.WriteFile(t, "config.yaml", "collectors:\n"+socketCollector("docker", "unix_socket: /var/run/docker.sock")))
	if err != nil {
		t.Fatal(err)
	}
	if got := fetch.CollectorPolicy(collectorNamed(t, c, "docker")).UnixSocket; got != "/var/run/docker.sock" {
		t.Fatalf("the socket is %q", got)
	}
	for name, document := range map[string]string{
		`collector "docker" request.unix_socket is nothing but blanks`:            "collectors:\n" + socketCollector("docker", `unix_socket: " "`),
		`collector "docker" request.unix_socket "docker.sock" is not an absolute`: "collectors:\n" + socketCollector("docker", "unix_socket: docker.sock"),
		`collector "docker" sets request.unix_socket and request.proxy_url`:       "collectors:\n" + socketCollector("docker", "unix_socket: /run/docker.sock, proxy_url: 'http://proxy.example:3128'"),
		"unix_socket": "collectors:\n  - name: names\n    request: {type: dns, unix_socket: /run/docker.sock}\n    transform: {type: jq}\n    metrics: [{name: v, expression: .v}]\n",
		"defaults cannot set request.unix_socket": "defaults:\n  request:\n    unix_socket: /run/docker.sock\ncollectors:\n" + defaultsCollector("web", "http"),
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want %q", err, name)
		}
	}
}

// A collector whose requests go to a socket takes neither the proxy of its
// file's defaults nor its credentials, which the others take; a defaults
// proxy that only such collectors would take is refused, saying why.
func TestACollectorOnAUnixSocketTakesNoProxyFromTheDefaults(t *testing.T) {
	proxy := "defaults:\n  request:\n    proxy_url: http://proxy.corp.example:3128\n    proxy_basic_auth_file: {username: /run/secrets/u, password: /run/secrets/p}\ncollectors:\n"
	c, err := Load(testutil.WriteFile(t, "config.yaml", proxy+defaultsCollector("web", "http")+socketCollector("docker", "unix_socket: /var/run/docker.sock")))
	if err != nil {
		t.Fatal(err)
	}
	docker := collectorNamed(t, c, "docker")
	if docker.Request.ProxyURL != "" || docker.Request.ProxyBasicAuthFile != nil || slices.Contains(docker.FromDefaults, "request.proxy_url") {
		t.Errorf("docker request = %+v, took %q; want no proxy", docker.Request, docker.FromDefaults)
	}
	if web := collectorNamed(t, c, "web"); web.Request.ProxyURL != "http://proxy.corp.example:3128" {
		t.Errorf("web request = %+v, want the defaults' proxy", web.Request)
	}
	_, err = Load(testutil.WriteFile(t, "config.yaml", proxy+socketCollector("docker", "unix_socket: /var/run/docker.sock")))
	if err == nil || !strings.Contains(err.Error(), "defaults.request.proxy_url applies to no collector of the file: each collector it would apply to sets request.proxy_url itself, or sends its requests to its request.unix_socket") {
		t.Errorf("err = %v", err)
	}
}
