package fetch

import (
	"slices"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The database drivers of the sql request type, by the names request.driver
// gives them (sqldriver.go). They are named in every build, so that the
// schema and the collectors page can name them whatever the build carries.
const (
	SQLDriverMySQL      = "mysql"
	SQLDriverPostgres   = "postgres"
	SQLDriverSQLServer  = "sqlserver"
	SQLDriverOracle     = "oracle"
	SQLDriverClickHouse = "clickhouse"
	SQLDriverSnowflake  = "snowflake"
	SQLDriverVertica    = "vertica"
)

// sqlDriverNames is the database drivers the sql request type was built with,
// each added by its own file's init (sqldriver_<name>.go): none in a build
// without the type, which has no driver to name.
var sqlDriverNames []string

// BuiltSQLDrivers lists the database drivers this binary's sql request type
// can use, sorted: what --version, the startup log and
// http_exporter_build_info report. Empty when the build has no sql type.
func BuiltSQLDrivers() []string {
	names := slices.Clone(sqlDriverNames)
	slices.Sort(names)
	return names
}

// SQLNamedResults reports whether a sql collector runs several statements
// (request.statements), whose document is an object of each one's rows by
// its name, which only jq, yq and python read, not the list of rows a csv
// transform reads by column. False in a build without the sql type.
func SQLNamedResults(c *model.Collector) bool {
	return sqlNamedResults != nil && sqlNamedResults(c)
}

// sqlNamedResults is SQLNamedResults of the sql type, set by its init.
var sqlNamedResults func(c *model.Collector) bool
