//go:build !select_request_types || (request_type_http && request_type_localfile)

package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sizedCollectors is a configuration, or a collector file, of an http
// collector and a localfile one with every key that takes a size written as
// size, and one whole number, max_metrics, written the same way.
func sizedCollectors(root, size string) string {
	return strings.ReplaceAll(`collectors:
  - name: web
    request:
      type: http
      max_response_bytes: SIZE
    transform: {type: regex}
    metrics:
      - name: v
        expression: 'v=(\d+)'
    limits:
      max_response_bytes: SIZE
      max_output_bytes: SIZE
      max_script_memory: SIZE
      max_metrics: SIZE
  - name: files
    request:
      type: localfile
      root: `+root+`
      files: ["*.prom"]
      max_total_bytes: SIZE
    transform: {type: prometheus}
`, "SIZE", size)
}

// sizesOf are the five sizes of sizedCollectors as they loaded, and its
// max_metrics.
func sizesOf(cfg *model.Config) ([]model.ByteSize, int) {
	web, files := &cfg.Collectors[0], &cfg.Collectors[1]
	return []model.ByteSize{web.Request.MaxResponseBytes, web.Limits.MaxResponseBytes, web.Limits.MaxOutputBytes, web.Limits.MaxScriptMemory, files.Request.MaxTotalBytes}, web.Limits.MaxMetrics
}

// Every key that takes a size takes a whole number of bytes however YAML
// writes the number — with an exponent, with a fraction of zero, with a
// sign — in the configuration and in a collector file, as max_metrics and
// every other key of a whole number already did, and as the schemas, which
// are handed the number, do. A size once refused 1e8 as no size and
// 100000000.0 as a fraction.
func TestEverySizeKeyTakesAWholeNumberHoweverItIsWritten(t *testing.T) {
	root := t.TempDir()
	for written, want := range map[string]model.ByteSize{
		"1e8": 100_000_000, "1E8": 100_000_000, "1e+8": 100_000_000, "+1e8": 100_000_000, "100000000.0": 100_000_000, "100000000.": 100_000_000,
		"2.5e8": 250_000_000, "2.50e8": 250_000_000, "0.5e9": 500_000_000, ".5e9": 500_000_000, "1000000000e-1": 100_000_000,
		"1_0e7": 100_000_000, "1.34217728e8": 128 << 20, "100000000": 100_000_000, "+100000000": 100_000_000,
	} {
		inFile := testutil.WriteIn(t, t.TempDir(), "collectors.yaml", sizedCollectors(root, written))
		for file, path := range map[string]string{
			"the configuration": testutil.WriteFile(t, "config.yaml", sizedCollectors(root, written)),
			"a collector file":  testutil.WriteFile(t, "config.yaml", "collector_files: ["+inFile+"]\n"),
		} {
			cfg, err := Load(path)
			if err != nil {
				t.Errorf("%s, sizes written %s: %v", file, written, err)
				continue
			}
			sizes, metrics := sizesOf(cfg)
			for i, size := range sizes {
				if size != want {
					t.Errorf("%s, sizes written %s: size %d of %d is %d, want %d", file, written, i+1, len(sizes), size, want)
				}
			}
			if metrics != int(want) {
				t.Errorf("%s, max_metrics written %s: %d, want %d", file, written, metrics, want)
			}
		}
	}
	// Written 0 in any of these ways, a size is the default, as 0 is.
	for _, zero := range []string{"0", "0.0", "0e0", "-0.0"} {
		cfg, err := Load(testutil.WriteFile(t, "config.yaml", sizedCollectors(root, zero)))
		if err != nil {
			t.Errorf("sizes written %s: %v", zero, err)
			continue
		}
		if web := &cfg.Collectors[0]; web.Limits.MaxOutputBytes != 1<<20 || web.Limits.MaxScriptMemory != 0 || web.Limits.MaxMetrics != 10000 {
			t.Errorf("sizes written %s: %+v, want the defaults", zero, web.Limits)
		}
	}
}

