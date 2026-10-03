//go:build !select_request_types || request_type_http

package exporter

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// examples/config.mempool.json-test.yaml reads three endpoints of
// mempool.space's REST API with three collectors. The fixtures under
// testdata/json are answers in the shape the API documents; the block height
// is a number alone.

const mempoolConfig = "../../examples/config.mempool.json-test.yaml"

func newMempool(t *testing.T, height string) (*standIn, *Server) {
	t.Helper()
	service, cfg := newStandIn(t, mempoolConfig, map[string]standInAnswer{
		"/api/v1/fees/recommended": {"application/json; charset=utf-8", readTestdata(t, "json/mempool-fees-recommended.json")},
		"/api/mempool":             {"application/json; charset=utf-8", readTestdata(t, "json/mempool-backlog.json")},
		"/api/blocks/tip/height":   {"text/plain; charset=utf-8", []byte(height)},
	})
	var names []string
	for _, collector := range cfg.Collectors {
		names = append(names, collector.Name)
	}
	if want := []string{"bitcoin_fees", "bitcoin_mempool", "bitcoin_chain"}; !slices.Equal(names, want) {
		t.Fatalf("%s holds the collectors %v, want %v", mempoolConfig, names, want)
	}
	return service, NewServer(config.NewManager(cfg, mempoolConfig, slog.Default()), "python3", slog.Default())
}

// Each collector asks for its own endpoint, with the request the three share,
// and gives its series: a fee rate per key of the answer, the keys renamed by
// the label's value_map; the backlog, its fees in bitcoin; and the height.
// Nothing is logged.
func TestTheMempoolExampleReadsItsThreeEndpoints(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	service, server := newMempool(t, "917532")

	sameSeries(t, probeStandIn(t, server, service, "bitcoin_fees", ""), []string{
		// In the order of the API's keys, which jq sorts.
		`bitcoin_recommended_fee_sats_per_vbyte{target="economy"} 4`,
		`bitcoin_recommended_fee_sats_per_vbyte{target="next_block"} 12`,
		`bitcoin_recommended_fee_sats_per_vbyte{target="half_hour"} 9`,
		`bitcoin_recommended_fee_sats_per_vbyte{target="hour"} 7`,
		`bitcoin_recommended_fee_sats_per_vbyte{target="minimum"} 2`,
	})
	sameSeries(t, probeStandIn(t, server, service, "bitcoin_mempool", ""), []string{
		`bitcoin_mempool_transactions 48213`,
		`bitcoin_mempool_virtual_bytes 3.187654e+07`,
		`bitcoin_mempool_fees_bitcoin 0.61250834`,
	})
	sameSeries(t, probeStandIn(t, server, service, "bitcoin_chain", ""), []string{
		`bitcoin_block_height 917532`,
	})
	want := []string{"/api/v1/fees/recommended", "/api/mempool", "/api/blocks/tip/height"}
	if asked := service.requests(); !slices.Equal(asked, want) {
		t.Errorf("the stand-in was asked %v, want %v", asked, want)
	}
	if logs.Len() != 0 {
		t.Errorf("reading the three endpoints logged:\n%s", logs)
	}
}

// The height is read with or without the line end a server may write after
// it, and an answer that is no number fails the probe, as error_mode: fail
// has it: a height that cannot be read is no height.
func TestTheMempoolExampleReadsABareNumber(t *testing.T) {
	testutil.CaptureLogs(t)
	service, server := newMempool(t, "917532\n")
	sameSeries(t, probeStandIn(t, server, service, "bitcoin_chain", ""), []string{`bitcoin_block_height 917532`})

	service, server = newMempool(t, "Too Many Requests")
	response := probeOnce(t, server, "/probe?collector=bitcoin_chain&target="+service.URL, nil)
	if response.Code == 200 || !strings.Contains(response.Body.String(), `"metric":"bitcoin_block_height"`) {
		t.Errorf("an answer that is no number: status=%d body=%s", response.Code, response.Body.String())
	}
}

// A key the API adds to the recommended fees is a series of its own, under
// the key as the API writes it, without the example changing.
func TestTheMempoolExampleKeepsAFeeTargetItDoesNotKnow(t *testing.T) {
	testutil.CaptureLogs(t)
	service, cfg := newStandIn(t, mempoolConfig, map[string]standInAnswer{
		"/api/v1/fees/recommended": {"application/json", []byte(`{"fastestFee":3,"dayFee":1}`)},
	})
	server := NewServer(config.NewManager(cfg, mempoolConfig, slog.Default()), "python3", slog.Default())
	sameSeries(t, probeStandIn(t, server, service, "bitcoin_fees", ""), []string{
		`bitcoin_recommended_fee_sats_per_vbyte{target="dayFee"} 1`,
		`bitcoin_recommended_fee_sats_per_vbyte{target="next_block"} 3`,
	})
}
