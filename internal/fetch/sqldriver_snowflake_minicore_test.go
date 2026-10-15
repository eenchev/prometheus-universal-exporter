//go:build (!select_request_types || request_type_sql) && sql_driver_snowflake && !minicore_disabled

package fetch

import "testing"

// The snowflake driver is built with gosnowflake's minicore_disabled tag,
// as tools/request-type-tags.sh gives it: without the tag, the driver's init
// makes a temporary directory at every start and writes a native library to
// it, or to ~/.cache/snowflake/minicore, to load it. This file is compiled
// only into a build that left the tag out.
func TestTheSnowflakeDriverIsBuiltWithMinicoreDisabled(t *testing.T) {
	t.Error("the snowflake driver was built without the minicore_disabled tag; build with the tags tools/request-type-tags.sh prints for it")
}
