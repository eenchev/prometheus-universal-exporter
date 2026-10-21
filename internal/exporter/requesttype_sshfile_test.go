//go:build !select_request_types || request_type_sshfile

package exporter

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"golang.org/x/crypto/ssh"
)

// sshHost is an SSH server serving a directory with a metrics file, and the
// configuration of two sshfile collectors that read it with one key, so they
// share a connection: remote and also.
type sshHost struct {
	server     *sshtest.Server
	root, dir  string
	keyFile    string
	password   string
	configPath string
}

const sshCollectors = `web:
  self_metrics:
    verbose: true
collectors:
  - name: remote
    request:
      type: sshfile
      root: ROOT
      path: app.prom
      username: metrics
      private_key_file: KEY
      known_hosts_file: KNOWN
    transform:
      type: prometheus
  - name: also
    request:
      type: sshfile
      root: ROOT
      path: app.prom
      username: metrics
      private_key_file: KEY
      known_hosts_file: KNOWN
    transform:
      type: prometheus
`

func newSSHHost(t *testing.T) *sshHost {
	t.Helper()
	h := &sshHost{root: t.TempDir(), dir: t.TempDir()}
	var key ssh.PublicKey
	h.keyFile, key = sshtest.WriteClientKey(t, h.dir, "id_ed25519", "")
	h.password = sshtest.WriteFile(t, h.dir, "password", "pw\n")
	h.server = sshtest.Start(t, sshtest.Options{Password: "pw", AuthorizedKeys: []ssh.PublicKey{key}})
	known := h.server.WriteKnownHosts(t, h.dir)
	testutil.WriteIn(t, h.root, "app.prom", "# TYPE app_jobs_total counter\napp_jobs_total 7\n")
	h.configPath = testutil.WriteIn(t, h.dir, "config.yaml", strings.NewReplacer("ROOT", h.root, "KEY", h.keyFile, "KNOWN", known).Replace(sshCollectors))
	t.Cleanup(fetch.CloseConnections)
	return h
}

// exporter is an exporter of the host's configuration, as it is on disk.
func (h *sshHost) exporter(t *testing.T) (*Server, *config.Manager) {
	t.Helper()
	cfg, err := config.Load(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, h.configPath, testutil.QuietLogger(t))
	return NewServer(manager, "python3", testutil.QuietLogger(t)), manager
}

