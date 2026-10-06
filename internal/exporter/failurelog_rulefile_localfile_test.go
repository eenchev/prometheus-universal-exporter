//go:build !select_request_types || request_type_localfile

package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A directory's file may have any name, and one named rule has its rules'
// recoveries logged as any other file has: the key the failure log tells
// such a file by ends as the marker that sets a rule's key off from its
// file's begins, and while the file a rule's failure was counted for was
// read back out of the rule's key, it was read a part short for this one
// name, so the scrape that asked about its own file was told of no failure,
// and the rule's recovery was never logged nor its failure forgotten. Probed
// with no target, with a directory named rule as the target — whose key
// holds the marker whole — and scraped as a static target, a rule that fails
// on rule and on rules.txt, fails again and then reads again is logged
// failed, repeated and recovered for each file, the same lines but for the
// file's name, and nothing of either is remembered afterwards.
func TestAFileNamedRuleHasItsRulesRecoveriesLogged(t *testing.T) {
	const lacking, whole = "app_jobs_total 7\n", "app_jobs_total 7\napp_workers 2\n"
	for _, scraped := range []struct{ name, directory, query string }{
		{"a probe without a target", "", "collector=dir"},
		{"a probe of a directory named rule", "rule", "collector=dir&target=rule"},
		{"a static target", "", ""},
	} {
		t.Run(scraped.name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, scraped.directory)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			c := dirCollector("dir", root, "rule*")
			c.Metrics = []model.MetricRule{{Name: "workers", Expression: "^app_workers$", ErrorMode: model.ErrorModeLog}}
			var server *Server
			scrape := func() {
				probeFile(t, server, scraped.query).must(t, http.StatusOK)
			}
			if scraped.query == "" {
				file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "textfiles", Collector: "dir"}}}
				server = newStaticServer(t, &model.Config{Collectors: []model.Collector{c}}, file)
				scrape = func() { server.scrapeStaticTargets(context.Background(), 10*time.Second) }
			} else {
				server = fileServer(t, c)
			}
			logs := debugLogs(server)
			for at, step := range []struct {
				body string
				want []string
			}{
				{lacking, []string{"WARN metric extraction failed"}},
				{lacking, []string{"DEBUG metric extraction failed"}},
				{whole, []string{"INFO metric extraction recovered"}},
				{whole, nil},
			} {
				for _, name := range []string{"rule", "rules.txt"} {
					if err := os.WriteFile(filepath.Join(directory, name), []byte(step.body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				logs.Reset()
				scrape()
				got := map[string][]string{}
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					if line == "" {
						continue
					}
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatalf("the log line %q is not JSON: %v", line, err)
					}
					if message, _ := record["msg"].(string); strings.HasPrefix(message, "metric extraction ") {
						file, _ := record["file"].(string)
						got[file] = append(got[file], record["level"].(string)+" "+message)
					}
				}
				for _, name := range []string{"rule", "rules.txt"} {
					if !reflect.DeepEqual(got[name], step.want) {
						t.Errorf("scrape %d, with the files holding %q: the lines of the rule of the file %s are %q, want %q\nthe log:\n%s", at+1, step.body, name, got[name], step.want, logs)
					}
				}
				if len(got) > 2 {
					t.Errorf("scrape %d: the rule's lines are of other files than the two: %v", at+1, got)
				}
			}
			if left := rememberedOf(server, "dir"); len(left) != 0 {
				t.Errorf("after the rule read again the failure log remembers of the collector %v, want nothing", left)
			}
			server.failures.mu.Lock()
			counted := len(server.failures.ruleFailures)
			server.failures.mu.Unlock()
			if counted != 0 {
				t.Errorf("after the rule read again rule failures are counted for %d files, want none", counted)
			}
		})
	}
}
