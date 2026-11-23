//go:build !select_request_types || request_type_radius

package repository

// radiusCollector is a configuration of one radius collector with its
// secret and an account, with a comment where a row writes a key of its
// request.
const radiusCollector = `collectors:
  - name: demo
    request:
      type: radius
      secret_file: /etc/exporter/radius-secret
      username: monitor
      password_file: /etc/exporter/radius-password
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .accepted
`

// radiusSchemaKeys are the keys of a radius collector that a schema holds
// to a rule: mode, of three values; nas_identifier, a line of at most 253
// bytes; and resend_interval, a duration of at least 100ms, 0 the default.
func radiusSchemaKeys() []schemaKey {
	request := "      # request\n"
	return []schemaKey{
		{key: "collectors[].request.mode", of: "radius", document: radiusCollector, at: request, setting: "      mode: %s\n", valid: "chap", invalid: "eap", absent: true, empty: true, emptyWas: refused, built: true},
		{key: "collectors[].request.nas_identifier", of: "radius", document: radiusCollector, at: request, setting: "      nas_identifier: %s\n", valid: "probe-1", invalid: `"a\tb"`, absent: true, empty: true, built: true},
		{key: "collectors[].request.resend_interval", of: "a radius collector's", document: radiusCollector, at: request, setting: "      resend_interval: %s\n", valid: "5s", invalid: "often", absent: true, duration: true, built: true},
		{key: "collectors[].request.resend_interval", of: "a radius collector's under its least", document: radiusCollector, at: request, setting: "      resend_interval: %s\n", valid: "100ms", invalid: "50ms", absent: true, invalidWas: taken, built: true},
	}
}
