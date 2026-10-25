//go:build !select_request_types || request_type_http

package fetch

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

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
	c.Request.Type, c.Request.Path = RequestTypeHTTP, "/api/{{param_tenant}}/{{param_version:2}}"
	editHTTP(&c, func(r *HTTPConfig) { r.Body = `{"service": {{param_service:web|json}}}` })
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
