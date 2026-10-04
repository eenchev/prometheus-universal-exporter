//go:build !select_request_types || request_type_localfile

package exporter

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The fixtures of testdata/csv read by localfile collectors, through /probe
// (see csvfixtures_helpers_test.go): one file, by the collector's path and
// by the probe's target, and a directory of them.

const csvFileConfig = `
collectors:
  # One file, named by the collector.
  - name: hosts
    request:
      type: localfile
      root: ROOT
      path: hosts-trailing-delimiter.csv
    transform:
      type: csv
    metrics:
      - name: host_cpu_percent
        expression: cpu_percent
        labels: &host
          - name: host
            expression: host
      - name: host_memory_percent
        expression: memory_percent
        labels: *host
      - name: host_disk_percent
        expression: disk_percent
        labels: *host

  # One file, named by the probe's target.
  - name: report
    request:
      type: localfile
      root: ROOT
    transform:
      type: csv
    metrics: &jobs
      - name: job_duration_seconds
        expression: duration_seconds
        labels:
          - name: job
            expression: job
          - name: state
            expression: state
      - name: job_records
        expression: records
        labels:
          - name: job
            expression: job

  # A log without a header, its lines ended in three ways.
  - name: scale
    request:
      type: localfile
      root: ROOT
      path: scale-mixed-line-ends.txt
    response:
      csv:
        delimiter: ";"
        header: false
        trim_space: true
    transform:
      type: csv
    metrics:
      - name: scale_weight_kilograms
        expression: "3"
        labels:
          - name: scale
            expression: "2"
          - name: at
            expression: "1"

  # A script, which reads what the decoder gives it: with the decoder left
  # to each file, that is rows only for a file named .csv.
  - name: script_auto
    request:
      type: localfile
      root: ROOT
    decoder:
      type: auto
    transform:
      type: python
      script: &rows |
        rows = isinstance(data, list)
        metric(name="csv_data_items", type="gauge",
               value=len(data) if rows else len(data.splitlines()),
               labels={"data": "rows" if rows else type(data).__name__})
    metrics: []
  - name: script_tabs
    request:
      type: localfile
      root: ROOT
    decoder:
      type: csv
    response:
      csv:
        delimiter: "\t"
    transform:
      type: python
      script: *rows
    metrics: []

  # A directory of reports of one shape.
  - name: reports
    request:
      type: localfile
      root: REPORTS
      files: ["jobs-*.csv"]
    transform:
      type: csv
    metrics: *jobs

  # A directory of lists in three encodings: the two with a byte order mark
  # are read by it, the one without by response.charset.
  - name: lists
    request:
      type: localfile
      root: LISTS
      files: ["*.csv"]
    response:
      charset: windows-1251
    transform:
      type: csv
    metrics:
      - name: city_temperature_celsius
        expression: température
        required: false
        labels:
          - name: city
            expression: città
      - name: city_temperature_celsius
        expression: температура
        required: false
        labels:
          - name: city
            expression: град
`

