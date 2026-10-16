//go:build !select_request_types || request_type_http || request_type_grpc || request_type_graphite

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What the tests of the checks at load of the types that reach a server
// share (loadchecks_*_test.go).

// loadCheckedTargets loads a target file against a configuration, as the
// exporter does at startup: the file, then its targets, then the targets
// against the collectors.
func loadCheckedTargets(t *testing.T, configuration, targets string) error {
	t.Helper()
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", configuration))
	if err != nil {
		t.Fatal(err)
	}
	f, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", targets))
	if err == nil {
		err = ValidateStaticTargets(f)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(f, cfg)
	}
	return err
}

// The message of a credential refused for a control character, after the
// key and the character.
const credentialAdvice = "; a line break at its end is usually one the Secret or the environment variable it came from ends with: remove it, or read the credential from a file with the key's _file form, which trims it"

// checkRefusal fails the test when err is not a refusal whose message holds
// want, or, for a want of nothing, when there is a refusal at all.
func checkRefusal(t *testing.T, name string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: refused: %v", name, err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("%s: %v, want it refused with %s", name, err, want)
	}
}
