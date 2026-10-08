package fetch

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// Placeholders in a collector's fixed label values (labelparams.go).

// labelCollector is a collector with the given transform.labels and one
// rule, m, with the given labels.
func labelCollector(collectorWide map[string]string, labels ...model.LabelRule) model.Collector {
	return model.Collector{
		Name:      "tenants",
		Transform: model.TransformConfig{Type: "jq", Labels: collectorWide},
		Metrics:   []model.MetricRule{{Name: "m", Expression: ".v", Labels: labels}},
	}
}

// readLabels reads c's label values for placeholders, as the configuration
// does when it loads, naming a rule by its metric.
func readLabels(c *model.Collector) error {
	labels, err := ParseLabelParams(c, func(index int) string { return fmt.Sprintf("metric %q", c.Metrics[index].Name) })
	c.LabelParams = labels
	return err
}

// labelled is c with its label values read, which must be well formed.
func labelled(t *testing.T, c model.Collector) *model.Collector {
	t.Helper()
	if err := readLabels(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// The values of transform.labels and of a rule's static label are read for
// placeholders as a header value is: {{param_<name>}} with or without a
// default, any number among text, and `{{` opening one only where param_
// follows. A collector whose label values hold none has nothing kept for
// it. A label that reads its value with an expression is not static, and
// its value is not read.
func TestTheLabelValuesOfACollectorAreReadForPlaceholders(t *testing.T) {
	c := labelCollector(
		map[string]string{"tenant": "{{param_tenant}}", "region": "{{param_region:eu}}", "site": "dc1", "braces": "{{not_a_param}} {{ x }} }}"},
		model.LabelRule{Name: "source", Value: "api-{{param_tenant}}-{{param_zone:a}}{{param_suffix:}}"},
		model.LabelRule{Name: "fixed", Value: "x"},
		model.LabelRule{Name: "read", Expression: ".r", Value: "{{param_ignored}}"},
	)
	if err := readLabels(&c); err != nil {
		t.Fatal(err)
	}
	want := &model.LabelParams{
		Collector: []model.LabelTemplate{
			{Name: "region", Rule: -1, Label: -1, Where: "transform.labels.region", Text: "{{param_region:eu}}", Placeholders: []model.LabelPlaceholder{{Param: "param_region", Default: "eu", HasDefault: true, Start: 0, End: 19}}},
			{Name: "tenant", Rule: -1, Label: -1, Where: "transform.labels.tenant", Text: "{{param_tenant}}", Placeholders: []model.LabelPlaceholder{{Param: "param_tenant", Start: 0, End: 16}}},
		},
		Rules: []model.LabelTemplate{
			{Name: "source", Rule: 0, Label: 0, Where: `metric "m" label "source" value`, Text: "api-{{param_tenant}}-{{param_zone:a}}{{param_suffix:}}", Placeholders: []model.LabelPlaceholder{
				{Param: "param_tenant", Start: 4, End: 20},
				{Param: "param_zone", Default: "a", HasDefault: true, Start: 21, End: 37},
				{Param: "param_suffix", HasDefault: true, Start: 37, End: 54},
			}},
		},
	}
	if !reflect.DeepEqual(c.LabelParams, want) {
		t.Fatalf("read as\n%+v\nwant\n%+v", c.LabelParams, want)
	}
	if fields := TemplatedFields(&c); !slices.Equal(fields, []string{"transform.labels.region", "transform.labels.tenant"}) {
		t.Errorf("the fields that are filled: %v", fields)
	}
	if !TemplatedRuleLabel(&c, 0, 0) || TemplatedRuleLabel(&c, 0, 1) || TemplatedRuleLabel(&c, 0, 2) || TemplatedRuleLabel(&c, 1, 0) {
		t.Error("the rule labels that are filled are not the one that holds placeholders")
	}

	plain := labelCollector(map[string]string{"site": "dc1", "braces": "{{x}} {{ y }}"}, model.LabelRule{Name: "fixed", Value: "{{}}"}, model.LabelRule{Name: "read", Expression: ".r"})
	if err := readLabels(&plain); err != nil || plain.LabelParams != nil {
		t.Fatalf("a collector without a label placeholder: %+v, %v", plain.LabelParams, err)
	}
	if len(TemplatedFields(&plain)) != 0 || TemplatedRuleLabel(&plain, 0, 0) {
		t.Error("a collector without a label placeholder has one that is filled")
	}
}

// A placeholder of a label value that is not well formed is refused when
// the configuration loads, naming the collector and the value: written with
// a space after the braces, with a filter, unclosed, named otherwise than
// param_<name>, with a brace in its default, or with a default that is not
// valid UTF-8. Every such value of a collector is reported.
func TestALabelPlaceholderThatIsNotWellFormedIsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"a space":            {"{{ param_x }}", "has a placeholder with a space after {{; write {{param_<name>}} without spaces"},
		"a tab":              {"{{\tparam_x}}", "has a placeholder with a space after {{"},
		"a filter":           {"{{param_x|json}}", "placeholder {{param_x|json}} has a filter; a label value is written one way, as it is given, so write the placeholder without a |, which a default cannot hold either"},
		"a filter of none":   {"{{param_x:a|raw}}", "placeholder {{param_x:a|raw}} has a filter; a label value is written one way"},
		"a bar in a default": {"{{param_x:a|b}}", "placeholder {{param_x:a|b}} has a filter; a label value is written one way"},
		"unclosed":           {"api-{{param_x", `has an unclosed placeholder at "{{param_x"; write {{param_name}} or {{param_name:default}}`},
		"a bad name":         {"{{param_x-y}}", "placeholder {{param_x-y}} is not a path parameter"},
		"no name":            {"{{param_}}", "placeholder {{param_}} is not a path parameter"},
		"a brace in default": {"{{param_x:${X}}}", "has a default containing a brace; if it is an environment reference, run with --config.expand-env"},
		"a default not text": {"{{param_x:a\xffb}}", ": the default of param_x is not valid UTF-8, which a label value must be; change the default"},
	} {
		wide := labelCollector(map[string]string{"tenant": tc.value})
		if err := readLabels(&wide); err == nil || !strings.HasPrefix(err.Error(), `collector "tenants" transform.labels.tenant`) || !strings.Contains(err.Error(), tc.want) || wide.LabelParams != nil {
			t.Errorf("%s, in transform.labels: %v, want %q", name, err, tc.want)
		}
		rule := labelCollector(nil, model.LabelRule{Name: "tenant", Value: tc.value})
		if err := readLabels(&rule); err == nil || !strings.HasPrefix(err.Error(), `collector "tenants" metric "m" label "tenant" value`) || !strings.Contains(err.Error(), tc.want) || rule.LabelParams != nil {
			t.Errorf("%s, in a rule's label: %v, want %q", name, err, tc.want)
		}
	}
	both := labelCollector(map[string]string{"tenant": "{{ param_x }}"}, model.LabelRule{Name: "source", Value: "{{param_y|json}}"})
	err := readLabels(&both)
	if err == nil || !strings.Contains(err.Error(), "transform.labels.tenant has a placeholder with a space") || !strings.Contains(err.Error(), `metric "m" label "source" value placeholder {{param_y|json}} has a filter`) {
		t.Errorf("two values that are not well formed: %v", err)
	}
	// What the request's own placeholders say of a filter is as it was.
	if _, err := parsePlaceholders("request.headers.X", "{{param_x|json}}", false, false); err == nil || err.Error() != "request.headers.X placeholder {{param_x|json}} has a filter; filters apply only in request.body, and a path, header or query value is always encoded one way" {
		t.Errorf("a filter in a header: %v", err)
	}
}

