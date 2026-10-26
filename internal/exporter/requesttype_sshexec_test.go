//go:build request_type_sshexec

package exporter

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"golang.org/x/crypto/ssh"
)

// execCollectors is a configuration of an sshexec collector that runs
// systemctl show on a unit a probe names, and reads its answer with a regex
// transform.
const execCollectors = `web:
  self_metrics:
    verbose: true
collectors:
  - name: unit
    request:
      type: sshexec
      command: [systemctl, show, "{{param_unit:nginx.service}}", --property=NRestarts]
      success_exit_codes: [0, 3]
      username: metrics
      private_key_file: KEY
      known_hosts_file: KNOWN
      denied_targets: [169.254.0.0/16]
    transform:
      type: regex
    metrics:
      - name: unit_restarts
        type: gauge
        expression: 'NRestarts=(\d+)'
`

// execHost is an SSH server that answers systemctl show, exiting with 3
// for a unit it does not know, and an exporter of execCollectors reaching
// it.
func execHost(t *testing.T) (*Server, *sshtest.Server) {
	t.Helper()
	dir := t.TempDir()
	keyFile, key := sshtest.WriteClientKey(t, dir, "id_ed25519", "")
	host := sshtest.Start(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{key}, Exec: func(_ context.Context, command string, stdout, _ io.Writer) int {
		if command == `'systemctl' 'show' 'nginx.service' '--property=NRestarts'` {
			_, _ = io.WriteString(stdout, "NRestarts=4\n")
			return 0
		}
		_, _ = io.WriteString(stdout, "NRestarts=0\n")
		return 3
	}})
	known := host.WriteKnownHosts(t, dir)
	path := testutil.WriteIn(t, dir, "config.yaml", strings.NewReplacer("KEY", keyFile, "KNOWN", known).Replace(execCollectors))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, path, testutil.QuietLogger(t))
	t.Cleanup(fetch.CloseConnections)
	return NewServer(manager, "python3", testutil.QuietLogger(t)), host
}

// A probe runs the command and a regex transform reads its output; its
// verbose series carry ssh://host:port and EXEC, and the collector's own
// connection families count the connection the two probes shared.
func TestAnSSHExecCollectorProbe(t *testing.T) {
	exp, host := execHost(t)
	recorder := probeOnce(t, exp, "/probe?collector=unit&target="+url.QueryEscape(host.Addr), nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "unit_restarts 4") {
		t.Fatalf("%d %s", recorder.Code, recorder.Body)
	}
	recorder = probeOnce(t, exp, "/probe?collector=unit&param_unit=cron.service&target="+url.QueryEscape(host.Addr), nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "unit_restarts 0") {
		t.Fatalf("an inactive unit, exit status 3 of success_exit_codes: %d %s", recorder.Code, recorder.Body)
	}
	metrics := probeOnce(t, exp, "/self-metrics", nil).Body.String()
	for _, want := range []string{
		"# HELP http_exporter_sshexec_connections " + selfMetricHelp["http_exporter_sshexec_connections"] + "\n# TYPE http_exporter_sshexec_connections gauge\n",
		`http_exporter_sshexec_connections{collector="unit"} 1`,
		`http_exporter_sshexec_connects_total{collector="unit"} 1`,
		`http_exporter_sshexec_limit_waits_total{collector="unit"} 0`,
		`http_exporter_sshexec_limit_failures_total{collector="unit"} 0`,
		`http_exporter_scrapes_total{collector="unit",http_method="EXEC",url="ssh://` + host.Addr + `"} 2`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %q:\n%s", want, metrics)
		}
	}
	if host.Accepted() != 1 {
		t.Errorf("the host took %d connections, want one kept", host.Accepted())
	}
}

