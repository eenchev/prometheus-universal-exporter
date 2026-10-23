//go:build !select_request_types || request_type_dns

package exporter

import (
	"encoding/binary"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// dnsCollectors are a dns collector counting a name's A records and one
// telling whether the name exists, both asking the server SERVER.
const dnsCollectors = `web:
  self_metrics:
    verbose: true
collectors:
  - name: records
    request:
      type: dns
      record_type: a
      server: SERVER
    transform:
      type: jq
    metrics:
      - name: dns_records
        expression: .count
      - name: dns_record_up
        items: .answers[]
        expression: "1"
        labels:
          - name: address
            expression: .address
      - name: dns_name_found
        expression: if .not_found then 0 else 1 end
`

// dnsResponder answers every A question of www.example.test over UDP with
// two addresses, and every other name with NXDOMAIN, and returns its
// address. It reads only the question, and answers with a pointer to it.
func dnsResponder(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			query := buf[:n]
			end := 12
			for end < n && query[end] != 0 {
				end += 1 + int(query[end])
			}
			if end+5 > n {
				continue
			}
			name := strings.ToLower(string(query[12:end]))
			qtype := binary.BigEndian.Uint16(query[end+1 : end+3])
			answer := append([]byte{}, query[:12]...)
			answer[2], answer[3] = 0x81, 0x80
			binary.BigEndian.PutUint16(answer[4:], 1)
			binary.BigEndian.PutUint16(answer[6:], 0)
			binary.BigEndian.PutUint16(answer[8:], 0)
			binary.BigEndian.PutUint16(answer[10:], 0)
			answer = append(answer, query[12:end+5]...)
			switch {
			case name != "\x03www\x07example\x04test":
				answer[3] |= 3
			case qtype == 1:
				binary.BigEndian.PutUint16(answer[6:], 2)
				for _, last := range []byte{1, 2} {
					answer = append(answer, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, last)
				}
			}
			_, _ = conn.WriteTo(answer, from)
		}
	}()
	return conn.LocalAddr().String()
}

func dnsExporter(t *testing.T) (*Server, string) {
	t.Helper()
	server := dnsResponder(t)
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", strings.Replace(dnsCollectors, "SERVER", server, 1))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t)), server
}

// A dns collector's probe gives the series its jq rules read from the
// document: the count of records, one series an address, and whether the
// name exists, a name not found being an answer of no records. The verbose
// self-metrics label the lookup with its DNS URI and QUERY.
func TestADNSCollectorProbe(t *testing.T) {
	server, address := dnsExporter(t)
	recorder := probeOnce(t, server, "/probe?collector=records&target=www.example.test", nil)
	body := recorder.Body.String()
	for _, want := range []string{"dns_records 2", `dns_record_up{address="192.0.2.1"} 1`, `dns_record_up{address="192.0.2.2"} 1`, "dns_name_found 1"} {
		if recorder.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("no %q in %d %s", want, recorder.Code, body)
		}
	}
	recorder = probeOnce(t, server, "/probe?collector=records&target=missing.example.test", nil)
	if body := recorder.Body.String(); recorder.Code != http.StatusOK || !strings.Contains(body, "dns_records 0") || !strings.Contains(body, "dns_name_found 0") {
		t.Errorf("a name not found: %d %s", recorder.Code, body)
	}
	metrics := probeOnce(t, server, "/self-metrics", nil).Body.String()
	if want := `http_exporter_scrapes_total{collector="records",http_method="QUERY",url="dns://` + address + `/www.example.test?type=A"} 1`; !strings.Contains(metrics, want) {
		t.Errorf("no %s in\n%s", want, metrics)
	}
}

