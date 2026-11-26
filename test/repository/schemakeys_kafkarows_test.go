//go:build !select_request_types || request_type_kafka

package repository

import "strings"

// kafkaCollector is a configuration of one kafka collector, with a comment
// where a row writes a key of its request.
const kafkaCollector = `collectors:
  - name: demo
    request:
      type: kafka
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .brokers | length
`

// kafkaLoginCollector is kafkaCollector with an account, which every
// mechanism but OAUTHBEARER sends.
var kafkaLoginCollector = strings.Replace(kafkaCollector, "      # request\n", "      username: monitor\n      password_file: /etc/kafka-password\n      # request\n", 1)

// kafkaSchemaKeys are the keys of a kafka collector that a schema holds to
// a rule: the SASL mechanism, one of four, and the patterns of topics and
// groups, each at most 1000 characters.
func kafkaSchemaKeys() []schemaKey {
	request := "      # request\n"
	long := `"` + strings.Repeat("a", 1001) + `"`
	return []schemaKey{
		{key: "collectors[].request.sasl_mechanism", of: "kafka", document: kafkaLoginCollector, at: request, setting: "      sasl_mechanism: %s\n", valid: "SCRAM-SHA-512", invalid: "GSSAPI", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.include_topics[]", of: "kafka", document: kafkaCollector, at: request, setting: "      include_topics: [%s]\n", valid: "'orders\\..*'", invalid: long, absent: true, built: true},
		{key: "collectors[].request.exclude_topics[]", of: "kafka", document: kafkaCollector, at: request, setting: "      exclude_topics: [%s]\n", valid: "'__.*'", invalid: long, absent: true, built: true},
		{key: "collectors[].request.include_groups[]", of: "kafka", document: kafkaCollector, at: request, setting: "      include_groups: [%s]\n", valid: "billing", invalid: long, absent: true, built: true},
		{key: "collectors[].request.exclude_groups[]", of: "kafka", document: kafkaCollector, at: request, setting: "      exclude_groups: [%s]\n", valid: "'console-.*'", invalid: long, absent: true, built: true},
	}
}
