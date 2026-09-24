package model

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ByteSize is a size in bytes. The configuration may write it as a plain
// number of bytes, 10485760, or with a unit, 10MiB: B; kB, MB, GB and TB,
// powers of 1000; KiB, MiB, GiB and TiB, powers of 1024. The unit is case
// insensitive and may follow a space, and the number may have a fraction,
// 1.5GiB, rounded down to whole bytes.
type ByteSize int64

// ByteSizePattern is what UnmarshalYAML accepts as a string.
const ByteSizePattern = `^[0-9]+(\.[0-9]+)? ?([kKmMgGtT][iI]?)?[bB]?$`

var byteSizeRE = regexp.MustCompile(ByteSizePattern)

var byteSizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1e3, "kb": 1e3, "m": 1e6, "mb": 1e6, "g": 1e9, "gb": 1e9, "t": 1e12, "tb": 1e12,
	"ki": 1 << 10, "kib": 1 << 10, "mi": 1 << 20, "mib": 1 << 20, "gi": 1 << 30, "gib": 1 << 30, "ti": 1 << 40, "tib": 1 << 40,
}

// parseByteSize reads a size as the configuration writes it.
func parseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	if !byteSizeRE.MatchString(s) {
		return 0, fmt.Errorf("size %q is not a number of bytes or a number with a unit such as 512KiB, 10MB or 1.5GiB", s)
	}
	end := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if end < 0 {
		end = len(s)
	}
	number, unit := s[:end], strings.ToLower(strings.TrimSpace(s[end:]))
	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	scale, ok := byteSizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("size %q has an unknown unit %q", s, unit)
	}
	bytes := math.Floor(value * scale)
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return ByteSize(bytes), nil
}

// UnmarshalYAML reads a size given as a number of bytes or as a string with
// a unit, such as 10MiB, reporting a bad one with its line.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return lineError(n, "expected a size, a number of bytes or a string such as 10MiB, not %s", describeNode(n))
	}
	if n.Tag == "!!int" {
		v, err := strconv.ParseInt(n.Value, 0, 64)
		if err != nil {
			return lineError(n, "size %q: %v", n.Value, err)
		}
		*b = ByteSize(v)
		return nil
	}
	v, err := parseByteSize(n.Value)
	if err != nil {
		return lineError(n, "%v", err)
	}
	*b = v
	return nil
}
