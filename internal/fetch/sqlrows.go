//go:build !select_request_types || request_type_sql

package fetch

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The rows a statement returns are handed to the transforms as JSON values
// are, a list of objects, one a row, of its columns' values by name, so a jq
// rule reads them as it reads a JSON answer's and a csv rule reads a column
// by name, one series a row:
//
//   - integers and floating-point numbers are numbers; a decimal, which
//     drivers give as text, is the number it writes, or its text when it
//     writes none a float can hold;
//   - a boolean is 1 or 0;
//   - a date or a timestamp is its Unix time in seconds, with the fraction;
//     a time of day, of a TIME column (TIMETZ, ClickHouse's Time and
//     Time64), the seconds since midnight, with the fraction, as its clock
//     reads where it was written: sqlserver, vertica and snowflake give it
//     as a time.Time of the year 1 or 0, postgres and mysql as text such as
//     12:30:00 (mysql's, a duration, may be negative or past 24 hours), and
//     clickhouse as a time.Duration, which is its seconds whatever the
//     column;
//   - text is text, and binary data is the text it holds when it is UTF-8,
//     and refused, naming its column, when it is not;
//   - NULL leaves the column out of the row: a jq rule finds no key, and a
//     csv rule reading the column has no value in that row.
//
// A column without a name, or with the name of another, is refused: its
// values could not be read by name.

