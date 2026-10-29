//go:build !select_request_types || request_type_sshfile

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sshCollector is a configuration of one sshfile collector that reads one
// file with a private key, with a comment where a row writes a key of its
// request.
const sshCollector = `collectors:
  - name: demo
    request:
      type: sshfile
      root: /var/lib/app
      path: status.txt
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
      # request
    transform:
      type: regex
    metrics:
      - name: v
        expression: 'v=(\d+)'
`

// sshPasswordCollector is sshCollector reading with a password, with a
// comment where a row writes its private key.
var sshPasswordCollector = strings.Replace(sshCollector, "      private_key_file: /etc/exporter/ssh/id_ed25519\n", "      password_file: /etc/exporter/ssh/password\n      # key\n", 1)

// sshTarget is a static target file of one target of sshCollector's
// collector, with a comment where a row writes its request.
const sshTarget = "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: db1.example.com\n    # target\n"

// sshfileSchemaKeys are the keys of an sshfile collector that a schema holds
// to a pattern: its two durations, which take 0 to switch what they time off;
// and its account and credential keys, a collector's and a static target's,
// which may not be blanks alone, "" being the key left out, or, for the
// required username and known_hosts_file, the key missing. The schemas took
// blanks before.
func sshfileSchemaKeys() []schemaKey {
	request, target, key := "      # request\n", "    # target\n", "      # key\n"
	// A target with a private key of its own, and a comment where a row
	// writes beside it.
	withKey := strings.Replace(sshTarget, target, "    request:\n      private_key_file: /k\n"+key, 1)
	return []schemaKey{
		{key: "collectors[].request.keepalive_interval", document: sshCollector, at: request, setting: "      keepalive_interval: %s\n", valid: "10s", invalid: "often", absent: true, duration: true},
		{key: "collectors[].request.keepalive_interval", of: "under its least", document: sshCollector, at: request, setting: "      keepalive_interval: %s\n", valid: "1s", invalid: "500ms", absent: true, invalidWas: taken},
		{key: "collectors[].request.idle_timeout", document: sshCollector, at: request, setting: "      idle_timeout: %s\n", valid: "1m", invalid: "later", absent: true, duration: true},
		{key: "collectors[].request.username", document: sshCollector, at: "      username: metrics\n", setting: "      username: %s\n", valid: "metrics", invalid: `"  "`, invalidWas: taken, emptyWas: taken},
		{key: "collectors[].request.known_hosts_file", document: sshCollector, at: "      known_hosts_file: /etc/exporter/ssh/known_hosts\n", setting: "      known_hosts_file: %s\n", valid: "/k", invalid: `" "`, invalidWas: taken, emptyWas: taken},
		{key: "collectors[].request.private_key_file", document: sshPasswordCollector, at: "      # key\n", setting: "      private_key_file: %s\n", valid: "/k", invalid: `" "`, absent: true, empty: true, invalidWas: taken},
		{key: "collectors[].request.password_file", document: sshCollector, at: request, setting: "      password_file: %s\n", valid: "/p", invalid: `"\t"`, absent: true, empty: true, invalidWas: taken},
		{key: "collectors[].request.passphrase_file", document: sshCollector, at: request, setting: "      passphrase_file: %s\n", valid: "/pp", invalid: `" "`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.username", file: inTargetFile, configuration: sshCollector, document: sshTarget, at: target, setting: "    request:\n      username: %s\n", valid: "backup", invalid: `" "`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.private_key_file", file: inTargetFile, configuration: sshCollector, document: strings.Replace(sshTarget, target, "    request:\n      password_file: /p\n"+key, 1), at: key, setting: "      private_key_file: %s\n", valid: "/k", invalid: `" "`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.password_file", file: inTargetFile, configuration: sshCollector, document: withKey, at: key, setting: "      password_file: %s\n", valid: "/p", invalid: `" "`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.passphrase_file", file: inTargetFile, configuration: sshCollector, document: withKey, at: key, setting: "      passphrase_file: %s\n", valid: "/pp", invalid: `" "`, absent: true, empty: true, invalidWas: taken},
		// A placeholder in them, used as written, is refused too; the
		// schemas took it before.
		{key: "targets[].request.username", of: "with a placeholder", file: inTargetFile, configuration: sshCollector, document: sshTarget, at: target, setting: "    request:\n      username: %s\n", valid: "backup", invalid: `"{{param_user}}"`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.private_key_file", of: "with a placeholder", file: inTargetFile, configuration: sshCollector, document: strings.Replace(sshTarget, target, "    request:\n      password_file: /p\n"+key, 1), at: key, setting: "      private_key_file: %s\n", valid: "/k", invalid: `"/k/{{param_x}}"`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.password_file", of: "with a placeholder", file: inTargetFile, configuration: sshCollector, document: withKey, at: key, setting: "      password_file: %s\n", valid: "/p", invalid: `"/p/{{param_x}}"`, absent: true, empty: true, invalidWas: taken},
		{key: "targets[].request.passphrase_file", of: "with a placeholder", file: inTargetFile, configuration: sshCollector, document: withKey, at: key, setting: "      passphrase_file: %s\n", valid: "/pp", invalid: `"/pp/{{param_x}}"`, absent: true, empty: true, invalidWas: taken},
	}
}

