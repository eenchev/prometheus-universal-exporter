//go:build request_type_snmp

package repository

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// snmpDocsValues are a switch's objects as docs/SNMP.md's collectors read
// them: its name and uptime, and two interfaces, one up and one down.
var snmpDocsValues = []testutil.SNMPAgentValue{
	testutil.SNMPString("1.3.6.1.2.1.1.5.0", "switch-1"),
	testutil.SNMPUnsigned("1.3.6.1.2.1.1.3.0", 0x43, 12345600),
	testutil.SNMPString("1.3.6.1.2.1.2.2.1.2.1", "Gi0/1"),
	testutil.SNMPString("1.3.6.1.2.1.2.2.1.2.2", "Gi0/2"),
	testutil.SNMPInteger("1.3.6.1.2.1.2.2.1.8.1", 1),
	testutil.SNMPInteger("1.3.6.1.2.1.2.2.1.8.2", 2),
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.15.1", 0x42, 1000),
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.15.2", 0x42, 1000),
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.6.1", 0x46, 9123456789),
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.6.2", 0x46, 0),
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.10.1", 0x46, 811223344),
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.10.2", 0x46, 0),
}

// The collectors of docs/SNMP.md work as written: the v2c switch gives its
// uptime labelled with its name and each interface's state, speed and
// traffic, and the v3 router of authPriv its uptime, against stand-in
// agents with the page's community and user, its files written in place of
// the page's paths; the static targets load against the collectors.
func TestSNMPDocumentationExamples(t *testing.T) {
	blocks := pageBlocks(t, "docs/SNMP.md", "yaml")
	dir := t.TempDir()
	community := testutil.WriteIn(t, dir, "community", "s3cr3t-ro")
	auth := testutil.WriteIn(t, dir, "auth", "auth-password")
	priv := testutil.WriteIn(t, dir, "priv", "priv-password")
	switchBlock := strings.Replace(pageBlock(t, blocks, "collectors:\n  - name: switch\n"), "/etc/exporter/snmp-community", community, 1)
	router := pageBlock(t, blocks, "collectors:\n  - name: router\n")
	router = strings.NewReplacer("/etc/exporter/snmp-auth", auth, "/etc/exporter/snmp-priv", priv).Replace(router)
	conf := switchBlock + strings.TrimPrefix(router, "collectors:\n")
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	switchAgent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "s3cr3t-ro", Values: snmpDocsValues})
	routerAgent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{EngineID: []byte{0x80, 0, 0x1f, 0x88, 4, 'r'}, Values: snmpDocsValues, Users: map[string]testutil.SNMPAgentUser{
		"exporter": {Auth: "SHA256", AuthPassword: "auth-password", Priv: "AES", PrivPassword: "priv-password"},
	}})
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	for _, tc := range []struct {
		collector, target string
		wants             []string
	}{
		{"switch", switchAgent.Addr, []string{
			`snmp_uptime_seconds{sys_name="switch-1"} 123456`, `if_up{interface="Gi0/1"} 1`, `if_up{interface="Gi0/2"} 0`,
			`if_speed_bits_per_second{interface="Gi0/1"} 1e+09`, `if_in_octets_total{interface="Gi0/1"} 9.123456789e+09`, `if_out_octets_total{interface="Gi0/1"} 8.11223344e+08`,
		}},
		{"router", routerAgent.Addr, []string{"snmp_uptime_seconds 123456"}},
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?target="+tc.target+"&collector="+tc.collector, nil))
		for _, want := range tc.wants {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("%s: %d, want %q in\n%s", tc.collector, recorder.Code, want, recorder.Body)
			}
		}
	}
	file, err := config.LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", pageBlock(t, blocks, "interval: 1m\ntargets:\n")))
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
}
