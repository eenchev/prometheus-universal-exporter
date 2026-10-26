package model

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// A collector's request block is written flat: request.type and, beside it,
// the keys of that type. Only the keys that several request types take are
// fields of RequestConfig, its common part; a key only one type takes is
// read into that type's own struct, which the type's registration declares
// (fetch.RequestType.Config) in the type's own file, so a build that leaves
// the type out carries neither. RequestConfig holds the struct of its type
// in Own.
//
// Which structs there are is the fetch package's to say (SetRequestKeys),
// as are the keys of the types of the source tree that this build left out:
// a configuration that sets one of them under another type is refused as
// setting a key of another type, as it is in a build with every type, and
// not as setting a key that does not exist.

// RequestKeys is what a request block holds beyond RequestConfig's common
// part.
type RequestKeys struct {
	// Structs are the request types of the build that take keys of their
	// own, each with the struct those keys are read into, in the order the
	// types are listed in.
	Structs []RequestStruct
	// Unbuilt are the keys of the request types of the source tree that
	// the build left out, each read into a value of Go type GoType: the
	// type of the field that holds it in a build with the type, or, for a
	// type of the request type's own, which this build does not have, one
	// made again to its shape.
	Unbuilt []UnbuiltRequestKey
	// TypeNames are the names of the structs made again, by their text,
	// for the errors of a decoding to name them as a build with the type
	// does.
	TypeNames map[string]string
	// Refuses says whether a request block of a type the build has, by
	// its name, refuses a key it sets, by its path (retry.codes for codes
	// in retry), as the validation does; false for a type the build does
	// not have, which the validation refuses the block for.
	Refuses func(requestType, key string) bool
}

// RequestStruct is a request type of the build and the struct of its own
// keys.
type RequestStruct struct {
	Type   string
	Struct reflect.Type
}

// UnbuiltRequestKey is a key of a request type the build left out.
type UnbuiltRequestKey struct {
	Type, Key string
	GoType    reflect.Type
}

// requestKeysSource is the function that gives the RequestKeys, which the
// fetch package sets; unset, a request block holds RequestConfig's common
// part alone.
var requestKeysSource atomic.Pointer[func() *RequestKeys]

// SetRequestKeys sets the function that gives what a request block holds
// beyond RequestConfig's common part. It is asked when a request block is
// first read, once every request type has registered, and is to give the
// same RequestKeys each time.
func SetRequestKeys(keys func() *RequestKeys) {
	requestKeysSource.Store(&keys)
}

// requestLayout is how one RequestKeys reads a request block: into a struct
// with the common part's fields followed by a field for each key of every
// type's own (Owned), whose values are then put where they belong
// (store).
type requestLayout struct {
	decoded reflect.Type
	// common are the fields of RequestConfig that the first fields of
	// decoded are copied into, by index.
	common []int
	// owned are the fields of decoded after the common part's.
	owned []ownedKey
	// structs are the built types' own structs by type.
	structs map[string]reflect.Type
	// typeNames are RequestKeys.TypeNames.
	typeNames map[string]string
	// refuses is RequestKeys.Refuses, nil without RequestKeys.
	refuses func(requestType, key string) bool
}

// ownedKey is a field of a decoded request block that holds a key of one
// type's own: at is the field's index in the decoded struct, and field the
// index of the field it goes into in the type's own struct, -1 for a key of
// a type the build left out.
type ownedKey struct {
	owner, key string
	at, field  int
}

var (
	nodeType = reflect.TypeOf(yaml.Node{})
	// requestLayouts holds the requestLayout of each RequestKeys.
	requestLayouts sync.Map
	// commonRequestLayout is the layout without RequestKeys: the common
	// part alone.
	commonRequestLayout = sync.OnceValue(func() *requestLayout { return newRequestLayout(nil) })
)

// currentRequestLayout is the layout a request block is read with now.
func currentRequestLayout() *requestLayout {
	source := requestKeysSource.Load()
	if source == nil {
		return commonRequestLayout()
	}
	keys := (*source)()
	if keys == nil {
		return commonRequestLayout()
	}
	if known, ok := requestLayouts.Load(keys); ok {
		return known.(*requestLayout)
	}
	known, _ := requestLayouts.LoadOrStore(keys, newRequestLayout(keys))
	return known.(*requestLayout)
}

