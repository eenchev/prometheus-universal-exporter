//go:build request_type_redis || request_type_ldap

package exporter

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSecrets writes each of secrets, by name, into a file of dir, and
// returns the files by name.
func writeSecrets(t *testing.T, dir string, secrets map[string]string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for name, secret := range secrets {
		files[name] = filepath.Join(dir, name)
		if err := os.WriteFile(files[name], []byte(secret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}
