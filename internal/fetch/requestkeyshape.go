package fetch

import (
	"encoding/json"
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A key of a type the build left out is read, as in a build with the type,
// into a value of the Go type of its field there, so that what the decoder
// and the load refuse of its value, at any depth, they refuse alike in
// every build. The catalogue holds the shape of that Go type: a type of
// internal/model, which every build has, by its name, and any other, a type
// of the left-out request type's own, by its structure, which is made again
// here (goTypeOf). A type that reads itself from YAML cannot be made again
// so, and must be one of internal/model's (catalogueTypes).

// typeShape is a Go type as the catalogue holds it.
type typeShape struct {
	// Name is the type's name, such as model.Duration or fetch.RedisCommand,
	// for a named type.
	Name string `json:"name,omitempty"`
	// Kind is the reflect.Kind, and Elem, Key and Fields the parts of a
	// composite type.
	Kind   string       `json:"kind"`
	Elem   *typeShape   `json:"elem,omitempty"`
	Key    *typeShape   `json:"key,omitempty"`
	Fields []fieldShape `json:"fields,omitempty"`
}

// fieldShape is an exported field of a struct of the catalogue.
type fieldShape struct {
	Name string    `json:"name"`
	Tag  string    `json:"tag,omitempty"`
	Type typeShape `json:"type"`
}

// catalogueTypes are the named types of internal/model a request type's
// own keys may be of, which the catalogue names rather than describes.
var catalogueTypes = func() map[string]reflect.Type {
	types := map[string]reflect.Type{}
	for _, value := range []any{model.Duration(0), model.ByteSize(0), model.SQLConnections{}, model.SQLKeepalive{}, model.TLSConfig{}, model.LoginConfig{}, model.RateLimitConfig{}, model.PaginationConfig{}, model.BasicAuth{}, model.BasicAuthFile{}, model.RetryConfig{}} {
		types[reflect.TypeOf(value).String()] = reflect.TypeOf(value)
	}
	return types
}()

// basicTypes are the Go types of the basic kinds, by kind.
var basicTypes = func() map[string]reflect.Type {
	types := map[string]reflect.Type{}
	for _, value := range []any{false, 0, int8(0), int16(0), int32(0), int64(0), uint(0), uint8(0), uint16(0), uint32(0), uint64(0), float32(0), float64(0), ""} {
		types[reflect.TypeOf(value).Kind().String()] = reflect.TypeOf(value)
	}
	types[reflect.Interface.String()] = reflect.TypeFor[any]()
	return types
}()

// GoTypeShape is the shape of t as the catalogue holds it, as JSON, or why
// the catalogue cannot hold it.
func GoTypeShape(t reflect.Type) (string, error) {
	shape, err := shapeOf(t)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(shape)
	return string(out), err
}

func shapeOf(t reflect.Type) (typeShape, error) {
	shape := typeShape{Kind: t.Kind().String()}
	if t.Name() != "" {
		shape.Name = t.String()
		if catalogueTypes[shape.Name] != nil {
			return shape, nil
		}
		if t.Kind() != reflect.Interface && reflect.PointerTo(t).Implements(reflect.TypeFor[yaml.Unmarshaler]()) {
			return shape, fmt.Errorf("the catalogue of request keys cannot hold %s, which reads itself from YAML; make it a type of internal/model and add it to catalogueTypes", shape.Name)
		}
	}
	var err error
	part := func(t reflect.Type) *typeShape {
		if err != nil {
			return nil
		}
		var sub typeShape
		sub, err = shapeOf(t)
		return &sub
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice:
		shape.Elem = part(t.Elem())
	case reflect.Map:
		shape.Key, shape.Elem = part(t.Key()), part(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if field := t.Field(i); field.IsExported() {
				shape.Fields = append(shape.Fields, fieldShape{Name: field.Name, Tag: string(field.Tag), Type: *part(field.Type)})
			}
		}
	default:
		if basicTypes[shape.Kind] == nil {
			return shape, fmt.Errorf("the catalogue of request keys cannot hold %s, of kind %s", t, shape.Kind)
		}
	}
	return shape, err
}

// goTypeOf makes the Go type of a shape again, adding to names the name of
// each struct made, by the type's own text, so that a decoding error that
// names it can name it as a build with the type does.
func goTypeOf(shape typeShape, names map[string]string) reflect.Type {
	if known := catalogueTypes[shape.Name]; known != nil {
		return known
	}
	switch shape.Kind {
	case reflect.Pointer.String():
		return reflect.PointerTo(goTypeOf(*shape.Elem, names))
	case reflect.Slice.String():
		return reflect.SliceOf(goTypeOf(*shape.Elem, names))
	case reflect.Map.String():
		return reflect.MapOf(goTypeOf(*shape.Key, names), goTypeOf(*shape.Elem, names))
	case reflect.Struct.String():
		fields := make([]reflect.StructField, len(shape.Fields))
		for i, field := range shape.Fields {
			fields[i] = reflect.StructField{Name: field.Name, Tag: reflect.StructTag(field.Tag), Type: goTypeOf(field.Type, names)}
		}
		made := reflect.StructOf(fields)
		if shape.Name != "" {
			names[made.String()] = shape.Name
		}
		return made
	}
	return basicTypes[shape.Kind]
}
