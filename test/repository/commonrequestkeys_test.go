package repository

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

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
		if key == "" || key == "-" || key == "type" {
			continue
		}
		if taking := typesTaking(keys, key, func(k requestTypeKeys) []string { return k.fields }); len(taking) < 2 {
			t.Errorf("request.%s is a field of model.RequestConfig, the common part, and taken by %v alone; make it a field of the type's own struct (fetch.RequestType.Config)", key, taking)
		}
	}
}
