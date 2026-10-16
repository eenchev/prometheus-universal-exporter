//go:build !select_request_types || request_type_http

package repository

import (
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The exporter's own web.basic_auth, once enabled, takes no username with a
// colon, which a client's Basic credential ends the username at, so that no
// client could present it: the schema and the exporter both refuse one, and
// both take one without; switched off, the block is unchecked by both, as
// it was. The username is the block's to require, which the exporter does
// and a schema does not, so it has no row of its own in the tables of the
// keys (schemakeys_http_test.go).
func TestSchemaAndExporterAgreeOnAColonInTheExportersOwnUsername(t *testing.T) {
	schema := loadSchema(t)
	auth := func(enabled, username string) string {
		return "web:\n  basic_auth:\n    enabled: " + enabled + "\n    username: " + username + "\n    password: s3cret\n" + testutil.MinimalConfig
	}
	refusedByBoth(t, schema, "a username with a colon", auth("true", `"admin:ops"`), `web.basic_auth.username holds a colon (":")`)
	refusedByBoth(t, schema, "a username of a colon alone", auth("true", `":"`), `web.basic_auth.username holds a colon (":")`)
	agree(t, schema, "a username without one", auth("true", "admin"), true)
	agree(t, schema, "switched off", auth("false", `"admin:ops"`), true)
}
