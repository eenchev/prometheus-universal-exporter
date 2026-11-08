//go:build request_type_ldap

package fetch

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/ldaptest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// sasl_external binds as the client certificate TLS presented, over
// ldaps:// and after StartTLS alike, sending no DN or password; a server
// that saw no certificate refuses the bind, which fails the scrape. It
// needs the certificate, and is refused beside bind_dn or password_file.
func TestAnLDAPCollectorBindsAsItsClientCertificate(t *testing.T) {
	certFile, keyFile := ldaptest.WriteClientCertificate(t, "exporter")
	for name, tc := range map[string]struct {
		server   *ldaptest.Server
		startTLS bool
	}{
		"ldaps":    {ldaptest.NewTLS(t), false},
		"StartTLS": {ldaptest.New(t), true},
	} {
		ldapPeople(tc.server)
		c := ldapCollector(t, func(c *model.Collector) {
			c.Request.TLS = model.TLSConfig{CAFile: tc.server.CAFile, CertFile: certFile, KeyFile: keyFile}
			editLDAP(c, func(r *LDAPConfig) { r.SASLExternal, r.StartTLS = true, tc.startTLS })
		})
		target := tc.server.Addr
		if !tc.startTLS {
			target = "ldaps://" + tc.server.Addr
		}
		if _, err := ldapScrape(t, c, target, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var bind *ldaptest.Request
		for _, r := range tc.server.Requests() {
			if r.Op == "BIND" {
				bind = &r
			}
		}
		if bind == nil || bind.Mechanism != "EXTERNAL" || bind.DN != "" || bind.Password != "" || bind.ClientCN != "exporter" || !bind.TLS {
			t.Errorf("%s: the bind %+v", name, bind)
		}
	}
	// A server that saw no certificate: the bind's own client certificate
	// is left out by a static answer the test makes.
	server := ldaptest.New(t)
	ldapPeople(server)
	server.Answer(func(r ldaptest.Request) ([]byte, bool) {
		if r.Op == "BIND" {
			return ldaptest.Message(r.ID, ldaptest.ResultOp(0x61, 48, "SASL EXTERNAL: no client certificate")), true
		}
		return nil, false
	})
	c := ldapCollector(t, func(c *model.Collector) {
		c.Request.TLS = model.TLSConfig{CAFile: server.CAFile, CertFile: certFile, KeyFile: keyFile}
		editLDAP(c, func(r *LDAPConfig) { r.SASLExternal, r.StartTLS = true, true })
	})
	if _, err := ldapScrape(t, c, server.Addr, nil); err == nil || !strings.Contains(err.Error(), "BIND SASL EXTERNAL") {
		t.Errorf("a refused bind: %v", err)
	}
	for name, edit := range map[string]func(*model.Collector){
		"no certificate": func(c *model.Collector) { editLDAP(c, func(r *LDAPConfig) { r.SASLExternal = true }) },
		"beside bind_dn": func(c *model.Collector) {
			c.Request.TLS = model.TLSConfig{CertFile: certFile, KeyFile: keyFile}
			c.Request.PasswordFile = "/p"
			editLDAP(c, func(r *LDAPConfig) { r.SASLExternal, r.BindDN = true, "cn=reader,dc=example,dc=org" })
		},
	} {
		c := &model.Collector{Name: "directory", Request: model.RequestConfig{Type: RequestTypeLDAP, Own: LDAPConfig{Searches: []LDAPSearch{{Name: "people", Base: "dc=example,dc=org"}}}}}
		edit(c)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), "sasl_external") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
