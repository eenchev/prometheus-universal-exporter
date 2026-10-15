//go:build select_request_types && !request_type_sql

package exporter

import (
	"strings"
	"testing"
)

// A build without the sql request type has no sql drivers, and --version
// says nothing of them, rather than naming an empty list.
func TestTheVersionOfABuildWithoutSQLNamesNoDrivers(t *testing.T) {
	version := VersionString()
	if strings.Contains(version, "sql drivers") || !strings.HasSuffix(version, ")") {
		t.Errorf("--version prints %q", version)
	}
	if drivers := BuildVersion().SQLDrivers; len(drivers) != 0 {
		t.Errorf("sql drivers %v in a build without the sql request type", drivers)
	}
}
