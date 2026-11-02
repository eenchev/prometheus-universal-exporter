//go:build request_type_sshexec && (!select_request_types || request_type_http)

package exporter

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"golang.org/x/crypto/ssh"
)

// The span of an sshexec run that went through tells its exit status and
// how many bytes it wrote to stdout and to stderr, and nothing it wrote,
// which the log never shows of a run that went through.
func TestAnSSHExecRunsSpanCarriesNoStderr(t *testing.T) {
	dir := t.TempDir()
	keyFile, key := sshtest.WriteClientKey(t, dir, "id_ed25519", "")
	const stderr = "warning: reading /etc/app/db.conf: host=db7.corp STDERR-MARK\n"
	host := sshtest.Start(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{key}, Exec: func(_ context.Context, _ string, stdout, errs io.Writer) int {
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(errs, stderr)
		_, _ = io.WriteString(stdout, "NRestarts=4\n")
		return 3
	}})
	known := host.WriteKnownHosts(t, dir)
	path := testutil.WriteIn(t, dir, "config.yaml", strings.NewReplacer("KEY", keyFile, "KNOWN", known).Replace(execCollectors))
	receiver := newLogReceiver(t)
	server := tracedServerOf(t, receiver, path)
	t.Cleanup(fetch.CloseConnections)
	if answer := probeOnce(t, server, "/probe?collector=unit&target="+url.QueryEscape(host.Addr), nil); answer.Code != http.StatusOK {
		t.Fatalf("the probe is answered %d: %s", answer.Code, answer.Body)
	}
	exportTracesNow(server)
	spans := clientSpans(t, receiver)
	if len(spans) != 1 {
		t.Fatalf("%d CLIENT spans, want the run's one", len(spans))
	}
	attrs := spanAttributes(spans[0].otlpSpan)
	if attrs["process.exit.code"] != "3" || attrs["sshexec.stdout.bytes"] != "12" || attrs["sshexec.stderr.bytes"] != strconv.Itoa(len(stderr)) || spans[0].Status != nil {
		t.Errorf("the run's span: %v %+v", attrs, spans[0].Status)
	}
	for _, text := range spanTexts(receiver.spans(t)) {
		if strings.Contains(text, "STDERR-MARK") {
			t.Errorf("a span carries the command's stderr: %q", text)
		}
	}
}