// readSQLRows reads the rows of rs, at most maxRows of them and, of their
// values with their columns' names, at most maxBytes: each value its size
// (sqlValue) and its column's name's length, a NULL nothing.
func readSQLRows(rs *sql.Rows, maxRows int, maxBytes int64) (*sqlResult, error) {
	columns, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	types := make([]string, len(columns))
	if columnTypes, err := rs.ColumnTypes(); err == nil {
		for i, t := range columnTypes {
			types[i] = t.DatabaseTypeName()
		}
	}
	place := make(map[string]int, len(columns))
	for i, name := range columns {
		if name == "" {
			return nil, fmt.Errorf("column %d of the statement's result has no name; name it with AS, as the rules read a column by its name", i+1)
		}
		if first, seen := place[name]; seen {
			return nil, model.Errorf("the statement's result names column %s twice, as columns %d and %d; give one of them another name with AS", model.Quoted(name), first+1, i+1)
		}
		place[name] = i
	}
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	rows := []any{}
	var size int64
	for rs.Next() {
		if len(rows) == maxRows {
			return nil, model.MarkError(fmt.Errorf("the statement returned more than request.max_rows, %d, rows; return fewer, with WHERE, GROUP BY or LIMIT, or raise max_rows", maxRows), model.ErrLimitExceeded)
		}
		if err := rs.Scan(targets...); err != nil {
			return nil, model.Errorf("row %d: %w", model.Position(len(rows)+1), err)
		}
		row := make(map[string]any, len(columns))
		for i, v := range values {
			value, present, n, err := sqlValue(v, types[i])
			values[i] = nil
			if err != nil {
				return nil, model.Errorf("row %d column %s: %w", model.Position(len(rows)+1), model.Quoted(columns[i]), err)
			}
			if present {
				// The column's name is a key of every row the column has a
				// value in, as the transforms read the rows and as their
				// JSON writes them.
				n += int64(len(columns[i]))
			}
			if size += n; size > maxBytes {
				return nil, model.MarkError(model.Errorf("the statement's rows hold more than the response limit of %d bytes of values and column names; return fewer rows or columns, or raise request.max_response_bytes or limits.max_response_bytes", model.Size(maxBytes)), model.ErrLimitExceeded)
			}
			if present {
				row[columns[i]] = value
			}
		}
		rows = append(rows, row)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	shown, err := json.Marshal(sqlShownRows(rows))
	if err != nil {
		return nil, err
	}
	return &sqlResult{rows: rows, columns: columns, shown: shown}, nil
}

// sqlValue is a value a driver read as the transforms read it, whether the
// column has one (it is not NULL), and its size towards the response limit:
// the length of text and binary data, and eight bytes for anything else.
// dbType is the column's type as the driver names it, which says whether
// text is a decimal.
func sqlValue(v any, dbType string) (value any, present bool, size int64, err error) {
	switch x := v.(type) {
	case nil:
		return nil, false, 0, nil
	case bool:
		if x {
			return 1, true, 8, nil
		}
		return 0, true, 8, nil
	case int64:
		return int(x), true, 8, nil
	case int32:
		return int(x), true, 8, nil
	case int16:
		return int(x), true, 8, nil
	case int8:
		return int(x), true, 8, nil
	case int:
		return x, true, 8, nil
	case uint64:
		return model.Normalize(x), true, 8, nil
	case uint32:
		return int(x), true, 8, nil
	case uint16:
		return int(x), true, 8, nil
	case uint8:
		return int(x), true, 8, nil
	case uint:
		return model.Normalize(x), true, 8, nil
	case float64:
		return x, true, 8, nil
	case float32:
		return float64(x), true, 8, nil
	case time.Time:
		if sqlTimeOfDayType(dbType) {
			hour, minute, second := x.Clock()
			return float64(hour*3600+minute*60+second) + float64(x.Nanosecond())/1e9, true, 8, nil
		}
		return float64(x.Unix()) + float64(x.Nanosecond())/1e9, true, 8, nil
	case time.Duration:
		return x.Seconds(), true, 8, nil
	case *big.Int:
		return model.Normalize(json.Number(x.String())), true, int64(len(x.String())), nil
	case string:
		return sqlText(x, dbType), true, int64(len(x)), nil
	case []byte:
		if !utf8.Valid(x) {
			return nil, false, 0, fmt.Errorf("holds binary data that is no UTF-8 text, of the column type %s; return it as text in the statement, such as its hexadecimal digits", sqlTypeName(dbType))
		}
		return sqlText(string(x), dbType), true, int64(len(x)), nil
	case fmt.Stringer:
		// A driver's own type of value, such as a decimal or a UUID, by
		// the text it writes.
		text := x.String()
		return sqlText(text, dbType), true, int64(len(text)), nil
	}
	return nil, false, 0, fmt.Errorf("holds a value of the Go type %T, of the column type %s, which the exporter does not read; convert it in the statement to a number or text", v, sqlTypeName(dbType))
}

// sqlTypeName names a column's type for a message.
func sqlTypeName(dbType string) string {
	if dbType == "" {
		return "the driver does not name"
	}
	return dbType
}

// sqlText is text read from a column: the number it writes when the column
// is of a numeric type, as a decimal or a number a driver reads as text is,
// and the text itself otherwise.
func sqlText(text, dbType string) any {
	if sqlTimeOfDayType(dbType) {
		if seconds, ok := sqlTimeOfDay(text); ok {
			return seconds
		}
		return text
	}
	if !sqlNumericType(dbType) {
		return text
	}
	trimmed := strings.TrimSpace(text)
	if model.WholeNumberText(trimmed) {
		return model.Normalize(json.Number(trimmed))
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return f
	}
	return text
}

// sqlNumericTypes are the names of the numeric types of the databases, as
// their drivers name them, besides INT8 to INT256 and UINT8 to UINT256.
var sqlNumericTypes = map[string]bool{
	"INT": true, "INTEGER": true, "BIGINT": true, "SMALLINT": true, "TINYINT": true, "MEDIUMINT": true,
	"DECIMAL": true, "NUMERIC": true, "NUMBER": true, "DEC": true, "FLOAT": true, "DOUBLE": true,
	"REAL": true, "MONEY": true, "SMALLMONEY": true, "BINARY_FLOAT": true, "BINARY_DOUBLE": true,
	"FLOAT4": true, "FLOAT8": true, "FLOAT32": true, "FLOAT64": true, "DOUBLE PRECISION": true,
	"DECIMAL32": true, "DECIMAL64": true, "DECIMAL128": true, "DECIMAL256": true,
	// Snowflake's driver names NUMBER (and its INT and DECIMAL) FIXED.
	"FIXED": true, "DECFLOAT": true,
}

// sqlNumericType says a column's type, as its driver names it, is numeric:
// one of sqlNumericTypes, an INT or UINT with its width, unsigned or not,
// with its precision, or inside ClickHouse's Nullable or LowCardinality.
func sqlNumericType(dbType string) bool {
	name := strings.ToUpper(strings.TrimSpace(dbType))
	for _, wrapper := range []string{"NULLABLE(", "LOWCARDINALITY("} {
		if strings.HasPrefix(name, wrapper) {
			name = strings.TrimSuffix(strings.TrimPrefix(name, wrapper), ")")
		}
	}
	name, _, _ = strings.Cut(name, "(")
	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), " UNSIGNED"))
	if sqlNumericTypes[name] {
		return true
	}
	digits := strings.TrimPrefix(strings.TrimPrefix(name, "U"), "INT")
	if digits == name || strings.HasPrefix(name, "U") && !strings.HasPrefix(name, "UINT") {
		return false
	}
	_, err := strconv.Atoi(digits)
	return err == nil
}