// A transform reads a collector whose label values hold placeholders as a
// copy with them filled in: by the probe's value, by the default when the
// probe gives none or an empty one, written as given with nothing escaped,
// among the value's own text. An empty default fills nothing in.
func TestTheLabelValuesAreFilledForATransform(t *testing.T) {
	c := labelled(t, labelCollector(
		map[string]string{"tenant": "{{param_tenant}}", "region": "{{param_region:eu}}", "site": "dc1", "braces": "{{not_a_param}}"},
		model.LabelRule{Name: "fixed", Value: "x"},
		model.LabelRule{Name: "source", Value: "api-{{param_tenant}}-{{param_zone:a}}{{param_suffix:}}", Truncate: true},
		model.LabelRule{Name: "read", Expression: ".r"},
	))
	for name, tc := range map[string]struct {
		params                 map[string]string
		tenant, region, source string
	}{
		"the value and the defaults":   {map[string]string{"param_tenant": "acme"}, "acme", "eu", "api-acme-a"},
		"every value":                  {map[string]string{"param_tenant": "acme", "param_region": "us", "param_zone": "b", "param_suffix": "!"}, "acme", "us", "api-acme-b!"},
		"an empty value is not given":  {map[string]string{"param_tenant": "acme", "param_region": "", "param_zone": ""}, "acme", "eu", "api-acme-a"},
		"written as given":             {map[string]string{"param_tenant": "a \"b\"\\\n{{param_region}} é", "param_region": "{x}"}, "a \"b\"\\\n{{param_region}} é", "{x}", "api-a \"b\"\\\n{{param_region}} é-a"},
		"a value of blanks is a value": {map[string]string{"param_tenant": "  "}, "  ", "eu", "api-  -a"},
	} {
		filled, err := FilledLabels(c, tc.params)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		wantLabels := map[string]string{"tenant": tc.tenant, "region": tc.region, "site": "dc1", "braces": "{{not_a_param}}"}
		wantRule := []model.LabelRule{{Name: "fixed", Value: "x"}, {Name: "source", Value: tc.source, Truncate: true}, {Name: "read", Expression: ".r"}}
		if !reflect.DeepEqual(filled.Transform.Labels, wantLabels) || !reflect.DeepEqual(filled.Metrics[0].Labels, wantRule) {
			t.Errorf("%s: filled as %v and %+v", name, filled.Transform.Labels, filled.Metrics[0].Labels)
		}
		if filled == c || filled.LabelParams != nil || filled.Name != c.Name || filled.Metrics[0].Expression != ".v" {
			t.Errorf("%s: the copy is not the collector with its labels filled", name)
		}
	}
	if _, err := FilledLabels(c, nil); err == nil || err.Error() != "transform.labels.tenant needs param_tenant, which the probe did not supply and which has no default; add &param_tenant=<value> to the probe, or give it a default as {{param_tenant:<default>}}" {
		t.Errorf("no value and no default: %v", err)
	}
	var missing *MissingParamError
	if _, err := FilledLabels(c, map[string]string{"param_tenant": ""}); !errors.As(err, &missing) || missing.Name != "param_tenant" {
		t.Errorf("an empty value and no default: %v", err)
	}
	if _, err := FilledLabels(c, map[string]string{"param_tenant": "a\xffb"}); err == nil || err.Error() != "transform.labels.tenant: the value of param_tenant is not valid UTF-8, which a label value must be" {
		t.Errorf("a value that is not UTF-8: %v", err)
	}
}

