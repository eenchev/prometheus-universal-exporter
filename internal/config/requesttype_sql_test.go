//go:build !select_request_types || request_type_sql

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// sqlCollector is a configuration of one sql collector, with a comment
// where a case writes more of it.
const sqlCollector = `collectors:
  - name: db
    request:
      type: sql
      driver: postgres
      username: metrics
      statement: SELECT state, count(*) AS n FROM jobs GROUP BY state
    transform:
      type: csv
    # more
    metrics:
      - name: jobs
        expression: n
`

// A sql collector's rows have no body: a decoder.type, a response setting
// and a transform that reads no rows are refused when the configuration
// loads, each saying what to do; csv, jq, yq and python are taken, with no
// warning that the decoder is left to each response.
func TestWhatReadsASQLCollectorsRowsIsCheckedWhenItLoads(t *testing.T) {
	for name, tc := range map[string]struct {
		replace, with, want string
	}{
		"a decoder":         {"    # more\n", "    decoder:\n      type: json\n", `collector "db" decodes with json, but a sql collector's rows are read by its driver, with no body to decode; leave decoder.type out`},
		"a response":        {"    # more\n", "    response:\n      charset: latin1\n", `collector "db" sets response settings, which apply to the body of a response, and a sql collector's rows have none`},
		"a regex transform": {"      type: csv\n", "      type: regex\n", `collector "db" transforms with regex, which cannot read the rows of a statement; use csv`},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(sqlCollector, tc.replace, tc.with, 1)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v\nwant %s", name, err, tc.want)
		}
	}
	for _, transform := range []string{"csv", "jq", "yq"} {
		document := strings.Replace(sqlCollector, "      type: csv\n", "      type: "+transform+"\n", 1)
		if transform != "csv" {
			document = strings.Replace(document, "        expression: n\n", "        items: .[]\n        expression: .n\n", 1)
		}
		c, err := Load(testutil.WriteFile(t, "config.yaml", document))
		if err != nil {
			t.Errorf("%s: %v", transform, err)
			continue
		}
		if c.Collectors[0].Decoder.Type != "auto" || len(c.Warnings) != 0 {
			t.Errorf("%s: decoder %q, warnings %q", transform, c.Collectors[0].Decoder.Type, c.Warnings)
		}
	}
}

// loadSQLTargets loads a target file of a target of sqlCollector's
// collector, whose request writes setting, as the exporter does at
// startup: the file, its targets, then the targets against the collectors.
func loadSQLTargets(t *testing.T, target, setting string) error {
	t.Helper()
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", sqlCollector))
	if err != nil {
		t.Fatal(err)
	}
	document := "interval: 1m\ntargets:\n  - name: a\n    collector: db\n    target: \"" + target + "\"\n" + setting
	f, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
	if err == nil {
		err = ValidateStaticTargets(f)
	}
	if err == nil {
		err = ValidateStaticTargetsAgainst(f, cfg)
	}
	return err
}

// A static sql target that ends in a colon, with no port after it, is
// refused when it loads, a host's and an IPv6 address's alike, saying what
// to write; with a port, or with neither colon nor port, it loads.
func TestAStaticSQLTargetWithAnEmptyPortIsRefused(t *testing.T) {
	for _, target := range []string{"db.example:", "[::1]:"} {
		want := `target "a": target "` + target + `" ends in a colon with no port after it; write the port after the colon, or leave the colon out for the driver's port`
		if err := loadSQLTargets(t, target, ""); err == nil || err.Error() != want {
			t.Errorf("%s: %v\nwant %s", target, err, want)
		}
	}
	for _, target := range []string{"db.example", "db.example:5433", "[::1]", "[::1]:5433"} {
		if err := loadSQLTargets(t, target, ""); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}
}

// A static sql target's username, password_file and private_key_file are
// used as written, with no probe to fill a placeholder, so one with a
// placeholder is refused when it loads, naming the key.
func TestAStaticSQLTargetsCredentialsTakeNoPlaceholder(t *testing.T) {
	for key, value := range map[string]string{"username": "{{param_user}}", "password_file": "/run/{{param_db}}/password", "private_key_file": "/run/{{param_db}}/key.p8"} {
		want := `request.` + key + ` "` + value + `" has a placeholder; a static target's account and credential files are used as they are written, with no probe to fill one, so write the value out`
		err := loadSQLTargets(t, "db.example", "    request:\n      "+key+": \""+value+"\"\n")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v\nwant %s", key, err, want)
		}
	}
	if err := loadSQLTargets(t, "db.example", "    request:\n      username: reader\n      password_file: /run/reader/password\n"); err != nil {
		t.Errorf("without a placeholder: %v", err)
	}
}