// sqlTimeOfDayTypes are the names of the time-of-day types, as the drivers
// name them, without a precision.
var sqlTimeOfDayTypes = map[string]bool{
	"TIME": true, "TIMETZ": true, "TIME64": true, "TIME WITH TIME ZONE": true, "TIME WITHOUT TIME ZONE": true,
}

// sqlTimeOfDayType says a column's type, as its driver names it, is a time
// of day: one of sqlTimeOfDayTypes, with its precision or without, inside
// ClickHouse's Nullable or not.
func sqlTimeOfDayType(dbType string) bool {
	name := strings.ToUpper(strings.TrimSpace(dbType))
	if strings.HasPrefix(name, "NULLABLE(") {
		name = strings.TrimSuffix(strings.TrimPrefix(name, "NULLABLE("), ")")
	}
	name, _, _ = strings.Cut(name, "(")
	return sqlTimeOfDayTypes[strings.TrimSpace(name)]
}

// sqlTimeOfDay reads a time of day written as text, [-]hours:minutes, then
// :seconds with a fraction or without, and a zone after it or not (+02,
// -05:30), as the seconds since midnight; ok is false for any other text.
func sqlTimeOfDay(text string) (seconds float64, ok bool) {
	text = strings.TrimSpace(text)
	sign := 1.0
	if rest, negative := strings.CutPrefix(text, "-"); negative {
		sign, text = -1, rest
	}
	if zone := strings.IndexAny(text, "+-Zz"); zone > 0 {
		text = text[:zone]
	}
	parts := strings.Split(text, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	hours, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || parts[0] == "" {
		return 0, false
	}
	minutes, err := strconv.ParseUint(parts[1], 10, 8)
	if err != nil || len(parts[1]) != 2 || minutes > 59 {
		return 0, false
	}
	var secs float64
	if len(parts) == 3 {
		whole, fraction, _ := strings.Cut(parts[2], ".")
		if len(whole) != 2 || strings.ContainsAny(parts[2], "eE+-") {
			return 0, false
		}
		if secs, err = strconv.ParseFloat(parts[2], 64); err != nil || secs >= 60 || strings.Contains(parts[2], ".") && fraction == "" {
			return 0, false
		}
	}
	return sign * (float64(hours)*3600 + float64(minutes)*60 + secs), true
}

// sqlShownRows is rows as they are written as JSON for what shows an
// answer: a number JSON cannot hold, NaN or an infinity, as its text.
func sqlShownRows(rows []any) []any {
	finite := true
	for _, row := range rows {
		for _, v := range row.(map[string]any) {
			if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
				finite = false
			}
		}
	}
	if finite {
		return rows
	}
	shown := make([]any, len(rows))
	for i, row := range rows {
		m := make(map[string]any, len(row.(map[string]any)))
		for k, v := range row.(map[string]any) {
			if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
				v = strconv.FormatFloat(f, 'g', -1, 64)
			}
			m[k] = v
		}
		shown[i] = m
	}
	return shown
}
