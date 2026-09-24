package exporter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
)

// testdata/config.grafanastatus.json-test.yaml turns an Atlassian Statuspage
// summary into metrics. This runs it against a trimmed capture of
// status.grafana.com's real summary, served locally, and pins the exposition.
//
// These tests belong to the external end-to-end suite, alongside
// external_e2e_test.go: like the rest of it they exercise a demo configuration
// for a third-party service, so they are opt-in, named TestExternal* so that
// `make test-external` selects them, and skipped unless EXTERNAL_E2E is set.
// They need no network themselves; TestExternalDemoConfigurationsStillMatchTheirSources
// probes the live page with the same configuration.
// The capture keeps what makes the page awkward: grouped and ungrouped
// components, a group holding three components with the same name, one
// component in partial outage, an unresolved incident, and maintenances both
// in progress and scheduled.

const grafanaStatusConfig = "../../testdata/config.grafanastatus.json-test.yaml"

func probeGrafanaStatus(t *testing.T, fixture string) string {
	t.Helper()
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var requested string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(target.Close)

	cfg, err := config.Load(grafanaStatusConfig)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.NewManager(cfg, grafanaStatusConfig, slog.Default()), "python3", slog.Default())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/probe?collector=statuspage&target="+url.QueryEscape(target.URL), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if requested != "/api/v2/summary.json" {
		t.Fatalf("the collector requested %q", requested)
	}
	return response.Body.String()
}

