//go:build !select_request_types || request_type_http

package exporter

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// A static target that a discovered_targets entry makes of a dns_sd
// source's A record is scraped as a written one is, once the exporter's
// discovery has found it.
func TestADiscoveredStaticTargetIsScraped(t *testing.T) {
	tier.Slow(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("value=7\n"))
	}))
	t.Cleanup(target.Close)
	u, _ := url.Parse(target.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	stub := testutil.NewDNSStub(t, testutil.DNSA("node.test", "127.0.0.1"))
	path := testutil.WriteFile(t, "targets.yaml", `interval: 1m
discovered_targets:
  - name: node
    collector: text
    target_template: http://{{address}}/metrics
    discovery:
      - dns_sd:
          names: [node.test]
          type: A
          port: `+port+`
          server: `+stub.Addr+`
`)
	cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("text", "text")}}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	file, err := config.LoadStaticTargets(path)
	if err == nil {
		err = config.ValidateStaticTargets(file)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(file, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	manager := config.NewManager(cfg, "", testutil.QuietLogger(t))
	manager.SetTargets("", file)
	server := NewServer(manager, "python3", slog.Default())
	server.logger = testutil.QuietLogger(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { manager.DiscoveryLoop(ctx); done <- struct{}{} }()
	go func() { server.StaticScrapeLoop(ctx); done <- struct{}{} }()
	t.Cleanup(func() { cancel(); <-done; <-done })
	name := "node_127_0_0_1_3A" + port
	testutil.WaitFor(t, "the discovered target to be scraped", func() bool {
		for _, r := range server.staticTargetResults() {
			if r.name == name {
				return true
			}
		}
		return false
	})
}
