//go:build request_type_snmp

package exporter

import (
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// snmpSwitch is a stand-in switch: its name and uptime, and two
// interfaces, one up and one down, with their octets in.
func snmpSwitch(t *testing.T, config testutil.SNMPAgentConfig) *testutil.SNMPAgent {
	t.Helper()
	config.Values = []testutil.SNMPAgentValue{
		testutil.SNMPString("1.3.6.1.2.1.1.5.0", "sw1"),
		testutil.SNMPUnsigned("1.3.6.1.2.1.1.3.0", 0x43, 12345600),
		testutil.SNMPString("1.3.6.1.2.1.2.2.1.2.1", "Gi0/1"),
		testutil.SNMPString("1.3.6.1.2.1.2.2.1.2.2", "Gi0/2"),
		testutil.SNMPInteger("1.3.6.1.2.1.2.2.1.8.1", 1),
		testutil.SNMPInteger("1.3.6.1.2.1.2.2.1.8.2", 2),
		testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.6.1", 0x46, 9123),
		testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.6.2", 0x46, 0),
	}
	return testutil.NewSNMPAgent(t, config)
}

// snmpCollectors is an snmp collector of the switch's uptime and its
// interfaces, of the community file community.
func snmpCollectors(community string) string {
	return `web:
  self_metrics:
    verbose: true
collectors:
  - name: switch
    request:
      type: snmp
      community_file: ` + community + `
      allowed_targets: [127.0.0.0/8]
      get:
        sysUpTime: 1.3.6.1.2.1.1.3.0
      walk:
        interfaces:
          ifDescr: 1.3.6.1.2.1.2.2.1.2
          ifOperStatus: 1.3.6.1.2.1.2.2.1.8
          ifHCInOctets: 1.3.6.1.2.1.31.1.1.1.6
    transform:
      type: jq
    metrics:
      - name: snmp_uptime_seconds
        expression: .get.sysUpTime.value / 100
      - name: if_up
        items: .walk.interfaces[]
        expression: 'if .ifOperStatus == 1 then 1 else 0 end'
        labels:
          - name: interface
            expression: .ifDescr
      - name: if_in_octets_total
        type: counter
        items: .walk.interfaces[]
        expression: .ifHCInOctets
        labels:
          - name: interface
            expression: .ifDescr
`
}

// snmpExporter is an exporter of snmpCollectors.
func snmpExporter(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	path := testutil.WriteIn(t, dir, "config.yaml", snmpCollectors(testutil.WriteIn(t, dir, "community", "public")))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
}

// An snmp collector's probe gives the series its jq rules read from the
// document: the uptime, and each interface's state and octets by its
// name. The verbose self-metrics label the request snmp://host:port and
// WALK.
func TestAnSNMPCollectorProbe(t *testing.T) {
	server, agent := snmpExporter(t), snmpSwitch(t, testutil.SNMPAgentConfig{Community: "public"})
	recorder := probeOnce(t, server, "/probe?collector=switch&target="+agent.Addr, nil)
	body := recorder.Body.String()
	for _, want := range []string{"snmp_uptime_seconds 123456", `if_up{interface="Gi0/1"} 1`, `if_up{interface="Gi0/2"} 0`, `if_in_octets_total{interface="Gi0/1"} 9123`} {
		if recorder.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%d, no %q in\n%s", recorder.Code, want, body)
		}
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="switch",http_method="WALK",url="snmp://` + agent.Addr + `"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
}

// A debug probe shows each request and the document the rules read; a
// target that is no host:port is answered 400 and one the target lists
// refuse 403 before anything is sent; an agent that never answers fails
// the probe in the snmp stage.
func TestADebugProbeOfAnSNMPCollector(t *testing.T) {
	server, agent := snmpExporter(t), snmpSwitch(t, testutil.SNMPAgentConfig{Community: "public"})
	server.SetDebug(DebugFull)
	body := probeOnce(t, server, "/probe?debug=true&collector=switch&target="+agent.Addr, nil).Body.String()
	for _, want := range []string{"1. GET snmp://" + agent.Addr + " -> 1 of 1 objects, in 1 requests", "Snmp-Objects: sysUpTime=1.3.6.1.2.1.1.3.0", "2. WALK snmp://" + agent.Addr + " -> 2 rows of 3 columns", `"ifDescr":"Gi0/1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	for query, want := range map[string]int{
		"/probe?collector=switch":                             http.StatusBadRequest,
		"/probe?collector=switch&target=snmp://" + agent.Addr: http.StatusBadRequest,
		"/probe?collector=switch&target=192.0.2.1":            http.StatusForbidden,
	} {
		if recorder := probeOnce(t, server, query, nil); recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
	}
	silent := snmpSwitch(t, testutil.SNMPAgentConfig{Community: "other"})
	recorder := probeOnce(t, server, "/probe?collector=switch&target="+silent.Addr+"&timeout=3s", nil)
	if want := "collector switch snmp failed: no answer from " + silent.Addr; recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), want) {
		t.Errorf("%d %s, want %s", recorder.Code, recorder.Body, want)
	}
}

// A python transform reads an snmp collector's document as data.
func TestAnSNMPCollectorsDocumentInPython(t *testing.T) {
	requirePython(t)
	agent := snmpSwitch(t, testutil.SNMPAgentConfig{Community: "public"})
	community := testutil.WriteIn(t, t.TempDir(), "community", "public")
	server := labelServer(t, `collectors:
  - name: switch
    request:
      type: snmp
      community_file: `+community+`
      walk:
        interfaces:
          ifOperStatus: 1.3.6.1.2.1.2.2.1.8
    transform:
      type: python
      script: |
        rows = data["walk"]["interfaces"]
        metric(name="if_down", type="gauge", value=sum(1 for r in rows if r["ifOperStatus"] != 1))
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=switch&target="+agent.Addr, nil)
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || len(got) != 1 || got[0] != "if_down 1" {
		t.Errorf("%d %v", recorder.Code, got)
	}
}

// Static targets of one caching snmp collector read each with its own
// account and its own request_timeout and retries: a target's community
// where the collector's is not the agent's, a v3 target's own user and
// password, and a target that waits less, sent its requests no more than
// its retries say; a target of the wrong community is not answered from
// the cache entry of the target beside it with the right one.
func TestSNMPStaticTargetsReadWithTheirOwnAccounts(t *testing.T) {
	agent := snmpSwitch(t, testutil.SNMPAgentConfig{Community: "tenant"})
	silent := snmpSwitch(t, testutil.SNMPAgentConfig{Community: "nobody"})
	engine := []byte{0x80, 0, 0x1f, 0x88, 4, 'x'}
	secure := snmpSwitch(t, testutil.SNMPAgentConfig{EngineID: engine, Users: map[string]testutil.SNMPAgentUser{"tenant": {Auth: "SHA256", AuthPassword: "tenant-pass"}}})
	dir := t.TempDir()
	files := writeSecrets(t, dir, map[string]string{"collector": "public", "tenant": "tenant", "auth": "collector-pass", "tenant-auth": "tenant-pass"})
	server := staticServerFromFiles(t, dir, `collectors:
  - name: switch
    request:
      type: snmp
      community_file: `+files["collector"]+`
      request_timeout: 2s
      retries: 3
      get:
        sysName: 1.3.6.1.2.1.1.5.0
    cache:
      ttl: 1h
    transform:
      type: jq
    metrics:
      - name: snmp_up
        expression: 'if .get.sysName.value == "sw1" then 1 else 0 end'
  - name: switch_v3
    request:
      type: snmp
      snmp_version: 3
      username: exporter
      security_level: authNoPriv
      auth_protocol: SHA256
      auth_password_file: `+files["auth"]+`
      get:
        sysName: 1.3.6.1.2.1.1.5.0
    transform:
      type: jq
    metrics:
      - name: snmp_up
        expression: 'if .get.sysName.value == "sw1" then 1 else 0 end'
`, `interval: 1m
concurrency: 1
targets:
  - name: own
    collector: switch
    target: `+agent.Addr+`
    request: {community_file: `+files["tenant"]+`}
  - name: collectors_account
    collector: switch
    target: `+agent.Addr+`
    request: {request_timeout: 20ms, retries: 0}
  - name: impatient
    collector: switch
    target: `+silent.Addr+`
    request: {request_timeout: 20ms, retries: 1}
  - name: secure
    collector: switch_v3
    target: `+secure.Addr+`
    request: {username: tenant, auth_password_file: `+files["tenant-auth"]+`}
`)
	server.scrapeStaticTargets(t.Context(), 0)
	body := probeOnce(t, server, "/static-targets", nil).Body.String()
	for _, want := range []string{`snmp_up{static_target="own"} 1`, `snmp_up{static_target="secure"} 1`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s in\n%s", want, body)
		}
	}
	checkTargetsUp(t, body, map[string]string{"own": "1", "collectors_account": "0", "impatient": "0", "secure": "1"})
	if n := silent.Requests(); n != 2 {
		t.Errorf("the impatient target sent %d requests, want 2", n)
	}
}

// A static target of an snmp collector may set no key of another type,
// and none of another version's account.
func TestAStaticTargetOfAnSNMPCollectorIsChecked(t *testing.T) {
	dir := t.TempDir()
	community := testutil.WriteIn(t, dir, "community", "public")
	cfg, err := config.Load(testutil.WriteIn(t, dir, "config.yaml", snmpCollectors(community)))
	if err != nil {
		t.Fatal(err)
	}
	for targets, want := range map[string]string{
		"interval: 1m\ntargets:\n  - name: a\n    collector: switch\n    target: sw1\n    request: {bind_dn: x}\n":             `request.bind_dn`,
		"interval: 1m\ntargets:\n  - name: a\n    collector: switch\n    target: sw1\n    request: {auth_password_file: /x}\n": "and collector \"switch\" reads with SNMP v2c",
		"interval: 1m\ntargets:\n  - name: a\n    collector: switch\n    target: sw1\n    request: {request_timeout: 1ms}\n":   "request.request_timeout is 1ms",
		"interval: 1m\ntargets:\n  - name: a\n    collector: switch\n    target: sw1:99999\n":                                  `has the port "99999"`,
	} {
		file, err := config.ParseStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", targets), []byte(targets))
		if err == nil {
			err = config.ValidateStaticTargets(file)
		}
		if err == nil {
			err = config.ValidateStaticTargetsAgainst(file, cfg)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", targets, err, want)
		}
	}
}
