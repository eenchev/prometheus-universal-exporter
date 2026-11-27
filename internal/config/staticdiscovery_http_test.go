//go:build !select_request_types || request_type_http

package config

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/discovery"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// discoveringTargets is a static target file of one written target and an
// entry of two dns_sd sources asking server, the second of A records.
func discoveringTargets(server string) string {
	return `interval: 1m
targets:
  - name: ta
    collector: a
    target: http://a.invalid
discovered_targets:
  - name: node
    collector: a
    target_template: http://{{address}}/metrics
    interval: 30s
    labels:
      team: infra
    discovery:
      - dns_sd:
          names: [_metrics._tcp.nodes.test]
          server: ` + server + `
          refresh_interval: 1s
        labels:
          service: __meta_dns_name
      - name: legacy
        dns_sd:
          names: [legacy.nodes.test]
          type: A
          port: 9100
          server: ` + server + `
          refresh_interval: 1s
`
}

// targetNames are the names and targets of f's targets.
func targetNames(f *model.StaticTargetFile) []string {
	var out []string
	for _, t := range f.Targets {
		out = append(out, t.Name+" "+t.Target)
	}
	return out
}

// A static target file's discovered_targets entry of dns_sd sources is read
// by the exporter: its sources named, the first by its kind and place, and
// the entry checked as a target at a sample address, with the file's
// interval under its own. A file of entries alone needs no targets.
func TestTheExporterReadsADNSSDEntry(t *testing.T) {
	f, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", discoveringTargets("127.0.0.1:53")))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Targets) != 1 || f.Written != 1 || len(f.Discovered) != 1 {
		t.Fatalf("targets %v, written %d, entries %d", targetNames(f), f.Written, len(f.Discovered))
	}
	e := f.Discovered[0]
	if e.Name != "node" || e.Sources[0].Name != "dns_sd_0" || e.Sources[1].Name != "legacy" || e.Sources[0].DNSSD.Type != "SRV" || e.Sample.Target != "http://192.0.2.1:9100/metrics" || e.Sample.Interval != model.Duration(30*time.Second) || e.Sample.Labels["team"] != "infra" {
		t.Errorf("entry %+v", e)
	}
	cfg := loadText(t, oneCollector)
	if err := ValidateStaticTargetsAgainst(f, cfg); err != nil {
		t.Errorf("against the configuration: %v", err)
	}
	alone, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", "interval: 1m\n"+discoveringTargets("127.0.0.1:53")[strings.Index(discoveringTargets(""), "discovered_targets:"):]))
	if err == nil {
		err = ValidateStaticTargets(alone)
	}
	if err != nil || len(alone.Targets) != 0 || len(alone.Discovered) != 1 {
		t.Errorf("a file of entries alone: %v, %+v", err, alone)
	}
}

