//go:build !select_request_types || request_type_snmp

package fetch

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// snmpCollector is a validated snmp collector, as edit makes it, of a
// community file holding public.
func snmpCollector(t *testing.T, edit ...func(*model.Collector, *SNMPConfig)) *model.Collector {
	t.Helper()
	r := SNMPConfig{CommunityFile: testutil.WriteFile(t, "community", "public\n"), Get: map[string]string{"sysName": "1.3.6.1.2.1.1.5.0"}}
	c := &model.Collector{Name: "switch", Request: model.RequestConfig{Type: RequestTypeSNMP}}
	for _, e := range edit {
		e(c, &r)
	}
	c.Request.Own = r
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// snmpAnswer is a scrape's document, as a test reads it.
type snmpAnswer struct {
	Get map[string]struct {
		Type  string
		Value any
	}
	Walk     map[string][]map[string]any
	Version  string
	Address  string
	EngineID string `json:"engine_id"`
	Requests int
}

// snmpScrape is a scrape of c of target, with overrides, and its
// document.
func snmpScrape(t *testing.T, c *model.Collector, target string, o RequestOverrides) (snmpAnswer, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, o, nil)
	if err != nil {
		return snmpAnswer{}, err
	}
	var doc snmpAnswer
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v: %s", err, resp.Body)
	}
	return doc, nil
}

// switchValues are a switch's objects: its system group and two rows of
// its interface tables, the second index 10 so that the rows are ordered as
// numbers, not text; ifAlias of the first row only.
var switchValues = []testutil.SNMPAgentValue{
	testutil.SNMPString("1.3.6.1.2.1.1.5.0", "sw1"),
	testutil.SNMPUnsigned("1.3.6.1.2.1.1.3.0", 0x43, 12345600),
	testutil.SNMPOIDValue("1.3.6.1.2.1.1.2.0", "1.3.6.1.4.1.9.1.1208"),
	testutil.SNMPString("1.3.6.1.2.1.2.2.1.2.2", "Gi0/2"),
	testutil.SNMPString("1.3.6.1.2.1.2.2.1.2.10", "Gi0/10"),
	testutil.SNMPInteger("1.3.6.1.2.1.2.2.1.8.2", 1),
	testutil.SNMPInteger("1.3.6.1.2.1.2.2.1.8.10", 2),
	{OID: "1.3.6.1.2.1.2.2.1.6.2", Tag: 0x04, Content: []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e}},
	{OID: "1.3.6.1.2.1.2.2.1.6.10", Tag: 0x04, Content: []byte("ABCDEF")},
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.6.2", 0x46, 18446744073709551615),
	testutil.SNMPUnsigned("1.3.6.1.2.1.31.1.1.1.6.10", 0x46, 0),
	testutil.SNMPString("1.3.6.1.2.1.31.1.1.1.18.2", "uplink"),
	{OID: "1.3.6.1.4.1.2021.10.1.6.1", Tag: 0x44, Content: []byte{0x9f, 0x78, 0x04, 0x3f, 0xc0, 0x00, 0x00}},
	{OID: "1.3.6.1.2.1.4.20.1.1.192.0.2.1", Tag: 0x40, Content: []byte{192, 0, 2, 1}},
	testutil.SNMPString("1.3.6.1.2.1.99.0", "after the tables"),
}

// switchObjects is a collector of the switch's objects and tables.
func switchObjects(_ *model.Collector, r *SNMPConfig) {
	r.Get = map[string]string{"sysName": "1.3.6.1.2.1.1.5.0", "sysUpTime": ".1.3.6.1.2.1.1.3.0", "sysObjectID": "1.3.6.1.2.1.1.2.0", "missing": "1.3.6.1.2.1.1.9.0", "load": "1.3.6.1.4.1.2021.10.1.6.1"}
	r.Walk = map[string]map[string]string{
		"interfaces": {"ifDescr": "1.3.6.1.2.1.2.2.1.2", "ifOperStatus": "1.3.6.1.2.1.2.2.1.8", "ifPhysAddress": "1.3.6.1.2.1.2.2.1.6", "ifHCInOctets": "1.3.6.1.2.1.31.1.1.1.6", "ifAlias": "1.3.6.1.2.1.31.1.1.1.18"},
		"addresses":  {"ipAdEntAddr": "1.3.6.1.2.1.4.20.1.1"},
	}
	r.Hex = []string{"interfaces.ifPhysAddress"}
}