// A debug probe shows the lookup and the document; a target that is no
// name is answered 400 before anything is asked.
func TestADebugProbeOfADNSCollector(t *testing.T) {
	server, address := dnsExporter(t)
	server.SetDebug(DebugFull)
	body := probeOnce(t, server, "/probe?debug=true&collector=records&target=www.example.test", nil).Body.String()
	for _, want := range []string{"1. QUERY dns://" + address + "/www.example.test -> 2 records", "Dns-Type: A", "Dns-Transport: udp", `"address":"192.0.2.1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	for query, want := range map[string]int{
		"/probe?collector=records":                                http.StatusBadRequest,
		"/probe?collector=records&target=http://www.example.test": http.StatusBadRequest,
		"/probe?collector=records&target=www.example.test:53":     http.StatusBadRequest,
		"/probe?collector=records&target=192.0.2.1":               http.StatusBadRequest,
	} {
		if recorder := probeOnce(t, server, query, nil); recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
	}
}

// The collectors page says what a dns collector's target is: the name to
// resolve.
func TestTheCollectorsPageNamesADNSTarget(t *testing.T) {
	server, _ := dnsExporter(t)
	if body := probeOnce(t, server, "/collectors", nil).Body.String(); !strings.Contains(body, "the name to resolve") {
		t.Errorf("the page does not say what the target is:\n%s", body)
	}
}

// A dns collector's label values take the probe's param_<name> parameters,
// as every type's do: the lookup is the same, and the series carry the
// value the probe gave.
func TestADNSCollectorsLabelValuesTakeProbeParameters(t *testing.T) {
	address := dnsResponder(t)
	server := labelServer(t, `collectors:
  - name: records
    request:
      type: dns
      server: `+address+`
    transform:
      type: jq
    metrics:
      - name: dns_records
        expression: .count
        labels:
          - name: env
            value: "{{param_env:dev}}"
`)
	for query, want := range map[string]string{
		"/probe?collector=records&target=www.example.test&param_env=prod": `dns_records{env="prod"} 2`,
		"/probe?collector=records&target=www.example.test":                `dns_records{env="dev"} 2`,
	} {
		recorder := probeOnce(t, server, query, nil)
		if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || len(got) != 1 || got[0] != want {
			t.Errorf("%s: %d %v, want %s", query, recorder.Code, got, want)
		}
	}
}

// A python transform reads a dns collector's document as data, the records
// a list of objects.
func TestAPythonTransformReadsADNSDocument(t *testing.T) {
	requirePython(t)
	address := dnsResponder(t)
	server := labelServer(t, `collectors:
  - name: records
    request:
      type: dns
      server: `+address+`
    transform:
      type: python
      script: |
        for answer in data["answers"]:
            metric(name="dns_address", type="gauge", value=1, labels={"address": answer["address"]})
        metric(name="dns_found", type="gauge", value=0 if data["not_found"] else 1)
    metrics: []
`)
	recorder := probeOnce(t, server, "/probe?collector=records&target=www.example.test", nil)
	want := []string{`dns_address{address="192.0.2.1"} 1`, `dns_address{address="192.0.2.2"} 1`, "dns_found 1"}
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v", recorder.Code, got, want)
	}
}

// A dns collector's name its denied_targets lists is answered 403 and
// counted in http_exporter_targets_refused_total, which a dns collector
// has a series in, as every type with targets to refuse does.
func TestADNSCollectorCountsTheNamesItRefuses(t *testing.T) {
	address := dnsResponder(t)
	yaml := strings.Replace(dnsCollectors, "      server: SERVER\n", "      server: "+address+"\n      denied_targets: [blocked.example.test]\n", 1)
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", yaml)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	if recorder := probeOnce(t, server, "/probe?collector=records&target=blocked.example.test", nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("a denied name: %d %s", recorder.Code, recorder.Body)
	}
	if metrics := probeOnce(t, server, "/self-metrics", nil).Body.String(); !strings.Contains(metrics, `http_exporter_targets_refused_total{collector="records"} 1`) {
		t.Errorf("the refusal is not counted:\n%s", metrics)
	}
}
