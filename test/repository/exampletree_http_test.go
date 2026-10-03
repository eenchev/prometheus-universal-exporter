//go:build !select_request_types || request_type_http

package repository

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
)

// An example's static target file is valid as an editor checks it, against
// the published schema, and as the exporter checks it at startup: on its own,
// and against the configuration in its directory, whose collectors and
// placeholders its targets name.
func TestExampleStaticTargetFilesMatchTheSchemaAndTheirConfiguration(t *testing.T) {
	tree := shippedExamples(t)
	if len(tree.staticTargets) == 0 {
		t.Fatal("no example static target file found; this must not pass by finding nothing")
	}
	schema := loadSchemaFile(t, staticTargetsSchemaFile)
	for _, path := range tree.staticTargets {
		t.Run(path, func(t *testing.T) {
			if errs := validateAgainstSchema(schema, readYAMLDocument(t, path)); len(errs) > 0 {
				t.Errorf("%s does not match the schema:\n%s", path, strings.Join(errs, "\n"))
			}
			var beside []string
			for _, candidate := range tree.configs {
				if filepath.Dir(candidate) == filepath.Dir(path) {
					beside = append(beside, candidate)
				}
			}
			if len(beside) != 1 {
				t.Fatalf("%s needs exactly one configuration in its directory to be checked against, found %v", path, beside)
			}
			cfg, err := config.Load(beside[0])
			if err != nil {
				t.Fatal(err)
			}
			file, err := config.LoadStaticTargets(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.ValidateStaticTargets(file); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if err := config.ValidateStaticTargetsAgainst(file, cfg); err != nil {
				t.Fatalf("%s does not match %s: %v", path, beside[0], err)
			}
		})
	}
}
