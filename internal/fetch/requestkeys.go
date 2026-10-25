package fetch

import (
	"reflect"
	"slices"
	"sync"

	"gopkg.in/yaml.v3"

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
// as reflect names it, and its configuration schema, as JSON.
type RequestKeyEntry struct {
	Type, Key, GoType, Schema string
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

// catalogueGoTypes are the Go types a key of a type the build left out is
// read into, by their names, when the build has them: a key of another
// Go type, one of the left-out type's own, is read as a yaml.Node.
var catalogueGoTypes = func() map[string]reflect.Type {
	types := map[string]reflect.Type{}
	for _, value := range []any{"", false, 0, []string(nil), map[string]string(nil), model.Duration(0), (*model.Duration)(nil), (*int)(nil), (*bool)(nil), model.ByteSize(0), (*model.SQLConnections)(nil)} {
		types[reflect.TypeOf(value).String()] = reflect.TypeOf(value)
	}
	return types
}()

// requestKeys is what a request block holds beyond model.RequestConfig's
// common part (model.SetRequestKeys): the own structs of the built types
// and the keys of the others.
var requestKeys = sync.OnceValue(func() *model.RequestKeys {
	keys := &model.RequestKeys{}
	for _, rt := range OwnKeyTypes() {
		keys.Structs = append(keys.Structs, model.RequestStruct{Type: rt.Name, Struct: reflect.TypeOf(rt.Config)})
	}
	for _, entry := range UnbuiltRequestKeys() {
		goType, ok := catalogueGoTypes[entry.GoType]
		if !ok {
			goType = reflect.TypeOf(yaml.Node{})
		}
		keys.Unbuilt = append(keys.Unbuilt, model.UnbuiltRequestKey{Type: entry.Type, Key: entry.Key, GoType: goType})
	}
	return keys
})

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
