package fetch

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// The check of a static target file against the configuration read the
// yaml keys of a target's request block from the struct tags for every
// target (setKeys), and found the placeholders of a collector's request for
// every target that names it, twice (CheckRequestParams). The keys are now
// read once for a type, and the placeholders once for a check of the
// parameters, or once for a collector in the check of a whole file
// (RequestParamsCheck). The two as they were are kept here, and the tests
// below hold the present ones to them.

// setKeysBefore is setKeys as it was while it read the struct tags for
// every value.
func setKeysBefore(v reflect.Value, skip ...string) []string {
	var keys []string
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if key == "" || key == "-" || slices.Contains(skip, key) {
			continue
		}
		if !v.Field(i).IsZero() {
			keys = append(keys, key)
		}
	}
	return keys
}

// checkRequestParamsBefore is CheckRequestParams as it was while it found
// the placeholders anew for every check, the path's and the templated
// fields' once to list the parameters used and once more to bind them.
func checkRequestParamsBefore(c *model.Collector, overrides RequestOverrides) (unused []string, err error) {
	used, err := requestParamNamesBefore(c, overrides)
	if err != nil {
		return nil, err
	}
	if !overrides.PathSet && HasPathParams(c.Request.Path) {
		if _, _, err := bindPathParams(c.Request.Path, overrides.Params); err != nil {
			return nil, err
		}
	}
	for _, f := range requestTemplates(c, overrides) {
		if _, err := f.render(overrides.Params); err != nil {
			return nil, err
		}
	}
	if labels := c.LabelParams; labels != nil {
		if err := checkLabelParams(labels, overrides.Params, used); err != nil {
			return nil, err
		}
	}
	for name := range overrides.Params {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	return unused, nil
}

// pick is one of values, each as likely as the others.
func pick[T any](r *rand.Rand, values ...T) T { return values[r.IntN(len(values))] }

// paramsCorpusCollector is a generated collector with placeholders, well
// formed or not, in any part of its request and in its label values, or in
// none: each part's that a check of parameters looks at, and each of the
// errors finding them gives.
func paramsCorpusCollector(t *testing.T, r *rand.Rand) model.Collector {
	t.Helper()
	c := labelCollector(pick(r, nil, map[string]string{"site": "dc1"}, map[string]string{"tenant": "{{param_tenant}}"}, map[string]string{"site": "{{param_site:dc1}}"}))
	c.Request.Path = pick(r, "", "/status", "/api/{{param_tenant}}", "/api/{{param_tenant}}/v{{param_version:2}}", "/api/{{param_tenant", "/api/{{ param_tenant }}", "/{{param_tenant|json}}")
	c.Request.Body = pick(r, "", "", "plain {x}", `{"s": {{param_service|json}}}`, `{"l": {{param_limit:10|number}}}`, "{{param_service|bogus}}", "{{param_service", "{{ param_service}}", "{{{param_service}}}")
	c.Request.Headers = pick(r, nil, nil, map[string]string{"X-T": "{{param_tenant}}", "Accept": "text/plain"}, map[string]string{"X-B": "{{param_tenant|json}}"})
	c.Request.Query = pick(r, nil, nil, map[string]string{"region": "{{param_region:eu}}"}, map[string]string{"bad": "{{param_region:${X}}}"})
	c.Request.Message = pick(r, "", "", `{"q": {{param_queue:orders|json}}}`, `{"q": {{param_queue|form}}}`)
	c.Request.Metadata = pick(r, nil, nil, map[string]string{"x-t": "{{param_meta:d}}"}, map[string]string{"x-t": "{{param_tenant}}"})
	c.Request.Targets = pick(r, nil, nil, []string{"app.{{param_host:web}}.requests", "cpu.load"}, []string{"{{param_host"})
	if err := readLabels(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// paramsCorpusOverrides is generated parameters, with values each place
// takes or refuses, and parts of the request a probe or a target replaces.
func paramsCorpusOverrides(r *rand.Rand) RequestOverrides {
	body, message := "{}", "{}"
	var overrides RequestOverrides
	if r.IntN(4) > 0 {
		overrides.Params = map[string]string{}
		for range r.IntN(5) {
			name := pick(r, "param_tenant", "param_version", "param_service", "param_limit", "param_region", "param_queue", "param_meta", "param_host", "param_site", "param_tenat")
			overrides.Params[name] = pick(r, "acme", "acme", "", "..", "a\nb", "ten", "5", "web*", "a\xffb")
		}
	}
	if r.IntN(4) == 0 {
		overrides.PathSet, overrides.Path = true, "/own"
	}
	if r.IntN(4) == 0 {
		overrides.Body = &body
	}
	if r.IntN(4) == 0 {
		overrides.Message = &message
	}
	if r.IntN(4) == 0 {
		overrides.Targets = []string{"a.b"}
	}
	return overrides
}

// Over generated collectors and parameters the check of a probe's or a
// target's parameters says what it said while it found the placeholders
// anew each time (checkRequestParamsBefore): the same parameters unused, or
// the same error, with the same text, of the same type, and the same
// parameter missing where one is. So does a RequestParamsCheck checking many
// of them against the same collectors, with whatever parts of the request
// each replaces, after it found a collector's placeholders for the first.
func TestTheCheckOfParametersSaysWhatItSaidWhileItFoundThePlaceholdersEachTime(t *testing.T) {
	cases := alloctest.UnlessRaced(4_000, 800)
	seen := map[string]int{}
	for seed := range cases {
		r := rand.New(rand.NewPCG(uint64(seed), 50))
		collectors := []model.Collector{paramsCorpusCollector(t, r), paramsCorpusCollector(t, r), paramsCorpusCollector(t, r)}
		var check RequestParamsCheck
		for round := range 8 {
			c := &collectors[r.IntN(len(collectors))]
			overrides := paramsCorpusOverrides(r)
			wasUnused, was := checkRequestParamsBefore(c, overrides)
			for _, way := range []struct {
				name  string
				check func(*model.Collector, RequestOverrides) ([]string, error)
			}{{"CheckRequestParams", CheckRequestParams}, {"RequestParamsCheck", check.CheckRequestParams}} {
				unused, err := way.check(c, overrides)
				if !slices.Equal(unused, wasUnused) || (err == nil) != (was == nil) || err != nil && (err.Error() != was.Error() || fmt.Sprintf("%T", err) != fmt.Sprintf("%T", was)) {
					t.Fatalf("case %d, check %d, %s of %+v with %+v:\n now %v, %v\n was %v, %v", seed, round, way.name, c.Request, overrides, unused, err, wasUnused, was)
				}
				var missing, wasMissing *MissingParamError
				if errors.As(err, &missing) != errors.As(was, &wasMissing) || missing != nil && *missing != *wasMissing {
					t.Fatalf("case %d, check %d, %s: the parameter missing is %+v, and was %+v", seed, round, way.name, missing, wasMissing)
				}
			}
			switch {
			case was == nil && len(wasUnused) == 0:
				seen["accepted"]++
			case was == nil:
				seen["parameters unused"]++
			case errors.As(was, new(*MissingParamError)):
				seen["a parameter missing"]++
			case strings.Contains(was.Error(), "placeholder"):
				seen["a placeholder refused"]++
			default:
				seen["a value refused"]++
			}
		}
	}
	// The corpus reaches what it is about: checks accepted, parameters
	// unused, and each kind of error.
	for _, what := range []string{"accepted", "parameters unused", "a parameter missing", "a placeholder refused", "a value refused"} {
		if seen[what] < cases/100 {
			t.Errorf("%d checks with %s among %d, want at least %d", seen[what], what, cases*8, cases/100)
		}
	}
}

// setKeysCorpusValue fills v, a settable value, with a generated value of
// its type: each field of a struct set or left zero, a pointer nil or to a
// zero or a set value, a map or a slice nil, empty or with one entry, and a
// string, a number or a bool zero or not. depth bounds how far it goes into
// nested types.
func setKeysCorpusValue(r *rand.Rand, v reflect.Value, depth int) {
	if depth > 3 || r.IntN(3) == 0 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() {
				setKeysCorpusValue(r, v.Field(i), depth+1)
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		setKeysCorpusValue(r, v.Elem(), depth+1)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		if r.IntN(2) == 0 {
			key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			setKeysCorpusValue(r, key, depth+1)
			setKeysCorpusValue(r, value, depth+1)
			v.SetMapIndex(key, value)
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), r.IntN(2), 1))
		if v.Len() > 0 {
			setKeysCorpusValue(r, v.Index(0), depth+1)
		}
	case reflect.String:
		v.SetString(pick(r, "x", "/status", ""))
	case reflect.Bool:
		v.SetBool(r.IntN(2) == 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(r.IntN(3)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(r.IntN(3)))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(r.IntN(3)))
	default:
		panic(fmt.Sprintf("setKeysCorpusValue: no value of kind %s", v.Kind()))
	}
}

// Over generated request blocks of a collector and of a static target, with
// every field set and left unset, a pointer nil and not, a map nil, empty
// and not, the keys set (setKeys) are those that were found while the
// struct tags were read for every value (setKeysBefore), in the same order,
// skipping a key or not, for a value and for one reached through a pointer.
func TestTheKeysARequestBlockSetsAreThoseFoundWhileTheTagsWereReadForEach(t *testing.T) {
	cases := alloctest.UnlessRaced(4_000, 800)
	keys := map[reflect.Type]map[string]bool{}
	for seed := range cases {
		r := rand.New(rand.NewPCG(uint64(seed), 51))
		for _, value := range []any{&model.RequestConfig{}, &model.TargetRequestConfig{}} {
			v := reflect.ValueOf(value).Elem()
			setKeysCorpusValue(r, v, -1)
			for _, skip := range [][]string{nil, {"type"}, {"path", "body"}} {
				was := setKeysBefore(reflect.ValueOf(v.Interface()), skip...)
				for _, now := range [][]string{setKeys(reflect.ValueOf(v.Interface()), skip...), setKeys(v, skip...)} {
					if !slices.Equal(now, was) {
						t.Fatalf("case %d, %T skipping %v: the keys set are %v, and were %v", seed, value, skip, now, was)
					}
				}
				if keys[v.Type()] == nil {
					keys[v.Type()] = map[string]bool{}
				}
				for _, key := range was {
					keys[v.Type()][key] = true
				}
			}
		}
	}
	// The corpus sets every key of each block in some case.
	for _, typ := range []reflect.Type{reflect.TypeFor[model.RequestConfig](), reflect.TypeFor[model.TargetRequestConfig]()} {
		for _, field := range yamlKeysOf(typ) {
			if !keys[typ][field.key] {
				t.Errorf("no case sets request.%s of %s", field.key, typ)
			}
		}
	}
}

// A RequestParamsCheck finds the placeholders of each collector once,
// however many checks it makes against it and whatever parts of the request
// each replaces; CheckRequestParams finds them once for each check, where it
// found them twice. And the yaml keys of a static target's request block are
// read from its type at most once, however many targets' blocks are
// checked, where they were read for each.
func TestThePlaceholdersOfACollectorAndTheKeysOfARequestBlockAreFoundOnceForManyTargets(t *testing.T) {
	collectors := []model.Collector{labelCollector(nil), labelCollector(nil), labelCollector(nil)}
	collectors[0].Request.Path = "/tenants/{{param_tenant}}/status"
	collectors[1].Request.Body = `{"t": {{param_tenant|json}}}`
	collectors[2].Request.Headers = map[string]string{"X-Tenant": "{{param_tenant:acme}}"}
	body := "{}"
	var parsed, read atomic.Int64
	countParsed, countRead := func() { parsed.Add(1) }, func() { read.Add(1) }
	placeholdersParsedHook.Store(&countParsed)
	yamlKeysReadHook.Store(&countRead)
	t.Cleanup(func() {
		placeholdersParsedHook.Store(nil)
		yamlKeysReadHook.Store(nil)
	})
	var check RequestParamsCheck
	const targets = 1000
	for i := range targets {
		overrides := RequestOverrides{Params: map[string]string{"param_tenant": fmt.Sprint("tenant-", i)}}
		if i%2 == 0 {
			overrides.PathSet, overrides.Path, overrides.Body = true, "/own", &body
		}
		c := &collectors[i%len(collectors)]
		if _, err := check.CheckRequestParams(c, overrides); err != nil && !errors.As(err, new(*MissingParamError)) {
			t.Fatal(err)
		}
	}
	if got := parsed.Load(); got != int64(len(collectors)) {
		t.Errorf("the placeholders were found %d times for %d checks of %d collectors, want once for each collector", got, targets, len(collectors))
	}
	parsed.Store(0)
	for i := range targets {
		_, _ = CheckRequestParams(&collectors[0], RequestOverrides{Params: map[string]string{"param_tenant": "acme"}})
		if got := parsed.Load(); got != int64(i+1) {
			t.Fatalf("the placeholders were found %d times for %d checks, want once for each", got, i+1)
		}
	}

	// A collector of a request type the build carries, whichever, since
	// the keys are read only for one of those.
	typed := labelCollector(nil)
	typed.Request.Type = slices.Sorted(maps.Keys(RequestTypes))[0]
	target := model.StaticTarget{Name: "t", Collector: typed.Name}
	target.Request.Timeout = model.Duration(time.Second)
	for range targets {
		if err := CheckTargetRequest(&target, &typed); err != nil {
			t.Fatal(err)
		}
	}
	if got := read.Load(); got > 1 {
		t.Errorf("the keys of a target's request block were read %d times for %d targets, want once at most", got, targets)
	}
}
