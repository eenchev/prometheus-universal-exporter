//go:build request_type_redis

package exporter

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/redistest"
)

// One caching redis collector, logging in as exporter, serves servers that
// each take an account of their own: a static target's username and
// password_file log in where the collector's account is refused, its own
// password_file alone logs in as the collector's user with that password,
// and its own tls block reaches a TLS server the collector would speak
// plaintext to,
// and a target with a wrong password is refused, not answered from the
// cache entry of the target beside it with the right one. A probe cannot
// name an account: its username and password_file are not the probe's to
// set, and the collector's account is used. The debug report of a target
// shows its AUTH with the password masked.
func TestRedisStaticTargetsLogInWithTheirOwnAccounts(t *testing.T) {
	tenant := redistest.New(t)
	tenant.RequirePassword("tenant", "t-pass")
	tenant.SetInfo(redistest.Section{Name: "Memory", Lines: []string{"used_memory:7"}})
	secure := redistest.NewTLS(t)
	secure.RequirePassword("exporter", "tls-pass")
	secure.SetInfo(redistest.Section{Name: "Memory", Lines: []string{"used_memory:9"}})
	dir := t.TempDir()
	files := writeSecrets(t, dir, map[string]string{"collector": "s3cret", "tenant": "t-pass", "wrong": "nope", "secure": "tls-pass"})
	server := staticServerFromFiles(t, dir, `collectors:
  - name: memory
    request:
      type: redis
      username: exporter
      password_file: `+files["collector"]+`
      info_sections: [memory]
    cache:
      ttl: 1h
    transform:
      type: jq
    metrics:
      - name: redis_memory_used_bytes
        expression: .info.memory.used_memory
`, `interval: 1m
concurrency: 1
targets:
  - name: own
    collector: memory
    target: `+tenant.Addr+`
    request: {username: tenant, password_file: `+files["tenant"]+`}
  - name: wrong
    collector: memory
    target: `+tenant.Addr+`
    request: {username: tenant, password_file: `+files["wrong"]+`}
  - name: collectors_account
    collector: memory
    target: `+tenant.Addr+`
  - name: secure
    collector: memory
    target: `+secure.Addr+`
    request:
      password_file: `+files["secure"]+`
      tls: {ca_file: `+secure.CAFile+`}
`)
	server.scrapeStaticTargets(t.Context(), 0)
	body := probeOnce(t, server, "/static-targets", nil).Body.String()
	for _, want := range []string{`redis_memory_used_bytes{static_target="own"} 7`, `redis_memory_used_bytes{static_target="secure"} 9`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s in\n%s", want, body)
		}
	}
	checkTargetsUp(t, body, map[string]string{"own": "1", "wrong": "0", "collectors_account": "0", "secure": "1"})
	auths := func() []string {
		var sent []string
		for _, command := range tenant.Commands() {
			if command[0] == "AUTH" {
				sent = append(sent, strings.Join(command, " "))
			}
		}
		return sent
	}
	if got, want := auths(), []string{"AUTH tenant t-pass", "AUTH tenant nope", "AUTH exporter s3cret"}; !slices.Equal(got, want) {
		t.Errorf("the server was sent %q, want %q", got, want)
	}

	before := len(auths())
	query := url.Values{"collector": {"memory"}, "target": {tenant.Addr}, "username": {"tenant"}, "password_file": {files["tenant"]}}
	if recorder := probeOnce(t, server, "/probe?"+query.Encode(), nil); recorder.Code == http.StatusOK {
		t.Errorf("a probe naming the target's account was answered: %s", recorder.Body)
	}
	if got := auths()[before:]; !slices.Equal(got, []string{"AUTH exporter s3cret"}) {
		t.Errorf("a probe naming an account logged in with %q, want the collector's account", got)
	}

	server.SetDebug(DebugFull)
	report := probeOnce(t, server, "/static-targets?debug=own", nil).Body.String()
	if !strings.Contains(report, "Redis-Command: AUTH tenant ***") || strings.Contains(report, "t-pass") {
		t.Errorf("the report does not show AUTH tenant *** alone:\n%s", report)
	}
}
