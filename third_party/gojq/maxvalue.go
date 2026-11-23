package gojq

import "context"

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
// call of exceeded, of each text or array a function of it gives that
// takes more than bytes: an array by the sixteen bytes of each of its
// items, whatever they are. The value is given all the same, and the
// program runs on unless exceeded cancels the context, which stops it
// before its next step as any cancelled context does.
//
// Every operator and builtin that makes a text or an array from others,
// addition, multiplication, a text with values in it, join, tojson,
// flatten and the formats among them, is such a function, so a program
// that doubles a value at every step is told of when the value first
// passes bytes, where nothing but its time or the machine's memory stopped
// it.
func WithMaxValueBytes(ctx context.Context, bytes int, exceeded func()) context.Context {
	return context.WithValue(ctx, maxValueKey{}, &maxValue{bytes: bytes, exceeded: exceeded})
}

func maxValueOf(ctx context.Context) *maxValue {
	most, _ := ctx.Value(maxValueKey{}).(*maxValue)
	return most
}

// made is told of the value a function gave.
func (m *maxValue) made(v any) {
	size := 0
	switch v := v.(type) {
	case string:
		size = len(v)
	case []any:
		size = 16 * len(v)
	}
	if size > m.bytes {
		m.exceeded()
	}
}
