//go:build !select_request_types || request_type_sql

package fetch

import (
	"strconv"
	"strings"
	"testing"
)

// sqlNumericTypesBefore and sqlNumericTypeBefore are sqlNumericTypes and
// sqlNumericType as they were before Snowflake's FIXED and DECFLOAT were
// added, the oracle of the test below.
var sqlNumericTypesBefore = map[string]bool{
	"INT": true, "INTEGER": true, "BIGINT": true, "SMALLINT": true, "TINYINT": true, "MEDIUMINT": true,
	"DECIMAL": true, "NUMERIC": true, "NUMBER": true, "DEC": true, "FLOAT": true, "DOUBLE": true,
	"REAL": true, "MONEY": true, "SMALLMONEY": true, "BINARY_FLOAT": true, "BINARY_DOUBLE": true,
	"FLOAT4": true, "FLOAT8": true, "FLOAT32": true, "FLOAT64": true, "DOUBLE PRECISION": true,
	"DECIMAL32": true, "DECIMAL64": true, "DECIMAL128": true, "DECIMAL256": true,
}

func sqlNumericTypeBefore(dbType string) bool {
	name := strings.ToUpper(strings.TrimSpace(dbType))
	for _, wrapper := range []string{"NULLABLE(", "LOWCARDINALITY("} {
		if strings.HasPrefix(name, wrapper) {
			name = strings.TrimSuffix(strings.TrimPrefix(name, wrapper), ")")
		}
	}
	name, _, _ = strings.Cut(name, "(")
	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), " UNSIGNED"))
	if sqlNumericTypesBefore[name] {
		return true
	}
	digits := strings.TrimPrefix(strings.TrimPrefix(name, "U"), "INT")
	if digits == name || strings.HasPrefix(name, "U") && !strings.HasPrefix(name, "UINT") {
		return false
	}
	_, err := strconv.Atoi(digits)
	return err == nil
}

// Snowflake's FIXED (its NUMBER) and DECFLOAT are numeric types, written
// however a type name may be, and every other type name is numeric or not
// as it was before: the type names of every driver, in every form the
// function reads, generated, against the function as it was.
func TestSnowflakeNumberTypesAreNumericAndNoOtherTypeChanged(t *testing.T) {
	names := []string{"FIXED", "DECFLOAT", "TEXT", "VARCHAR", "BOOL", "BOOLEAN", "DATE", "TIMESTAMP_NTZ", "TIMESTAMPTZ", "BYTEA", "UUID",
		"VARIANT", "OBJECT", "ARRAY", "BINARY", "TIME", "INT", "UINT", "INT0", "UINTX", "U8", "UNSIGNED", "FIX", "FIXEDX", "DECFLOATS", "", "INTERVAL"}
	for name := range sqlNumericTypesBefore {
		names = append(names, name)
	}
	for _, bits := range []int{8, 16, 32, 64, 128, 256} {
		names = append(names, "INT"+strconv.Itoa(bits), "UINT"+strconv.Itoa(bits))
	}
	var forms []string
	for _, name := range names {
		for _, form := range []string{"%s", "%s(38,0)", "%s UNSIGNED", "Nullable(%s)", "LowCardinality(%s)", "  %s  ", "lower"} {
			if form == "lower" {
				forms = append(forms, strings.ToLower(name))
				continue
			}
			forms = append(forms, strings.ReplaceAll(form, "%s", name))
		}
	}
	for _, dbType := range forms {
		got, before := sqlNumericType(dbType), sqlNumericTypeBefore(dbType)
		name := strings.ToUpper(strings.TrimSpace(dbType))
		snowflake := strings.Contains(name, "FIXED") && !strings.Contains(name, "FIXEDX") || strings.Contains(name, "DECFLOAT") && !strings.Contains(name, "DECFLOATS")
		switch {
		case snowflake && !got:
			t.Errorf("%q is not numeric", dbType)
		case !snowflake && got != before:
			t.Errorf("%q is numeric: %v, before %v", dbType, got, before)
		}
	}
	for _, tc := range []struct {
		text, dbType string
		want         any
	}{
		{"42", "FIXED", 42}, {"1.50", "FIXED", 1.5}, {"-0.05", "FIXED", -0.05}, {"1E+3", "DECFLOAT", 1000.0}, {"nan?", "FIXED", "nan?"}, {"42", "TEXT", "42"},
	} {
		if got := sqlText(tc.text, tc.dbType); got != tc.want {
			t.Errorf("%s %q reads as %#v, want %#v", tc.dbType, tc.text, got, tc.want)
		}
	}
}
