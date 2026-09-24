package model

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestByteSizes(t *testing.T) {
	for in, want := range map[string]ByteSize{
		"0": 0, "1024": 1024, "10B": 10, "1kB": 1000, "1KB": 1000, "1k": 1000, "2KiB": 2048, "2kib": 2048,
		"10MB": 10_000_000, "64MiB": 64 << 20, "64 MiB": 64 << 20, "1.5GiB": 3 << 29, "1GB": 1e9, "1TiB": 1 << 40, "0.5KiB": 512, "1.0001B": 1,
	} {
		got, err := parseByteSize(in)
		if err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "-1", "10 XB", "1e3", "10MiBs", "ten", "99999999999TiB"} {
		if _, err := parseByteSize(in); err == nil {
			t.Errorf("parseByteSize(%q) was accepted", in)
		}
	}
	var doc struct {
		A ByteSize `yaml:"a"`
		B ByteSize `yaml:"b"`
		C ByteSize `yaml:"c"`
	}
	if err := yaml.Unmarshal([]byte("a: 1048576\nb: 64MiB\nc: \"512\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.A != 1<<20 || doc.B != 64<<20 || doc.C != 512 {
		t.Fatalf("decoded %+v", doc)
	}
	if err := yaml.Unmarshal([]byte("a: [1]\n"), &doc); err == nil {
		t.Fatal("a list was accepted as a size")
	}
}
