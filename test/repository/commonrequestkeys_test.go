package repository

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// pendingCommonKeys are the keys of one request type that are still fields
// of the common part, until the type's own struct holds them.
var pendingCommonKeys = []string{
	"method", "body", "pagination", "accept_codes", "targets", "from", "until",
	"rpc", "message", "metadata", "descriptors", "protoset_file", "proto_files", "proto_import_paths",
	"passphrase_file", "known_hosts_file", "keepalive_interval", "keepalive_count_max", "idle_timeout", "max_open_connections",
	"driver", "database", "statement", "args", "options", "service_name", "max_rows", "connections",
	"resource", "group", "version", "namespaces", "all_namespaces", "label_selector", "field_selector", "max_objects", "api_server",
}

// The common part of a collector's request block, model.RequestConfig's
// fields, holds type and the keys several request types take, and no key
// of one type alone: such a key is a field of the type's own struct, in the
// type's file (fetch.RequestType.Config), so a build without the type does
// not carry it. The types that take a key are read from the registrations
// of every type of the tree, built or not.
func TestTheCommonPartHoldsNoKeyOfOneType(t *testing.T) {
	keys := registeredRequestTypeKeys(t)
	common := reflect.TypeFor[model.RequestConfig]()
	for i := 0; i < common.NumField(); i++ {
		key, _, _ := strings.Cut(common.Field(i).Tag.Get("yaml"), ",")
		if key == "" || key == "-" || key == "type" || slices.Contains(pendingCommonKeys, key) {
			continue
		}
		if taking := typesTaking(keys, key, func(k requestTypeKeys) []string { return k.fields }); len(taking) < 2 {
			t.Errorf("request.%s is a field of model.RequestConfig, the common part, and taken by %v alone; make it a field of the type's own struct (fetch.RequestType.Config)", key, taking)
		}
	}
}
