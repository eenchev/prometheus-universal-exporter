package gojq

import (
	"context"
	"reflect"
	"unsafe"
)

// This file, and the three lines that use it in env.go and execute.go, are
// the whole of what prometheus-universal-exporter changes in gojq: see
// PATCH.md beside it.

// maxValue is the most bytes one value a function gives may take, and what
// is told when one takes more.
type maxValue struct {
	bytes    int
	exceeded func()
}

type maxValueKey struct{}

// WithMaxValueBytes returns a context with which a program is told, by a
// call of exceeded, of each text, array or object a function of it makes
// that takes more than bytes: an array by the sixteen bytes of each of its
// items and an object by the forty-eight of each of its keys, whatever
// they are and however long the keys. The value is given all the same, and the
// program runs on unless exceeded cancels the context, which stops it
// before its next step as any cancelled context does.
//
// Every operator and builtin that makes a text or an array from others,
// addition, multiplication, a text with values in it, join, tojson,
// flatten and the formats among them, is such a function, so a program
// that doubles a value at every step is told of when the value first
// passes bytes, where nothing but its time or the machine's memory stopped
// it.
//
// A function that gives a value it was given makes none, and is not told
// of: the value itself, as debug and tostring of a text give, a part of
// it, as a slice and ltrimstr give, or one of the items or values of an
// array or an object it was given, as an index, max and min_by give, and
// what getpath finds at any depth. An object is the one it was given or
// another. So a program is not told of a large
// value its input holds for reading it.
func WithMaxValueBytes(ctx context.Context, bytes int, exceeded func()) context.Context {
	return context.WithValue(ctx, maxValueKey{}, &maxValue{bytes: bytes, exceeded: exceeded})
}

func maxValueOf(ctx context.Context) *maxValue {
	most, _ := ctx.Value(maxValueKey{}).(*maxValue)
	return most
}

// made is told of the value v that the function name gave for the input x
// and the arguments args.
func (m *maxValue) made(v, x any, args []any, name string) {
	size := 0
	switch v := v.(type) {
	case string:
		size = len(v)
	case []any:
		size = 16 * len(v)
	case map[string]any:
		size = 48 * len(v)
	}
	if size <= m.bytes || name == "getpath" || given(v, x) {
		return
	}
	for _, arg := range args {
		if given(v, arg) {
			return
		}
	}
	m.exceeded()
}

// given says whether v is from, a text, an array or an object the function
// was given, or a part of it, or one of the items or values of from.
func given(v, from any) bool {
	switch from := from.(type) {
	case string:
		return within(v, from)
	case []any:
		if within(v, from) {
			return true
		}
		for _, item := range from {
			if within(v, item) {
				return true
			}
		}
	case map[string]any:
		if within(v, from) {
			return true
		}
		for _, value := range from {
			if within(v, value) {
				return true
			}
		}
	}
	return false
}

// within says whether the text or array v is in the memory of whole, which
// is of its kind: the same value, or a part of it; and whether the object
// v is the object whole.
func within(v, whole any) bool {
	switch v := v.(type) {
	case string:
		w, ok := whole.(string)
		if !ok || len(v) > len(w) {
			return false
		}
		start, from := uintptr(unsafe.Pointer(unsafe.StringData(v))), uintptr(unsafe.Pointer(unsafe.StringData(w)))
		return start >= from && start+uintptr(len(v)) <= from+uintptr(len(w))
	case []any:
		w, ok := whole.([]any)
		if !ok || len(v) > cap(w) {
			return false
		}
		const item = unsafe.Sizeof(any(nil))
		start, from := uintptr(unsafe.Pointer(unsafe.SliceData(v))), uintptr(unsafe.Pointer(unsafe.SliceData(w)))
		return start >= from && start+uintptr(len(v))*item <= from+uintptr(cap(w))*item
	case map[string]any:
		w, ok := whole.(map[string]any)
		return ok && reflect.ValueOf(v).UnsafePointer() == reflect.ValueOf(w).UnsafePointer()
	}
	return false
}
