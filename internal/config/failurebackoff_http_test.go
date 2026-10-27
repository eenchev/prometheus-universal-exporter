//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// failure_backoff takes after, initial, max and min_budget, fills in what it
// leaves out (after 3, initial 30s, max 10m or initial when longer,
// min_budget 1s), and refuses an after below 1, an initial or min_budget that
// is not a positive duration, a max shorter than initial and a key it does
// not know, naming the collector and the key.
func TestFailureBackoffIsCheckedAndFilledIn(t *testing.T) {
	var floor time.Duration
	load := func(block string) (*time.Duration, *time.Duration, *int, error) {
		t.Helper()
		floor = 0
		path := testutil.WriteIn(t, t.TempDir(), "config.yaml", testutil.CollectorsDocument("dead")+block)
		cfg, err := Load(path)
		if err != nil {
			return nil, nil, nil, err
		}
		b := cfg.Collectors[0].FailureBackoff
		if b == nil {
			return nil, nil, nil, nil
		}
		if b.After == nil || b.Initial == nil || b.Max == nil || b.MinBudget == nil {
			return nil, nil, nil, errors.New("failure_backoff loaded with a key left unfilled")
		}
		floor = time.Duration(*b.MinBudget)
		initial, longest := time.Duration(*b.Initial), time.Duration(*b.Max)
		return &initial, &longest, b.After, nil
	}
	if initial, _, _, err := load(""); err != nil || initial != nil {
		t.Fatalf("a collector without failure_backoff loads with %v, %v; want none", initial, err)
	}
	for _, tc := range []struct {
		block                   string
		after                   int
		initial, longest, floor time.Duration
		refused                 string
	}{
		{block: "    failure_backoff: {}\n", after: 3, initial: 30 * time.Second, longest: 10 * time.Minute, floor: time.Second},
		{block: "    failure_backoff: {after: 1, initial: 5s, max: 40s, min_budget: 250ms}\n", after: 1, initial: 5 * time.Second, longest: 40 * time.Second, floor: 250 * time.Millisecond},
		{block: "    failure_backoff: {initial: 20m}\n", after: 3, initial: 20 * time.Minute, longest: 20 * time.Minute, floor: time.Second},
		{block: "    failure_backoff: {initial: 1m, max: 1m}\n", after: 3, initial: time.Minute, longest: time.Minute, floor: time.Second},
		{block: "    failure_backoff: {min_budget: 0s}\n", refused: `collector "dead" failure_backoff.min_budget must be a positive duration`},
		{block: "    failure_backoff: {min_budget: -1s}\n", refused: `failure_backoff.min_budget must be a positive duration`},
		{block: "    failure_backoff: {after: 0}\n", refused: `collector "dead" failure_backoff.after must be at least 1`},
		{block: "    failure_backoff: {after: -2}\n", refused: `failure_backoff.after must be at least 1`},
		{block: "    failure_backoff: {initial: 0s}\n", refused: `collector "dead" failure_backoff.initial must be a positive duration`},
		{block: "    failure_backoff: {initial: -1s}\n", refused: `failure_backoff.initial must be a positive duration`},
		{block: "    failure_backoff: {initial: 1m, max: 30s}\n", refused: `collector "dead" failure_backoff.max 30s must not be shorter than failure_backoff.initial 1m0s`},
		{block: "    failure_backoff: {max: 5s}\n", refused: `failure_backoff.max 5s must not be shorter than failure_backoff.initial 30s`},
		{block: "    failure_backoff: {afte: 3}\n", refused: `failure_backoff has the unknown key "afte"; it takes enabled, after, initial, max and min_budget`},
		{block: "    failure_backoff: 30s\n", refused: "failure_backoff is a mapping: write failure_backoff: {initial: 30s}"},
	} {
		initial, longest, after, err := load(tc.block)
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%q loads with %v, want it refused with %q", tc.block, err, tc.refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q is refused: %v", tc.block, err)
			continue
		}
		if *after != tc.after || *initial != tc.initial || *longest != tc.longest || floor != tc.floor {
			t.Errorf("%q loads as after %d, initial %s, max %s, min_budget %s; want %d, %s, %s, %s", tc.block, *after, *initial, *longest, floor, tc.after, tc.initial, tc.longest, tc.floor)
		}
	}
}
