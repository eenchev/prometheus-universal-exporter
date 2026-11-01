//go:build !select_request_types || request_type_grpc

package config

import (
	"strings"
	"testing"
)

// A grpc collector read for the debug page, whose request is never made, is
// not asked for the files of its descriptors, which Edit a copy leaves out:
// neither protoset_file nor proto_files. Read WithRequestMade, as a
// dangerous playground's own request is, each is required, as a load
// requires it, and as a load without the named files, the allocator's,
// still does.
func TestParseCollectorAsksForTheDescriptorFilesWhenTheRequestIsMade(t *testing.T) {
	for descriptors, want := range map[string]string{
		"protoset": "has descriptors: protoset but no request.protoset_file",
		"proto":    "has descriptors: proto but no request.proto_files",
	} {
		doc := []byte("name: queue\nrequest:\n  type: grpc\n  rpc: acme.queue.v1.QueueService/GetStats\n  descriptors: " + descriptors + "\ntransform:\n  type: jq\nmetrics:\n  - name: up\n    expression: '1'\n")
		if _, err := ParseCollector(doc, nil); err != nil {
			t.Errorf("%s, read for a request not made: %v", descriptors, err)
		}
		if _, err := ParseCollector(doc, nil, WithRequestMade()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s, read for a request made: %v", descriptors, err)
		}
	}
}
