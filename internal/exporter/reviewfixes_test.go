package exporter

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Where one list of a probe's parameters ends and the next begins is part of
// its cache key, so two different probes never share a key.
func TestCacheKeysKeepListsApart(t *testing.T) {
	c := testutil.Collector("keys", "text")
	a := probeCacheKey(&c, "http://h", url.Values{"method": {"GET"}, "path": {"/admin"}}, nil)
	b := probeCacheKey(&c, "http://h", url.Values{"method": {"GET", "path", "/admin"}}, nil)
	if a == "" || a == b {
		t.Fatal("two different probes share a cache key")
	}
	h1 := probeCacheKey(&c, "http://h", nil, http.Header{"X-A": {"1", "X-B"}})
	h2 := probeCacheKey(&c, "http://h", nil, http.Header{"X-A": {"1"}, "X-B": {}})
	if h1 == h2 {
		t.Fatal("header values run into the next header")
	}
	// A header name in any case is the same header.
	if probeCacheKey(&c, "http://h", nil, http.Header{"x-a": {"1"}}) != probeCacheKey(&c, "http://h", nil, http.Header{"X-A": {"1"}}) {
		t.Fatal("a header's case changes the key")
	}
}

// The process start time is the same at every scrape.
func TestTheProcessStartTimeIsStable(t *testing.T) {
	first, ok := processStartSeconds()
	if !ok {
		t.Skip("no /proc")
	}
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		if again, _ := processStartSeconds(); again != first {
			t.Fatalf("start time moved from %f to %f", first, again)
		}
	}
	if now := float64(time.Now().Unix()); first > now+1 || first < now-7*24*3600 {
		t.Fatalf("start time %f is not near now, %f", first, now)
	}
}

// Where a static target's own sections end is part of its cache key: a
// target value that reads like a section name cannot make two different
// targets share a key.
func TestTargetOwnSectionsAreCounted(t *testing.T) {
	a := targetOwnRequest(&model.StaticTarget{Request: model.TargetRequestConfig{
		Targets: []string{"a", "metadata", "k", "v"},
	}})
	b := targetOwnRequest(&model.StaticTarget{Request: model.TargetRequestConfig{
		Targets:  []string{"a"},
		Metadata: map[string]string{"k": "v"},
	}})
	if strings.Join(a, "\x00") == strings.Join(b, "\x00") {
		t.Fatalf("two different targets share their own sections: %q", a)
	}
	c := testutil.Collector("keys", "text")
	if probeCacheKey(&c, "http://h", nil, nil, a...) == probeCacheKey(&c, "http://h", nil, nil, b...) {
		t.Fatal("two different targets share a cache key")
	}
}

// Every setting of a collector is part of its cache key: the key is built
// from the whole definition, and this keeps it so for a setting added later,
// such as one kept out of the encoding. Each settable leaf of the collector,
// set on its own, changes the key; lists and mappings are set as a whole.
func TestEverySettingChangesTheCacheKey(t *testing.T) {
	type leaf struct {
		name  string
		index []int
	}
	var leaves func(typ reflect.Type, prefix string, index []int) []leaf
	leaves = func(typ reflect.Type, prefix string, index []int) []leaf {
		var out []leaf
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if !field.IsExported() || key == "" {
				continue
			}
			if key == "-" {
				t.Errorf("%s%s is kept out of the collector's encoding, so out of its cache key", prefix, field.Name)
				continue
			}
			at := append(append([]int(nil), index...), i)
			inner := field.Type
			if inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				out = append(out, leaves(inner, prefix+key+".", at)...)
				continue
			}
			out = append(out, leaf{prefix + key, at})
		}
		return out
	}
	fieldAt := func(v reflect.Value, index []int) reflect.Value {
		for _, i := range index {
			if v.Kind() == reflect.Pointer {
				if v.IsNil() {
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()
			}
			v = v.Field(i)
		}
		return v
	}
	var sample func(typ reflect.Type) reflect.Value
	sample = func(typ reflect.Type) reflect.Value {
		v := reflect.New(typ).Elem()
		switch typ.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			v.SetInt(7)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			v.SetUint(7)
		case reflect.Float32, reflect.Float64:
			v.SetFloat(1.5)
		case reflect.Slice:
			v = reflect.MakeSlice(typ, 0, 1)
			v = reflect.Append(v, sample(typ.Elem()))
		case reflect.Map:
			v = reflect.MakeMap(typ)
			v.SetMapIndex(sample(typ.Key()), sample(typ.Elem()))
		case reflect.Pointer:
			v = reflect.New(typ.Elem())
			v.Elem().Set(sample(typ.Elem()))
		case reflect.Interface:
			v.Set(reflect.ValueOf("x"))
		}
		return v
	}
	base := collectorFingerprint(&model.Collector{})
	all := leaves(reflect.TypeOf(model.Collector{}), "", nil)
	if len(all) < 50 {
		t.Fatalf("only %d settings found; the walk is broken", len(all))
	}
	for _, l := range all {
		var c model.Collector
		target := fieldAt(reflect.ValueOf(&c).Elem(), l.index)
		target.Set(sample(target.Type()))
		if collectorFingerprint(&c) == base {
			t.Errorf("%s does not change the cache key", l.name)
		}
	}
}
