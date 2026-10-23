//go:build !select_request_types || request_type_redis

package repository

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

// redisSchemaKeys are the keys of a redis collector that a schema holds to
// a rule: a section of info_sections, lower-case letters and _; the name a
// command's reply is put under, letters, digits and _ not starting with a
// digit, neither of which may be written "", a section of that name there
// being none of and a command needing a name; and password_file, which a
// username needs beside it, so that written "" beside one it is refused,
// as the schema as it was, without the rule, took it. They are held in every
// build's schema, the default build's among them, which does not carry the
// opt-in type (TestEveryConstrainedKeyOfTheSchemasIsInATable), and the two
// verdicts are compared in a build with it (schemakeys_redis_test.go).
func redisSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.info_sections[]", document: redisCollector, at: request, setting: "      info_sections: [%s]\n", valid: "memory", invalid: "Memory", absent: true, built: true},
		{key: "collectors[].request.password_file", of: "a redis collector's, beside username", document: redisCollector, at: request, setting: "      username: exporter\n      password_file: %s\n", valid: "/var/run/secrets/redis/password", invalid: `" "`, absent: true, emptyWas: taken, invalidWas: taken, built: true},
		{key: "collectors[].request.commands[].name", document: redisCollector, at: request, setting: "      commands:\n        - name: %s\n          command: [DBSIZE]\n", valid: "keys", invalid: "queue-len", absent: true, built: true},
	}
}