// A v2c collector reads its objects with Get and walks its tables with
// GetBulk: each object the agent has is its type and value, one it does
// not have left out; each table's columns are merged into rows by their
// index, in the order of the index as numbers, a row having the columns
// the agent has of it; a Counter64 keeps every digit, bytes that are not
// text are hexadecimal by colons, and so are those of a column of hex;
// TimeTicks are hundredths, an OID dotted, an IpAddress dotted and an
// Opaque float its number.
func TestAnSNMPCollectorReadsObjectsAndWalksTables(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "public", Values: switchValues})
	c := snmpCollector(t, switchObjects)
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, agent.Addr, c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), `"ifHCInOctets":18446744073709551615`) {
		t.Errorf("a Counter64 lost digits: %s", resp.Body)
	}
	var doc snmpAnswer
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "2c" || doc.Address != agent.Addr || doc.EngineID != "" {
		t.Errorf("document %+v", doc)
	}
	for name, want := range map[string]string{"sysName": "OctetString sw1", "sysUpTime": "TimeTicks 12345600", "sysObjectID": "OID 1.3.6.1.4.1.9.1.1208", "load": "Opaque 1.5"} {
		got, ok := doc.Get[name]
		if !ok || got.Type+" "+strings.TrimSpace(jsonText(got.Value)) != want {
			t.Errorf("get %s: %+v, want %s", name, got, want)
		}
	}
	if _, ok := doc.Get["missing"]; ok || len(doc.Get) != 4 {
		t.Errorf("get %+v", doc.Get)
	}
	rows := doc.Walk["interfaces"]
	if len(rows) != 2 {
		t.Fatalf("interfaces %+v", rows)
	}
	first, second := rows[0], rows[1]
	if first["index"] != "2" || first["ifDescr"] != "Gi0/2" || first["ifOperStatus"] != 1.0 || first["ifPhysAddress"] != "00:1a:2b:3c:4d:5e" || first["ifAlias"] != "uplink" {
		t.Errorf("row 2: %+v", first)
	}
	if second["index"] != "10" || second["ifDescr"] != "Gi0/10" || second["ifPhysAddress"] != "41:42:43:44:45:46" || second["ifHCInOctets"] != 0.0 {
		t.Errorf("row 10: %+v", second)
	}
	if _, ok := second["ifAlias"]; ok {
		t.Errorf("row 10 has an ifAlias: %+v", second)
	}
	if a := doc.Walk["addresses"]; len(a) != 1 || a[0]["index"] != "192.0.2.1" || a[0]["ipAdEntAddr"] != "192.0.2.1" {
		t.Errorf("addresses %+v", a)
	}
	pdus := agent.PDUs()
	if pdus[0] != "get" || !strings.HasPrefix(pdus[1], "getbulk 25") {
		t.Errorf("pdus %v", pdus)
	}
	var traced []string
	for _, r := range trace.Requests() {
		traced = append(traced, r.Method+" "+r.URL+" -> "+r.Outcome)
	}
	want := []string{
		"GET snmp://" + agent.Addr + " -> 4 of 5 objects, in 1 requests",
		"WALK snmp://" + agent.Addr + " -> 1 rows of 1 columns, in 1 requests",
		"WALK snmp://" + agent.Addr + " -> 2 rows of 5 columns, in 5 requests",
	}
	if strings.Join(traced, "\n") != strings.Join(want, "\n") {
		t.Errorf("traced\n%s\nwant\n%s", strings.Join(traced, "\n"), strings.Join(want, "\n"))
	}
	if got := trace.Requests()[0].Header.Get("Snmp-Objects"); !strings.Contains(got, "sysName=1.3.6.1.2.1.1.5.0") {
		t.Errorf("Snmp-Objects %q", got)
	}
	if method := RequestTypes[RequestTypeSNMP].Method(c, RequestOverrides{}); method != snmpMethodWalk {
		t.Errorf("method %q", method)
	}
	if method := RequestTypes[RequestTypeSNMP].Method(snmpCollector(t), RequestOverrides{}); method != snmpMethodGet {
		t.Errorf("method %q", method)
	}
	if label, err := RequestLabelFor(agent.Addr, c, RequestOverrides{}); err != nil || label != "snmp://"+agent.Addr {
		t.Errorf("label %q, %v", label, err)
	}
}

// jsonText is a JSON value written as text.
func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