// What is no whole number of bytes from 0 up to 2^63 is refused at every size
// key, once, at its line, by the size and not a second time by the check of
// the whole numbers beside it: a fraction and infinity as they were, and a
// whole number that is negative or too large for what it is, in the words of
// one written as an integer, where -1e8 and 1e19 were once "not a number of
// bytes or a number with a unit" and 9223372036854775808.0 "not a whole
// number of bytes". Quoted, a number with a point or an exponent is text,
// which is a size only with a unit.
func TestEverySizeKeyRefusesWhatIsNoWholeNumberOfBytes(t *testing.T) {
	const (
		noSize   = `size %q is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`
		fraction = `size %q is not a whole number of bytes; write a whole number, or a number with a unit such as 1.5KiB`
		negative = "size %s is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB"
		tooLarge = "size %s is too large; a size is under 8EiB, which is 2^63 bytes"
	)
	root := t.TempDir()
	for written, message := range map[string]string{
		"1.5": fraction, "100000000.5": fraction, "1e-1": noSize, "1.5e0": noSize, "-1.5": noSize, "-1e-1": noSize,
		"-1e8": negative, "-1.0": negative, "-100000000.0": negative, "-1e19": negative,
		"1e19": tooLarge, "9.223372036854775808e18": tooLarge, "9223372036854775808.0": tooLarge, "1e300": tooLarge,
		".inf": noSize, "-.inf": noSize, ".nan": noSize,
		"'1e8'": noSize, `"1e8"`: noSize, "'2.5e8'": noSize, "'100000000.0'": fraction, `"1.0"`: fraction, "'1.5'": fraction,
		"'-1e8'": noSize, `"1e19"`: noSize, "'9223372036854775808.0'": fraction,
	} {
		// max_metrics takes a whole number or refuses in words of its own.
		document := strings.Replace(sizedCollectors(root, written), "      max_metrics: "+written+"\n", "", 1)
		_, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err == nil {
			t.Errorf("sizes written %s load", written)
			continue
		}
		want := fmt.Sprintf(message, strings.Trim(written, `'"`))
		for _, line := range []int{5, 11, 12, 13, 19} {
			if at := fmt.Sprintf("line %d: %s", line, want); strings.Count(err.Error(), at) != 1 {
				t.Errorf("sizes written %s: want %q once; got %v", written, at, err)
			}
		}
		if problems := strings.Count(err.Error(), "line "); problems != 5 {
			t.Errorf("sizes written %s: %d problems, want one for each of the five sizes: %v", written, problems, err)
		}
	}
	for written, want := range map[string]string{
		"-1":                  "size -1 is negative; a size is a number of bytes from 0, or a number with a unit such as 10MiB",
		"9223372036854775808": "size 9223372036854775808 is too large; a size is under 8EiB, which is 2^63 bytes",
		"8388608TiB":          `size "8388608TiB" is too large; a size is under 8EiB, which is 2^63 bytes`,
	} {
		document := strings.Replace(sizedCollectors(root, written), "      max_metrics: "+written+"\n", "", 1)
		if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || strings.Count(err.Error(), want) != 5 {
			t.Errorf("sizes written %s: want %q of each of the five sizes; got %v", written, want, err)
		}
	}
}

// A size past what a float64 holds exactly is the number that is written, at
// a size key, and the largest size written with an exponent is the largest
// size.
func TestASizeKeyReadsAWholeNumberExactly(t *testing.T) {
	for written, want := range map[string]model.ByteSize{"9007199254740993.0": 1<<53 + 1, "9.223372036854775807e18": 1<<63 - 1, "9223372036854775807": 1<<63 - 1} {
		cfg, err := loadChecked(t, "", "    limits: {max_output_bytes: "+written+"}\n", "")
		if err != nil || cfg.Collectors[0].Limits.MaxOutputBytes != want {
			t.Errorf("max_output_bytes: %s: %v, want %d", written, err, want)
		}
	}
}

// Under --config.expand-env a size that comes from the environment is what it
// would be written there by hand: a reference without quotes is the number
// its value spells, an exponent and all, and one in quotes is text, which is
// a size only with a unit.
func TestASizeFromTheEnvironmentIsReadAsItWouldBeWritten(t *testing.T) {
	for value, want := range map[string]model.ByteSize{"1e6": 1_000_000, "1000000.0": 1_000_000, "2.5e6": 2_500_000, "1000000": 1_000_000, "1MB": 1_000_000} {
		t.Setenv("PUE_TEST_SIZE", value)
		path := testutil.WriteFile(t, "config.yaml", checkedCollector("      max_response_bytes: ${PUE_TEST_SIZE}\n", "", ""))
		cfg, err := Load(path, WithEnvExpansion())
		if err != nil || cfg.Collectors[0].Request.MaxResponseBytes != want {
			t.Errorf("max_response_bytes: ${PUE_TEST_SIZE} of %s: %v, want %d", value, err, want)
		}
	}
	for value, want := range map[string]string{
		"1e6":       `line 5: size "1e6" is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB`,
		"1000000.0": `line 5: size "1000000.0" is not a whole number of bytes; write a whole number, or a number with a unit such as 1.5KiB`,
	} {
		t.Setenv("PUE_TEST_SIZE", value)
		path := testutil.WriteFile(t, "config.yaml", checkedCollector("      max_response_bytes: \"${PUE_TEST_SIZE}\"\n", "", ""))
		if _, err := Load(path, WithEnvExpansion()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("max_response_bytes: \"${PUE_TEST_SIZE}\" of %s: %v, want %q", value, err, want)
		}
	}
	t.Setenv("PUE_TEST_SIZE", "1.5")
	path := testutil.WriteFile(t, "config.yaml", checkedCollector("      max_response_bytes: ${PUE_TEST_SIZE}\n", "", ""))
	if _, err := Load(path, WithEnvExpansion()); err == nil || !strings.Contains(err.Error(), `line 5: size "1.5" is not a whole number of bytes`) {
		t.Errorf("max_response_bytes: ${PUE_TEST_SIZE} of 1.5: %v", err)
	}
}
