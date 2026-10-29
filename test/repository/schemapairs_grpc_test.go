//go:build !select_request_types || request_type_grpc

package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The files of a grpc collector's source of descriptors go with it alone,
// to the exporter (validateDescriptorKeys) and to the schema, which took
// protoset_file, proto_files and proto_import_paths beside any source, and
// a source without its files: protoset_file with descriptors: protoset,
// which requires it, written with something other than blanks, and
// proto_files and proto_import_paths with descriptors: proto, which
// requires proto_files.
func TestSchemaAndExporterAgreeOnTheFilesOfASourceOfDescriptors(t *testing.T) {
	schema := loadSchema(t)
	// agree, of the tests of a build with http, is not in a build with
	// grpc alone.
	agree := func(t *testing.T, schema map[string]any, name, document string, accepted bool) {
		t.Helper()
		problems := schemaProblems(t, schema, document)
		_, err := config.Load(testutil.WriteFile(t, "config.yaml", document))
		if (len(problems) == 0) != accepted || (err == nil) != accepted {
			t.Errorf("%s: want accepted %v by both; the schema says %v, the exporter %v\n%s", name, accepted, problems, err, document)
		}
	}
	request := func(keys string) string {
		return strings.Replace(queueCollector, "      # request\n", keys, 1)
	}
	for name, keys := range map[string]string{
		"reflection with a protoset_file":         "      descriptors: reflection\n      protoset_file: /d.protoset\n",
		"reflection with proto_files":             "      descriptors: reflection\n      proto_files: [queue.proto]\n",
		"reflection with proto_import_paths":      "      descriptors: reflection\n      proto_import_paths: [/protos]\n",
		"proto with a protoset_file":              "      descriptors: proto\n      proto_files: [queue.proto]\n      protoset_file: /d.protoset\n",
		"protoset with proto_import_paths":        "      descriptors: protoset\n      protoset_file: /d.protoset\n      proto_import_paths: [/protos]\n",
		"protoset without its file":               "      descriptors: protoset\n",
		"protoset with a file of blanks":          "      descriptors: protoset\n      protoset_file: '  '\n",
		"proto without its files":                 "      descriptors: proto\n",
		"proto with import paths and no files":    "      descriptors: proto\n      proto_import_paths: [/protos]\n",
		"the health service with a protoset_file": "      protoset_file: /d.protoset\n",
	} {
		document := request(keys)
		if name == "the health service with a protoset_file" {
			document = strings.Replace(healthCollector, "      # request\n", keys, 1)
		}
		agree(t, schema, name, document, false)
	}
	for name, keys := range map[string]string{
		"reflection alone":                    "      descriptors: reflection\n",
		"reflection with files written empty": "      descriptors: reflection\n      protoset_file: \"\"\n      proto_files: []\n      proto_import_paths: []\n",
	} {
		agree(t, schema, name, request(keys), true)
	}
}
