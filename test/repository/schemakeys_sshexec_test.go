//go:build !select_request_types || request_type_sshexec

package repository

import "testing"

// The keys of an sshexec collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSSHExecKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, sshexecSchemaKeys())
}

// sshexecCollector is a configuration of one sshexec collector that runs a
// command with a private key, with a comment where a row writes a key of
// its request.
const sshexecCollector = `collectors:
  - name: demo
    request:
      type: sshexec
      command: [uptime]
      username: metrics
      private_key_file: /etc/exporter/ssh/id_ed25519
      known_hosts_file: /etc/exporter/ssh/known_hosts
      # request
    transform:
      type: regex
    metrics:
      - name: v
        expression: 'load average: ([\d.]+)'
`

// sshexecSchemaKeys are the keys of an sshexec collector that a schema
// holds to a rule: its account and known_hosts_file, which may not be
// blanks alone, "" being the key left out, or, required, the key missing,
// as an sshfile collector's, which the schemas took before; and an entry of allowed_targets and
// denied_targets, which is not "" and is written in ASCII, as for every
// type that reaches a host. The command's words and the exit statuses are
// held to no pattern. The schema and the exporter give each one verdict
// (TestEveryConstrainedKeyOfTheSchemasIsInATable reads the table).
func sshexecSchemaKeys() []schemaKey {
	request := "      # request\n"
	of := "an sshexec collector's"
	return []schemaKey{
		{key: "collectors[].request.username", of: of, document: sshexecCollector, at: "      username: metrics\n", setting: "      username: %s\n", valid: "metrics", invalid: `"  "`, invalidWas: taken, emptyWas: taken},
		{key: "collectors[].request.known_hosts_file", of: of, document: sshexecCollector, at: "      known_hosts_file: /etc/exporter/ssh/known_hosts\n", setting: "      known_hosts_file: %s\n", valid: "/k", invalid: `" "`, invalidWas: taken, emptyWas: taken},
		{key: "collectors[].request.password_file", of: of, document: sshexecCollector, at: request, setting: "      password_file: %s\n", valid: "/p", invalid: `"\t"`, absent: true, empty: true, invalidWas: taken},
		{key: "collectors[].request.passphrase_file", of: of, document: sshexecCollector, at: request, setting: "      passphrase_file: %s\n", valid: "/pp", invalid: `" "`, absent: true, empty: true, invalidWas: taken},
		{key: "collectors[].request.allowed_targets[]", of: of, document: sshexecCollector, at: request, setting: "      allowed_targets: [%s]\n", valid: "db1.example.com", invalid: "bücher.example", absent: true, emptyWas: taken},
		{key: "collectors[].request.denied_targets[]", of: of, document: sshexecCollector, at: request, setting: "      denied_targets: [%s]\n", valid: "10.0.0.0/8", invalid: "bücher.example", absent: true, emptyWas: taken},
	}
}