func newRequestLayout(keys *RequestKeys) *requestLayout {
	layout := &requestLayout{structs: map[string]reflect.Type{}}
	common := reflect.TypeOf(RequestConfig{})
	var fields []reflect.StructField
	for i := 0; i < common.NumField(); i++ {
		field := common.Field(i)
		if key, _, _ := strings.Cut(field.Tag.Get("yaml"), ","); key == "" || key == "-" {
			continue
		}
		field.Index, field.Offset = nil, 0
		fields = append(fields, field)
		layout.common = append(layout.common, i)
	}
	add := func(owner, key string, goType reflect.Type, field int) {
		layout.owned = append(layout.owned, ownedKey{owner: owner, key: key, at: len(fields), field: field})
		fields = append(fields, reflect.StructField{Name: "K" + strconv.Itoa(len(layout.owned)), Type: goType, Tag: reflect.StructTag(`yaml:"` + key + `"`)})
	}
	if keys != nil {
		layout.typeNames = keys.TypeNames
		layout.refuses = keys.Refuses
		for _, own := range keys.Structs {
			layout.structs[own.Type] = own.Struct
			for i := 0; i < own.Struct.NumField(); i++ {
				field := own.Struct.Field(i)
				if key, _, _ := strings.Cut(field.Tag.Get("yaml"), ","); key != "" && key != "-" {
					add(own.Type, key, field.Type, i)
				}
			}
		}
		for _, key := range keys.Unbuilt {
			add(key.Type, key.Key, key.GoType, -1)
		}
	}
	layout.decoded = reflect.StructOf(fields)
	return layout
}

// UnmarshalYAML reads a request block: its common part into RequestConfig's
// fields, the keys of its type's own into the type's struct (Own), and the
// names of the keys of other types that it sets into Foreign, for the
// validation to refuse in its words, and the order of its keys into
// Order. A key of no type is refused as the decoder refuses an unknown key,
// unless a key the validation refuses comes first in the file, and a value
// of the wrong kind as the decoder refuses one, whichever type takes the
// key.
func (r *RequestConfig) UnmarshalYAML(n *yaml.Node) error {
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return notAMapping(n, "model.RequestConfig")
	}
	layout := currentRequestLayout()
	var problems []string
	collect := func(err error) error {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			problems = append(problems, typeErr.Errors...)
			return nil
		}
		return err
	}
	// Node.Decode does not refuse unknown keys the way the file's decoder
	// does, so they are checked here, or a misspelt key would be ignored.
	unknown := unknownKeys(n, layout.decoded, "model.RequestConfig")
	decoded := reflect.New(layout.decoded)
	if err := collect(n.Decode(decoded.Interface())); err != nil {
		return err
	}
	layout.store(decoded.Elem(), r)
	r.Order = keyOrder(n, decoded.Elem())
	// A key the validation refuses that comes before the unknown ones in
	// the file is the one the error names, so they are left to be refused
	// once it is gone.
	if first, ok := layout.firstRefused(n, decoded.Elem(), r.Type); ok {
		unknown = slices.DeleteFunc(unknown, func(p keyProblem) bool { return !before(p.line, p.column, first) })
	}
	messages := make([]string, 0, len(unknown)+len(problems))
	for _, problem := range unknown {
		messages = append(messages, problem.message)
	}
	problems = append(messages, problems...)
	if len(problems) == 0 {
		return nil
	}
	for i := range problems {
		for made, name := range layout.typeNames {
			problems[i] = strings.ReplaceAll(problems[i], made, name)
		}
	}
	return &yaml.TypeError{Errors: problems}
}