// A probe reads the file on the host and passes its series through; the two
// collectors' probes share one connection, which each collector's gauge
// counts and only the first made; and a read's verbose series carry its
// sftp:// URL and READ, with no per-request connection series.
func TestAnSSHFileCollectorProbe(t *testing.T) {
	h := newSSHHost(t)
	server, _ := h.exporter(t)
	for _, collector := range []string{"remote", "remote", "also"} {
		recorder := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(h.server.Addr), nil)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "app_jobs_total 7") {
			t.Fatalf("%s: %d %s", collector, recorder.Code, recorder.Body)
		}
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		"# HELP http_exporter_sshfile_connections " + selfMetricHelp["http_exporter_sshfile_connections"] + "\n# TYPE http_exporter_sshfile_connections gauge\n",
		"# TYPE http_exporter_sshfile_connects_total counter\n",
		`http_exporter_sshfile_connections{collector="remote"} 1`,
		`http_exporter_sshfile_connections{collector="also"} 1`,
		`http_exporter_sshfile_connects_total{collector="remote"} 1`,
		`http_exporter_sshfile_connects_total{collector="also"} 0`,
		`http_exporter_scrapes_total{collector="remote",http_method="READ",url="sftp://` + h.server.Addr + filepath.Join(h.root, "app.prom") + `"} 2`,
		`http_exporter_targets_refused_total{collector="remote"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, `http_exporter_sshfile_connections{collector="remote",`) {
		t.Error("the connections have a per-request series")
	}
	if h.server.Accepted() != 1 {
		t.Errorf("the host took %d connections, want one shared", h.server.Accepted())
	}
}

// A probe without a target, or with a target that is no host, is answered
// 400 before anything is sent; one the collector's denied_targets lists, 403.
func TestSSHFileProbeTargets(t *testing.T) {
	h := newSSHHost(t)
	server, _ := h.exporter(t)
	for query, want := range map[string]int{
		"/probe?collector=remote":                                     http.StatusBadRequest,
		"/probe?collector=remote&target=" + url.QueryEscape("u@host"): http.StatusBadRequest,
		"/probe?collector=remote&target=http://host":                  http.StatusBadRequest,
		"/probe?collector=remote&target=169.254.169.254":              http.StatusForbidden,
	} {
		if recorder := probeOnce(t, server, query, nil); recorder.Code != want {
			t.Errorf("%s: %d %s, want %d", query, recorder.Code, recorder.Body, want)
		}
	}
	if h.server.Accepted() != 0 {
		t.Errorf("the host took %d connections", h.server.Accepted())
	}
}

// A path probe parameter with a backslash, which a Windows SFTP server takes
// for a separator, is answered 400 naming the parameter, before anything is
// sent.
func TestAnSSHFileProbeRefusesABackslashInItsPath(t *testing.T) {
	h := newSSHHost(t)
	server, _ := h.exporter(t)
	recorder := probeOnce(t, server, "/probe?collector=remote&target="+url.QueryEscape(h.server.Addr)+"&path="+url.QueryEscape(`..\..\secret.prom`), nil)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `probe parameter path "..\\..\\secret.prom" has a backslash`) {
		t.Errorf("%d %s", recorder.Code, recorder.Body)
	}
	if h.server.Accepted() != 0 {
		t.Errorf("the host took %d connections", h.server.Accepted())
	}
}

// A reload that changes one collector's connection settings closes nothing
// another collector still uses: the changed one connects anew, the other
// goes on with the connection it had. One that removes the last collector
// using a connection closes it.
func TestAReloadClosesTheSSHConnectionsOnlyAChangedCollectorUsed(t *testing.T) {
	h := newSSHHost(t)
	server, manager := h.exporter(t)
	probe := func(collector string) {
		t.Helper()
		if recorder := probeOnce(t, server, "/probe?collector="+collector+"&target="+url.QueryEscape(h.server.Addr), nil); recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", collector, recorder.Code, recorder.Body)
		}
	}
	probe("remote")
	probe("also")
	// also reads with the password now.
	written, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(written), "      private_key_file: "+h.keyFile+"\n", "      password_file: "+h.password+"\n", 2)
	changed = strings.Replace(changed, "      password_file: "+h.password+"\n", "      private_key_file: "+h.keyFile+"\n", 1)
	testutil.WriteIn(t, h.dir, "config.yaml", changed)
	if err := manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	selfMetrics(t, server)
	if open := h.server.Open(); open != 1 {
		t.Errorf("%d connections open after the reload, want remote's kept", open)
	}
	probe("also")
	probe("remote")
	if accepted := h.server.Accepted(); accepted != 2 {
		t.Errorf("the host took %d connections, want remote's and also's new one", accepted)
	}
	// remote goes: its connection is used by no one.
	testutil.WriteIn(t, h.dir, "config.yaml", strings.Replace(changed, "  - name: remote\n", "  - name: gone\n", 1))
	if err := manager.Reload("test"); err != nil {
		t.Fatal(err)
	}
	selfMetrics(t, server)
	testutil.WaitFor(t, "remote's connection to end", func() bool { return h.server.Open() == 1 })
}

// A debug probe shows the read: the host and file as an sftp:// URL, the
// user, and whether the connection was made for it or kept; no credential.
func TestADebugProbeOfAnSSHFileCollector(t *testing.T) {
	h := newSSHHost(t)
	server, _ := h.exporter(t)
	server.SetDebug(DebugFull)
	key, err := os.ReadFile(h.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, connection := range []string{"new", "reused"} {
		body := probeOnce(t, server, "/probe?debug=true&collector=remote&target="+url.QueryEscape(h.server.Addr), nil).Body.String()
		for _, want := range []string{
			"1. READ sftp://" + h.server.Addr + filepath.Join(h.root, "app.prom") + " -> read in",
			"Ssh-User: metrics", "Ssh-Connection: " + connection, "app_jobs_total 7",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("no %q in the report:\n%s", want, body)
			}
		}
		if strings.Contains(body, strings.Split(string(key), "\n")[1]) {
			t.Errorf("the report shows the private key:\n%s", body)
		}
	}
}

// Static targets of one caching collector, one with credentials of its own,
// read with each and cache apart: a target's account and credential files
// enter its cache key.
func TestSSHFileStaticTargetsWithTheirOwnCredentials(t *testing.T) {
	h := newSSHHost(t)
	cfg, err := config.Load(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Collectors[0].Cache.TTL = model.Duration(time.Hour)
	// One at a time, so the second would find the first's result cached
	// were their keys the same.
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Concurrency: 1, Targets: []model.StaticTarget{
		{Name: "with_key", Collector: "remote", Target: h.server.Addr},
		{Name: "with_password", Collector: "remote", Target: h.server.Addr, Request: model.TargetRequestConfig{PasswordFile: h.password}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.scrapeStaticTargets(t.Context(), 0)
	recorder := probeOnce(t, server, "/static-targets", nil)
	for _, want := range []string{`app_jobs_total{static_target="with_key"} 7`, `app_jobs_total{static_target="with_password"} 7`} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("missing %q:\n%s", want, recorder.Body)
		}
	}
	if accepted := h.server.Accepted(); accepted != 2 {
		t.Errorf("the host took %d connections, want one for each account", accepted)
	}
}

// An sshfile collector's label values take the probe's parameters as its
// path does; one no placeholder names is refused as unused, and a collector
// reading a directory refuses the path parameter, having no file to name.
func TestAnSSHFileCollectorsLabelsTakeAProbeParameter(t *testing.T) {
	h := newSSHHost(t)
	testutil.WriteIn(t, h.root, "billing.prom", "# TYPE app_jobs_total counter\napp_jobs_total 3\n")
	known := filepath.Join(h.dir, "known_hosts")
	server := labelServer(t, `collectors:
  - name: apps
    request: {type: sshfile, root: `+h.root+`, path: "{{param_app:app}}.prom", username: metrics, private_key_file: `+h.keyFile+`, known_hosts_file: `+known+`}
    transform:
      type: prometheus
      labels:
        app: "{{param_app:app}}"
  - name: dir
    request: {type: sshfile, root: `+h.root+`, files: ["*.prom"], username: metrics, private_key_file: `+h.keyFile+`, known_hosts_file: `+known+`}
    transform:
      type: prometheus
`)
	target := "&target=" + url.QueryEscape(h.server.Addr)
	for query, want := range map[string][]string{
		"collector=apps":                   {`app_jobs_total{app="app"} 7`},
		"collector=apps&param_app=billing": {`app_jobs_total{app="billing"} 3`},
		"collector=dir":                    {`app_jobs_total{file="app.prom"} 7`, `app_jobs_total{file="billing.prom"} 3`, `localfile_files_skipped 0`, `localfile_mtime_seconds{file="app.prom"}`, `localfile_mtime_seconds{file="billing.prom"}`, `localfile_scrape_error{file="app.prom"} 0`, `localfile_scrape_error{file="billing.prom"} 0`},
	} {
		recorder := probeOnce(t, server, "/probe?"+query+target, nil)
		got := seriesLines(recorder.Body.String())
		if recorder.Code != http.StatusOK || len(got) != len(want) {
			t.Errorf("%s: %d %v, want %v", query, recorder.Code, got, want)
			continue
		}
		for i := range want {
			if !strings.HasPrefix(got[i], want[i]) {
				t.Errorf("%s: %v, want %v", query, got, want)
			}
		}
	}
	for query, want := range map[string]string{
		"collector=apps&param_ap=x":        `probe parameters param_ap are not used by collector "apps": no placeholder in its request.path ("{{param_app:app}}.prom") or its label values names them`,
		"collector=dir&path=app.prom":      `reads every file of request.root that request.files matches, so there is no file for it to name`,
		"collector=apps&param_app=" + "..": `must not be ".."`,
	} {
		recorder := probeOnce(t, server, "/probe?"+query+target, nil)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%s: %d %s, want 400 saying %q", query, recorder.Code, recorder.Body, want)
		}
	}
}

// A Python transform sees a file read over SSH as localfile's: no status,
// and the file's content type and length as its headers.
func TestAnSSHFileCollectorWithPython(t *testing.T) {
	requirePython(t)
	h := newSSHHost(t)
	testutil.WriteIn(t, h.root, "status.json", `{"temperature": 41.5}`)
	server := labelServer(t, `collectors:
  - name: status
    request: {type: sshfile, root: `+h.root+`, path: status.json, username: metrics, private_key_file: `+h.keyFile+`, known_hosts_file: `+filepath.Join(h.dir, "known_hosts")+`}
    transform:
      type: python
      script: |
        metric(name="no_status", value=1 if response.status_code is None else 0)
        metric(name="json_file", value=1 if response.header("content-type") == "application/json" else 0)
        metric(name="file_bytes", value=float(response.header("content-length")))
        metric(name="temperature", value=data["temperature"])
`)
	recorder := probeOnce(t, server, "/probe?collector=status&target="+url.QueryEscape(h.server.Addr), nil)
	if got, want := seriesLines(recorder.Body.String()), []string{"file_bytes 21", "json_file 1", "no_status 1", "temperature 41.5"}; recorder.Code != http.StatusOK || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%d %v, want %v", recorder.Code, got, want)
	}
}

// An sshfile collector's scrapes that waited for room at its
// max_open_connections, and those that failed there, are counted in two
// families of the collector alone, 0 for one without the limit: a probe of a
// host that never answers leaves its connection being made for no one, and
// a probe of another host waits for it to be given up, then reads.
func TestAnSSHFileCollectorCountsItsScrapesAtMaxOpenConnections(t *testing.T) {
	h := newSSHHost(t)
	public, err := os.ReadFile(h.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(public)
	if err != nil {
		t.Fatal(err)
	}
	other := sshtest.Start(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{signer.PublicKey()}})
	known := sshtest.WriteFile(t, h.dir, "known_both", h.server.KnownHostsLine()+other.KnownHostsLine())
	limited := strings.Replace(sshCollectors, "  - name: also\n", "  - name: limited\n    request:\n      type: sshfile\n      root: ROOT\n      path: app.prom\n      username: metrics\n      private_key_file: KEY\n      known_hosts_file: BOTH\n      max_open_connections: 1\n    transform:\n      type: prometheus\n  - name: also\n", 1)
	testutil.WriteIn(t, h.dir, "config.yaml", strings.NewReplacer("ROOT", h.root, "KEY", h.keyFile, "KNOWN", known, "BOTH", known).Replace(limited))
	server, _ := h.exporter(t)
	h.server.Freeze()
	t.Cleanup(h.server.Thaw)
	if recorder := probeOnce(t, server, "/probe?collector=limited&timeout=200ms&target="+url.QueryEscape(h.server.Addr), nil); strings.Contains(recorder.Body.String(), "app_jobs_total 7") {
		t.Fatalf("the frozen host answered: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := probeOnce(t, server, "/probe?collector=limited&target="+url.QueryEscape(other.Addr), nil); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "app_jobs_total 7") {
		t.Fatalf("the other host: %d %s", recorder.Code, recorder.Body)
	}
	metrics := selfMetrics(t, server)
	for _, want := range []string{
		"# HELP http_exporter_sshfile_limit_waits_total " + selfMetricHelp["http_exporter_sshfile_limit_waits_total"] + "\n# TYPE http_exporter_sshfile_limit_waits_total counter\n",
		"# HELP http_exporter_sshfile_limit_failures_total " + selfMetricHelp["http_exporter_sshfile_limit_failures_total"] + "\n# TYPE http_exporter_sshfile_limit_failures_total counter\n",
		`http_exporter_sshfile_limit_waits_total{collector="limited"} 1`,
		`http_exporter_sshfile_limit_failures_total{collector="limited"} 0`,
		`http_exporter_sshfile_limit_waits_total{collector="remote"} 0`,
		`http_exporter_sshfile_limit_failures_total{collector="remote"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %q:\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, `http_exporter_sshfile_limit_waits_total{collector="limited",`) {
		t.Error("the counts at the limit have a per-request series")
	}
}