// A v1 collector walks with GetNext, one value a request, until the
// agent's noSuchName, and a Get of an object the agent does not have,
// which v1 answers with noSuchName naming it, is asked again without it.
func TestAnSNMPv1CollectorWalksWithGetNext(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "public", V1: true, Values: switchValues[:8]})
	c := snmpCollector(t, func(_ *model.Collector, r *SNMPConfig) {
		r.Version = 1
		r.Get = map[string]string{"sysName": "1.3.6.1.2.1.1.5.0", "missing": "1.3.6.1.2.1.1.9.0"}
		r.Walk = map[string]map[string]string{"interfaces": {"ifDescr": "1.3.6.1.2.1.2.2.1.2"}, "last": {"x": "1.3.6.1.2.1.2.2.1.8"}}
	})
	doc, err := snmpScrape(t, c, agent.Addr, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != "1" || doc.Get["sysName"].Value != "sw1" || len(doc.Get) != 1 {
		t.Errorf("get %+v", doc)
	}
	if rows := doc.Walk["interfaces"]; len(rows) != 2 || rows[1]["ifDescr"] != "Gi0/10" {
		t.Errorf("interfaces %+v", rows)
	}
	if rows := doc.Walk["last"]; len(rows) != 2 {
		t.Errorf("a walk to the end of the agent's objects: %+v", rows)
	}
	want := []string{"get", "get", "getnext", "getnext", "getnext", "getnext", "getnext", "getnext"}
	if got := agent.PDUs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pdus %v, want %v", got, want)
	}
}

// A GetBulk the agent answers tooBig is asked again for half as many
// values, until the agent answers it.
func TestAnSNMPWalkAsksForFewerValuesWhenTheAnswerIsTooBig(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "public", BulkLimit: 5, Values: switchValues})
	c := snmpCollector(t, func(_ *model.Collector, r *SNMPConfig) {
		r.Get, r.Walk = nil, map[string]map[string]string{"interfaces": {"ifDescr": "1.3.6.1.2.1.2.2.1.2"}}
	})
	doc, err := snmpScrape(t, c, agent.Addr, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Walk["interfaces"]) != 2 {
		t.Errorf("interfaces %+v", doc.Walk)
	}
	if got := agent.PDUs(); strings.Join(got, ",") != "getbulk 25,getbulk 12,getbulk 6,getbulk 3" {
		t.Errorf("pdus %v", got)
	}
}

// A column of more values than walk_limit fails the scrape as a limit
// exceeded, and so does a document past the response limit.
func TestAnSNMPWalkIsHeldToItsLimits(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "public", Values: switchValues})
	c := snmpCollector(t, func(_ *model.Collector, r *SNMPConfig) {
		r.Get, r.Walk, r.WalkLimit = nil, map[string]map[string]string{"interfaces": {"ifDescr": "1.3.6.1.2.1.2.2.1.2"}}, 1
	})
	if _, err := snmpScrape(t, c, agent.Addr, RequestOverrides{}); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "more than request.walk_limit's 1 values") {
		t.Errorf("walk_limit: %v", err)
	}
	small := snmpCollector(t, func(c *model.Collector, _ *SNMPConfig) { c.Request.MaxResponseBytes = 50 })
	if _, err := snmpScrape(t, small, agent.Addr, RequestOverrides{}); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("max_response_bytes: %v", err)
	}
}

// A request unanswered within request_timeout is sent again, retries times,
// and one never answered fails the scrape saying how many were sent; a
// static target's own request_timeout, retries and community are used in
// place of the collector's.
func TestAnSNMPRequestUnansweredIsSentAgain(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "public", Drop: 1, Values: switchValues})
	c := snmpCollector(t, func(_ *model.Collector, r *SNMPConfig) { r.RequestTimeout = model.Duration(50 * time.Millisecond) })
	doc, err := snmpScrape(t, c, agent.Addr, RequestOverrides{})
	if err != nil || doc.Requests != 2 || doc.Get["sysName"].Value != "sw1" {
		t.Errorf("one dropped: %+v, %v", doc, err)
	}
	other := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "private", Values: switchValues})
	began := time.Now()
	_, err = snmpScrape(t, c, other.Addr, RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "no answer from "+other.Addr+" to 2 requests, each waited for 50ms") {
		t.Errorf("a community the agent drops: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("two requests of 50ms took %s", took)
	}
	zero, wait := 0, 30*time.Millisecond
	_, err = snmpScrape(t, c, other.Addr, RequestOverrides{Retries: &zero, RequestTimeout: &wait})
	if err == nil || !strings.Contains(err.Error(), "to 1 requests, each waited for 30ms") || other.Requests() != 3 {
		t.Errorf("the target's own retries and request_timeout: %v, %d requests", err, other.Requests())
	}
	private := testutil.WriteFile(t, "private", "private")
	if doc, err := snmpScrape(t, c, other.Addr, RequestOverrides{CommunityFile: private}); err != nil || doc.Get["sysName"].Value != "sw1" {
		t.Errorf("the target's own community: %+v, %v", doc, err)
	}
	if _, err := snmpScrape(t, c, testutil.RefusingAddress(t), RequestOverrides{}); err == nil {
		t.Error("a port no agent listens on answered")
	}
}

