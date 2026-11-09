//go:build !select_request_types || request_type_http || request_type_graphite

package fetch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A login that panics is its callers' error, on the goroutine the login
// runs on for all of them, and the exporter serves on.
func TestALoginThatPanicsIsItsCallersError(t *testing.T) {
	cache := &loginCache{entries: map[string]*loginEntry{}, made: map[string]uint64{}, failed: map[string]uint64{}, refreshed: map[string]uint64{}}
	_, err := cache.get(context.Background(), "key", "api", "fp", func(context.Context) (loginToken, error) {
		panic("reading the token")
	})
	var p *goroutinePanic
	if !errors.As(err, &p) || !strings.Contains(err.Error(), "reading the token") {
		t.Fatalf("the login's caller was told %v", err)
	}
}
