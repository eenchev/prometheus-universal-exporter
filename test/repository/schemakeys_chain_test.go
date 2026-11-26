//go:build !select_request_types || request_type_http

package repository

import "strings"

// chainedCollector is jqCollector whose request reads a value and sends it
// in one step, by path and query, with a comment where a row writes a key
// of the step.
var chainedCollector = strings.Replace(jqCollector, "      # request\n", `      extract:
        id: {json: .id}
      then:
        - name: detail
          path: /x/{{chain.id}}
          query: {q: '{{chain.id}}'}
          # step
`, 1)

// fanOutCollector is jqCollector fanning out over a list under answer: all,
// with a comment where a row writes a key of the fan-out step.
var fanOutCollector = strings.Replace(strings.Replace(jqCollector, "      # request\n", `      extract:
        jobs: {json: '[.jobs[]]'}
      then:
        - name: builds
          for_each: jobs
          path: /job/{{each}}
          # step
`, 1), "    # collector\n", "    answer: all\n", 1)

// chainSchemaKeys are the keys of a chain the schema holds to values, a
// pattern or a length. A value's name is written where it is read and where
// it is sent, since a value no step sends is refused.
func chainSchemaKeys() []schemaKey {
	collector, request, step := "    # collector\n", "      # request\n", "          # step\n"
	return []schemaKey{
		{key: "collectors[].answer", document: chainedCollector, at: collector, setting: "    answer: %s\n", valid: "all", invalid: "every", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.extract{}", document: jqCollector, at: request, setting: "      extract: {%[1]s: {json: .id}}\n      then: [{name: d, path: '/x/{{chain.%[1]s}}'}]\n", valid: "job_id", invalid: "9id", absent: true},
		{key: "collectors[].request.then[].extract{}", document: jqCollector, at: request, setting: "      extract: {id: {json: .id}}\n      then: [{name: d, path: '/x/{{chain.id}}', extract: {%[1]s: {json: .x}}}, {name: e, path: '/y/{{chain.%[1]s}}'}]\n", valid: "build", invalid: "9b", absent: true},
		{key: "collectors[].request.then[].name", document: chainedCollector, at: "        - name: detail\n", setting: "        - name: %s\n", valid: "last_build", invalid: "Detail"},
		{key: "collectors[].request.then[].method", document: chainedCollector, at: step, setting: "          method: %s\n", valid: "head", invalid: "delete", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.then[].url", document: chainedCollector, at: "          path: /x/{{chain.id}}\n", setting: "          url: %s\n", valid: "'{{chain.id}}'", invalid: "'/x/{{chain.id}}'", absent: true, empty: true, emptyWas: refused},
		{key: "collectors[].request.then[].accept_status[]", document: chainedCollector, at: step, setting: "          accept_status: [%s]\n", valid: "503", invalid: "600", absent: true},
		{key: "collectors[].request.then[].headers{}", document: chainedCollector, at: step, setting: "          headers: {%s: v}\n", valid: "X-Tenant", invalid: "Authorization", absent: true},
		{key: "collectors[].request.then[].query{}", document: fanOutCollector, at: step, setting: "          query: {%s: v}\n", valid: "page", absent: true},
		{key: "collectors[].request.then[].on_item_error", document: fanOutCollector, at: step, setting: "          on_item_error: %s\n", valid: "skip", invalid: "retry", absent: true, empty: true, emptyWas: refused},
	}
}
