//go:build !select_request_types || request_type_redis

package repository

import "testing"

// The keys of a redis collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnRedisKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, redisSchemaKeys())
}

// redisCollector is a configuration of one redis collector, with a comment
// where a row writes a key of its request.
const redisCollector = `collectors:
  - name: demo
    request:
      type: redis
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .info.server.uptime_in_seconds
`

// redisSentinel is a configuration of one redis collector whose target is
// a Sentinel, its master and password file each a line of its own, so that
// either may be left out.
const redisSentinel = `collectors:
  - name: demo
    request:
      type: redis
      sentinel:
        username: watcher
        master: mymaster
        password_file: /run/sentinel-pass
    transform:
      type: jq
    metrics:
      - name: v
        expression: .sentinel.quorum
`

// redisSchemaKeys are the keys of a redis collector that a schema holds to
// a rule: a section of info_sections, lower-case letters and _; the name a
// command's reply is put under, letters, digits and _ not starting with a
// digit, neither of which may be written "", a section of that name there
// being none of and a command needing a name; sentinel's master, required
// and not blanks; and password_file, the collector's and sentinel's, which a
// username needs beside it, so that written "" beside one it is refused,
// as the schema as it was, without the rule, took it. The schema and the exporter give each one verdict
// (TestEveryConstrainedKeyOfTheSchemasIsInATable reads the table).
func redisSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.info_sections[]", document: redisCollector, at: request, setting: "      info_sections: [%s]\n", valid: "memory", invalid: "Memory", absent: true},
		{key: "collectors[].request.password_file", of: "a redis collector's, beside username", document: redisCollector, at: request, setting: "      username: exporter\n      password_file: %s\n", valid: "/var/run/secrets/redis/password", invalid: `" "`, absent: true, emptyWas: taken, invalidWas: taken},
		{key: "collectors[].request.sentinel.master", document: redisSentinel, at: "        master: mymaster\n", setting: "        master: %s\n", valid: "cache-a", invalid: `" "`},
		{key: "collectors[].request.sentinel.password_file", of: "beside sentinel.username", document: redisSentinel, at: "        password_file: /run/sentinel-pass\n", setting: "        password_file: %s\n", valid: "/p", invalid: `" "`},
		{key: "collectors[].request.commands[].name", document: redisCollector, at: request, setting: "      commands:\n        - name: %s\n          command: [DBSIZE]\n", valid: "keys", invalid: "queue-len", absent: true},
	}
}