// csvFileServer serves csvFileConfig with testdata/csv as the root of the
// collectors that read one file, and a directory of its own, holding copies
// of the fixtures named, modified a minute apart from 2026-10-03T09:00:00Z
// in the order given, for each of the two that read a directory.
func csvFileServer(t *testing.T, reports, lists []string) *Server {
	t.Helper()
	root, err := filepath.Abs("../../testdata/csv")
	if err != nil {
		t.Fatal(err)
	}
	directory := func(names []string) string {
		dir := t.TempDir()
		for i, name := range names {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, csvFixture(t, name), 0o600); err != nil {
				t.Fatal(err)
			}
			at := time.Unix(1791018000, 0).Add(time.Duration(i) * time.Minute)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	document := strings.NewReplacer("ROOT", root, "REPORTS", directory(reports), "LISTS", directory(lists)).Replace(csvFileConfig)
	return csvFixtureServer(t, document)
}

// probeCSVFile probes a localfile collector and returns the body of its
// answer in the text format.
func probeCSVFile(t *testing.T, server *Server, query string) string {
	t.Helper()
	response := probeOnce(t, server, "/probe?"+query, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return response.Body.String()
}

// A localfile collector reads a CSV file as an http one reads a response:
// the file its path names, the delimiter ending each of its lines leaving
// nothing behind, and the file a probe's target names, one without a final
// line end as one with, rows shorter than the header missing their values,
// which is logged once per rule with the number of rows.
func TestCSVFixtureFilesAreReadByPathAndByTarget(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := csvFileServer(t, nil, nil)

	answersSeries(t, probeCSVFile(t, server, "collector=hosts"), []string{
		`host_cpu_percent{host="web01"} 72`,
		`host_cpu_percent{host="web02"} 31`,
		`host_cpu_percent{host="web03"} 5`,
		`host_cpu_percent{host="db01"} 88`,
		`host_cpu_percent{host="db02"} 64`,
		`host_cpu_percent{host="cache01"} 12`,
		`host_cpu_percent{host="batch01"} 0`,
		`host_cpu_percent{host="batch02"} 97`,
		`host_cpu_percent{host="ingress01"} 23`,
		`host_cpu_percent{host="ingress02"} 27`,
		`host_memory_percent{host="web01"} 61`,
		`host_memory_percent{host="web02"} 48`,
		`host_memory_percent{host="web03"} 12`,
		`host_memory_percent{host="db01"} 93`,
		`host_memory_percent{host="db02"} 71`,
		`host_memory_percent{host="cache01"} 84`,
		`host_memory_percent{host="batch01"} 4`,
		`host_memory_percent{host="batch02"} 55`,
		`host_memory_percent{host="ingress01"} 30`,
		`host_memory_percent{host="ingress02"} 31`,
		`host_disk_percent{host="web01"} 40`,
		`host_disk_percent{host="web02"} 38`,
		`host_disk_percent{host="web03"} 11`,
		`host_disk_percent{host="db01"} 79`,
		`host_disk_percent{host="db02"} 81`,
		`host_disk_percent{host="cache01"} 5`,
		`host_disk_percent{host="batch01"} 22`,
		`host_disk_percent{host="batch02"} 60`,
		`host_disk_percent{host="ingress01"} 14`,
		`host_disk_percent{host="ingress02"} 14`,
	}, []string{"# TYPE host_cpu_percent gauge", "# TYPE host_memory_percent gauge", "# TYPE host_disk_percent gauge"})
	types := []string{"# TYPE job_duration_seconds gauge", "# TYPE job_records gauge"}
	answersSeries(t, probeCSVFile(t, server, "collector=report&target=jobs-no-final-newline.csv"), []string{
		`job_duration_seconds{job="backup-db",state="ok"} 412.5`,
		`job_duration_seconds{job="backup-files",state="ok"} 1290`,
		`job_duration_seconds{job="rotate-logs",state="ok"} 3.25`,
		`job_duration_seconds{job="reindex",state="failed"} 7.5`,
		`job_duration_seconds{job="sync-ldap",state="ok"} 12`,
		`job_duration_seconds{job="export-billing",state="ok"} 96.75`,
		`job_duration_seconds{job="vacuum",state="ok"} 48`,
		`job_duration_seconds{job="send-reports",state="ok"} 31`,
		`job_duration_seconds{job="prune-cache",state="ok"} 0.5`,
		`job_duration_seconds{job="renew-certs",state="skipped"} 0`,
		`job_records{job="backup-db"} 18250`,
		`job_records{job="backup-files"} 96412`,
		`job_records{job="rotate-logs"} 14`,
		`job_records{job="reindex"} 0`,
		`job_records{job="sync-ldap"} 830`,
		`job_records{job="export-billing"} 5120`,
		`job_records{job="vacuum"} 0`,
		`job_records{job="send-reports"} 64`,
		`job_records{job="prune-cache"} 221`,
		`job_records{job="renew-certs"} 0`,
	}, types)
	loggedOnly(t, logs)

	answersSeries(t, probeCSVFile(t, server, "collector=report&target=jobs-short-rows.csv"), []string{
		`job_duration_seconds{job="backup-db",state="ok"} 412.5`,
		`job_duration_seconds{job="backup-files",state="ok"} 1290`,
		`job_duration_seconds{job="rotate-logs",state="ok"} 3.25`,
		`job_duration_seconds{job="sync-ldap",state="ok"} 12`,
		`job_duration_seconds{job="export-billing",state="ok"} 96.75`,
		`job_duration_seconds{job="vacuum",state="ok"} 48`,
		`job_duration_seconds{job="prune-cache",state="ok"} 0.5`,
		`job_records{job="backup-db"} 18250`,
		`job_records{job="rotate-logs"} 14`,
		`job_records{job="sync-ldap"} 830`,
		`job_records{job="export-billing"} 5120`,
		`job_records{job="vacuum"} 0`,
		`job_records{job="prune-cache"} 221`,
	}, types)
	loggedOnly(t, logs,
		`WARN report job_duration_seconds: 3 failed: CSV column "duration_seconds" is empty in row 4`,
		`WARN report job_records: 4 failed: CSV column "records" is empty in row 2`,
	)
}

// A file whose lines end with a carriage return alone, with CRLF and with a
// line feed is read by a localfile collector as a response is: a series of
// each line that holds a value, and the line without one logged by its row.
// The same report with a carriage return after every line, named by the
// probe's target, is read as its rows by rules that find none of their
// columns in it, which the log says of each with the columns the file has.
func TestCSVFixtureFilesWithLinesEndedByACarriageReturnAreRead(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := csvFileServer(t, nil, nil)

	answersSeries(t, probeCSVFile(t, server, "collector=scale"), []string{
		`scale_weight_kilograms{at="2026-10-03T09:00:00Z",scale="A1"} 12.5`,
		`scale_weight_kilograms{at="2026-10-03T09:01:00Z",scale="A1"} 12.75`,
		`scale_weight_kilograms{at="2026-10-03T09:02:00Z",scale="A2"} 7.25`,
		`scale_weight_kilograms{at="2026-10-03T09:03:00Z",scale="A2"} 7.5`,
		`scale_weight_kilograms{at="2026-10-03T09:05:00Z",scale="B7"} 0.5`,
	}, []string{"# TYPE scale_weight_kilograms gauge"})
	loggedOnly(t, logs, `WARN scale scale_weight_kilograms: 1 failed: CSV column "3" is empty in row 5`)
	logs.Reset()

	if body := probeCSVFile(t, server, "collector=report&target=volumes-cr.csv"); body != "" {
		t.Errorf("a report without the rules' columns answered\n%s", body)
	}
	const has = `whose columns are "free_gib", "note", "pool", "used_percent", "volume"; column names are matched exactly`
	loggedOnly(t, logs,
		`WARN report job_duration_seconds: 5 failed: CSV column "duration_seconds" is not in the response, `+has,
		`WARN report job_records: 5 failed: CSV column "records" is not in the response, `+has,
	)
}

// With decoder.type auto a file is decoded by its extension: a script is
// given the rows of a file named .csv, and the text of a CSV file named
// .tsv or .txt, which no extension says is CSV. With decoder.type csv, and
// the delimiter the file has, it is given the rows of that one too.
func TestCSVFixtureFilesAScriptIsGivenRowsByTheExtensionOrTheDecoder(t *testing.T) {
	requirePython(t)
	logs := testutil.CaptureLogs(t)
	server := csvFileServer(t, nil, nil)
	types := []string{"# TYPE csv_data_items gauge"}
	for query, want := range map[string]string{
		// A header and two rows.
		"collector=script_auto&target=status.csv": `csv_data_items{data="rows"} 2`,
		// A header, twelve rows, and for psql's result a footer.
		"collector=script_auto&target=sensors.tsv":     `csv_data_items{data="str"} 13`,
		"collector=script_auto&target=queues-pipe.txt": `csv_data_items{data="str"} 14`,
		"collector=script_tabs&target=sensors.tsv":     `csv_data_items{data="rows"} 12`,
	} {
		answersSeries(t, probeCSVFile(t, server, query), []string{want}, types)
	}
	loggedOnly(t, logs)
}

// A directory of reports of one shape, read with request.files: every
// file's rows become series labelled with its name, each family's together
// under one TYPE line, beside each file's modification time and scrape
// error and the count of files skipped. The three ways a writer leaves
// such a report are read as each alone is: blank lines are no rows, the
// last line needs no line end, and what a line of blanks and rows shorter
// than the header are missing is logged for each file, with its name, while
// none of them fails. A report with a row longer than its header fails its
// decode, as it does read alone: it is left out, with a scrape error of 1
// and a warning naming the file, the line and the column, and the others
// are answered.
func TestCSVFixtureFilesADirectoryOfReportsIsReadFileByFile(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := csvFileServer(t, []string{"jobs-blank-lines.csv", "jobs-no-final-newline.csv", "jobs-short-rows.csv", "numbers.csv", "jobs-unquoted-comma.csv"}, nil)

	body := probeCSVFile(t, server, "collector=reports")
	answersSeries(t, body, []string{
		`job_duration_seconds{file="jobs-blank-lines.csv",job="backup-db",state="ok"} 412.5`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="backup-files",state="ok"} 1290`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="rotate-logs",state="ok"} 3.25`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="reindex",state="failed"} 7.5`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="sync-ldap",state="ok"} 12`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="export-billing",state="ok"} 96.75`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="vacuum",state="ok"} 48`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="send-reports",state="ok"} 31`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="prune-cache",state="ok"} 0.5`,
		`job_duration_seconds{file="jobs-blank-lines.csv",job="renew-certs",state="skipped"} 0`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="backup-db",state="ok"} 412.5`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="backup-files",state="ok"} 1290`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="rotate-logs",state="ok"} 3.25`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="reindex",state="failed"} 7.5`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="sync-ldap",state="ok"} 12`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="export-billing",state="ok"} 96.75`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="vacuum",state="ok"} 48`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="send-reports",state="ok"} 31`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="prune-cache",state="ok"} 0.5`,
		`job_duration_seconds{file="jobs-no-final-newline.csv",job="renew-certs",state="skipped"} 0`,
		`job_duration_seconds{file="jobs-short-rows.csv",job="backup-db",state="ok"} 412.5`,
		`job_duration_seconds{file="jobs-short-rows.csv",job="backup-files",state="ok"} 1290`,
		`job_duration_seconds{file="jobs-short-rows.csv",job="rotate-logs",state="ok"} 3.25`,
		`job_duration_seconds{file="jobs-short-rows.csv",job="sync-ldap",state="ok"} 12`,
		`job_duration_seconds{file="jobs-short-rows.csv",job="export-billing",state="ok"} 96.75`,
		`job_duration_seconds{file="jobs-short-rows.csv",job="vacuum",state="ok"} 48`,
		`job_duration_seconds{file="jobs-short-rows.csv",job="prune-cache",state="ok"} 0.5`,
		`job_records{file="jobs-blank-lines.csv",job="backup-db"} 18250`,
		`job_records{file="jobs-blank-lines.csv",job="backup-files"} 96412`,
		`job_records{file="jobs-blank-lines.csv",job="rotate-logs"} 14`,
		`job_records{file="jobs-blank-lines.csv",job="reindex"} 0`,
		`job_records{file="jobs-blank-lines.csv",job="sync-ldap"} 830`,
		`job_records{file="jobs-blank-lines.csv",job="export-billing"} 5120`,
		`job_records{file="jobs-blank-lines.csv",job="vacuum"} 0`,
		`job_records{file="jobs-blank-lines.csv",job="send-reports"} 64`,
		`job_records{file="jobs-blank-lines.csv",job="prune-cache"} 221`,
		`job_records{file="jobs-blank-lines.csv",job="renew-certs"} 0`,
		`job_records{file="jobs-no-final-newline.csv",job="backup-db"} 18250`,
		`job_records{file="jobs-no-final-newline.csv",job="backup-files"} 96412`,
		`job_records{file="jobs-no-final-newline.csv",job="rotate-logs"} 14`,
		`job_records{file="jobs-no-final-newline.csv",job="reindex"} 0`,
		`job_records{file="jobs-no-final-newline.csv",job="sync-ldap"} 830`,
		`job_records{file="jobs-no-final-newline.csv",job="export-billing"} 5120`,
		`job_records{file="jobs-no-final-newline.csv",job="vacuum"} 0`,
		`job_records{file="jobs-no-final-newline.csv",job="send-reports"} 64`,
		`job_records{file="jobs-no-final-newline.csv",job="prune-cache"} 221`,
		`job_records{file="jobs-no-final-newline.csv",job="renew-certs"} 0`,
		`job_records{file="jobs-short-rows.csv",job="backup-db"} 18250`,
		`job_records{file="jobs-short-rows.csv",job="rotate-logs"} 14`,
		`job_records{file="jobs-short-rows.csv",job="sync-ldap"} 830`,
		`job_records{file="jobs-short-rows.csv",job="export-billing"} 5120`,
		`job_records{file="jobs-short-rows.csv",job="vacuum"} 0`,
		`job_records{file="jobs-short-rows.csv",job="prune-cache"} 221`,
		// 2026-10-03T09:00:00Z, and a minute and two after it.
		`localfile_mtime_seconds{file="jobs-blank-lines.csv"} 1.791018e+09`,
		`localfile_mtime_seconds{file="jobs-no-final-newline.csv"} 1.79101806e+09`,
		`localfile_mtime_seconds{file="jobs-short-rows.csv"} 1.79101812e+09`,
		// The file after numbers.csv, which no pattern matches.
		`localfile_mtime_seconds{file="jobs-unquoted-comma.csv"} 1.79101824e+09`,
		`localfile_scrape_error{file="jobs-blank-lines.csv"} 0`,
		`localfile_scrape_error{file="jobs-no-final-newline.csv"} 0`,
		`localfile_scrape_error{file="jobs-short-rows.csv"} 0`,
		`localfile_scrape_error{file="jobs-unquoted-comma.csv"} 1`,
		`localfile_files_skipped 0`,
	}, []string{
		"# TYPE job_duration_seconds gauge", "# TYPE job_records gauge",
		"# TYPE localfile_mtime_seconds gauge", "# TYPE localfile_scrape_error gauge", "# TYPE localfile_files_skipped gauge",
	})
	contiguousFamilies(t, body)
	if strings.Contains(body, "numbers.csv") {
		t.Errorf("a file the patterns do not match was read:\n%s", body)
	}
	if err := parseExposition([]byte(body)); err != nil {
		t.Errorf("the answer does not parse: %v", err)
	}
	failures, others := ruleFailureLogs(t, logs)
	sameLines(t, "logged rule failures", failures, []string{
		`WARN reports job_duration_seconds in jobs-blank-lines.csv: 1 failed: CSV column "duration_seconds" is empty in row 5`,
		`WARN reports job_records in jobs-blank-lines.csv: 1 failed: CSV column "records" is empty in row 5`,
		`WARN reports job_duration_seconds in jobs-short-rows.csv: 3 failed: CSV column "duration_seconds" is empty in row 4`,
		`WARN reports job_records in jobs-short-rows.csv: 4 failed: CSV column "records" is empty in row 2`,
	})
	if len(others) != 1 || !strings.Contains(others[0], `"level":"WARN"`) || !strings.Contains(others[0], `"file":"jobs-unquoted-comma.csv","stage":"decode"`) ||
		!strings.Contains(others[0], "CSV line 5 has a value in column 5, which the header does not name") {
		t.Errorf("also logged %v, want the one warning of the file with a row longer than its header", others)
	}
}

// A directory of lists in three encodings is read by one collector: a file
// declares no encoding, so the ones in UTF-8 and UTF-16 are read by their
// byte order marks, and the one in windows-1251 by the collector's
// response.charset, which the byte order marks of the others come before.
// The two rules of one metric each read the header one kind of list has,
// and, not required, make nothing of the other kind, without a word.
func TestCSVFixtureFilesADirectoryInSeveralEncodingsIsReadByMarkAndCharset(t *testing.T) {
	logs := testutil.CaptureLogs(t)
	server := csvFileServer(t, nil, []string{"cities-utf16le-bom.csv", "cities-utf8-bom.csv", "oblasti-windows-1251.csv"})

	body := probeCSVFile(t, server, "collector=lists")
	answersSeries(t, body, []string{
		`city_temperature_celsius{city="София",file="cities-utf16le-bom.csv"} 14.5`,
		`city_temperature_celsius{city="Пловдив",file="cities-utf16le-bom.csv"} 17`,
		`city_temperature_celsius{city="Zürich",file="cities-utf16le-bom.csv"} 9.25`,
		`city_temperature_celsius{city="São Paulo",file="cities-utf16le-bom.csv"} 23`,
		`city_temperature_celsius{city="Kraków",file="cities-utf16le-bom.csv"} 7.5`,
		`city_temperature_celsius{city="Αθήνα",file="cities-utf16le-bom.csv"} 21`,
		`city_temperature_celsius{city="東京",file="cities-utf16le-bom.csv"} 19`,
		`city_temperature_celsius{city="北京",file="cities-utf16le-bom.csv"} 12`,
		`city_temperature_celsius{city="Reykjavík",file="cities-utf16le-bom.csv"} 3`,
		`city_temperature_celsius{city="Đà Nẵng",file="cities-utf16le-bom.csv"} 29.5`,
		`city_temperature_celsius{city="İstanbul",file="cities-utf16le-bom.csv"} 16`,
		`city_temperature_celsius{city="Québec, QC",file="cities-utf16le-bom.csv"} 5`,
		`city_temperature_celsius{city="София",file="cities-utf8-bom.csv"} 14.5`,
		`city_temperature_celsius{city="Пловдив",file="cities-utf8-bom.csv"} 17`,
		`city_temperature_celsius{city="Zürich",file="cities-utf8-bom.csv"} 9.25`,
		`city_temperature_celsius{city="São Paulo",file="cities-utf8-bom.csv"} 23`,
		`city_temperature_celsius{city="Kraków",file="cities-utf8-bom.csv"} 7.5`,
		`city_temperature_celsius{city="Αθήνα",file="cities-utf8-bom.csv"} 21`,
		`city_temperature_celsius{city="東京",file="cities-utf8-bom.csv"} 19`,
		`city_temperature_celsius{city="北京",file="cities-utf8-bom.csv"} 12`,
		`city_temperature_celsius{city="Reykjavík",file="cities-utf8-bom.csv"} 3`,
		`city_temperature_celsius{city="Đà Nẵng",file="cities-utf8-bom.csv"} 29.5`,
		`city_temperature_celsius{city="İstanbul",file="cities-utf8-bom.csv"} 16`,
		`city_temperature_celsius{city="Québec, QC",file="cities-utf8-bom.csv"} 5`,
		`city_temperature_celsius{city="София",file="oblasti-windows-1251.csv"} 14.5`,
		`city_temperature_celsius{city="Пловдив",file="oblasti-windows-1251.csv"} 17`,
		`city_temperature_celsius{city="Варна",file="oblasti-windows-1251.csv"} 18.25`,
		`city_temperature_celsius{city="Бургас",file="oblasti-windows-1251.csv"} 19`,
		`city_temperature_celsius{city="Русе",file="oblasti-windows-1251.csv"} 13`,
		`city_temperature_celsius{city="Стара Загора",file="oblasti-windows-1251.csv"} 16.5`,
		`city_temperature_celsius{city="Плевен",file="oblasti-windows-1251.csv"} 12`,
		`city_temperature_celsius{city="Велико Търново",file="oblasti-windows-1251.csv"} 11.75`,
		`city_temperature_celsius{city="Благоевград",file="oblasti-windows-1251.csv"} 15`,
		`city_temperature_celsius{city="Банско, ски зона",file="oblasti-windows-1251.csv"} -2.5`,
		`localfile_mtime_seconds{file="cities-utf16le-bom.csv"} 1.791018e+09`,
		`localfile_mtime_seconds{file="cities-utf8-bom.csv"} 1.79101806e+09`,
		`localfile_mtime_seconds{file="oblasti-windows-1251.csv"} 1.79101812e+09`,
		`localfile_scrape_error{file="cities-utf16le-bom.csv"} 0`,
		`localfile_scrape_error{file="cities-utf8-bom.csv"} 0`,
		`localfile_scrape_error{file="oblasti-windows-1251.csv"} 0`,
		`localfile_files_skipped 0`,
	}, []string{"# TYPE city_temperature_celsius gauge", "# TYPE localfile_mtime_seconds gauge", "# TYPE localfile_scrape_error gauge", "# TYPE localfile_files_skipped gauge"})
	contiguousFamilies(t, body)
	loggedOnly(t, logs)
}
