package transform

import (
	"context"
	"fmt"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
)

// One compiled program serves concurrent scrapes.
func TestCompiledJQIsSafeConcurrently(t *testing.T) {
	code, err := expr.CompileJQ(`.n * 2`)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func(n int) {
			values, err := evaluateJQ(context.Background(), map[string]any{"n": n}, nil, `.n * 2`)
			if err == nil && (len(values) != 1 || values[0] != n*2) {
				err = fmt.Errorf("%d*2 = %v", n, values)
			}
			done <- err
		}(i)
	}
	for i := 0; i < 50; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	_ = code
}

func BenchmarkJQCompiledOnce(b *testing.B) {
	data := map[string]any{"servers": []any{map[string]any{"cpu": 1}, map[string]any{"cpu": 2}}}
	for b.Loop() {
		if _, err := evaluateJQ(context.Background(), data, data, `[.servers[].cpu] | add`); err != nil {
			b.Fatal(err)
		}
	}
}
