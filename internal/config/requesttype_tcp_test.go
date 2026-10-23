//go:build request_type_tcp

package config

import (
	"context"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A tcp collector's answer is the server's text, handed to the decoder as
// a body is: a regex transform reads it as text, a csv one as CSV, and a
// decoder written out is kept, since the answer may be JSON, YAML or
// anything else a body is. send and terminator are read as YAML writes
// them, \r\n in double quotes a carriage return and a line feed.
func TestATCPCollectorDecodesAsItsTransformReads(t *testing.T) {
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", `collectors:
  - name: memcached
    request:
      type: tcp
      send: "stats\r\n"
      terminator: "END\r\n"
    transform:
      type: regex
    metrics:
      - name: memcached_curr_connections
        expression: 'STAT curr_connections (\d+)'
  - name: haproxy
    request:
      type: tcp
      send: "show stat\n"
    transform:
      type: csv
    metrics:
      - name: haproxy_current_sessions
        expression: scur
  - name: status
    request:
      type: tcp
    decoder:
      type: json
    transform:
      type: jq
    metrics:
      - name: up
        expression: .up
`))
	if err != nil {
		t.Fatal(err)
	}
	var decoders []string
	for _, c := range cfg.Collectors {
		decoders = append(decoders, c.Decoder.Type)
	}
	if got := strings.Join(decoders, " "); got != "text csv json" {
		t.Errorf("decoders %s", got)
	}
	if r := cfg.Collectors[0].Request; r.Send != "stats\r\n" || r.Terminator != "END\r\n" {
		t.Errorf("send %q, terminator %q", r.Send, r.Terminator)
	}
}

// tcpTLSCollectors are tcp collectors whose tls block is written empty:
// spelt out, as an alias of an anchored empty mapping, merged in with the
// request that holds it, and in a collector file.
const tcpTLSCollectors = `collectors:
  - name: written
    request: &request
      type: tcp
      send: "stats\r\n"
      tls: &empty {}
    transform: {type: regex}
    metrics: [{name: v, expression: 'STAT v (\d+)'}]
  - name: aliased
    request:
      type: tcp
      tls: *empty
    transform: {type: regex}
    metrics: [{name: v, expression: 'STAT v (\d+)'}]
  - name: merged
    request:
      <<: *request
    transform: {type: regex}
    metrics: [{name: v, expression: 'STAT v (\d+)'}]
collector_files: [more.yaml]
`

// A tls block written empty, tls: {}, makes a tcp scrape speak TLS,
// verified with the system roots, and its label tls://: spelt out, as an
// alias, merged in and in a collector file alike. It used to be the zero
// block, and the scrape went out in plaintext.
func TestAnEmptyTLSBlockMakesATCPScrapeSpeakTLS(t *testing.T) {
	server := testutil.NewTLSTCPStub(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "STAT v 1\r\n") })
	dir := t.TempDir()
	testutil.WriteIn(t, dir, "more.yaml", `collectors:
  - name: in_a_file
    request:
      type: tcp
      tls: {}
    transform: {type: regex}
    metrics: [{name: v, expression: 'STAT v (\d+)'}]
`)
	cfg, err := Load(testutil.WriteIn(t, dir, "config.yaml", tcpTLSCollectors))
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Collectors {
		c := &cfg.Collectors[i]
		want := "tls://" + server.Addr
		if label, err := fetch.RequestLabelFor(server.Addr, c, fetch.RequestOverrides{}); err != nil || label != want {
			t.Errorf("%s: label %q, %v; want %s", c.Name, label, err, want)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := fetch.FetchCollector(ctx, server.Addr, c, fetch.RequestOverrides{}, nil)
		cancel()
		// The stand-in's certificate is not among the system roots: the
		// handshake was made, and failed on the certificate.
		if err == nil || !strings.Contains(err.Error(), "TLS handshake with "+server.Addr) || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("%s: %v", c.Name, err)
		}
	}
}

// A reload tells a collector that writes tls: {} from the one it was
// without a tls block, as the exporter's cache key does: the two scrape
// differently.
func TestAReloadSeesAnEmptyTLSBlockWritten(t *testing.T) {
	collector := `collectors:
  - name: stats
    request:
      type: tcp%s
    transform: {type: regex}
    metrics: [{name: v, expression: 'STAT v (\d+)'}]
`
	before, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(collector, "%s", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	after, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(collector, "%s", "\n      tls: {}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if changes := collectorChanges(before, after, nil); !slices.Equal(changes.changed, []string{"stats"}) {
		t.Errorf("changes %+v", changes)
	}
}