// A label whose filled value is empty is the label left out. A
// transform.labels value of "" sets nothing, as one written "" does; a
// rule's label is taken out of the copy's rule, so the rule gives its series
// no such label, and the labels beside it stay in their order.
func TestALabelFilledToNothingIsLeftOut(t *testing.T) {
	c := labelled(t, labelCollector(
		map[string]string{"tenant": "{{param_tenant:}}", "site": "dc1"},
		model.LabelRule{Name: "first", Value: "{{param_first:}}"},
		model.LabelRule{Name: "fixed", Value: "x"},
		model.LabelRule{Name: "pair", Value: "{{param_a:}}{{param_b:}}"},
		model.LabelRule{Name: "last", Value: "{{param_last:}}", Truncate: true},
	))
	filled, err := FilledLabels(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(filled.Transform.Labels, map[string]string{"tenant": "", "site": "dc1"}) {
		t.Errorf("transform.labels filled as %v", filled.Transform.Labels)
	}
	if !reflect.DeepEqual(filled.Metrics[0].Labels, []model.LabelRule{{Name: "fixed", Value: "x"}}) {
		t.Errorf("the rule's labels filled as %+v", filled.Metrics[0].Labels)
	}
	filled, err = FilledLabels(c, map[string]string{"param_b": "b", "param_last": "z"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []model.LabelRule{{Name: "fixed", Value: "x"}, {Name: "pair", Value: "b"}, {Name: "last", Value: "z", Truncate: true}}; !reflect.DeepEqual(filled.Metrics[0].Labels, want) {
		t.Errorf("the rule's labels filled as %+v", filled.Metrics[0].Labels)
	}
}

// Filling writes nothing into the collector, which the probes of a
// collector share and fill at the same time, each with its own values: the
// collector reads afterwards as it was read before, and every probe gets
// its own values.
func TestFillingLabelsLeavesTheSharedCollectorAsItWas(t *testing.T) {
	build := func() model.Collector {
		c := labelCollector(
			map[string]string{"tenant": "{{param_tenant}}", "site": "dc1"},
			model.LabelRule{Name: "source", Value: "api-{{param_tenant}}"},
			model.LabelRule{Name: "gone", Value: "{{param_gone:}}"},
		)
		c.Metrics = append(c.Metrics, model.MetricRule{Name: "n", Expression: ".n", Labels: []model.LabelRule{{Name: "fixed", Value: "x"}}})
		return c
	}
	c, pristine := labelled(t, build()), labelled(t, build())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", i)
			for range 50 {
				filled, err := FilledLabels(c, map[string]string{"param_tenant": tenant})
				if err != nil || filled.Transform.Labels["tenant"] != tenant || filled.Metrics[0].Labels[0].Value != "api-"+tenant || len(filled.Metrics[0].Labels) != 1 {
					t.Errorf("%s: filled as %+v, %v", tenant, filled, err)
					return
				}
				// What a transform does with its copy is its own.
				filled.Transform.Labels["site"] = tenant
				filled.Metrics[1].Name = tenant
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(c, pristine) {
		t.Fatalf("the collector reads\n%+v\nand was\n%+v", c, pristine)
	}
}

// Labels that are no longer those the placeholders were read from are not
// filled: the copy would hold another label's value, or none.
func TestLabelsThatChangedSinceTheyWereReadAreNotFilled(t *testing.T) {
	for name, change := range map[string]func(c *model.Collector){
		"a transform.labels value rewritten": func(c *model.Collector) { c.Transform.Labels = map[string]string{"tenant": "x"} },
		"a transform.labels value taken out": func(c *model.Collector) { c.Transform.Labels = nil },
		"a rule's label rewritten":           func(c *model.Collector) { c.Metrics[0].Labels = []model.LabelRule{{Name: "source", Value: "x"}} },
		"a rule's label taken out":           func(c *model.Collector) { c.Metrics[0].Labels = nil },
		"the rules taken out":                func(c *model.Collector) { c.Metrics = nil },
	} {
		c := labelled(t, labelCollector(map[string]string{"tenant": "{{param_tenant:a}}"}, model.LabelRule{Name: "source", Value: "{{param_tenant:a}}"}))
		c.Transform.Labels = map[string]string{"tenant": "{{param_tenant:a}}"}
		change(c)
		if _, err := FilledLabels(c, nil); !errors.Is(err, errLabelsChanged) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A probe parameter that fills a label value is used, whatever the request
// names and whatever of it the probe replaced: with the path or the body,
// which take away the request's use of a parameter and not a label's. One
// no place uses is still unused. A label's placeholder with no value and no
// default is the probe's mistake, as the request's is, an empty value is no
// value, and a value that is not valid UTF-8 is refused naming the
// parameter.
func TestAParameterThatFillsALabelIsUsed(t *testing.T) {
	body := "{}"
	c := labelCollector(map[string]string{"tenant": "{{param_tenant}}", "region": "{{param_region:eu}}"}, model.LabelRule{Name: "source", Value: "api-{{param_source:x}}"})
	c.Request.Path = "/api/{{param_tenant}}/{{param_version:2}}"
	c.Request.Body = `{"service": {{param_service:web|json}}}`
	labelledCollector := labelled(t, c)
	for name, tc := range map[string]struct {
		overrides RequestOverrides
		unused    []string
		err       string
	}{
		"the request and the labels alike":      {RequestOverrides{Params: map[string]string{"param_tenant": "acme"}}, nil, ""},
		"every parameter":                       {RequestOverrides{Params: map[string]string{"param_tenant": "acme", "param_region": "us", "param_source": "s", "param_version": "3", "param_service": "db"}}, nil, ""},
		"one no place uses":                     {RequestOverrides{Params: map[string]string{"param_tenant": "acme", "param_tenat": "acme"}}, []string{"param_tenat"}, ""},
		"a path that replaces the request's":    {RequestOverrides{PathSet: true, Path: "/x", Params: map[string]string{"param_tenant": "acme", "param_region": "us"}}, nil, ""},
		"what only the replaced path used":      {RequestOverrides{PathSet: true, Path: "/x", Params: map[string]string{"param_tenant": "acme", "param_version": "3"}}, []string{"param_version"}, ""},
		"what only the replaced body used":      {RequestOverrides{Body: &body, Params: map[string]string{"param_tenant": "acme", "param_service": "db"}}, []string{"param_service"}, ""},
		"no value for the label":                {RequestOverrides{PathSet: true, Path: "/x"}, nil, "transform.labels.tenant needs param_tenant, which the probe did not supply and which has no default"},
		"an empty value for the label":          {RequestOverrides{PathSet: true, Path: "/x", Params: map[string]string{"param_tenant": ""}}, nil, "transform.labels.tenant needs param_tenant"},
		"the request's placeholder comes first": {RequestOverrides{}, nil, "request.path needs param_tenant"},
		"a value that is not UTF-8":             {RequestOverrides{PathSet: true, Path: "/x", Params: map[string]string{"param_tenant": "a\xffb"}}, nil, "transform.labels.tenant: the value of param_tenant is not valid UTF-8, which a label value must be"},
		"a rule's value that is not UTF-8":      {RequestOverrides{Params: map[string]string{"param_tenant": "acme", "param_source": "\xc3"}}, nil, `metric "m" label "source" value: the value of param_source is not valid UTF-8`},
	} {
		unused, err := CheckRequestParams(labelledCollector, tc.overrides)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: %v, want %q", name, err, tc.err)
			}
			continue
		}
		if err != nil || !slices.Equal(unused, tc.unused) {
			t.Errorf("%s: unused %v, %v; want %v", name, unused, err, tc.unused)
		}
	}
	// A parameter only a label uses is used by a collector whose request
	// names none, and the refusal of one nothing uses says where it looked.
	only := labelled(t, labelCollector(map[string]string{"tenant": "{{param_tenant}}"}))
	if err := CheckPathParams(only, RequestOverrides{Params: map[string]string{"param_tenant": "acme"}}); err != nil {
		t.Errorf("a parameter only a label uses: %v", err)
	}
	err := CheckPathParams(only, RequestOverrides{Params: map[string]string{"param_tenant": "acme", "param_tenat": "x"}})
	if err == nil || err.Error() != `probe parameters param_tenat are not used by collector "tenants": no placeholder in its request.path (""), body, header or query values or its label values names them` {
		t.Errorf("a parameter nothing uses: %v", err)
	}
	err = CheckPathParams(only, RequestOverrides{PathSet: true, Params: map[string]string{"param_tenant": "acme", "param_tenat": "x"}})
	if err == nil || err.Error() != "probe parameters param_tenat are not used: the path probe parameter replaces request.path, and nothing else in the request and no label value of the collector names them" {
		t.Errorf("a parameter nothing uses beside a path: %v", err)
	}
	// The parameters a collector takes list those of its labels with the
	// request's: required when one placeholder of the name has no default.
	params := RequestParams(labelledCollector)
	want := []RequestParam{
		{Name: "param_region", Default: "eu"}, {Name: "param_service", Default: "web"}, {Name: "param_source", Default: "x"},
		{Name: "param_tenant", Required: true}, {Name: "param_version", Default: "2"},
	}
	if !reflect.DeepEqual(params, want) {
		t.Errorf("the parameters of the collector: %+v", params)
	}
}

// checkRequestParamsBeforeLabels is CheckRequestParams as it was before a
// label value took placeholders.
func checkRequestParamsBeforeLabels(c *model.Collector, overrides RequestOverrides) (unused []string, err error) {
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
	for name := range overrides.Params {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	return unused, nil
}

// requestParamNamesBefore is requestParamNames, which listed the parameters
// a collector's request uses, as it was before the check of a static target
// file found each collector's placeholders once (RequestParamsCheck).
func requestParamNamesBefore(c *model.Collector, overrides RequestOverrides) (map[string]bool, error) {
	used := map[string]bool{}
	if !overrides.PathSet && HasPathParams(c.Request.Path) {
		placeholders, err := parsePathParams(c.Request.Path)
		if err != nil {
			return nil, err
		}
		for _, p := range placeholders {
			used[p.Name] = true
		}
	}
	for _, f := range requestTemplates(c, overrides) {
		placeholders, err := f.parse()
		if err != nil {
			return nil, err
		}
		for _, p := range placeholders {
			used[p.Name] = true
		}
	}
	return used, nil
}

// A collector whose label values hold no placeholder is checked and read as
// it was: over collectors with placeholders in every part of a request, and
// in none, and every combination of parameters and replaced parts, the check
// of a probe's parameters gives what it gave, in as many allocations, the
// parameters listed are those of the request, and a transform reads the
// collector itself, for which nothing is allocated.
func TestACollectorWithoutLabelPlaceholdersIsCheckedAsItWas(t *testing.T) {
	body, message := "{}", "{}"
	var collectors []model.Collector
	for _, path := range []string{"", "/status", "/api/{{param_tenant}}/v{{param_version:2}}", "/api/{{param_tenant"} {
		for _, templated := range []bool{false, true} {
			c := labelCollector(map[string]string{"site": "dc1", "braces": "{{x}}"}, model.LabelRule{Name: "fixed", Value: "{{}} x"}, model.LabelRule{Name: "read", Expression: ".r"})
			c.Request.Path = path
			if templated {
				c.Request.Body = `{"service": {{param_service|json}}, "limit": {{param_limit:10|number}}}`
				c.Request.Headers = map[string]string{"X-Tenant": "{{param_tenant}}", "Accept": "text/plain"}
				c.Request.Query = map[string]string{"region": "{{param_region:eu}}"}
				c.Request.Message = `{"queue": {{param_queue:orders|json}}}`
				c.Request.Metadata = map[string]string{"x-tenant": "{{param_meta:default}}"}
				c.Request.Targets = []string{"app.{{param_host:web}}.requests", "cpu.load"}
			}
			collectors = append(collectors, c)
		}
	}
	paramSets := []map[string]string{
		nil, {}, {"param_tenant": "acme"}, {"param_tenant": ""}, {"param_tenant": "acme", "param_service": "db"},
		{"param_tenant": "acme", "param_service": "db", "param_limit": "ten"}, {"param_tenant": "..", "param_service": "db"},
		{"param_tenant": "a\nb", "param_service": "db"}, {"param_tenant": "a\xffb", "param_service": "db"},
		{"param_tenant": "acme", "param_service": "db", "param_tenat": "x", "param_site": "y"}, {"param_host": "web*", "param_tenant": "acme", "param_service": "db"},
		{"param_tenant": "acme", "param_service": "db", "param_region": "us", "param_queue": "q", "param_meta": "m", "param_host": "h", "param_version": "3", "param_limit": "5"},
	}
	compared := 0
	for i := range collectors {
		c := &collectors[i]
		if err := readLabels(c); err != nil || c.LabelParams != nil {
			t.Fatalf("collector %d has label placeholders: %+v, %v", i, c.LabelParams, err)
		}
		if filled, err := FilledLabels(c, map[string]string{"param_tenant": "acme"}); filled != c || err != nil {
			t.Errorf("collector %d is read as a copy: %v", i, err)
		}
		for _, params := range paramSets {
			for _, overrides := range []RequestOverrides{{}, {PathSet: true, Path: "/x"}, {Body: &body}, {Message: &message}, {Targets: []string{"a.b"}}, {PathSet: true, Body: &body, Message: &message}} {
				overrides.Params = params
				unused, err := CheckRequestParams(c, overrides)
				wasUnused, wasErr := checkRequestParamsBeforeLabels(c, overrides)
				if !slices.Equal(unused, wasUnused) || (err == nil) != (wasErr == nil) || err != nil && err.Error() != wasErr.Error() {
					t.Errorf("collector %d, parameters %v, overrides %+v:\n now %v, %v\n was %v, %v", i, params, overrides, unused, err, wasUnused, wasErr)
				}
				compared++
			}
		}
	}
	if compared != 8*12*6 {
		t.Fatalf("%d checks compared", compared)
	}
	c := &collectors[5]
	overrides := RequestOverrides{Params: map[string]string{"param_tenant": "acme", "param_service": "db", "param_tenat": "x"}}
	// The check must not allocate more than it did. Under the race detector
	// either count is seen to be 63 in some measurements and 64 in others,
	// so there the new one may be one over the least seen of the old one,
	// and without the detector not at all.
	before, _ := alloctest.Allocations(100, func() { _, _ = checkRequestParamsBeforeLabels(c, overrides) })
	most := before + alloctest.UnlessRaced(0.0, 1.0)
	if now := alloctest.AllocsAtMost(100, most, func() { _, _ = CheckRequestParams(c, overrides) }); now > most {
		t.Errorf("the check of a probe's parameters allocates %.0f times, and before %.0f", now, before)
	}
	if allocs := alloctest.AllocsAtMost(100, 0, func() { _, _ = FilledLabels(c, overrides.Params) }); allocs != 0 {
		t.Errorf("reading a collector without label placeholders allocates %.0f times", allocs)
	}
}

// What filling costs a collector that has label placeholders is modest: the
// copy of the collector, its transform.labels, and the rules with the
// labels of the one that has a filled label. A value that is one
// placeholder and nothing else is the parameter's value itself, and is not
// written anew.
func TestFillingLabelsAllocatesTheCopyAndLittleElse(t *testing.T) {
	c := labelled(t, labelCollector(map[string]string{"tenant": "{{param_tenant}}", "region": "{{param_region:eu}}"}, model.LabelRule{Name: "source", Value: "{{param_tenant}}"}))
	params := map[string]string{"param_tenant": "acme"}
	// The collector, the map of transform.labels, the rules, and the labels
	// of the rule.
	if allocs := alloctest.AllocsAtMost(100, 5, func() { _, _ = FilledLabels(c, params) }); allocs > 5 {
		t.Errorf("filling the labels allocates %.0f times, more than 5", allocs)
	}
	if allocs := alloctest.AllocsAtMost(100, 0, func() { _, _ = labelValue(&c.LabelParams.Collector[1], params) }); allocs != 0 {
		t.Errorf("a value that is one placeholder allocates %.0f times", allocs)
	}
}

// A value that holds placeholders is measured, when the configuration
// loads, as a probe that gives no parameter fills it: each placeholder
// replaced by its default, and by nothing where it has none. A value that
// holds none is not one of those, and is measured as written.
func TestALabelValueIsMeasuredWithItsDefaults(t *testing.T) {
	c := labelled(t, labelCollector(
		map[string]string{"tenant": "t-{{param_tenant}}-{{param_zone:eu}}", "site": "dc1"},
		model.LabelRule{Name: "fixed", Value: "x"},
		model.LabelRule{Name: "source", Value: "{{param_a:one}}{{param_b}}{{param_c:}}-end"},
	))
	if least, templated := CollectorLabelDefaults(c, "tenant", c.Transform.Labels["tenant"]); least != "t--eu" || !templated {
		t.Errorf("transform.labels.tenant: %q, %v", least, templated)
	}
	if least, templated := CollectorLabelDefaults(c, "site", "dc1"); least != "dc1" || templated {
		t.Errorf("transform.labels.site: %q, %v", least, templated)
	}
	if least, templated := RuleLabelDefaults(c, 0, 1, c.Metrics[0].Labels[1].Value); least != "one-end" || !templated {
		t.Errorf("the rule's source: %q, %v", least, templated)
	}
	if least, templated := RuleLabelDefaults(c, 0, 0, "x"); least != "x" || templated {
		t.Errorf("the rule's fixed: %q, %v", least, templated)
	}
	plain := labelCollector(map[string]string{"site": "dc1"})
	if least, templated := CollectorLabelDefaults(&plain, "site", "dc1"); least != "dc1" || templated {
		t.Errorf("a collector without placeholders: %q, %v", least, templated)
	}
}