// The target lists are kept to, and the cloud metadata service is refused
// without them, before anything is sent.
func TestAnSNMPCollectorKeepsToItsTargetLists(t *testing.T) {
	if _, err := snmpScrape(t, snmpCollector(t), "169.254.169.254", RequestOverrides{}); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("the metadata service: %v", err)
	}
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "public", Values: switchValues})
	denied := snmpCollector(t, func(c *model.Collector, _ *SNMPConfig) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := snmpScrape(t, denied, agent.Addr, RequestOverrides{}); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("denied: %v", err)
	}
	if n := agent.Requests(); n != 0 {
		t.Errorf("a refused target was sent %d requests", n)
	}
}

// RFC 3414 § A.3's keys: the password maplesyrup, of MD5 and of SHA, made
// a key and localized to the engine 000000000000000000000002.
func TestSNMPv3KeysAreRFC3414s(t *testing.T) {
	engine, _ := hex.DecodeString("000000000000000000000002")
	for name, tc := range map[string]struct{ ku, kul string }{
		"MD5": {"9faf3283884e92834ebc9847d8edd963", "526f5eed9fcce26f8964c2930787d82b"},
		"SHA": {"9fb5cc0381497b3793528939ff788d5d79145211", "6695febc9288e36282235fc7151f128497b38f3f"},
	} {
		auth := snmpAuths[name]
		ku := snmpPasswordKey(auth, []byte("maplesyrup"))
		if hex.EncodeToString(ku) != tc.ku {
			t.Errorf("%s Ku %x, want %s", name, ku, tc.ku)
		}
		if kul := snmpLocalize(auth, ku, engine); hex.EncodeToString(kul) != tc.kul {
			t.Errorf("%s Kul %x, want %s", name, kul, tc.kul)
		}
	}
	// A key too short for AES-256 is extended one way or the other, the
	// two ways giving different keys of the same start.
	auth := snmpAuths["MD5"]
	blumenthal := snmpPrivKey(auth, snmpPrivs["AES256"], []byte("maplesyrup"), engine)
	reeder := snmpPrivKey(auth, snmpPrivs["AES256C"], []byte("maplesyrup"), engine)
	if len(blumenthal) != 32 || len(reeder) != 32 || hex.EncodeToString(blumenthal[:16]) != "526f5eed9fcce26f8964c2930787d82b" || string(blumenthal) == string(reeder) || string(blumenthal[:16]) != string(reeder[:16]) {
		t.Errorf("extended keys %x and %x", blumenthal, reeder)
	}
}

// snmpV3Collector is a collector of user exporter at level, its
// protocols auth and priv, of the passwords authpassword and
// privpassword.
func snmpV3Collector(t *testing.T, level, auth, priv string) *model.Collector {
	t.Helper()
	return snmpCollector(t, func(c *model.Collector, r *SNMPConfig) {
		r.Version, r.CommunityFile, r.SecurityLevel = 3, "", level
		c.Request.Username = "exporter"
		if auth != "" {
			r.AuthProtocol, r.AuthPasswordFile = auth, testutil.WriteFile(t, "auth", "authpassword")
		}
		if priv != "" {
			r.PrivProtocol, r.PrivPasswordFile = priv, testutil.WriteFile(t, "priv", "privpassword")
		}
	})
}

// A v3 collector discovers the engine, then reads as its user at each
// security level, with each authentication protocol and each privacy
// protocol, both ways of extending a short key among them; the document
// names the engine.
func TestAnSNMPv3CollectorReadsAtEachSecurityLevel(t *testing.T) {
	engine := []byte{0x80, 0x00, 0x1f, 0x88, 0x04, 'p', 'u', 'e'}
	type user struct{ level, auth, priv string }
	users := []user{{snmpNoAuthNoPriv, "", ""}}
	for _, auth := range []string{"MD5", "SHA", "SHA224", "SHA256", "SHA384", "SHA512"} {
		users = append(users, user{snmpAuthNoPriv, auth, ""})
	}
	for _, priv := range []string{"DES", "AES", "AES192", "AES256", "AES192C", "AES256C"} {
		users = append(users, user{snmpAuthPriv, "MD5", priv}, user{snmpAuthPriv, "SHA", priv}, user{snmpAuthPriv, "SHA512", priv})
	}
	for _, u := range users {
		agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{EngineID: engine, Boots: 7, Values: switchValues, Users: map[string]testutil.SNMPAgentUser{
			"exporter": {Auth: u.auth, AuthPassword: "authpassword", Priv: u.priv, PrivPassword: "privpassword"},
		}})
		c := snmpV3Collector(t, u.level, u.auth, u.priv)
		r := snmpRequest(c)
		r.Walk = map[string]map[string]string{"interfaces": {"ifDescr": "1.3.6.1.2.1.2.2.1.2"}}
		c.Request.Own = r
		doc, err := snmpScrape(t, c, agent.Addr, RequestOverrides{})
		if err != nil {
			t.Errorf("%+v: %v", u, err)
			continue
		}
		if doc.Version != "3" || doc.EngineID != hex.EncodeToString(engine) || doc.Get["sysName"].Value != "sw1" || len(doc.Walk["interfaces"]) != 2 {
			t.Errorf("%+v: %+v", u, doc)
		}
		if pdus := agent.PDUs(); pdus[0] != "discovery" {
			t.Errorf("%+v: pdus %v", u, pdus)
		}
	}
}

