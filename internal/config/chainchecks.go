package config

import (
	"fmt"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// checkChainAnswer checks the collector's answer, which says what an http
// collector with request.then reads (fetch/chain.go). Under answer: all
// the response is one JSON document of every answer, so the decoder is
// json.
func checkChainAnswer(x *model.Collector) error {
	if fetch.CollectorPolicy(x).Chain == nil {
		if x.Answer != "" {
			return fmt.Errorf("collector %q sets answer, which says what an http collector with request.then reads; it has no request.then", x.Name)
		}
		return nil
	}
	if x.Answer != model.ChainAnswerAll {
		return nil
	}
	switch x.Decoder.Type {
	case "auto", "json":
		x.Decoder.Type = "json"
	default:
		return fmt.Errorf("collector %q decodes with %s, but answer: all makes one JSON document of every answer; leave decoder.type out, or set json", x.Name, x.Decoder.Type)
	}
	return nil
}
