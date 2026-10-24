//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// rateLimitsConfig is a configuration with one collector and one entry of
// rate_limits, named name.
func rateLimitsConfig(t *testing.T, name string) *model.Config {
	t.Helper()
	cfg := &model.Config{
		Collectors: []model.Collector{testutil.Collector("text", "text")},
		RateLimits: []model.HostRateLimit{{Name: name, Hosts: []string{"api.example"}, RateLimit: model.RateLimitConfig{RequestsPerSecond: 1}}},
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Of two followings made close together, the one of the earlier
// configuration may tell the request types of it last: its caller had made
// it, and released the statistics lock, when the later reload's following
// was made and told. The entries of rate_limits in force are still the later
// configuration's, and are not put back to the earlier's, which would stay
// in force until the next reload.
func TestAnEarlierFollowingToldLastLeavesTheLaterInForce(t *testing.T) {
	fetch.CloseConnections()
	t.Cleanup(fetch.CloseConnections)
	server := NewServer(config.NewManager(rateLimitsConfig(t, "start"), "", slog.Default()), "python3", testutil.QuietLogger(t))
	earlier := config.NewManager(rateLimitsConfig(t, "earlier"), "", slog.Default())
	later := config.NewManager(rateLimitsConfig(t, "later"), "", slog.Default())

	reached, resume := make(chan uint64), make(chan struct{})
	held := false
	hook := func(generation uint64) {
		// Only the first following, the earlier configuration's, is held.
		if !held {
			held = true
			reached <- generation
			<-resume
		}
	}
	followAppliesHook.Store(&hook)
	t.Cleanup(func() { followAppliesHook.Store(nil) })

	server.manager = earlier
	done := make(chan *followedConfig)
	go func() { done <- server.reconcile() }()
	heldAt := <-reached
	// The earlier following is made and not yet told: the later is made and
	// told meanwhile, as a reload's would be.
	server.manager = later
	if followed := server.reconcile(); followed.generation <= heldAt {
		t.Fatalf("the later following has generation %d, the earlier %d", followed.generation, heldAt)
	}
	close(resume)
	<-done
	var names []string
	for _, count := range fetch.HostRateLimitCounts() {
		names = append(names, count.Name)
	}
	if len(names) != 1 || names[0] != "later" {
		t.Errorf("the entries of rate_limits in force are %v, want [later]: the earlier following, told last, put its own back", names)
	}
}
