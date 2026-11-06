//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// binaryTestCollector is a valid collector of the binary decoder.
func binaryTestCollector() model.Collector {
	return model.Collector{
		Name:      "modbus",
		Request:   model.RequestConfig{Type: "http"},
		Decoder:   model.DecoderConfig{Type: "binary"},
		Response:  model.ResponseConfig{Binary: model.BinaryConfig{Fields: []model.BinaryField{{Name: "registers", Type: "u16", Count: 2}}}},
		Transform: model.TransformConfig{Type: "jq"},
		Metrics:   []model.MetricRule{{Name: "register", Expression: ".registers[0]"}},
	}
}

// response.binary and the binary decoder are checked when the configuration
// loads: the decoder needs a layout, the layout the decoder, and a field a
// type, a name for the document but for a skip, a length for a string and a
// skip, and none for a number.
func TestTheBinaryLayoutIsCheckedAtLoad(t *testing.T) {
	five := 5
	minus := -1
	for name, test := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"no fields":       {func(c *model.Collector) { c.Response.Binary.Fields = nil }, "decodes with binary and response.binary.fields is empty"},
		"another decoder": {func(c *model.Collector) { c.Decoder.Type = "json" }, "sets response.binary, the layout of the binary decoder, but its decoder is json"},
		"no decoder":      {func(c *model.Collector) { c.Decoder.Type = "" }, "but its decoder is"},
		"a regex transform": {func(c *model.Collector) {
			c.Transform.Type = "regex"
			c.Metrics = []model.MetricRule{{Name: "v", Expression: "(.*)"}}
		}, "decodes a binary answer, which a regex transform cannot read; use jq, yq or python"},
		"a bad byte order": {func(c *model.Collector) { c.Response.Binary.ByteOrder = "Big" }, `response.binary.byte_order is "Big"; want big or little`},
		"a bad type":       {func(c *model.Collector) { c.Response.Binary.Fields[0].Type = "u24" }, `fields[0] type is "u24"; want one of u8,`},
		"no name":          {func(c *model.Collector) { c.Response.Binary.Fields[0].Name = "" }, "fields[0] has no name"},
		"a named skip": {func(c *model.Collector) {
			c.Response.Binary.Fields = append(c.Response.Binary.Fields, model.BinaryField{Name: "pad", Type: "skip", Length: 2})
		}, `fields[1] is a skip and has the name "pad"`},
		"a skip without length": {func(c *model.Collector) {
			c.Response.Binary.Fields = append(c.Response.Binary.Fields, model.BinaryField{Type: "skip"})
		}, "fields[1] is a skip and has no length"},
		"a string without length": {func(c *model.Collector) {
			c.Response.Binary.Fields = append(c.Response.Binary.Fields, model.BinaryField{Name: "s", Type: "string"})
		}, "fields[1] is a string and has no length"},
		"a number with length": {func(c *model.Collector) { c.Response.Binary.Fields[0].Length = 2 }, "is a u16, whose length is its type's, and sets length"},
		"a string with count": {func(c *model.Collector) {
			c.Response.Binary.Fields = append(c.Response.Binary.Fields, model.BinaryField{Name: "s", Type: "cstring", Count: 2})
		}, "fields[1] is a cstring and sets count"},
		"a name twice": {func(c *model.Collector) {
			c.Response.Binary.Fields = append(c.Response.Binary.Fields, model.BinaryField{Name: "registers", Type: "u8", Offset: &five})
		}, `fields[1] has the name "registers" of fields[0]`},
		"a negative offset": {func(c *model.Collector) { c.Response.Binary.Fields[0].Offset = &minus }, "fields[0] offset is -1"},
		"a negative count":  {func(c *model.Collector) { c.Response.Binary.Fields[0].Count = -1 }, "fields[0] count is -1"},
		"a count too large": {func(c *model.Collector) { c.Response.Binary.Fields[0].Count = model.MaxBinaryCount + 1 }, "count is 65537"},
		"a bad field order": {func(c *model.Collector) { c.Response.Binary.Fields[0].ByteOrder = "middle" }, `fields[0] byte_order is "middle"`},
	} {
		t.Run(name, func(t *testing.T) {
			c := binaryTestCollector()
			test.edit(&c)
			if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
	for name, edit := range map[string]func(*model.Collector){
		"as written": func(*model.Collector) {},
		"python": func(c *model.Collector) {
			c.Transform = model.TransformConfig{Type: "python", Script: "pass"}
			c.Metrics = nil
		},
		"every type": func(c *model.Collector) {
			c.Response.Binary.ByteOrder = "little"
			for _, kind := range model.BinaryTypes {
				f := model.BinaryField{Name: "f_" + kind, Type: kind, ByteOrder: "big"}
				switch kind {
				case "skip":
					f.Name, f.Length = "", 1
				case "string":
					f.Length = 3
				}
				c.Response.Binary.Fields = append(c.Response.Binary.Fields, f)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := binaryTestCollector()
			edit(&c)
			if err := Validate(&model.Config{Collectors: []model.Collector{c}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