// loadText is the configuration of text.
func loadText(t *testing.T, text string) *model.Config {
	t.Helper()
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", text))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// What an entry may not be is refused when the file loads, or, for what
// needs the configuration, when it is checked against it.
func TestADNSSDEntryIsChecked(t *testing.T) {
	base := discoveringTargets("127.0.0.1:53")
	// without is base cut before the entry's discovery, and entry the entry
	// alone.
	without, _, _ := strings.Cut(base, "    discovery:")
	_, entry, _ := strings.Cut(base, "discovered_targets:\n")
	for want, text := range map[string]string{
		"has no discovery":                                            without,
		"discovery must not be empty":                                 without + "    discovery: []\n",
		"must set dns_sd, the one kind":                               without + "    discovery:\n      - name: x\n",
		`entry "node" is defined twice`:                               base + entry,
		`begins with entry "node"'s name and _`:                       base + strings.Replace(entry, "  - name: node", "  - name: node_eu", 1),
		"target is set, but an entry's targets":                       strings.Replace(base, "    target_template:", "    target: http://x.invalid\n    target_template:", 1),
		"weight is read by the target allocator":                      strings.Replace(base, "    interval: 30s", "    weight: 2", 1),
		"has no {{address}}":                                          strings.Replace(base, "http://{{address}}/metrics", "http://fixed/metrics", 1),
		"dns_sd.port is set for type SRV":                             strings.Replace(base, "          names: [_metrics._tcp.nodes.test]", "          names: [_metrics._tcp.nodes.test]\n          port: 80", 1),
		`labels sets "__x"`:                                           strings.Replace(base, "          service: __meta_dns_name", "          __x: __meta_dns_name", 1),
		"refresh_interval 500ms is under the least":                   strings.Replace(base, "refresh_interval: 1s", "refresh_interval: 500ms", 1),
		"line 7: name is written true, which YAML reads as a boolean": strings.Replace(base, "  - name: node", "  - name: true", 1),
		"line 18: a source has no key refresh; it has name, dns_sd, labels, max_targets": strings.Replace(base, "        labels:\n          service", "        refresh: 1s\n        labels:\n          service", 1),
		"line 17: dns_sd has no key ports":                                               strings.Replace(base, "          refresh_interval: 1s\n        labels:", "          ports: 1\n        labels:", 1),
	} {
		_, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", text))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	// An http collector needs a URL: without a template, the address is no
	// target of it, which the check against the configuration says.
	f, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", strings.Replace(base, "    target_template: http://{{address}}/metrics\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStaticTargetsAgainst(f, loadText(t, oneCollector)); err == nil || !strings.Contains(err.Error(), `discovered_targets entry "node", checked with the address 192.0.2.1:9100 in place of a discovered one: target "node":`) {
		t.Errorf("an entry its collector refuses: %v", err)
	}
}

// The targets an entry makes of what its sources found come after the
// file's own: named <entry>_<address>, or the address's digest when the file
// writes that name; with the entry's target, its labels over the source's
// (none starting with __), and its other keys; an address two sources give
// taken once, from the first; and one the checks refuse left out, saying
// why.
func TestAnEntryMakesTargetsOfWhatItsSourcesFound(t *testing.T) {
	f, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", strings.Replace(discoveringTargets("127.0.0.1:53"), "  - name: ta\n", "  - name: node_10_0_0_2_3A9100\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	e := &f.Discovered[0]
	found := map[string][]discovery.Target{
		discoverySourceKey(e, &e.Sources[0]): {
			{Address: "10.0.0.1:9100", Labels: map[string]string{"service": "_metrics._tcp.nodes.test", "team": "web", "__meta_dns_name": "_metrics._tcp.nodes.test"}},
			{Address: "10.0.0.2:9100"},
			{Address: "bad host:9100"},
		},
		discoverySourceKey(e, &e.Sources[1]): {{Address: "10.0.0.1:9100", Labels: map[string]string{"other": "x"}}},
	}
	out, skipped, _ := expandDiscovered(f, found, loadText(t, oneCollector), discoveryChecks{})
	names := targetNames(out)
	want := []string{"node_10_0_0_2_3A9100 http://a.invalid", "node_10_0_0_1_3A9100 http://10.0.0.1:9100/metrics", "node_" + discovery.HashedAddress("10.0.0.2:9100") + " http://10.0.0.2:9100/metrics"}
	if !slices.Equal(names, want) {
		t.Errorf("targets %q, want %q", names, want)
	}
	if one := out.Targets[1]; one.Labels["team"] != "infra" || one.Labels["service"] != "_metrics._tcp.nodes.test" || one.Labels["other"] != "" || one.Interval != model.Duration(30*time.Second) || len(one.Labels) != 2 {
		t.Errorf("target %+v", one)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], `discovered_targets entry "node": target "http://bad host:9100/metrics" from bad host:9100:`) {
		t.Errorf("skipped %q", skipped)
	}
	if out.Written != 1 || len(f.Targets) != 1 {
		t.Errorf("the file read is changed: %d written, %d targets", out.Written, len(f.Targets))
	}
}

