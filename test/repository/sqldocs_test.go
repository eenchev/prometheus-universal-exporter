//go:build !select_request_types || request_type_sql

package repository

import (
	"context"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/exporter"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/sqltest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The examples of docs/SQL.md work as written against a stand-in database,
// their password files replaced by the test's own: the collector's probe
// binds its parameter and reads its rows one series a row, and the static
// targets load against it; and the probe's answer is the series the page
// says.
func TestSQLDocumentationExamples(t *testing.T) {
	doc := read(t, "docs/SQL.md")
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
		t.Fatalf("docs/SQL.md has no example starting %q", prefix)
		return ""
	}
	db := &sqltest.DB{Results: map[string]sqltest.Result{
		"SELECT state, count(*) AS jobs FROM jobs WHERE queue = $1 GROUP BY state": {Columns: []string{"state", "jobs"}, Types: []string{"TEXT", "INT8"}, Rows: [][]driver.Value{{"open", int64(3)}, {"done", int64(40)}}},
	}}
	t.Cleanup(fetch.UseSQLTestDriver(func(ctx context.Context, _ fetch.SQLConnectInfo) (driver.Conn, error) { return db.Connect(ctx) }))
	t.Cleanup(fetch.CloseConnections)
	dir := t.TempDir()
	password := testutil.WriteIn(t, dir, "password", "pw\n")
	path := testutil.WriteIn(t, dir, "config.yaml", strings.ReplaceAll(find("collectors:\n"), "/etc/exporter/db/password", password))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	exp := exporter.NewServer(config.NewManager(cfg, path, testutil.QuietLogger(t)), "python3", testutil.QuietLogger(t))
	recorder := httptest.NewRecorder()
	exp.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe?collector=jobs&target=db1.example.com&param_queue=mail", nil))
	for _, want := range []string{`app_jobs{state="open"} 3`, `app_jobs{state="done"} 40`} {
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("%d, want %q in\n%s", recorder.Code, want, recorder.Body)
		}
	}
	if queries := db.Queries(); len(queries) != 1 || queries[0].Args[0] != "mail" {
		t.Errorf("queries %+v", queries)
	}
	targets := testutil.WriteIn(t, dir, "targets.yaml", strings.ReplaceAll(find("interval: 1m\ntargets:\n"), "/etc/exporter/db/legacy-password", password))
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
	// The connections block of the page is one a collector takes.
	connections := strings.Replace(find("collectors:\n"), "    transform:\n", strings.TrimPrefix(find("    request:\n      connections:\n"), "    request:\n")+"    transform:\n", 1)
	if _, err := config.Load(testutil.WriteIn(t, dir, "connections.yaml", connections)); err != nil {
		t.Errorf("%v\n%s", err, connections)
	}
}

// The vertica driver answers a server's file-transfer requests on any
// statement and the exporter cannot refuse them, so a server at a vertica
// target can read the exporter's files. docs/SQL.md's limitations say so,
// and say to hold vertica collectors to trusted servers with
// allowed_targets: a page that dropped it would leave the risk unstated.
func TestSQLDocumentationSaysAVerticaServerCanReadTheExportersFiles(t *testing.T) {
	doc := read(t, "docs/SQL.md")
	_, limitations, ok := strings.Cut(doc, "\n## Limitations\n")
	if !ok {
		t.Fatal("docs/SQL.md has no Limitations section")
	}
	limitations, _, _ = strings.Cut(limitations, "\n## ")
	bullet := ""
	for part := range strings.SplitSeq(limitations, "\n- ") {
		if strings.HasPrefix(part, "**Vertica's driver does what the server asks of the exporter's files.**") {
			bullet = strings.Join(strings.Fields(part), " ")
		}
	}
	if bullet == "" {
		t.Fatal("docs/SQL.md's limitations do not say that the vertica driver answers a server's file-transfer requests")
	}
	for _, want := range []string{"open whatever path the server names", "standard input", "can read any file the exporter's process can read", "`allowed_targets`", "servers you trust"} {
		if !strings.Contains(bullet, want) {
			t.Errorf("the vertica file-transfer limitation does not say %q:\n%s", want, bullet)
		}
	}
}