// A probe parameter whose value could be more than one word is answered
// 400 before anything is sent; a target the collector's denied_targets
// lists is answered 403 and counted as refused; and one that is no host,
// 400.
func TestAnSSHExecProbesRefusals(t *testing.T) {
	exp, host := execHost(t)
	for query, want := range map[string]int{
		"param_unit=" + url.QueryEscape("x;reboot") + "&target=" + url.QueryEscape(host.Addr): http.StatusBadRequest,
		"param_unit=" + url.QueryEscape("x y") + "&target=" + url.QueryEscape(host.Addr):      http.StatusBadRequest,
		"param_unit=" + url.QueryEscape(`x"y'`) + "&target=" + url.QueryEscape(host.Addr):     http.StatusBadRequest,
		"target=169.254.169.254":                          http.StatusForbidden,
		"target=" + url.QueryEscape("ssh://u@"+host.Addr): http.StatusBadRequest,
	} {
		recorder := probeOnce(t, exp, "/probe?collector=unit&"+query, nil)
		if recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
		if want == http.StatusBadRequest && strings.HasPrefix(query, "param_unit") && !strings.Contains(recorder.Body.String(), "request.command[2]: the value of param_unit holds") {
			t.Errorf("%s: %s, want the value refused", query, recorder.Body)
		}
	}
	if host.Accepted() != 0 {
		t.Errorf("the host took %d connections", host.Accepted())
	}
	if metrics := probeOnce(t, exp, "/self-metrics", nil).Body.String(); !strings.Contains(metrics, `http_exporter_targets_refused_total{collector="unit"} 1`) {
		t.Errorf("the refusal is not counted:\n%s", metrics)
	}
}

// A debug probe shows the command line as it was sent, quoted, the account,
// how the command ended, and its output as the body; no credential.
func TestADebugProbeOfAnSSHExecCollector(t *testing.T) {
	exp, host := execHost(t)
	exp.SetDebug(DebugFull)
	body := probeOnce(t, exp, "/probe?debug=true&collector=unit&target="+url.QueryEscape(host.Addr), nil).Body.String()
	for _, want := range []string{
		"1. EXEC ssh://" + host.Addr + " -> exited with 0, read 12 bytes",
		"Ssh-User: metrics", "Ssh-Connection: new", `Ssh-Command: 'systemctl' 'show' 'nginx.service' '--property=NRestarts'`, "unit_restarts 4",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the report:\n%s", want, body)
		}
	}
	if strings.Contains(body, "PRIVATE KEY") {
		t.Errorf("the report shows the private key:\n%s", body)
	}
}

// An sshexec collector's output is read by a python transform as a body's
// text, and its label values take the probe's parameters.
func TestAnSSHExecCollectorsOutputInPython(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	keyFile, key := sshtest.WriteClientKey(t, dir, "id_ed25519", "")
	host := sshtest.Start(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{key}, Exec: func(_ context.Context, _ string, stdout, _ io.Writer) int {
		_, _ = io.WriteString(stdout, "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 42 58 42% /\n")
		return 0
	}})
	t.Cleanup(fetch.CloseConnections)
	known := host.WriteKnownHosts(t, dir)
	exp := labelServer(t, `collectors:
  - name: df
    request:
      type: sshexec
      command: [df, -P, /]
      username: metrics
      private_key_file: `+keyFile+`
      known_hosts_file: `+known+`
    transform:
      type: python
      labels:
        site: "{{param_site:lab}}"
      script: |
        for line in response.text.splitlines()[1:]:
            fields = line.split()
            metric(name="fs_used_ratio", type="gauge", value=int(fields[2]) / int(fields[1]), labels={"mount": fields[5]})
    metrics: []
`)
	recorder := probeOnce(t, exp, "/probe?collector=df&param_site=rack1&target="+url.QueryEscape(host.Addr), nil)
	want := []string{`fs_used_ratio{mount="/",site="rack1"} 0.42`}
	if got := seriesLines(recorder.Body.String()); recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v\n%s", recorder.Code, got, want, recorder.Body)
	}
}
