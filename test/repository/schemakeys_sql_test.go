//go:build !select_request_types || request_type_sql

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sqlCollector is a configuration of one sql collector, with a comment
// where a row writes a key of its request.
const sqlCollector = `collectors:
  - name: demo
    request:
      type: sql
      driver: postgres
      username: metrics
      statement: SELECT 1 AS v
      # request
    transform:
      type: csv
    metrics:
      - name: v
        expression: v
`

// sqlTarget is a static target file of one target of sqlCollector's
// collector, with a comment where a row writes its request.
const sqlTarget = "interval: 1m\ntargets:\n  - name: a\n    collector: demo\n    target: db.example\n    # target\n"

// sqlSchemaKeys are the keys of a sql collector that a schema holds to
// allowed values or a pattern: the driver, required, in any case; the statement,
// required, and the database and service_name, none of them blanks alone;
// the durations and the check of request.connections, a collector's and
// a static target's; and a static target's account and password file, which
// take no placeholder.
func sqlSchemaKeys() []schemaKey {
	request, target := "      # request\n", "    # target\n"
	connections := func(file keyFile, key, setting, valid, invalid string) schemaKey {
		row := schemaKey{key: key, document: sqlCollector, at: request, setting: "      connections:\n" + setting, valid: valid, invalid: invalid, absent: true, duration: true}
		if file == inTargetFile {
			row.file, row.configuration, row.document, row.at = inTargetFile, sqlCollector, sqlTarget, target
			row.setting = "    request:\n" + row.setting
		}
		return row
	}
	var rows []schemaKey
	for _, file := range []keyFile{inConfiguration, inTargetFile} {
		prefix, indent := "collectors[].request.connections.", "        "
		if file == inTargetFile {
			prefix = "targets[].request.connections."
		}
		rows = append(rows,
			connections(file, prefix+"idle_timeout", indent+"idle_timeout: %s\n", "1m", "later"),
			connections(file, prefix+"max_lifetime", indent+"max_lifetime: %s\n", "2h", "never"),
			connections(file, prefix+"keepalive.interval", indent+"keepalive:\n"+indent+"  interval: %s\n", "10s", "often"),
		)
		// An interval of one part under the least, 1s, is refused by both.
		least := connections(file, prefix+"keepalive.interval", indent+"keepalive:\n"+indent+"  interval: %s\n", "1s", "500ms")
		least.of, least.duration, least.invalidWas = "under its least", false, taken
		rows = append(rows, least)
		// The check beside an interval, so that the block is there written
		// "" or left out, and loads as the same either way.
		check := schemaKey{key: prefix + "keepalive.check", document: strings.Replace(sqlCollector, request, "      connections:\n        keepalive:\n          interval: 30s\n          # check\n", 1), at: "          # check\n", setting: "          check: %s\n", valid: "ping", invalid: "icmp", absent: true, empty: true, emptyWas: refused}
		if file == inTargetFile {
			check.file, check.configuration = inTargetFile, sqlCollector
			check.document = strings.Replace(sqlTarget, target, "    request:\n      connections:\n        keepalive:\n          interval: 30s\n          # check\n", 1)
		}
		rows = append(rows, check)
	}
	return append(rows,
		schemaKey{key: "collectors[].request.driver", document: sqlCollector, at: "      driver: postgres\n", setting: "      driver: %s\n", valid: "mysql", invalid: "db2"},
		// A driver is matched in any case and with blanks around it or not,
		// by both.
		schemaKey{key: "collectors[].request.driver", of: "in another case", document: sqlCollector, at: "      driver: postgres\n", setting: "      driver: %s\n", valid: "MySQL", invalid: "DB2"},
		schemaKey{key: "collectors[].request.driver", of: "with blanks around it", document: sqlCollector, at: "      driver: postgres\n", setting: "      driver: %s\n", valid: `" SQLServer\t"`, invalid: `" mysql x"`},
		// max_targets is the collector's: a static target's is refused by
		// both, whatever its value, "" as every whole number's is.
		schemaKey{key: "targets[].request.connections.max_targets", file: inTargetFile, configuration: sqlCollector, document: sqlTarget, at: target, setting: "    request:\n      connections:\n        max_targets: %s\n", invalid: "5", absent: true},
		schemaKey{key: "collectors[].request.statement", document: sqlCollector, at: "      statement: SELECT 1 AS v\n", setting: "      statement: %s\n", valid: "SELECT 2 AS v", invalid: `" "`, emptyWas: taken},
		schemaKey{key: "collectors[].request.database", document: sqlCollector, at: request, setting: "      database: %s\n", valid: "app", invalid: `" "`, absent: true, empty: true},
		schemaKey{key: "collectors[].request.service_name", document: sqlCollector, at: request, setting: "      service_name: %s\n", invalid: `" "`, absent: true, empty: true,
			booleanAlone: "sets request.service_name, which only the oracle driver takes", numberAlone: "sets request.service_name, which only the oracle driver takes"},
		// A static target's account and password file are used as written,
		// so a placeholder in them is refused; the schemas took it before.
		schemaKey{key: "targets[].request.username", of: "a sql target's, with a placeholder", file: inTargetFile, configuration: sqlCollector, document: sqlTarget, at: target, setting: "    request:\n      username: %s\n", valid: "reader", invalid: `"{{param_user}}"`, absent: true, empty: true, invalidWas: taken},
		schemaKey{key: "targets[].request.password_file", of: "a sql target's, with a placeholder", file: inTargetFile, configuration: sqlCollector, document: sqlTarget, at: target, setting: "    request:\n      password_file: %s\n", valid: "/p", invalid: `"/run/{{param_db}}/password"`, absent: true, empty: true, invalidWas: taken},
	)
}