// A placeholder in a sql collector's setting that no probe fills, its
// username, is refused when the configuration loads, the refusal naming
// the database and the args among the places a probe's parameters fill;
// placeholders in those two load.
func TestASQLCollectorsUnfilledPlaceholderNamesWhereTheyAreFilled(t *testing.T) {
	document := strings.Replace(sqlCollector, "      username: metrics\n", "      username: \"{{param_user}}\"\n", 1)
	want := `collector "db" request.username has a {{param_...}} placeholder, which is not filled in there and would be used as written; a probe's parameters fill placeholders only in the request's path, body, header and query values, a grpc message and metadata values, a graphite collector's targets, a sql collector's database and args, and the label values of transform.labels and of a metric rule's static label`
	if _, err := Load(testutil.WriteFile(t, "config.yaml", document)); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%v\nwant %s", err, want)
	}
	filled := strings.Replace(sqlCollector, "      username: metrics\n", "      username: metrics\n      database: \"{{param_db:app}}\"\n      args: [\"{{param_queue:mail}}\"]\n", 1)
	if _, err := Load(testutil.WriteFile(t, "config.yaml", filled)); err != nil {
		t.Errorf("placeholders in database and args: %v", err)
	}
}

// A tls block written empty, tls: {}, asks a sql collector for TLS like a
// block with keys: beside sslmode disable it is refused, where it used to
// be the zero block and loaded, the scrape plaintext.
func TestAnEmptyTLSBlockAsksASQLCollectorForTLS(t *testing.T) {
	_, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(sqlCollector, "      type: sql\n", "      type: sql\n      tls: {}\n      options: {sslmode: disable}\n", 1)))
	if err == nil || !strings.Contains(err.Error(), "sets request.tls, but request.options.sslmode is \"disable\", which uses no TLS") {
		t.Errorf("tls: {} beside sslmode disable: %v", err)
	}
	if _, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(sqlCollector, "      type: sql\n", "      type: sql\n      tls: {}\n", 1))); err != nil {
		t.Errorf("tls: {}: %v", err)
	}
}

// arg_types is read as a sql collector's own key, and a static target whose
// params fill an arg with a value not of its type is refused when its file
// loads, naming the target and the arg.
func TestASQLCollectorsArgTypesHoldItsStaticTargetsParams(t *testing.T) {
	typed := strings.Replace(sqlCollector, "      username: metrics\n", "      username: metrics\n      args: [\"{{param_limit:10}}\"]\n      arg_types: [int]\n", 1)
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", typed))
	if err != nil {
		t.Fatal(err)
	}
	for value, want := range map[string]string{
		"25":  "",
		"ten": `target "a" uses collector "db": request.args[0] is "ten", which is no whole number`,
		"2.5": `target "a" uses collector "db": request.args[0] is "2.5", which is no whole number`,
		// A parameter left empty is one not given: the default fills it.
		"":     "",
		"1e3":  `target "a" uses collector "db": request.args[0] is "1e3", which is no whole number`,
		"-100": "",
	} {
		document := "interval: 1m\ntargets:\n  - name: a\n    collector: db\n    target: db.example\n    params:\n      param_limit: \"" + value + "\"\n"
		f, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
		if err == nil {
			err = ValidateStaticTargets(f)
		}
		if err == nil {
			err = ValidateStaticTargetsAgainst(f, cfg)
		}
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("param_limit %q: %v, want %q", value, err, want)
		}
	}
	bad := strings.Replace(typed, "arg_types: [int]", "arg_types: [int, text]", 1)
	if _, err := Load(testutil.WriteFile(t, "config.yaml", bad)); err == nil || !strings.Contains(err.Error(), `collector "db" request.arg_types has 2 types for 1 args`) {
		t.Errorf("two types for one arg: %v", err)
	}
}
