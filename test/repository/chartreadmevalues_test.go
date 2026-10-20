package repository

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The chart README's tables name values, and its Values table lists, for an
// object, the keys it takes. A value removed from values.yaml and the schema
// stayed in those tables once (allocator.cache), telling a reader to set a
// key the schema refuses. These hold every value a table names, and every key
// an object's row lists, to the values schema, which refuses any key it does
// not declare.

// readmeValueCell is a table row's first cell made of values alone, such as
// `image.repository` / `image.tag` or `allocator.tokenSecret.name` / `key`.
var readmeValueCell = regexp.MustCompile("^\\| (`[A-Za-z][A-Za-z0-9.]*`(?: / `[A-Za-z][A-Za-z0-9.]*`)*) \\|")

// readmeKeyRun is a run of three or more keys in a row's description, such as
// `enabled`, `minAvailable` or `maxUnavailable`, `unhealthyPodEvictionPolicy`.
var readmeKeyRun = regexp.MustCompile("`[A-Za-z]+`(?:\\s*(?:,\\s*(?:and\\s+|or\\s+)?|\\s+or\\s+|\\s+and\\s+)`[A-Za-z]+`){2,}")

// schemaHas reports whether the values schema declares the value at path.
func schemaHas(schema map[string]any, path string) bool {
	node := schema
	for key := range strings.SplitSeq(path, ".") {
		properties, _ := node["properties"].(map[string]any)
		next, ok := properties[key].(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	return true
}

func TestChartReadmeNamesOnlyValuesTheSchemaDeclares(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(readChartFile(t, "values.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	readme := readChartFile(t, "README.md")
	rows, objects := 0, 0
	for line := range strings.SplitSeq(readme, "\n") {
		match := readmeValueCell.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		top, _, _ := strings.Cut(strings.Trim(match[1], "`"), ".")
		if !schemaHas(schema, top) {
			continue // a table of something other than values
		}
		rows++
		parent := ""
		var paths []string
		for part := range strings.SplitSeq(match[1], " / ") {
			path := strings.Trim(part, "`")
			if strings.Contains(path, ".") || parent == "" {
				// A path of its own, which later bare keys are relative to.
				if i := strings.LastIndex(path, "."); i >= 0 {
					parent = path[:i+1]
				} else {
					parent = "-"
				}
			} else if parent != "-" {
				path = parent + path
			}
			paths = append(paths, path)
		}
		for _, path := range paths {
			if !schemaHas(schema, path) {
				t.Errorf("the chart README's row %q names %s, which values.schema.json does not declare", match[1], path)
			}
		}
		cells := strings.Split(line, " | ")
		if len(paths) != 1 || len(cells) < 4 || cells[1] != "object" {
			continue
		}
		// What the row says, without the asides in parentheses, which give
		// a key's values rather than keys.
		description := regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(strings.Join(cells[3:], " | "), "")
		run := readmeKeyRun.FindString(description)
		if run == "" {
			continue
		}
		objects++
		for _, key := range regexp.MustCompile("`([A-Za-z]+)`").FindAllStringSubmatch(run, -1) {
			if !schemaHas(schema, paths[0]+"."+key[1]) {
				t.Errorf("the chart README's row for %s lists the key %s, which values.schema.json does not declare", paths[0], key[1])
			}
		}
	}
	// The allocator's and sshAuth's tables, and the Values table's objects
	// such as server and autoscaling.
	if rows < 40 || objects < 5 {
		t.Fatalf("read %d value rows and %d object rows listing keys from the chart README; the reader has stopped finding them", rows, objects)
	}
}
