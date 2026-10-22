//go:build !select_request_types || request_type_kubernetes

package repository

import "testing"

// kubernetesCollector is a configuration of one kubernetes collector, with
// a comment where a row writes a key of its request; kubernetesNamed is one
// that names its resource already.
const (
	kubernetesCollector = `collectors:
  - name: demo
    request:
      type: kubernetes
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .count
`
	kubernetesNamed = `collectors:
  - name: demo
    request:
      type: kubernetes
      resource: pods
      # request
    transform:
      type: jq
    metrics:
      - name: v
        expression: .count
`
)

// kubernetesSchemaKeys are the keys of a kubernetes collector that a
// schema holds to a rule: the resource, required, a plural name and not the
// core group's secrets; the group, a group's name and never core; the
// version and each namespace, a DNS label; the selectors, not blanks alone;
// api_server, an https URL beside a credential; and its tls block's
// insecure_skip_verify, refused true beside api_server, the token being
// sent to whoever answers. "" is each key left out, as it is to the
// exporter: the resource is then missing.
func kubernetesSchemaKeys() []schemaKey {
	request := "      # request\n"
	server := "      api_server: https://k8s.example:6443\n      bearer_token_file: /var/run/token\n      namespaces: [shop]\n"
	return []schemaKey{
		{key: "collectors[].request.resource", document: kubernetesCollector, at: request, setting: "      resource: %s\n", valid: "deployments", invalid: "Deployments", emptyWas: refused},
		{key: "collectors[].request.resource", of: "the core group's secrets", document: kubernetesCollector, at: request, setting: "      resource: %s\n", valid: "configmaps", invalid: "secrets", invalidWas: taken, emptyWas: refused},
		{key: "collectors[].request.group", document: kubernetesNamed, at: request, setting: "      group: %s\n", valid: "cert-manager.io", invalid: "core", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.group", of: "in upper case", document: kubernetesNamed, at: request, setting: "      group: %s\n", valid: "apps", invalid: "Apps", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.version", document: kubernetesNamed, at: request, setting: "      version: %s\n", valid: "v1beta1", invalid: "V1", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.namespaces[]", document: kubernetesNamed, at: request, setting: "      namespaces: [%s]\n", valid: "shop", invalid: "shop_1", absent: true},
		{key: "collectors[].request.label_selector", document: kubernetesNamed, at: request, setting: "      label_selector: %s\n", valid: "app=shop", invalid: `" "`, absent: true, empty: true},
		{key: "collectors[].request.field_selector", document: kubernetesNamed, at: request, setting: "      field_selector: %s\n", valid: "status.phase=Running", invalid: `" "`, absent: true, empty: true},
		{key: "collectors[].request.api_server", document: kubernetesNamed, at: request, setting: "      api_server: %s\n      bearer_token_file: /var/run/token\n      namespaces: [shop]\n", valid: "https://k8s.example:6443/proxy", invalid: "http://k8s.example:6443", absent: true},
		{key: "collectors[].request.tls.insecure_skip_verify", of: "a kubernetes collector's", document: kubernetesNamed, at: request, setting: server + "      tls:\n        insecure_skip_verify: %s\n", valid: "false", invalid: "true", absent: true, invalidWas: taken},
	}
}

// The keys of a kubernetes collector get one verdict from the schema and
// the exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnKubernetesKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, kubernetesSchemaKeys())
}