// The durations of an sshfile collector get one verdict from the schema and
// the exporter, left out, written "", written well and written badly: left
// out each is its default, written "" each is no duration to both, and the
// number 0, which switches it off, is the duration it is.
func TestSchemaAndExporterAgreeOnSSHFileKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, sshfileSchemaKeys())
}

// The schema and the exporter agree on what an sshfile collector requires
// and on keepalive_count_max and max_open_connections: root, username and
// known_hosts_file left out or written "", no private key and no password, a
// passphrase without a private key, a count below 1 and a limit below 0 are
// refused by both; a password alone, a key with its passphrase, a count of
// 1, a limit of 0 or 2 and a root of / are taken by both.
func TestSchemaAndExporterAgreeOnWhatAnSSHFileCollectorRequires(t *testing.T) {
	schema := loadSchema(t)
	key := "      private_key_file: /etc/exporter/ssh/id_ed25519\n"
	for name, tc := range map[string]struct {
		document string
		taken    bool
	}{
		"as written":            {sshCollector, true},
		"no username":           {strings.Replace(sshCollector, "      username: metrics\n", "", 1), false},
		"username written \"\"": {strings.Replace(sshCollector, "username: metrics", `username: ""`, 1), false},
		"no known_hosts_file":   {strings.Replace(sshCollector, "      known_hosts_file: /etc/exporter/ssh/known_hosts\n", "", 1), false},
		"no root":               {strings.Replace(sshCollector, "      root: /var/lib/app\n", "", 1), false},
		"no credential":         {strings.Replace(sshCollector, key, "", 1), false},
		"a password alone":      {strings.Replace(sshCollector, key, "      password_file: /etc/exporter/ssh/password\n", 1), true},
		"a key and passphrase":  {strings.Replace(sshCollector, key, key+"      passphrase_file: /etc/exporter/ssh/passphrase\n", 1), true},
		"a lone passphrase":     {strings.Replace(sshCollector, key, "      password_file: /p\n      passphrase_file: /pp\n", 1), false},
		"count 1":               {strings.Replace(sshCollector, "      # request\n", "      keepalive_count_max: 1\n", 1), true},
		"count 0":               {strings.Replace(sshCollector, "      # request\n", "      keepalive_count_max: 0\n", 1), false},
		"count -1":              {strings.Replace(sshCollector, "      # request\n", "      keepalive_count_max: -1\n", 1), false},
		"no connection limit":   {strings.Replace(sshCollector, "      # request\n", "      max_open_connections: 0\n", 1), true},
		"a connection limit":    {strings.Replace(sshCollector, "      # request\n", "      max_open_connections: 2\n", 1), true},
		"a limit below 0":       {strings.Replace(sshCollector, "      # request\n", "      max_open_connections: -1\n", 1), false},
		"root /":                {strings.Replace(sshCollector, "root: /var/lib/app", "root: /", 1), true},
	} {
		problems := schemaProblems(t, schema, tc.document)
		_, err := config.Load(testutil.WriteFile(t, "config.yaml", tc.document))
		if (len(problems) == 0) != tc.taken || (err == nil) != tc.taken {
			t.Errorf("%s: the schema finds %v and the exporter %v; want both to take it: %v", name, problems, err, tc.taken)
		}
	}
}
