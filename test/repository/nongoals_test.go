package repository

import (
	"strings"
	"testing"
)

// The specification's non-goals were written before the exporter read
// anything but HTTP, and "Database connectors" stayed one after the sql
// request type shipped. A non-goal that names something the exporter does
// tells a reader the feature is out of scope while the docs beside it
// explain how to use it, so this keeps the list to what is still left out.

// implementedFeatures are, per feature the exporter has, the words a
// non-goal naming it would use; a non-goal may still mention one as the
// place the feature lives ("databases are the `sql` request type").
var implementedFeatures = map[string][]string{
	"sql":            {"database connector", "sql database", "databases"},
	"grpc":           {"grpc client", "grpc call"},
	"graphite":       {"graphite"},
	"localfile":      {"local file", "reading files"},
	"sshfile":        {"ssh", "sftp"},
	"static targets": {"static targets scraped", "static target"},
}

func TestNonGoalsNameNothingTheExporterDoes(t *testing.T) {
	spec := read(t, "docs/SPECIFICATION-EXPORTER.md")
	_, section, ok := strings.Cut(spec, "\n# 37. Non-goals")
	if !ok {
		t.Fatal("the specification has no \"# 37. Non-goals\" section; it has moved")
	}
	section, _, _ = strings.Cut(section, "\n---")
	var entries []string
	for _, line := range strings.Split(section, "\n") {
		if entry, ok := strings.CutPrefix(line, "- "); ok {
			entries = append(entries, entry)
		}
	}
	if len(entries) < 5 {
		t.Fatalf("found %d non-goals; the list has moved", len(entries))
	}
	for _, entry := range entries {
		// What follows "(" or "other than" says where the feature is,
		// or what of it stays in scope, and names it on purpose.
		goal := strings.ToLower(entry)
		for _, cut := range []string{"(", " other than "} {
			goal, _, _ = strings.Cut(goal, cut)
		}
		for feature, words := range implementedFeatures {
			for _, word := range words {
				if strings.Contains(goal, word) {
					t.Errorf("the non-goal %q names %s (%q), which the exporter has", entry, feature, word)
				}
			}
		}
	}
}