// The manager asks the sources of the file in force and puts the targets
// they find in force beside the file's own, without counting a reload; a
// change of the records changes them; a failure keeps the last answer and
// is counted; a reload of the file that keeps a source keeps what it found
// at once. The sources are in the self-metrics.
func TestTheManagerPutsTheDiscoveredTargetsInForce(t *testing.T) {
	stub := testutil.NewDNSStub(t,
		testutil.DNSSRV("_metrics._tcp.nodes.test", 0, 0, 9100, "node-a.test"),
		testutil.DNSA("legacy.nodes.test", "10.0.0.9"),
	)
	p := newPair(t, oneCollector, discoveringTargets(stub.Addr))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.manager.DiscoveryLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !slices.Equal(targetNames(p.manager.StaticTargetFile()), want) {
			if time.Now().After(deadline) {
				t.Fatalf("targets %q, want %q", targetNames(p.manager.StaticTargetFile()), want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("ta http://a.invalid", "node_node_2Da_test_3A9100 http://node-a.test:9100/metrics", "node_10_0_0_9_3A9100 http://10.0.0.9:9100/metrics")
	if successes, _ := p.reloads(ReloadFileStaticTargets); successes != 0 {
		t.Errorf("a discovery counted %d reloads", successes)
	}
	stub.SetRecords(testutil.DNSSRV("_metrics._tcp.nodes.test", 0, 0, 9200, "node-b.test"), testutil.DNSA("legacy.nodes.test", "10.0.0.9"))
	waitFor("ta http://a.invalid", "node_node_2Db_test_3A9200 http://node-b.test:9200/metrics", "node_10_0_0_9_3A9100 http://10.0.0.9:9100/metrics")
	stub.SetRcode(2) // SERVFAIL
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(metricsText(p.manager.DiscoveryMetrics()), `http_exporter_discovery_refreshes_total{entry="node",result="failure",source="dns_sd_0"} 1`) {
		if time.Now().After(deadline) {
			t.Fatalf("no failure counted: %s", metricsText(p.manager.DiscoveryMetrics()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := targetNames(p.manager.StaticTargetFile()); len(got) != 3 {
		t.Errorf("a failure changed the targets: %q", got)
	}
	text := metricsText(p.manager.DiscoveryMetrics())
	for _, want := range []string{`http_exporter_discovery_targets{entry="node",source="dns_sd_0"} 1`, `http_exporter_discovery_targets{entry="node",source="legacy"} 1`, `http_exporter_discovery_last_success_timestamp_seconds{entry="node",source="legacy"}`} {
		if !strings.Contains(text, want) {
			t.Errorf("no %s in\n%s", want, text)
		}
	}
	// A reload of the file that keeps the sources keeps their targets.
	p.write(t, p.targetsAt, strings.Replace(discoveringTargets(stub.Addr), "  - name: ta\n", "  - name: tb\n", 1))
	if err := p.manager.Reload(ReloadTriggerSignal); err != nil {
		t.Fatal(err)
	}
	if got := targetNames(p.manager.StaticTargetFile()); len(got) != 3 || got[0] != "tb http://a.invalid" {
		t.Errorf("after a reload that keeps the sources: %q", got)
	}
}

// metricsText is metrics one a line, name{labels} value, labels sorted.
func metricsText(metrics []model.Metric) string {
	var b strings.Builder
	for _, m := range metrics {
		labels := make([]string, 0, len(m.Labels))
		for _, name := range model.SortedKeys(m.Labels) {
			labels = append(labels, fmt.Sprintf("%s=%q", name, m.Labels[name]))
		}
		fmt.Fprintf(&b, "%s{%s} %s\n", m.Name, strings.Join(labels, ","), strconv.FormatFloat(m.Value, 'f', -1, 64))
	}
	return b.String()
}
