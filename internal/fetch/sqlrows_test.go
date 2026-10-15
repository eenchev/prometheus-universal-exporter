//go:build !select_request_types || request_type_sql

package fetch

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A time of day is the seconds since midnight, with the fraction, as its
// clock reads where it was written, in each form a driver gives it: a
// time.Time of the year 1 (sqlserver's time, snowflake's TIME) or the year 0
// in the session's zone (vertica's TIME and TIMETZ), text (postgres's TIME
// and TIMETZ, mysql's TIME, negative or past 24 hours too) and a
// time.Duration (clickhouse's Time and Time64). A date and a timestamp
// stay Unix times, and text of a TIME column that is no time stays text.
func TestASQLTimeOfDayIsItsSecondsSinceMidnight(t *testing.T) {
	plus2 := time.FixedZone("", 2*3600)
	for _, tc := range []struct {
		value  any
		dbType string
		want   any
	}{
		{time.Date(1, 1, 1, 13, 45, 30, 500_000_000, time.UTC), "TIME", 49530.5},
		{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), "TIME", 0.0},
		{time.Date(0, 1, 1, 23, 59, 59, 0, plus2), "TIME", 86399.0},
		{time.Date(0, 1, 1, 1, 0, 0, 0, plus2), "TIMETZ", 3600.0},
		{"13:45:30.25", "TIME", 49530.25},
		{"13:45:30+02", "TIMETZ", 49530.0},
		{"08:00:00-05:30", "TIMETZ", 28800.0},
		{[]byte("838:59:59"), "TIME", 3020399.0},
		{[]byte("-01:00:00"), "TIME", -3600.0},
		{"12:30", "TIME", 45000.0},
		{90*time.Minute + 500*time.Millisecond, "Time", 5400.5},
		{90 * time.Minute, "Nullable(Time64(3))", 5400.0},
		{"later", "TIME", "later"},
		{"25:61:00", "TIME", "25:61:00"},
		{time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "DATE", 1704153600.0},
		{time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), "DATETIME", 1704164645.0},
		{time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), "TIMESTAMP", 1704164645.0},
		{"13:45:30", "VARCHAR", "13:45:30"},
	} {
		value, present, _, err := sqlValue(tc.value, tc.dbType)
		if err != nil || !present || !reflect.DeepEqual(value, tc.want) {
			t.Errorf("%#v %s: %#v %v %v, want %#v", tc.value, tc.dbType, value, present, err, tc.want)
		}
	}
}

// Every value of a column that is no time of day, and every value but a
// time.Time, text and a time.Duration of one that is, is read as it was:
// the same value, presence, size and error, over a table of the values
// drivers give and the column types they name.
func TestASQLValueIsReadAsItWasButForATimeOfDay(t *testing.T) {
	values := []any{nil, true, false, int64(-7), int32(3), int16(2), int8(1), 9, uint64(1 << 63), uint32(4), uint16(5), uint8(6), uint(7),
		1.5, float32(0.25), time.Unix(1_700_000_000, 5).UTC(), time.Date(1, 1, 1, 13, 0, 0, 0, time.UTC), new(big.Int).SetInt64(42),
		"12:30:00", "3.25", " 7 ", "n/a", "", []byte("12:30"), []byte{0xff}, []byte("1e3"), json.Number("8"), struct{}{}}
	types := []string{"", "TEXT", "VARCHAR", "NUMERIC", "DECIMAL(10,2)", "INT8", "Nullable(UInt64)", "DATE", "DATETIME", "DATETIME2", "TIMESTAMP", "TIMESTAMPTZ", "INTERVAL", "DateTime64(3)", "TIME", "TIMETZ", "Time", "Time64(3)", "time(3)"}
	for _, dbType := range types {
		for _, v := range values {
			value, present, size, err := sqlValue(v, dbType)
			want, wantPresent, wantSize, wantErr := sqlValueAsItWas(v, dbType)
			if sqlTimeOfDayType(dbType) {
				switch v.(type) {
				case time.Time, string, []byte:
					continue
				}
			}
			if !reflect.DeepEqual(value, want) || present != wantPresent || size != wantSize || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Errorf("%#v %s: %#v %v %d %v, as it was %#v %v %d %v", v, dbType, value, present, size, err, want, wantPresent, wantSize, wantErr)
			}
		}
	}
}

// sqlValueAsItWas and sqlTextAsItWas are sqlValue and sqlText before a time
// of day was its seconds since midnight.
func sqlValueAsItWas(v any, dbType string) (value any, present bool, size int64, err error) {
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
		return float64(x.Unix()) + float64(x.Nanosecond())/1e9, true, 8, nil
	case *big.Int:
		return model.Normalize(json.Number(x.String())), true, int64(len(x.String())), nil
	case string:
		return sqlTextAsItWas(x, dbType), true, int64(len(x)), nil
	case []byte:
		if !utf8.Valid(x) {
			return nil, false, 0, fmt.Errorf("holds binary data that is no UTF-8 text, of the column type %s; return it as text in the statement, such as its hexadecimal digits", sqlTypeName(dbType))
		}
		return sqlTextAsItWas(string(x), dbType), true, int64(len(x)), nil
	case fmt.Stringer:
		// A driver's own type of value, such as a decimal or a UUID, by
		// the text it writes.
		text := x.String()
		return sqlTextAsItWas(text, dbType), true, int64(len(text)), nil
	}
	return nil, false, 0, fmt.Errorf("holds a value of the Go type %T, of the column type %s, which the exporter does not read; convert it in the statement to a number or text", v, sqlTypeName(dbType))
}
func sqlTextAsItWas(text, dbType string) any {
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
