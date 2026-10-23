package model

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// cache.revalidate is a key of the cache, true when left out.
func TestCacheRevalidateIsTrueUnlessTurnedOff(t *testing.T) {
	for document, want := range map[string]bool{
		"ttl: 1m\n":                    true,
		"ttl: 1m\nrevalidate: true\n":  true,
		"ttl: 1m\nrevalidate: false\n": false,
	} {
		var c Collector
		if err := yaml.Unmarshal([]byte(document), &c.Cache); err != nil {
			t.Fatalf("%q: %v", document, err)
		}
		if got := Revalidates(&c); got != want {
			t.Errorf("%q revalidates %v, want %v", document, got, want)
		}
	}
}
