//go:build !select_request_types || request_type_snmp

package repository

import "testing"

// The keys of an snmp collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSNMPKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, snmpSchemaKeys())
}

// snmpCollector is a configuration of one snmp collector, with a comment
// where a row writes a key of its request.
const snmpCollector = `collectors:
  - name: demo
    request:
      type: snmp
      community_file: /etc/exporter/snmp-community
      get:
        sysUpTime: 1.3.6.1.2.1.1.3.0
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .get.sysUpTime.value
`

// snmpV3Collector is a configuration of one SNMPv3 collector of
// authPriv, with a comment where a row writes a key of its request.
const snmpV3Collector = `collectors:
  - name: demo
    request:
      type: snmp
      snmp_version: 3
      username: exporter
      auth_password_file: /etc/exporter/snmp-auth
      priv_password_file: /etc/exporter/snmp-priv
      get:
        sysUpTime: 1.3.6.1.2.1.1.3.0
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .get.sysUpTime.value
`

// snmpTarget is a static target file of one target of snmpCollector's
// collector, with a comment where a row writes a key of its request.
const snmpTarget = "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: switch.example\n    request:\n      retries: 1\n      # target\n"

// snmpSchemaKeys are the keys of an snmp collector that a schema holds to
// a rule: security_level, auth_protocol and priv_protocol, each one of a
// few words, exactly, and request_timeout, a duration of at least 10ms, a
// collector's and a static target's.
// The schema and the exporter give each one verdict
// (TestEveryConstrainedKeyOfTheSchemasIsInATable reads the table).
func snmpSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.security_level", of: "an snmp collector's", document: snmpV3Collector, at: request, setting: "      security_level: %s\n      auth_protocol: SHA\n      priv_protocol: AES\n", valid: "authPriv", invalid: "authpriv", absentWas: taken},
		{key: "collectors[].request.auth_protocol", of: "an snmp collector's", document: snmpV3Collector, at: request, setting: "      security_level: authPriv\n      priv_protocol: AES\n      auth_protocol: %s\n", valid: "SHA256", invalid: "sha256", absentWas: taken},
		{key: "collectors[].request.priv_protocol", of: "an snmp collector's", document: snmpV3Collector, at: request, setting: "      security_level: authPriv\n      auth_protocol: SHA\n      priv_protocol: %s\n", valid: "AES256C", invalid: "AES-256", absentWas: taken},
		{key: "collectors[].request.request_timeout", of: "an snmp collector's", document: snmpCollector, at: request, setting: "      request_timeout: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true},
		{key: "targets[].request.request_timeout", of: "an snmp target's", file: inTargetFile, configuration: snmpCollector, document: snmpTarget, at: "      # target\n", setting: "      request_timeout: %s\n", valid: "2s", invalid: "soon", absent: true, duration: true},
		{key: "targets[].request.request_timeout", of: "an snmp target's under its least", file: inTargetFile, configuration: snmpCollector, document: snmpTarget, at: "      # target\n", setting: "      request_timeout: %s\n", valid: "10ms", invalid: "1ms", absent: true, invalidWas: taken},
		{key: "collectors[].request.request_timeout", of: "an snmp collector's under its least", document: snmpCollector, at: request, setting: "      request_timeout: %s\n", valid: "10ms", invalid: "1ms", absent: true, invalidWas: taken},
	}
}
