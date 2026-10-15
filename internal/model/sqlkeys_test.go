package model

import (
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// A collector that sets none of the sql type's keys is written as YAML
// without them, as the fingerprint of its cache writes it: a collector of
// another type is written, and costs, as it did before they were added. One
// that sets them is written with them.
func TestACollectorIsWrittenWithoutTheSQLKeysItLeavesOut(t *testing.T) {
	keys := []string{"driver", "database", "statement", "args", "options", "service_name", "max_rows", "connections"}
	has := func(written []byte, key string) bool {
		return regexp.MustCompile(`(?m)^\s+` + key + `:`).Match(written)
	}
	written, err := yaml.Marshal(&Collector{Name: "web", Request: RequestConfig{Type: "http", Path: "/status"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if has(written, key) {
			t.Errorf("%s is written:\n%s", key, written)
		}
	}
	one := 1
	written, err = yaml.Marshal(&Collector{Name: "db", Request: RequestConfig{Type: "sql", Driver: "postgres", Database: "app", Statement: "SELECT 1", Args: []string{"a"},
		Options: map[string]string{"sslmode": "disable"}, ServiceName: "s", MaxRows: 5, Connections: &SQLConnections{MaxPerTarget: &one}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if !has(written, key) {
			t.Errorf("%s is not written:\n%s", key, written)
		}
	}
}