// firstRefused is the first key in the file, of those a request block of
// the type sets and of those of each mapping it holds directly, that the
// validation refuses as one the type does not take (RequestKeys.Refuses);
// false when there is none, or the build does not have the type.
func (l *requestLayout) firstRefused(n *yaml.Node, decoded reflect.Value, requestType string) (keyAt, bool) {
	if l.refuses == nil {
		return keyAt{}, false
	}
	requestType = strings.ToLower(strings.TrimSpace(requestType))
	for _, key := range blockKeys(n, decoded) {
		if key.path != "type" && l.refuses(requestType, key.path) {
			return key, true
		}
	}
	return keyAt{}, false
}

// store puts what a request block was decoded into where it belongs.
func (l *requestLayout) store(decoded reflect.Value, r *RequestConfig) {
	*r = RequestConfig{}
	out := reflect.ValueOf(r).Elem()
	for i, field := range l.common {
		out.Field(field).Set(decoded.Field(i))
	}
	// The type is taken as the validation names it, whatever its case and
	// the blanks around it.
	owner := strings.ToLower(strings.TrimSpace(r.Type))
	var own reflect.Value
	if structType, ok := l.structs[owner]; ok {
		own = reflect.New(structType).Elem()
	}
	for _, key := range l.owned {
		value := decoded.Field(key.at)
		switch {
		case key.owner == owner && own.IsValid():
			own.Field(key.field).Set(value)
		case key.owner != owner && isSet(value):
			r.Foreign = append(r.Foreign, key.key)
		}
	}
	if own.IsValid() {
		r.Own = own.Interface()
	}
}

// isSet says whether a decoded value is one a request block sets: anything
// but the zero value, as the validation counts the keys of the common part
// it sets, and, for the value of a key whose Go type is not in the build,
// anything but no value.
func isSet(value reflect.Value) bool {
	if value.Type() == nodeType {
		node := value.Interface().(yaml.Node)
		return node.Kind != 0 && node.ShortTag() != nullTag
	}
	return !value.IsZero()
}

// MarshalYAML writes a request block as it is read: the common part's keys
// and then those of its type's own struct, flat beside them, so the YAML of
// a collector, which a reload compares to tell a changed collector
// (config.encodedDefinition) and the exporter's cache key is made from
// (collectorFingerprint), holds every key its type takes.
func (r RequestConfig) MarshalYAML() (any, error) {
	type plain RequestConfig
	if r.Own == nil {
		return plain(r), nil
	}
	var out, own yaml.Node
	if err := out.Encode(plain(r)); err != nil {
		return nil, err
	}
	if err := own.Encode(r.Own); err != nil {
		return nil, err
	}
	out.Content = append(out.Content, own.Content...)
	return &out, nil
}

// RequestView is r as a request block is read: a struct with a field for
// each key of the common part and of every request type's own, holding r's
// values, which a caller may change through it, as the load does when it
// reads again what the decoder took for a mistake, and then write back
// into r (StoreRequestView).
func RequestView(r *RequestConfig) reflect.Value {
	layout := currentRequestLayout()
	view := reflect.New(layout.decoded).Elem()
	in := reflect.ValueOf(r).Elem()
	for i, field := range layout.common {
		view.Field(i).Set(in.Field(field))
	}
	owner := strings.ToLower(strings.TrimSpace(r.Type))
	if r.Own != nil && reflect.TypeOf(r.Own) == layout.structs[owner] {
		own := reflect.ValueOf(r.Own)
		for _, key := range layout.owned {
			if key.owner == owner {
				view.Field(key.at).Set(own.Field(key.field))
			}
		}
	}
	return view
}

// StoreRequestView writes a RequestView of r back into r as reading the
// block would have: the common part, Own, and Foreign with the keys of
// other types the view now sets beside those r had, whose values the view
// does not hold.
func StoreRequestView(view reflect.Value, r *RequestConfig) {
	layout := currentRequestLayout()
	foreign, order := r.Foreign, r.Order
	layout.store(view, r)
	r.Order = order
	if len(foreign) == 0 {
		return
	}
	var merged []string
	for _, key := range layout.owned {
		if slices.Contains(foreign, key.key) || slices.Contains(r.Foreign, key.key) {
			merged = append(merged, key.key)
		}
	}
	r.Foreign = merged
}

// RequestViewType is the type of a RequestView.
func RequestViewType() reflect.Type {
	return currentRequestLayout().decoded
}