// A v3 request the agent refuses fails the scrape saying why: an unknown
// user, a wrong authentication password, a wrong privacy password; and a
// Report that the request was outside the engine's time window has it sent
// again, once, with the time the Report brings.
func TestAnSNMPv3RequestTheAgentRefusesSaysWhy(t *testing.T) {
	engine := []byte{0x80, 0x00, 0x1f, 0x88, 0x04, 'p', 'u', 'e'}
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{EngineID: engine, Values: switchValues, SkewOnce: true, Users: map[string]testutil.SNMPAgentUser{
		"exporter": {Auth: "SHA", AuthPassword: "authpassword", Priv: "AES", PrivPassword: "privpassword"},
	}})
	c := snmpV3Collector(t, snmpAuthPriv, "SHA", "AES")
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, agent.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Errorf("a Report of the time window: %v", err)
	}
	if r := trace.Requests(); len(r) != 2 || r[0].Method != snmpMethodDiscover || !strings.HasPrefix(r[0].Outcome, "engine 80001f8804707565, boots 0") || r[0].Header.Get("Snmp-User") != "exporter" {
		t.Errorf("traced %+v", r)
	}
	for name, tc := range map[string]struct {
		edit func(*model.Collector, *SNMPConfig)
		want string
	}{
		"an unknown user": {func(c *model.Collector, _ *SNMPConfig) { c.Request.Username = "nobody" }, "the agent knows no such user (usmStatsUnknownUserNames)"},
		"a wrong auth password": {func(_ *model.Collector, r *SNMPConfig) {
			r.AuthPasswordFile = testutil.WriteFile(t, "wrong", "wrongpassword")
		}, "usmStatsWrongDigests"},
		"a wrong priv password": {func(_ *model.Collector, r *SNMPConfig) {
			r.PrivPasswordFile = testutil.WriteFile(t, "wrong", "wrongpassword")
		}, "usmStatsDecryptionErrors"},
		"another level": {func(_ *model.Collector, r *SNMPConfig) {
			r.SecurityLevel, r.PrivProtocol, r.PrivPasswordFile = snmpAuthNoPriv, "", ""
		}, "usmStatsUnsupportedSecLevels"},
		"a short password":       {func(_ *model.Collector, r *SNMPConfig) { r.AuthPasswordFile = testutil.WriteFile(t, "short", "short") }, "is shorter than the 8 characters"},
		"an empty password file": {func(_ *model.Collector, r *SNMPConfig) { r.AuthPasswordFile = testutil.WriteFile(t, "empty", "\n") }, "is empty"},
	} {
		bad := snmpV3Collector(t, snmpAuthPriv, "SHA", "AES")
		r := snmpRequest(bad)
		tc.edit(bad, &r)
		bad.Request.Own = r
		if _, err := snmpScrape(t, bad, agent.Addr, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// A wrong privacy password is a decryption error whatever the salt: what
// the agent decrypts with the wrong key is garbage that, now and then, reads
// as the start of a value, and is no scoped PDU all the same.
func TestAWrongSNMPv3PrivPasswordIsADecryptionErrorEachTime(t *testing.T) {
	tier.Slow(t)
	engine := []byte{0x80, 0x00, 0x1f, 0x88, 0x04, 'p', 'u', 'e'}
	for _, priv := range []string{"DES", "AES"} {
		agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{EngineID: engine, Values: switchValues, Users: map[string]testutil.SNMPAgentUser{
			"exporter": {Auth: "SHA", AuthPassword: "authpassword", Priv: priv, PrivPassword: "privpassword"},
		}})
		for i := range 40 {
			bad := snmpV3Collector(t, snmpAuthPriv, "SHA", priv)
			r := snmpRequest(bad)
			r.PrivPasswordFile = testutil.WriteFile(t, "wrong", "wrongpassword")
			bad.Request.Own = r
			if _, err := snmpScrape(t, bad, agent.Addr, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "usmStatsDecryptionErrors") {
				t.Fatalf("%s, scrape %d: %v", priv, i, err)
			}
		}
	}
}

// The keys of an account are those of its version and level, each it
// needs required; the objects are named and numbered; the limits are
// held to.
func TestAnSNMPCollectorsKeysAreChecked(t *testing.T) {
	file := testutil.WriteFile(t, "secret", "secretpassword")
	v3 := func(edit func(*model.Collector, *SNMPConfig)) func(*model.Collector, *SNMPConfig) {
		return func(c *model.Collector, r *SNMPConfig) {
			r.Version, r.CommunityFile, r.SecurityLevel, r.AuthProtocol, r.AuthPasswordFile = 3, "", snmpAuthNoPriv, "SHA", file
			c.Request.Username = "exporter"
			edit(c, r)
		}
	}
	negative := -1
	for name, tc := range map[string]struct {
		edit func(*model.Collector, *SNMPConfig)
		want string
	}{
		"version 4":               {func(_ *model.Collector, r *SNMPConfig) { r.Version = 4 }, "request.snmp_version is 4; it is 1, 2 for v2c, or 3"},
		"no community":            {func(_ *model.Collector, r *SNMPConfig) { r.CommunityFile = "" }, "has no request.community_file"},
		"a v3 key of v2c":         {func(_ *model.Collector, r *SNMPConfig) { r.AuthProtocol = "SHA" }, "sets request.auth_protocol with snmp_version 2; it is a key of SNMPv3's user"},
		"a username of v1":        {func(c *model.Collector, r *SNMPConfig) { r.Version, c.Request.Username = 1, "x" }, "sets request.username with snmp_version 1"},
		"a community of v3":       {v3(func(_ *model.Collector, r *SNMPConfig) { r.CommunityFile = file }), "sets request.community_file with snmp_version 3"},
		"no user":                 {v3(func(c *model.Collector, _ *SNMPConfig) { c.Request.Username = "" }), "has no request.username"},
		"no level":                {v3(func(_ *model.Collector, r *SNMPConfig) { r.SecurityLevel = "" }), "has no request.security_level"},
		"a level of another case": {v3(func(_ *model.Collector, r *SNMPConfig) { r.SecurityLevel = "authnopriv" }), `request.security_level is "authnopriv"`},
		"no auth protocol":        {v3(func(_ *model.Collector, r *SNMPConfig) { r.AuthProtocol = "" }), `request.auth_protocol is ""; with security_level authNoPriv it is one of MD5`},
		"no auth password":        {v3(func(_ *model.Collector, r *SNMPConfig) { r.AuthPasswordFile = "" }), "has no request.auth_password_file"},
		"priv without authPriv":   {v3(func(_ *model.Collector, r *SNMPConfig) { r.PrivProtocol = "AES" }), "sets request.priv_protocol or priv_password_file with security_level authNoPriv"},
		"auth with noAuthNoPriv":  {v3(func(_ *model.Collector, r *SNMPConfig) { r.SecurityLevel = snmpNoAuthNoPriv }), "with security_level noAuthNoPriv, which neither signs nor encrypts"},
		"a priv protocol of none": {v3(func(_ *model.Collector, r *SNMPConfig) { r.SecurityLevel, r.PrivProtocol = snmpAuthPriv, "3DES" }), `request.priv_protocol is "3DES"`},
		"no priv password":        {v3(func(_ *model.Collector, r *SNMPConfig) { r.SecurityLevel, r.PrivProtocol = snmpAuthPriv, "AES" }), "has no request.priv_password_file"},
		"nothing read":            {func(_ *model.Collector, r *SNMPConfig) { r.Get = nil }, "reads nothing"},
		"a name of a dash":        {func(_ *model.Collector, r *SNMPConfig) { r.Get = map[string]string{"sys-name": "1.3.6"} }, `request.get names "sys-name"`},
		"a MIB name":              {func(_ *model.Collector, r *SNMPConfig) { r.Get = map[string]string{"x": "sysName.0"} }, "as the names of a MIB are not read"},
		"one arc":                 {func(_ *model.Collector, r *SNMPConfig) { r.Get = map[string]string{"x": "1"} }, "it has 1 arcs"},
		"a first arc of 3":        {func(_ *model.Collector, r *SNMPConfig) { r.Get = map[string]string{"x": "3.1"} }, "begins with 3"},
		"a second arc of 40":      {func(_ *model.Collector, r *SNMPConfig) { r.Get = map[string]string{"x": "1.40"} }, "its second arc is 40"},
		"an object twice":         {func(_ *model.Collector, r *SNMPConfig) { r.Get = map[string]string{"a": "1.3.6.1", "b": ".1.3.6.1"} }, "names 1.3.6.1 twice, as a and b"},
		"a table of no column":    {func(_ *model.Collector, r *SNMPConfig) { r.Walk = map[string]map[string]string{"t": {}} }, "request.walk.t names no column"},
		"a column named index": {func(_ *model.Collector, r *SNMPConfig) {
			r.Walk = map[string]map[string]string{"t": {"index": "1.3.6.1"}}
		}, "names a column index"},
		"a hex of nothing":         {func(_ *model.Collector, r *SNMPConfig) { r.Hex = []string{"ifPhysAddress"} }, `request.hex names "ifPhysAddress", which is no object`},
		"max_repetitions of v1":    {func(_ *model.Collector, r *SNMPConfig) { r.Version, r.MaxRepetitions = 1, 25 }, "sets request.max_repetitions with snmp_version 1"},
		"max_repetitions 256":      {func(_ *model.Collector, r *SNMPConfig) { r.MaxRepetitions = 256 }, "request.max_repetitions is 256"},
		"a request_timeout of 1ms": {func(_ *model.Collector, r *SNMPConfig) { r.RequestTimeout = model.Duration(time.Millisecond) }, "request.request_timeout is 1ms"},
		"retries -1":               {func(_ *model.Collector, r *SNMPConfig) { r.Retries = &negative }, "request.retries is -1"},
		"walk_limit -1":            {func(_ *model.Collector, r *SNMPConfig) { r.WalkLimit = -1 }, "request.walk_limit is -1"},
		"a key of another type":    {func(c *model.Collector, _ *SNMPConfig) { c.Request.Foreign = []string{"send"} }, "sets request.send, which does not apply"},
		"a tls block":              {func(c *model.Collector, _ *SNMPConfig) { c.Request.TLS.CAFile = "/ca.pem" }, "sets request.tls, which does not apply"},
	} {
		r := SNMPConfig{CommunityFile: file, Get: map[string]string{"sysName": "1.3.6.1.2.1.1.5.0"}}
		c := &model.Collector{Name: "switch", Request: model.RequestConfig{Type: RequestTypeSNMP}}
		tc.edit(c, &r)
		c.Request.Own = r
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	c := snmpCollector(t)
	if r := snmpRequest(c); r.Version != 2 || r.MaxRepetitions != 25 || time.Duration(r.RequestTimeout) != time.Second || *r.Retries != 1 || r.WalkLimit != 10000 {
		t.Errorf("defaults %+v", r)
	}
	for target, want := range map[string]string{
		"switch-1.example.com": "",
		"192.0.2.1:1161":       "",
		"[2001:db8::1]":        "",
		"[2001:db8::1]:161":    "",
		"2001:db8::1":          "",
		"snmp://switch":        "has a scheme",
		"switch/x":             "has a path, query or user",
		"switch:0":             `has the port "0"`,
		"switch:snmp":          `has the port "snmp"`,
		":161":                 "names no host",
		"":                     "target is required",
	} {
		err := CheckTarget(c, target, false)
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", target, err, want)
		}
	}
}

// A static target's own account goes with its collector's version and
// level, and its request_timeout and retries are held as the collector's
// are; --dry-run warns of a file a scrape would fail on.
func TestAStaticTargetOfAnSNMPCollectorKeepsToItsAccount(t *testing.T) {
	file := testutil.WriteFile(t, "secret", "secretpassword")
	v2 := snmpCollector(t)
	v3 := snmpV3Collector(t, snmpAuthNoPriv, "SHA", "")
	short, negative := model.Duration(time.Millisecond), -1
	for name, tc := range map[string]struct {
		c    *model.Collector
		own  model.TargetRequestConfig
		want string
	}{
		"a community of v2c":      {v2, model.TargetRequestConfig{CommunityFile: file}, ""},
		"a user of v2c":           {v2, model.TargetRequestConfig{Username: "x"}, "sets request.username, and collector \"switch\" reads with SNMP v2c"},
		"a community of v3":       {v3, model.TargetRequestConfig{CommunityFile: file}, "reads with SNMPv3, which has no community"},
		"a user and auth of v3":   {v3, model.TargetRequestConfig{Username: "other", AuthPasswordFile: file}, ""},
		"a priv of authNoPriv":    {v3, model.TargetRequestConfig{PrivPasswordFile: file}, "is of security_level authNoPriv, which encrypts nothing"},
		"a placeholder":           {v2, model.TargetRequestConfig{CommunityFile: "/{{param_x}}"}, "has a placeholder"},
		"a short request_timeout": {v2, model.TargetRequestConfig{RequestTimeout: &short}, "request.request_timeout is 1ms"},
		"retries -1":              {v2, model.TargetRequestConfig{Retries: &negative}, "request.retries is -1"},
	} {
		err := checkSNMPTargetRequest(tc.c, &model.StaticTarget{Name: "sw", Request: tc.own})
		if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	missing := snmpCollector(t, func(_ *model.Collector, r *SNMPConfig) { r.CommunityFile = "/nonexistent/community" })
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.community_file") {
		t.Errorf("warnings %v", warnings)
	}
	if warnings := FileWarnings(v2, &model.StaticTarget{Name: "sw", Request: model.TargetRequestConfig{CommunityFile: "/nonexistent/x"}}); len(warnings) != 1 || !strings.Contains(warnings[0], `target "sw"`) {
		t.Errorf("warnings %v", warnings)
	}
}

// A v3 answer is held to the security level of its request: one signed
// but not encrypted, to a request of authPriv, fails the scrape.
func TestAnSNMPv3AnswerIsHeldToTheRequestsSecurityLevel(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{EngineID: []byte{0x80, 0, 0x1f, 0x88, 4, 'u'}, Values: switchValues, Unencrypted: true, Users: map[string]testutil.SNMPAgentUser{
		"exporter": {Auth: "SHA", AuthPassword: "authpassword", Priv: "AES", PrivPassword: "privpassword"},
	}})
	if _, err := snmpScrape(t, snmpV3Collector(t, snmpAuthPriv, "SHA", "AES"), agent.Addr, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "answered without encrypting its answer, which security_level authPriv asks of it") {
		t.Errorf("an answer in the clear: %v", err)
	}
}