// The keys of a sql collector get one verdict from the schema and the
// exporter, left out, written "", written well and written badly.
func TestSchemaAndExporterAgreeOnSQLKeysWrittenEmpty(t *testing.T) {
	checkSchemaKeys(t, sqlSchemaKeys())
}

// The schema and the exporter agree on what a sql collector requires and
// on its counts: driver, statement and username left out or written "" are
// refused by both, as are a count below 0 and max_rows below 0; zero
// counts and a max_rows are taken by both.
func TestSchemaAndExporterAgreeOnWhatASQLCollectorRequires(t *testing.T) {
	schema := loadSchema(t)
	for name, tc := range map[string]struct {
		document string
		taken    bool
	}{
		"as written":             {sqlCollector, true},
		"no driver":              {strings.Replace(sqlCollector, "      driver: postgres\n", "", 1), false},
		"no statement":           {strings.Replace(sqlCollector, "      statement: SELECT 1 AS v\n", "", 1), false},
		"statement written \"\"": {strings.Replace(sqlCollector, "statement: SELECT 1 AS v", `statement: ""`, 1), false},
		"no username":            {strings.Replace(sqlCollector, "      username: metrics\n", "", 1), false},
		"no limits":              {strings.Replace(sqlCollector, "      # request\n", "      connections:\n        max_per_target: 0\n        max_idle_per_target: 0\n        max_targets: 0\n", 1), true},
		"a limit below 0":        {strings.Replace(sqlCollector, "      # request\n", "      connections:\n        max_per_target: -1\n", 1), false},
		"max_rows":               {strings.Replace(sqlCollector, "      # request\n", "      max_rows: 5\n", 1), true},
		"max_rows below 0":       {strings.Replace(sqlCollector, "      # request\n", "      max_rows: -1\n", 1), false},
	} {
		problems := schemaProblems(t, schema, tc.document)
		_, err := config.Load(testutil.WriteFile(t, "config.yaml", tc.document))
		if (len(problems) == 0) != tc.taken || (err == nil) != tc.taken {
			t.Errorf("%s: the schema finds %v and the exporter %v; want both to take it: %v", name, problems, err, tc.taken)
		}
	}
}
