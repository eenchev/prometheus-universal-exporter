package config

import (
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// numbersAround are decimal numbers near c, the least divided by a unit:
// c to more and more digits, rounded down and up, and with a digit more or
// less at the end, besides some that every unit is tried with. A pattern
// that goes wrong at the boundary goes wrong on one of them.
func numbersAround(c *big.Rat) []string {
	numbers := []string{"0", "00", "1", "01", "5", "9", "10", "99", "100", "999", "1000", "1001", "1023", "1024", "32", "33", "33.5", "0.5", ".5", "5.", "0.0", ".0", "0.", "1.5", "0.001", "0.0009", "0.00001", "65535", "65536", "32767", "32768", "33554431", "33554432"}
	for places := 0; places <= 20; places++ {
		down := c.FloatString(places)
		numbers = append(numbers, down, strings.TrimLeft(down, "0"), "0"+down)
		if places > 0 {
			numbers = append(numbers, down+"0", down+"9", down[:len(down)-1]+"9", strings.TrimSuffix(down, "0"))
		}
		up := new(big.Rat).Add(c, new(big.Rat).SetFrac64(1, pow10(places))).FloatString(places)
		numbers = append(numbers, up, up+"1")
	}
	return numbers
}

func pow10(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// The pattern of text under a duration's least takes, of every text that is
// one number with a unit, what the exporter reads as under the least, and no
// other text it reads as a duration; with zero taken, a text that comes to
// no whole nanosecond is not under the least either, and neither is a zero
// written in any way. The texts are numbers around the least in each unit,
// with a sign and without.
func TestTheLeastDurationRuleTakesWhatTheExporterReadsAsUnderTheLeast(t *testing.T) {
	read := func(text string) (time.Duration, bool) {
		var d model.Duration
		err := d.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: text})
		return time.Duration(d), err == nil
	}
	tried := 0
	durationRE := regexp.MustCompile(durationPattern)
	for _, least := range []time.Duration{time.Nanosecond, time.Millisecond, time.Second} {
		for _, zero := range []bool{true, false} {
			rule := fetch.SchemaLeastDuration(least, zero)
			for _, unit := range []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h"} {
				scale := map[string]int64{"ns": 1, "us": 1e3, "µs": 1e3, "μs": 1e3, "ms": 1e6, "s": 1e9, "m": 6e10, "h": 36e11}[unit]
				for _, number := range numbersAround(big.NewRat(int64(least), scale)) {
					for _, sign := range []string{"", "+", "-"} {
						text := sign + number + unit
						d, ok := read(text)
						if !ok || d < 0 || !durationRE.MatchString(text) || !exact(number, scale, int64(d)) {
							continue
						}
						tried++
						refused := d < least && (!zero || d > 0)
						if got := refusedBy(rule, text); got != refused {
							t.Errorf("least %s, zero taken %v: %q, which the exporter reads as %s: the rule refuses it: %v", least, zero, text, d, got)
						}
					}
				}
			}
		}
	}
	// Of several parts, each under the least, the sum is the exporter's to
	// tell; zero in any way is refused where it is not taken.
	for text, refused := range map[string]bool{"500us500us": false, "0s0m": true, "-0s": true, "0": true} {
		if got := refusedBy(fetch.SchemaLeastDuration(time.Millisecond, false), text); got != refused {
			t.Errorf("%q: the rule refuses it: %v", text, got)
		}
	}
	if tried < 5000 {
		t.Fatalf("only %d texts were tried", tried)
	}
	t.Logf("%d texts tried", tried)
}

// The pattern of text under a size's least takes, of every text that is a
// number, with a unit or without, what the exporter reads as at least a byte
// and under the least, and no other size.
func TestTheLeastSizeRuleTakesWhatTheExporterReadsAsUnderTheLeast(t *testing.T) {
	tried := 0
	for _, least := range []int64{38, 1 << 10, 64 << 10, 32 << 20} {
		rule := fetch.SchemaLeastSize(least)
		for _, unit := range []struct {
			pattern string
			scale   int64
		}{{"", 1}, {"b", 1}, {"k", 1e3}, {"m", 1e6}, {"g", 1e9}, {"t", 1e12}, {"ki", 1 << 10}, {"mi", 1 << 20}, {"gi", 1 << 30}, {"ti", 1 << 40}} {
			for _, spelling := range map[int64][]string{1: {"", "B", " b"}, 1e3: {"k", "KB", " kB"}, 1e6: {"M", "mb", " MB"}, 1e9: {"G", " gb"}, 1e12: {"T", "tB"}, 1 << 10: {"Ki", "KiB", " kib"}, 1 << 20: {"Mi", "MiB", " MIB"}, 1 << 30: {"GiB", " Gi"}, 1 << 40: {"TiB", "ti"}}[unit.scale] {
				if unit.pattern == "" && spelling != "" || unit.pattern != "" && spelling == "" {
					continue
				}
				for _, number := range numbersAround(big.NewRat(least, unit.scale)) {
					text := number + spelling
					var size model.ByteSize
					if size.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: text}) != nil || !exact(number, unit.scale, int64(size)) {
						continue
					}
					tried++
					refused := size > 0 && int64(size) < least
					if got := refusedBy(rule, text); got != refused {
						t.Errorf("least %d: %q, which the exporter reads as %d bytes: the rule refuses it: %v", least, text, size, got)
					}
				}
			}
		}
	}
	if tried < 3000 {
		t.Fatalf("only %d texts were tried", tried)
	}
	t.Logf("%d texts tried", tried)
}

// exact says whether read, what the exporter read number times scale as,
// is the whole part of the product: the exporter multiplies in floating
// point, which at the twentieth digit or so of a fraction rounds across a
// whole nanosecond or byte, 0.00100000000000us coming to 0ns and
// 0.9999999999999999999ns to 1ns. Where it does, the pattern, which reads
// the digits, gives the verdict of the number written, and the case is not
// tried.
func exact(number string, scale, read int64) bool {
	value, ok := new(big.Rat).SetString(strings.TrimSuffix(number, "."))
	if !ok {
		value = new(big.Rat)
	}
	product := value.Mul(value, big.NewRat(scale, 1))
	whole := new(big.Int).Quo(product.Num(), product.Denom())
	return whole.IsInt64() && whole.Int64() == read
}

// compiledPatterns are the rules' patterns, each compiled once.
var compiledPatterns = map[string]*regexp.Regexp{}

// refusedBy says whether a rule made of not's subschema matches text: that
// is, whether a key with the rule refuses it. It reads the shapes the two
// rules are made of: a pattern with a not beside it, and anyOf of those and
// const.
func refusedBy(rule map[string]any, text string) bool {
	if anyOf, ok := rule["anyOf"].([]any); ok {
		for _, alternative := range anyOf {
			if refusedBy(alternative.(map[string]any), text) {
				return true
			}
		}
		return false
	}
	if _, ok := rule["const"]; ok {
		// The number 0, which no text is.
		return false
	}
	pattern := rule["pattern"].(string)
	re, ok := compiledPatterns[pattern]
	if !ok {
		re = regexp.MustCompile(pattern)
		compiledPatterns[pattern] = re
	}
	if !re.MatchString(text) {
		return false
	}
	if not, ok := rule["not"].(map[string]any); ok {
		return !refusedBy(not, text)
	}
	return true
}
