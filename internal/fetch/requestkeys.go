package fetch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The keys only one request type takes are read into that type's own
// struct (RequestType.Config), declared in the type's file, so a build
// that leaves the type out carries neither. Every build still knows them,
// from requestKeysCatalogue (requestkeys_catalogue.go), the own keys of
// every type of the source tree, generated from a build with every type by
// make schemas: a configuration that sets a key of a type the build left
// out under another type is refused as setting a key of another type, as a
// build with every type refuses it, and the configuration schema, which is
// the same in every build, describes it.

// RequestKeyEntry is one key of a request type's own, as the catalogue
// holds it: the type, the key, the Go type of the field it is read into,
// as reflect names it and by its shape (GoTypeShape), and its
// configuration schema, as JSON.
type RequestKeyEntry struct {
	Type, Key, GoType, Shape, Schema string
}

// RequestKeyCatalogue is the catalogue of the own keys of every request
// type of the source tree (requestkeys_catalogue.go).
func RequestKeyCatalogue() []RequestKeyEntry {
	return requestKeysCatalogue
}

// registered are the request types this binary was built with, as they
// registered: RequestTypes, which a test may change, without the changes.
var registered = map[string]*RequestType{}

// OwnKeyTypes are the request types this binary was built with that take
// keys of their own (Config), in the order of the types of the tree.
func OwnKeyTypes() []*RequestType {
	var out []*RequestType
	for _, name := range knownRequestTypes {
		if rt := registered[name]; rt != nil && rt.Config != nil {
			out = append(out, rt)
		}
	}
	return out
}

// UnbuiltRequestTypes are the request types of the source tree this
// binary was built without.
func UnbuiltRequestTypes() []string {
	var out []string
	for _, name := range knownRequestTypes {
		if registered[name] == nil {
			out = append(out, name)
		}
	}
	return out
}

// UnbuiltRequestKeys are the entries of the catalogue of the types the
// build left out.
func UnbuiltRequestKeys() []RequestKeyEntry {
	var out []RequestKeyEntry
	for _, entry := range RequestKeyCatalogue() {
		if registered[entry.Type] == nil {
			out = append(out, entry)
		}
	}
	return out
}

// requestKeys is what a request block holds beyond model.RequestConfig's
// common part (model.SetRequestKeys): the own structs of the built types
// and the keys of the others.
var requestKeys = sync.OnceValue(func() *model.RequestKeys {
	keys := &model.RequestKeys{}
	for _, rt := range OwnKeyTypes() {
		keys.Structs = append(keys.Structs, model.RequestStruct{Type: rt.Name, Struct: reflect.TypeOf(rt.Config)})
	}
	keys.TypeNames = map[string]string{}
	for _, entry := range UnbuiltRequestKeys() {
		var shape typeShape
		if err := json.Unmarshal([]byte(entry.Shape), &shape); err != nil {
			panic(fmt.Sprintf("internal/fetch/requestkeys_catalogue.go: request.%s: %v", entry.Key, err))
		}
		keys.Unbuilt = append(keys.Unbuilt, model.UnbuiltRequestKey{Type: entry.Type, Key: entry.Key, GoType: goTypeOf(shape, keys.TypeNames)})
	}
	keys.Refuses = refusesRequestKey
	return keys
})

// refusesRequestKey says whether the validation refuses a key a request
// block of the type sets, by its path, as one the type does not take
// (model.RequestKeys.Refuses): a key of the block it does not take, or a
// key of retry that does not apply to it (checkRetryKeys).
func refusesRequestKey(requestType, key string) bool {
	rt := RequestTypes[requestType]
	if rt == nil {
		return false
	}
	if inner, ok := strings.CutPrefix(key, "retry."); ok {
		return retryKeyRefusal(rt, inner) != nil
	}
	return !strings.Contains(key, ".") && !rt.Takes(key)
}

func init() {
	model.SetRequestKeys(requestKeys)
}

// OwnKeys are the keys of the type's own struct (Config), in its order.
func (rt *RequestType) OwnKeys() []string {
	if rt.Config == nil {
		return nil
	}
	fields := yamlKeysOf(reflect.TypeOf(rt.Config))
	keys := make([]string, len(fields))
	for i, field := range fields {
		keys[i] = field.key
	}
	return keys
}

// Keys are every request key the type accepts: the keys of the common part
// it lists (Fields) and those of its own struct.
func (rt *RequestType) Keys() []string {
	return append(slices.Clip(rt.Fields), rt.OwnKeys()...)
}
