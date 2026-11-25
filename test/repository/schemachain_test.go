//go:build !select_request_types || request_type_http

package repository

import (
	"strings"
	"testing"
)

// chainDocument is a configuration of one jq collector named jobs whose
// request is request, indented under it, and whose collector-level keys
// are extra, indented as the collector's.
func chainDocument(request, extra string) string {
	return `collectors:
  - name: jobs
    request:
      type: http
      path: /api/json
` + chainIndent(request, "      ") + chainIndent(extra, "    ") + `    transform:
      type: jq
    metrics:
      - name: jobs_up
        expression: "1"
`
}

// chainIndent indents each line of text by prefix.
func chainIndent(text, prefix string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// The schema and the exporter give the same verdict on the keys of a
// chain: its steps and their bounds, the values a step reads, the fan-out's
// keys, and what a chain does not work beside. A value used before a step
// reads it, a value no step uses, and a path with a host, the exporter
// alone refuses, as the descriptions say.
func TestSchemaAndExporterAgreeOnChains(t *testing.T) {
	schema := loadSchema(t)
	extract := "extract:\n  jobs: {json: '[.jobs[].name]'}\n"
	idExtract := "extract:\n  id: {json: .id}\n"
	step := func(body string) string { return idExtract + "then:\n  - name: detail\n" + chainIndent(body, "    ") }
	fanOut := func(body string) string {
		return extract + "then:\n  - name: last_build\n    for_each: jobs\n    path: /job/{{each}}/api/json\n" + chainIndent(body, "    ")
	}
	accepted := map[string]struct{ request, extra string }{
		"a step":                     {step("path: /item/{{chain.id}}"), ""},
		"a fan-out under all":        {fanOut("concurrency: 16\nmax_items: 1000\non_item_error: skip"), "answer: all"},
		"a step and a link":          {"extract:\n  next: {json: .next}\nthen:\n  - name: detail\n    url: '{{chain.next}}'", "answer: last"},
		"a json body and a post":     {step("method: put\njson: {id: '{{chain.id}}'}"), ""},
		"forwarded headers left out": {step("path: '/x/{{chain.id}}'\nforwarded_headers: false"), ""},
		"a secret header value":      {"extract:\n  token: {header: X-Token, secret: true}\nthen:\n  - name: detail\n    headers: {X-Session: '{{chain.token}}'}", ""},
	}
	for name, d := range accepted {
		agree(t, schema, name, chainDocument(d.request, d.extra), true)
	}
	refused := map[string]struct{ request, extra, message string }{
		"no steps":                     {idExtract + "then: []", "", "request.then has 0 steps"},
		"thirteen steps":               {idExtract + "then:\n" + strings.Repeat("  - {name: s, path: '/x/{{chain.id}}'}\n", 13), "", "request.then has 13 steps"},
		"a step named first":           {idExtract + "then:\n  - {name: first, path: '/x/{{chain.id}}'}", "", `is named "first"`},
		"a step name in capitals":      {idExtract + "then:\n  - {name: Detail, path: '/x/{{chain.id}}'}", "", `is named "Detail"`},
		"two bodies":                   {step("path: /x\nbody: '{{chain.id}}'\nform: {a: b}"), "", "sets 2 of body, form and json"},
		"a path and a url":             {step("path: /x\nurl: '{{chain.id}}'"), "", "sets both path and url"},
		"a url that is more":           {step("url: 'https://x.com/{{chain.id}}'"), "", "a url is one placeholder"},
		"a method outside the list":    {step("method: DELETE\npath: '/x/{{chain.id}}'"), "", `has the method "DELETE"`},
		"concurrency without for_each": {step("path: '/x/{{chain.id}}'\nconcurrency: 2"), "", "without for_each"},
		"concurrency past 16":          {fanOut("concurrency: 17"), "answer: all", "concurrency is 17"},
		"max_items past 1000":          {fanOut("max_items: 1001"), "answer: all", "max_items is 1001"},
		"on_item_error unknown":        {fanOut("on_item_error: retry"), "answer: all", `on_item_error is "retry"`},
		"a value read two ways":        {"extract:\n  id: {json: .id, header: X-Id}\nthen:\n  - {name: d, path: '/x/{{chain.id}}'}", "", "sets 2 of regex, json, form, header and cookie"},
		"answer unknown":               {step("path: '/x/{{chain.id}}'"), "answer: first", `answer is "first"`},
		"answer without then":          {"", "answer: all", "it has no request.then"},
		"pagination beside then":       {step("path: '/x/{{chain.id}}'") + "pagination: {type: link, items: .}", "", "request.pagination beside request.then"},
		"trusted hosts beside then":    {step("path: '/x/{{chain.id}}'") + "follow_redirects: true\nredirect_trusted_hosts: [sso.example]", "", "redirect_trusted_hosts beside request.then"},
		"revalidate beside then":       {step("path: '/x/{{chain.id}}'"), "cache: {ttl: 30s, revalidate: true}", "cache.revalidate beside request.then"},
		"answer all and xml":           {step("path: '/x/{{chain.id}}'"), "answer: all\ndecoder: {type: xml}", "answer: all makes one JSON document"},
	}
	for name, d := range refused {
		refusedByBoth(t, schema, name, chainDocument(d.request, d.extra), d.message)
	}
	alone := map[string]struct{ request, extra, message string }{
		"a value never read":         {"extract:\n  id: {json: .id}\n  other: {json: .other}\nthen:\n  - {name: d, path: '/x/{{chain.id}}'}", "", `reads the value "other" in step "first", which no later step sends`},
		"a value read later":         {"then:\n  - {name: a, path: '/x/{{chain.id}}', extract: {id: {json: .id}}}", "", "sends {{chain.id}}, which no step before it reads"},
		"each outside a fan-out":     {step("path: '/x/{{each}}/{{chain.id}}'"), "", "sends {{each}}, which only a fan-out step has"},
		"for_each over a header":     {"extract:\n  jobs: {header: X-Jobs}\nthen:\n  - {name: d, for_each: jobs, path: '/x/{{each}}'}", "answer: all", "is not a json value"},
		"two fan-outs":               {extract + "then:\n  - {name: a, for_each: jobs, path: '/a/{{each}}', extract: {more: {json: '[.x]'}}}\n  - {name: b, for_each: more, path: '/b/{{each}}'}", "answer: all", "a chain has at most one fan-out"},
		"a fan-out last under last":  {fanOut(""), "", `ends with the fan-out step "last_build" under answer: last`},
		"a path with a host":         {step("path: '//x.com/{{chain.id}}'"), "", "names a scheme or a host"},
		"a step setting credentials": {step("path: '/x/{{chain.id}}'\nheaders: {authorization: x}"), "", "sets Authorization, which carries the collector's credentials"},
		"a chain value in the first": {"query: {x: '{{chain.id}}'}\n" + step("path: '/x/{{chain.id}}'"), "", "it is the chain's first request"},
		"two steps of one name":      {idExtract + "then:\n  - {name: a, path: '/x/{{chain.id}}'}\n  - {name: a, path: /y}", "", `two steps named "a"`},
	}
	for name, d := range alone {
		loadersAlone(t, schema, name, chainDocument(d.request, d.extra), d.message)
	}
}
