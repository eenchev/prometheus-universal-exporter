package config

import (
	"reflect"
	"slices"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// setOwnKey sets key of c's request, a key of its type's own struct, to
// value, as a request block that writes it does: a corpus of collectors of
// any type puts a placeholder in each part a type fills, whichever type
// the collector has. A key its type does not own, or of a type the build
// left out, is left out, as no part of the collector's type it is.
func setOwnKey(c *model.Collector, key string, value any) {
	for _, rt := range fetch.OwnKeyTypes() {
		if rt.Name != c.Request.Type || !slices.Contains(rt.OwnKeys(), key) {
			continue
		}
		own := reflect.New(reflect.TypeOf(rt.Config)).Elem()
		if c.Request.Own != nil {
			own.Set(reflect.ValueOf(c.Request.Own))
		}
		for i := 0; i < own.NumField(); i++ {
			if name, _, _ := strings.Cut(own.Type().Field(i).Tag.Get("yaml"), ","); name == key {
				own.Field(i).Set(reflect.ValueOf(value))
			}
		}
		c.Request.Own = own.Interface()
	}
}

// ownKey is the value of key of c's request, a key of its type's own
// struct, and nil when its type has no such key or c's request holds none.
func ownKey(c *model.Collector, key string) any {
	if c.Request.Own == nil {
		return nil
	}
	own := reflect.ValueOf(c.Request.Own)
	for i := 0; i < own.NumField(); i++ {
		if name, _, _ := strings.Cut(own.Type().Field(i).Tag.Get("yaml"), ","); name == key {
			return own.Field(i).Interface()
		}
	}
	return nil
}