// An answer to a request sent before it was sent again is its answer
// still, of v3 as of v2c, though each sending of v3 has a msgID of its
// own.
func TestAnSNMPAnswerToAnEarlierSendingIsTaken(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{EngineID: []byte{0x80, 0, 0x1f, 0x88, 4, 'l'}, Values: switchValues, Late: 150 * time.Millisecond, Users: map[string]testutil.SNMPAgentUser{"exporter": {}}})
	c := snmpV3Collector(t, snmpNoAuthNoPriv, "", "")
	r := snmpRequest(c)
	r.RequestTimeout = model.Duration(100 * time.Millisecond)
	c.Request.Own = r
	doc, err := snmpScrape(t, c, agent.Addr, RequestOverrides{})
	if err != nil || doc.Get["sysName"].Value != "sw1" || doc.Requests != 3 {
		t.Errorf("%+v, %v", doc, err)
	}
}

// deadlineOnly is a context whose deadline has come before it says it is
// done, as a socket's deadline can come before the context's timer fires.
type deadlineOnly struct {
	context.Context
	deadline time.Time
}

func (d deadlineOnly) Deadline() (time.Time, bool) { return d.deadline, true }

// A scrape whose deadline comes while it waits fails with the deadline's
// error, though the context does not say so yet, rather than sending its
// requests again into a deadline that is past.
func TestAnSNMPScrapeEndsAtItsDeadline(t *testing.T) {
	agent := testutil.NewSNMPAgent(t, testutil.SNMPAgentConfig{Community: "other", Values: switchValues})
	c := snmpCollector(t)
	ctx := deadlineOnly{context.Background(), time.Now().Add(100 * time.Millisecond)}
	_, err := FetchCollector(ctx, agent.Addr, c, RequestOverrides{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || agent.Requests() != 1 {
		t.Errorf("%v, %d requests", err, agent.Requests())
	}
}

// An identifier an agent sends with arcs past 4294967295, the first two
// together among them, is refused, as it could not be written again.
func TestAnSNMPIdentifierOfAnArcTooLargeIsRefused(t *testing.T) {
	for name, content := range map[string][]byte{
		"a first two past":      {0x90, 0x80, 0x80, 0x80, 0x32},
		"an arc past":           {0x2b, 0x90, 0x80, 0x80, 0x80, 0x00},
		"one that does not end": {0x2b, 0x86},
	} {
		if oid, err := snmpOIDValue(content); err == nil {
			t.Errorf("%s: %s", name, oid)
		}
	}
	if oid, err := snmpOIDValue([]byte{0x2b, 0x06, 0x01, 0x8f, 0xff, 0xff, 0xff, 0x7f}); err != nil || oid.String() != "1.3.6.1.4294967295" {
		t.Errorf("the largest arc: %s, %v", oid, err)
	}
}
