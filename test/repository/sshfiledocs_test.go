//go:build !select_request_types || request_type_sshfile

package repository

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"golang.org/x/crypto/ssh"
)

// The examples of docs/SSHFILE.md work as written against an SSH server,
// their directories and files replaced by the test's own: the textfile read,
// the job status with its placeholder, the directory and the appliance's
// status with a password; and its static targets load against them.
func TestSSHFileDocumentationExamples(t *testing.T) {
	doc := read(t, "docs/SSHFILE.md")
	var blocks []string
	for _, part := range strings.Split(doc, "```yaml\n")[1:] {
		block, _, _ := strings.Cut(part, "```")
		blocks = append(blocks, block)
	}
	find := func(prefix string) string {
		for _, block := range blocks {
			if strings.HasPrefix(block, prefix) {
				return block
			}
		}
		t.Fatalf("docs/SSHFILE.md has no example starting %q", prefix)
		return ""
	}
	dir := t.TempDir()
	keyFile, key := sshtest.WriteClientKey(t, dir, "id_ed25519", "")
	password := sshtest.WriteFile(t, dir, "password", "appliance-pw\n")
	server := sshtest.Start(t, sshtest.Options{User: "metrics", AuthorizedKeys: []ssh.PublicKey{key}})
	appliance := sshtest.Start(t, sshtest.Options{User: "monitor", Password: "appliance-pw"})
	known := server.WriteKnownHosts(t, dir)
	applianceKnown := sshtest.WriteFile(t, dir, "appliance_known_hosts", appliance.KnownHostsLine())
	textfiles, jobs, status := t.TempDir(), t.TempDir(), t.TempDir()
	testutil.WriteIn(t, textfiles, "backup.prom", "# TYPE backup_last_success_timestamp_seconds gauge\nbackup_last_success_timestamp_seconds 1.7e+09\n")
	testutil.WriteIn(t, jobs, "nightly.json", `{"last_success": 1700000000}`)
	testutil.WriteIn(t, status, "health.json", `{"sensors": {"temperature": 41.5}, "fan": {"state": "ok"}}`)
	conf := find("collectors:\n") + find("  - name: job_status\n") + find("  - name: textfiles\n") + find("  - name: appliance_status\n")
	conf = strings.NewReplacer(
		"/var/lib/node_exporter/textfile_collector", textfiles, "/srv/jobs/status", jobs, "root: /status", "root: "+status,
		"/etc/exporter/ssh/id_ed25519", keyFile, "/etc/exporter/ssh/known_hosts", known,
		"/etc/exporter/appliance/password", password, "/etc/exporter/appliance/known_hosts", applianceKnown,
	).Replace(conf)
	path := testutil.WriteIn(t, dir, "config.yaml", conf)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, conf)
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	t.Cleanup(fetch.CloseConnections)
	for query, want := range map[string]string{
		"collector=backups&target=" + url.QueryEscape(server.Addr):                      "backup_last_success_timestamp_seconds 1.7e+09",
		"collector=job_status&param_job=nightly&target=" + url.QueryEscape(server.Addr): "job_last_success_timestamp_seconds 1.7e+09",
		"collector=textfiles&target=" + url.QueryEscape(server.Addr):                    `localfile_scrape_error{file="backup.prom"} 0`,
		"collector=appliance_status&target=" + url.QueryEscape("ssh://"+appliance.Addr): "appliance_fan_ok 1",
	} {
		recorder := httptest.NewRecorder()
		exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?"+query, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%s: %d, want %q in\n%s", query, recorder.Code, want, recorder.Body)
		}
	}
	targets := testutil.WriteIn(t, dir, "targets.yaml", strings.ReplaceAll(find("interval: 1m\ntargets:\n"), "/etc/exporter/ssh/legacy-password", password))
	file, err := config.LoadStaticTargets(targets)
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
