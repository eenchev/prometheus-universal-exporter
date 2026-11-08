//go:build (!select_request_types || request_type_tcp) && (!select_request_types || request_type_http)

package exporter

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The span of a tcp scrape that went through tells the sizes of the
// greeting and that request.greeting matched it, and nothing the server
// greeted with, which the log never shows of a scrape that went through.
func TestATCPRequestsSpanCarriesNoGreeting(t *testing.T) {
	const greeting = "SSH-2.0-OpenSSH_9.6 internal-host-db7.corp secret-build-4711\r\n"
	banner := testutil.NewTCPStub(t, func(conn net.Conn) {
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(conn, greeting)
		_, _ = io.Copy(io.Discard, conn)
	})
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", `collectors:
  - name: ssh
    request:
      type: tcp
      greeting: '^SSH-2\.0-'
    transform:
      type: regex
    metrics:
      - name: ssh_openssh_version
        expression: '^SSH-2\.0-OpenSSH_(\d+\.\d+)'
`)
	receiver := newLogReceiver(t)
	server := tracedServerOf(t, receiver, path)
	if answer := probeOnce(t, server, "/probe?collector=ssh&target="+url.QueryEscape(banner.Addr), nil); answer.Code != http.StatusOK {
		t.Fatalf("the probe is answered %d: %s", answer.Code, answer.Body)
	}
	exportTracesNow(server)
	spans := clientSpans(t, receiver)
	if len(spans) != 1 {
		t.Fatalf("%d CLIENT spans, want the scrape's one", len(spans))
	}
	attrs := spanAttributes(spans[0].otlpSpan)
	if attrs["tcp.greeting.bytes"] != strconv.Itoa(len(greeting)) || attrs["tcp.greeting.matched"] != "true" || spans[0].Status != nil {
		t.Errorf("the scrape's span: %v %+v", attrs, spans[0].Status)
	}
	for _, text := range spanTexts(receiver.spans(t)) {
		if strings.Contains(text, "secret-build-4711") {
			t.Errorf("a span carries what the server greeted with: %q", text)
		}
	}
}