func TestExternalGrafanaStatusPageMetrics(t *testing.T) {
	requireExternalE2E(t)
	body := probeGrafanaStatus(t, "../../testdata/json/grafana-status-summary.json")
	for _, want := range []string{
		// The page's name is on one series only.
		`statuspage_info{page="Grafana Cloud",time_zone="Etc/UTC"} 1`,

		// Each status is one series, labelled with the current value.
		`statuspage_status{indicator="minor"} 1`,

		// A grouped component, with its group's name.
		`statuspage_component_status{cloud_provider="AWS",cloud_zone="prod-sa-east-1",component="AWS Brazil - prod-sa-east-1",component_id="9zgd4n4s874v",group="Grafana",status="operational"} 1`,
		// A component outside any group has an empty group label.
		`statuspage_component_status{component="Incident Management and Response (IRM)",component_id="1mxpk7q1bkb6",status="partial_outage"} 1`,
		// Three components share a name inside one group; the id keeps them apart.
		`statuspage_component_status{cloud_provider="AWS",cloud_zone="prod-eu-west-6",component="AWS Ireland - prod-eu-west-6",component_id="s16nnryhyp57",group="Grafana Assistant",status="under_maintenance"} 1`,
		`statuspage_component_status{cloud_provider="AWS",cloud_zone="prod-eu-west-6",component="AWS Ireland - prod-eu-west-6",component_id="pshdmtd378dc",group="Grafana Assistant",status="under_maintenance"} 1`,
		`statuspage_component_status{cloud_provider="AWS",cloud_zone="prod-eu-west-6",component="AWS Ireland - prod-eu-west-6",component_id="5q4p7yvyhvwb",group="Grafana Assistant",status="under_maintenance"} 1`,

		`statuspage_component_group_status{group="Grafana",status="under_maintenance"} 1`,
		`statuspage_component_group_status{group="Website and Support",status="operational"} 1`,

		`statuspage_unresolved_incidents{impact="major"} 1`,
		`statuspage_unresolved_incidents{impact="minor"} 0`,

		`statuspage_scheduled_maintenances{status="in_progress"} 2`,
		`statuspage_scheduled_maintenances{status="scheduled"} 2`,
		`statuspage_scheduled_maintenances{status="verifying"} 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}

	// Timestamps, parsed from Statuspage's millisecond ISO 8601.
	next := time.Date(2026, 9, 23, 7, 0, 0, 0, time.UTC).Unix()
	updated := time.Date(2026, 9, 22, 18, 0, 7, 0, time.UTC).Unix()
	for series, want := range map[string]int64{
		"statuspage_next_maintenance_start_timestamp_seconds": next,
		"statuspage_updated_timestamp_seconds":                updated,
	} {
		if got, ok := sampleValue(body, series); !ok || int64(got) != want {
			t.Errorf("%s = %v (present %t), want %d", series, got, ok, want)
		}
	}

	// Only the current status is exported: one series per component that is
	// not a group, one per group, one for the page, all 1. A status that does
	// not apply has no series, rather than a 0.
	const leaves, groups = 10, 3
	for prefix, want := range map[string]int{
		"statuspage_status{":                 1,
		"statuspage_component_status{":       leaves,
		"statuspage_component_group_status{": groups,
	} {
		if got := countLinesWithPrefix(body, prefix); got != want {
			t.Errorf("%d %s series, want %d", got, prefix, want)
		}
		if got := countLinesWithSuffix(body, prefix, "} 1"); got != want {
			t.Errorf("%d %s series are 1, want all %d", got, prefix, want)
		}
	}
	for _, inactive := range []string{
		`statuspage_status{indicator="none"}`,
		`statuspage_component_status{cloud_provider="AWS",cloud_zone="prod-sa-east-1",component="AWS Brazil - prod-sa-east-1",component_id="9zgd4n4s874v",group="Grafana",status="major_outage"}`,
		`statuspage_component_group_status{group="Grafana",status="operational"}`,
	} {
		if strings.Contains(body, inactive) {
			t.Errorf("a status that does not apply is exported: %s", inactive)
		}
	}
}

// With nothing scheduled the next-maintenance timestamp is simply absent, and
// the probe still succeeds.
func TestExternalGrafanaStatusPageWithNothingScheduled(t *testing.T) {
	requireExternalE2E(t)
	raw, err := os.ReadFile("../../testdata/json/grafana-status-summary.json")
	if err != nil {
		t.Fatal(err)
	}
	quiet := strings.ReplaceAll(string(raw), `"status": "scheduled"`, `"status": "completed"`)
	path := t.TempDir() + "/summary.json"
	if err := os.WriteFile(path, []byte(quiet), 0o600); err != nil {
		t.Fatal(err)
	}
	body := probeGrafanaStatus(t, path)
	if strings.Contains(body, "statuspage_next_maintenance_start_timestamp_seconds{") {
		t.Error("a page with nothing scheduled should have no next-maintenance timestamp")
	}
	if !strings.Contains(body, `statuspage_scheduled_maintenances{status="scheduled"} 0`+"\n") {
		t.Error("the scheduled count should drop to 0")
	}
}

// sampleValue returns the value of the series written exactly as given.
func sampleValue(body, series string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if value, found := strings.CutPrefix(line, series+" "); found {
			v, err := strconv.ParseFloat(value, 64)
			return v, err == nil
		}
	}
	return 0, false
}

// The components that are not operational are explained by the incident or
// maintenance behind them, with that event's latest update, on a series of
// their own.
func TestExternalGrafanaStatusPageExplainsComponentsThatAreNotOperational(t *testing.T) {
	requireExternalE2E(t)
	body := probeGrafanaStatus(t, "../../testdata/json/grafana-status-summary.json")
	for _, want := range []string{
		// An unresolved incident, with its newest update rather than the first
		// one ("We are currently investigating...").
		`statuspage_component_incident_info{component="Incident Management and Response (IRM)",component_id="1mxpk7q1bkb6",impact="major",incident="IRM Access Issues for a Small Group of Users",incident_status="monitoring",message="A fix is currently being rolled out and will be deployed progressively over the next few hours. We appreciate your patience as this reaches all affected customers.",status="partial_outage"} 1`,
		// A maintenance in progress, for each of the three same-named components.
		`statuspage_component_incident_info{cloud_provider="AWS",cloud_zone="prod-eu-west-6",component="AWS Ireland - prod-eu-west-6",component_id="s16nnryhyp57",group="Grafana Assistant",impact="maintenance",incident="Scheduled Authentication Cache Maintenance",incident_status="in_progress",message="Scheduled maintenance is currently in progress. We will provide updates as necessary.",status="under_maintenance"} 1`,
		`statuspage_component_incident_info{cloud_provider="AWS",cloud_zone="prod-eu-west-6",component="AWS Ireland - prod-eu-west-6",component_id="pshdmtd378dc",group="Grafana Assistant",impact="maintenance",incident="Scheduled Authentication Cache Maintenance",incident_status="in_progress",message="Scheduled maintenance is currently in progress. We will provide updates as necessary.",status="under_maintenance"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	// Only the four components that are not operational have one.
	if got := strings.Count(body, "\nstatuspage_component_incident_info{"); got != 4 {
		t.Errorf("%d incident info series, want 4", got)
	}
	if seriesLine(body, "statuspage_component_incident_info", `component="Grafana.com"`) != "" {
		t.Error("an operational component has an incident info series")
	}
}

// A component down with only a scheduled maintenance, or nothing at all,
// listing it still gets a series, with the event labels empty: maintenance that
// has not started is not the cause of anything yet.
func TestExternalGrafanaStatusPageComponentDownWithoutAnActiveEvent(t *testing.T) {
	requireExternalE2E(t)
	path := mutatedGrafanaStatus(t, func(summary map[string]any) {
		var tickets map[string]any
		for _, raw := range summary["components"].([]any) {
			component := raw.(map[string]any)
			switch component["name"] {
			case "Support Tickets":
				component["status"] = "major_outage"
				tickets = component
			case "Grafana.com":
				component["status"] = "degraded_performance"
			}
		}
		maintenances := summary["scheduled_maintenances"].([]any)
		upcoming := maintenances[len(maintenances)-1].(map[string]any)
		upcoming["updated_at"] = "2030-01-01T00:00:00.000Z"
		upcoming["components"] = []any{tickets}
	})
	body := probeGrafanaStatus(t, path)
	for _, want := range []string{
		`statuspage_component_incident_info{component="Support Tickets",component_id="8s9ngcb02t90",group="Website and Support",status="major_outage"} 1`,
		`statuspage_component_incident_info{component="Grafana.com",component_id="m0qkgdzwyr6l",group="Website and Support",status="degraded_performance"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
}

// Updates run to several hundred characters, over several lines, and not
// always in ASCII. The message is one line cut to the label length limit, and the
// scrape does not fail on the label length limit.
func TestExternalGrafanaStatusPageMessageIsOneBoundedLine(t *testing.T) {
	requireExternalE2E(t)
	long := "Line one –\n\n  " + strings.Repeat("ünïcödé ", 100)
	path := mutatedGrafanaStatus(t, func(summary map[string]any) {
		incident := summary["incidents"].([]any)[0].(map[string]any)
		updates := incident["incident_updates"].([]any)
		updates[0].(map[string]any)["body"] = long
	})
	body := probeGrafanaStatus(t, path)
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, `component_id="1mxpk7q1bkb6"`) || !strings.HasPrefix(line, "statuspage_component_incident_info{") {
			continue
		}
		_, rest, _ := strings.Cut(line, `message="`)
		message, _, _ := strings.Cut(rest, `",`)
		if !strings.HasPrefix(message, "Line one – ünïcödé") || !strings.HasSuffix(message, "…") {
			t.Fatalf("message=%q", message)
		}
		// truncate: true cuts to limits.max_label_value_length, 300 bytes in
		// the configuration, on a character boundary.
		if len(message) > 300 || len(message) < 290 || !utf8.ValidString(message) {
			t.Fatalf("message is %d bytes, want at most 300 and cut on a character boundary: %q", len(message), message)
		}
		return
	}
	t.Fatalf("no incident info for IRM:\n%s", body)
}

// The cloud provider and zone are parsed from component names, in every shape
// status.grafana.com uses, and are empty for names that carry neither.
func TestExternalGrafanaStatusPageCloudProviderAndZone(t *testing.T) {
	requireExternalE2E(t)
	tests := []struct{ name, provider, zone string }{
		{"AWS Ireland - prod-eu-west-6", "AWS", "prod-eu-west-6"},
		{"AWS Ireland - prod-eu-west-6: API", "AWS", "prod-eu-west-6"},
		{"AWS US East - prod-us-east-0: Alertmanager and Rules Configuration API", "AWS", "prod-us-east-0"},
		{"AWS East (VA) prod-us-east-1", "AWS", "prod-us-east-1"},
		{"AWS Canada prod-ca-central-1", "AWS", "prod-ca-central-1"},
		{"AWS Ireland eu-west-1", "AWS", "eu-west-1"},
		{"Azure US Central - us-central2", "Azure", "us-central2"},
		{"Azure US Central - prod-us-central7: Public Probes", "Azure", "prod-us-central7"},
		{"GCP Saudi Arabia - prod-me-central-0: API", "GCP", "prod-me-central-0"},
		{"GCS US - cortex-prod-04: Ingestion", "GCS", "cortex-prod-04"},
		{"Federal Cloud - AWS US Gov West", "", ""},
		{"Support Tickets", "", ""},
		{"GCk6 App", "", ""},
		{"play.grafana.org", "", ""},
	}
	var components []any
	for i, test := range tests {
		components = append(components, map[string]any{"id": fmt.Sprintf("c%d", i), "name": test.name, "group": false, "status": "major_outage"})
	}
	path := mutatedGrafanaStatus(t, func(summary map[string]any) {
		summary["components"] = components
	})
	body := probeGrafanaStatus(t, path)
	for i, test := range tests {
		for _, metric := range []string{"statuspage_component_status", "statuspage_component_incident_info"} {
			line := seriesLine(body, metric, fmt.Sprintf(`component_id="c%d"`, i))
			if line == "" {
				t.Errorf("%s: no %s series", test.name, metric)
				continue
			}
			for label, want := range map[string]string{"cloud_provider": test.provider, "cloud_zone": test.zone} {
				if want == "" {
					// A name without a provider leaves the label off.
					if strings.Contains(line, label+"=") {
						t.Errorf("%s: %s has %s, want none: %s", test.name, metric, label, line)
					}
					continue
				}
				if !strings.Contains(line, label+"="+strconv.Quote(want)) {
					t.Errorf("%s: %s %s, want %q in %s", test.name, metric, label, want, line)
				}
			}
		}
	}
}

func seriesLine(body, metric, contains string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metric+"{") && strings.Contains(line, contains) {
			return line
		}
	}
	return ""
}

// mutatedGrafanaStatus writes a copy of the captured summary, changed by edit.
func mutatedGrafanaStatus(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/json/grafana-status-summary.json")
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatal(err)
	}
	edit(summary)
	out, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/summary.json"
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func countLinesWithPrefix(body, prefix string) int {
	return countLinesWithSuffix(body, prefix, "")
}

func countLinesWithSuffix(body, prefix, suffix string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
			n++
		}
	}
	return n
}
