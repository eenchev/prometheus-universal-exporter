package exporter

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A static target's own account and tls block are part of the key its
// answers are cached under, which is also the key of the trip it shares
// with identical scrapes and probes and of failure_backoff
// (statictarget.go): targets that differ in the account they log in as,
// the bind_dn among it, or in any key of their tls block, an empty block
// and none among them, share no cached answer, no trip and no backoff.
func TestATargetsOwnAccountAndTLSAreInItsKey(t *testing.T) {
	c := &model.Collector{Name: "server", Request: model.RequestConfig{Type: "redis"}}
	base := model.StaticTarget{Name: "a", Collector: "server", Target: "db.example:6379"}
	with := func(change func(r *model.TargetRequestConfig)) model.StaticTarget {
		target := base
		change(&target.Request)
		return target
	}
	targets := map[string]model.StaticTarget{
		"the collector's":    base,
		"a password file":    with(func(r *model.TargetRequestConfig) { r.PasswordFile = "/secrets/a" }),
		"another password":   with(func(r *model.TargetRequestConfig) { r.PasswordFile = "/secrets/b" }),
		"a user":             with(func(r *model.TargetRequestConfig) { r.Username, r.PasswordFile = "tenant", "/secrets/a" }),
		"a bind_dn":          with(func(r *model.TargetRequestConfig) { r.BindDN, r.PasswordFile = "cn=a,dc=example", "/secrets/a" }),
		"another bind_dn":    with(func(r *model.TargetRequestConfig) { r.BindDN, r.PasswordFile = "cn=b,dc=example", "/secrets/a" }),
		"an empty tls block": with(func(r *model.TargetRequestConfig) { r.TLS = &model.TLSConfig{Set: true} }),
		"a CA":               with(func(r *model.TargetRequestConfig) { r.TLS = &model.TLSConfig{CAFile: "/ca/a.pem", Set: true} }),
		"another CA":         with(func(r *model.TargetRequestConfig) { r.TLS = &model.TLSConfig{CAFile: "/ca/b.pem", Set: true} }),
		"a client certificate": with(func(r *model.TargetRequestConfig) {
			r.TLS = &model.TLSConfig{CAFile: "/ca/a.pem", CertFile: "/tls/a.crt", KeyFile: "/tls/a.key", Set: true}
		}),
		"a server name": with(func(r *model.TargetRequestConfig) {
			r.TLS = &model.TLSConfig{CAFile: "/ca/a.pem", ServerName: "db.internal", Set: true}
		}),
		"no verification": with(func(r *model.TargetRequestConfig) {
			r.TLS = &model.TLSConfig{CAFile: "/ca/a.pem", InsecureSkipVerify: true, Set: true}
		}),
	}
	seen := map[string]string{}
	for name, target := range targets {
		key := probeCacheKey(c, target.Target, targetCacheQuery(&target), nil, targetOwnRequest(&target)...)
		if key == "" {
			t.Fatalf("%s: no key", name)
		}
		if other, ok := seen[key]; ok {
			t.Errorf("a target with %s has the key of one with %s", name, other)
		}
		seen[key] = name
	}
}
