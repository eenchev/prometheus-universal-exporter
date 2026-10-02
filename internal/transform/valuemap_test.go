package transform

import (
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// value_map and scale work the same in every transform with rules.
func TestValueMapAndScaleInEveryTransform(t *testing.T) {
	states := map[string]float64{"up": 1, "degraded": 0.5, "down": 0, "*": -1}
	milli := 0.001
	for name, tc := range map[string]struct {
		decoder, transform, contentType, body string
		rules                                 []model.MetricRule
	}{
		"regex": {"text", "regex", "text/plain", "state: degraded\nstate: gone\nlatency: 250\n", []model.MetricRule{
			{Name: "state", Expression: `state: (\w+)`, ValueMap: states},
			{Name: "latency_seconds", Expression: `latency: (\d+)`, Scale: &milli},
		}},
		"css": {"html", "css", "text/html", `<p class="s">degraded</p><p class="l">250</p>`, []model.MetricRule{
			{Name: "state", Expression: "p.s", ValueMap: states},
			{Name: "latency_seconds", Expression: "p.l", Scale: &milli},
		}},
		"xpath": {"xml", "xpath", "application/xml", `<r><s>degraded</s><l>250</l></r>`, []model.MetricRule{
			{Name: "state", Expression: "/r/s", ValueMap: states},
			{Name: "latency_seconds", Expression: "number(/r/l)", Scale: &milli},
		}},
		"csv": {"csv", "csv", "text/csv", "state,latency\ndegraded,250\n", []model.MetricRule{
			{Name: "state", Expression: "state", ValueMap: states},
			{Name: "latency_seconds", Expression: "latency", Scale: &milli},
		}},
		"jq": {"json", "jq", "application/json", `{"state": "degraded", "latency": 250, "ok": true}`, []model.MetricRule{
			{Name: "state", Expression: ".state", ValueMap: states},
			{Name: "latency_seconds", Expression: ".latency", Scale: &milli},
			{Name: "ok", Expression: ".ok", ValueMap: map[string]float64{"true": 2, "false": 3}},
		}},
		"yq": {"yaml", "yq", "application/yaml", "state: degraded\nlatency: 250\n", []model.MetricRule{
			{Name: "state", Expression: ".state", ValueMap: states},
			{Name: "latency_seconds", Expression: ".latency", Scale: &milli},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			for i := range tc.rules {
				tc.rules[i].Type = model.GaugeMetricType
			}
			c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: tc.decoder}, Transform: model.TransformConfig{Type: tc.transform}, Metrics: tc.rules}
			for i := range c.Metrics {
				if err := CheckMetricRule(&c, &c.Metrics[i]); err != nil {
					t.Fatal(err)
				}
			}
			set, err := runBody(t, c, tc.contentType, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			var states []float64
			for _, m := range set.Metrics {
				switch m.Name {
				case "state":
					states = append(states, m.Value)
				case "latency_seconds":
					if m.Value != 0.25 {
						t.Errorf("latency %v, want 0.25", m.Value)
					}
				case "ok":
					if m.Value != 2 {
						t.Errorf("ok %v, want 2", m.Value)
					}
				}
			}
			if len(states) == 0 || states[0] != 0.5 {
				t.Fatalf("states %v, want 0.5 first", states)
			}
			if name == "regex" && (len(states) != 2 || states[1] != -1) {
				t.Fatalf("an unlisted state %v, want the * default, -1", states)
			}
		})
	}
}

// Without "*", a value the map does not list is read as a number, and one
// that is not a number fails naming value_map.
func TestValueMapWithoutADefault(t *testing.T) {
	c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "state", Type: model.GaugeMetricType, Expression: ".state", ValueMap: map[string]float64{"up": 1}, ErrorMode: model.ErrorModeFail}}}
	set, err := runBody(t, c, "application/json", `{"state": " 7 "}`)
	if err != nil || set.Metrics[0].Value != 7 {
		t.Fatalf("%v %v", err, set)
	}
	if _, err := runBody(t, c, "application/json", `{"state": "sideways"}`); err == nil || !strings.Contains(err.Error(), "neither in value_map nor a number") {
		t.Fatalf("err=%v", err)
	}
	set, err = runBody(t, c, "application/json", `{"state": " up "}`)
	if err != nil || set.Metrics[0].Value != 1 {
		t.Fatalf("a value is looked up without its blanks: %v %v", err, set)
	}
}

// A scale that is exactly one over a whole number, as 0.001 is written,
// divides by that number, so the value is the nearest float to the exact
// quotient, not a product off by the representation error of 0.001 or
// 0.000000001. Any other scale multiplies.
//
// "One over a whole number" was taken within a relative tolerance of 1e-9,
// which every scale below about 2e-9 is within and 0.3333333333 too: 2e9
// scaled by 1.5e-9 gave 2.9999999985 and 3e10 scaled by 0.3333333333 gave
// 1e10. Only the scale that is the float64 nearest to the reciprocal
// divides; one that comes close to it multiplies, as on main.
func TestAScaleOfOneOverAWholeNumberDivides(t *testing.T) {
	for _, c := range []struct{ value, scale, want float64 }{
		{412, 0.001, 0.412},
		{3121894012, 0.000000001, 3.121894012},
		{3121894012, 1e-9, 3.121894012},
		{17.5, 0.001, 0.0175},
		{7, -0.01, -0.07},
		{7, 0.5, 3.5},
		{7, 0.25, 1.75},
		{7, 0.1, 0.7},
		{7, 0.2, 1.4},
		{0.25, 100, 25},
		{5, 0.4, 2},
		{5, 2.5, 12.5},
		{2e9, 1.5e-9, 3},
		{3e10, 0.3333333333, 9999999999},
	} {
		if got := scaled(model.MetricRule{Scale: &c.scale}, c.value); got != c.want {
			t.Errorf("%v scaled by %v = %v, want %v", c.value, c.scale, got, c.want)
		}
	}
	// Which scales divide, and by what: with a value the quotient and the
	// product differ for, the result says which was taken.
	for _, c := range []struct{ scale, whole float64 }{{0.001, 1000}, {0.000000001, 1e9}, {1e-9, 1e9}, {-0.01, -100}, {0.5, 2}, {0.25, 4}, {0.1, 10}, {0.2, 5}} {
		for _, value := range []float64{412, 3121894012, 17.5, 7, 3, 1e-300, -5.5e20} {
			if got := scale(value, c.scale); got != value/c.whole {
				t.Errorf("%v scaled by %v = %v, want it divided by %v: %v", value, c.scale, got, c.whole, value/c.whole)
			}
		}
	}
	for _, scaleBy := range []float64{1.5e-9, 7e-11, 3e-10, 0.3333333333, 0.9999999995, 0.3, 2.5, 100, 1, -1, 7.5e-10, 2.78e-13, 0.50000000099, 5e-324, 1e308, math.MaxFloat64} {
		for _, value := range []float64{2e9, 3e10, 1e10, 412, 7, 3, 5} {
			if got := scale(value, scaleBy); got != value*scaleBy {
				t.Errorf("%v scaled by %v = %v, want the product %v", value, scaleBy, got, value*scaleBy)
			}
		}
	}
	// Every exact reciprocal divides by its whole number: small ones, powers
	// of ten and of two, and whole numbers at random up to 2^50.
	wholes := []float64{-1e9, -1000, -3, -2}
	for n := 2.0; n <= 5000; n++ {
		wholes = append(wholes, n)
	}
	for n := 10.0; n <= 1e15; n *= 10 {
		wholes = append(wholes, n, n*3, n-1)
	}
	for n := 4.0; n <= 1<<50; n *= 2 {
		wholes = append(wholes, n, n+1, n-1)
	}
	random := rand.New(rand.NewPCG(20261002, 6))
	for range 20000 {
		wholes = append(wholes, float64(2+random.Int64N(1<<50)))
	}
	for _, whole := range wholes {
		for _, value := range []float64{412, 3121894012, 0.3, 1 + random.Float64()*1e6} {
			if got := scale(value, 1/whole); got != value/whole {
				t.Fatalf("%v scaled by 1/%v = %v, want %v", value, whole, got, value/whole)
			}
		}
	}
}

// scale multiplies a prometheus rule's samples, and refuses a histogram;
// value_map, and either on a python transform, is refused at load, as is a
// scale of 0.
func TestValueRulesPerTransform(t *testing.T) {
	half := 0.5
	// Each body holds one of the two metrics, so neither rule is required.
	optional := false
	c := model.Collector{Name: "p", Decoder: model.DecoderConfig{Type: "prometheus"}, Transform: model.TransformConfig{Type: "prometheus"},
		Metrics: []model.MetricRule{{Expression: "^up$", Scale: &half, Required: &optional}, {Expression: "^h$", Scale: &half, ErrorMode: model.ErrorModeFail, Required: &optional}}}
	set, err := runBody(t, c, "text/plain", "# TYPE up gauge\nup 4\n")
	if err != nil || len(set.Metrics) != 1 || set.Metrics[0].Value != 2 {
		t.Fatalf("%v %+v", err, set)
	}
	if _, err := runBody(t, c, "text/plain", "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_sum 3\nh_count 2\n"); err == nil || !strings.Contains(err.Error(), "scale cannot apply") {
		t.Fatalf("err=%v", err)
	}
	zero := 0.0
	for transformType, rule := range map[string]model.MetricRule{
		"prometheus": {Name: "x", Expression: "x", ValueMap: map[string]float64{"a": 1}},
		"python":     {Name: "x", Scale: &half},
		"jq":         {Name: "x", Expression: ".x", Scale: &zero},
	} {
		x := model.Collector{Name: "c", Transform: model.TransformConfig{Type: transformType}}
		if err := CheckMetricRule(&x, &rule); err == nil {
			t.Errorf("%s: %+v accepted", transformType, rule)
		}
	}
	blank := model.Collector{Name: "c", Transform: model.TransformConfig{Type: "jq"}}
	if err := CheckMetricRule(&blank, &model.MetricRule{Name: "x", Expression: ".x", ValueMap: map[string]float64{" up": 1}}); err == nil {
		t.Error("a key with blanks was accepted")
	}
}

// $status is the HTTP status, a grpc call's status code, or null for an
// answer without one, such as a local file's; Python's
// response.status_code is the same.
func TestTheStatusRulesRead(t *testing.T) {
	grpcNotFound := 5
	for name, tc := range map[string]struct {
		response *fetch.HTTPResponse
		want     string
	}{
		"http":      {&fetch.HTTPResponse{StatusCode: 503}, "503"},
		"grpc":      {&fetch.HTTPResponse{StatusCode: 200, GRPCCode: &grpcNotFound}, "5"},
		"localfile": {&fetch.HTTPResponse{StatusCode: 200, NoStatus: true}, "none"},
	} {
		t.Run(name, func(t *testing.T) {
			c := model.Collector{Name: "s", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
				Metrics: []model.MetricRule{{Name: "up", Type: model.GaugeMetricType, Expression: "1", Labels: []model.LabelRule{{Name: "status", Expression: `$status // "none" | tostring`}}}}}
			r := tc.response
			r.Body, r.Headers = []byte("{}"), http.Header{"Content-Type": {"application/json"}}
			d, err := decode.Decode(r, &c)
			if err != nil {
				t.Fatal(err)
			}
			set, err := Transform(t.Context(), d, r, &c, "python3")
			if err != nil || set.Metrics[0].Labels["status"] != tc.want {
				t.Fatalf("%v %+v", err, set)
			}
			requirePython(t)
			py := model.Collector{Name: "py", Request: model.RequestConfig{Type: fetch.RequestTypeHTTP}, Transform: model.TransformConfig{Type: "python", Script: `metric(name="up", value=1, labels={"status": "none" if response.status_code is None else response.status_code})`},
				Limits: model.Limits{ScriptTimeout: model.Duration(2 * time.Second), MaxOutputBytes: 1 << 20}}
			set, err = executePython(t.Context(), "python3", py.Transform.Script, d, r, &py)
			if err != nil || set.Metrics[0].Labels["status"] != tc.want {
				t.Fatalf("python: %v %+v", err, set)
			}
		})
	}
}

// A label's value_map turns the value its expression gives into another,
// "*" catches the rest, and a value mapped to "" leaves the label off.
func TestLabelValueMaps(t *testing.T) {
	states := map[string]string{"1": "running", "2": "stopped", "0": "", "*": "unknown"}
	c := model.Collector{Name: "l", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "worker_up", Type: model.GaugeMetricType, Items: ".workers[]", Expression: "1", Labels: []model.LabelRule{
			{Name: "name", Expression: ".name"},
			{Name: "state", Expression: ".state", ValueMap: states},
		}}}}
	for i := range c.Metrics {
		if err := CheckMetricRule(&c, &c.Metrics[i]); err != nil {
			t.Fatal(err)
		}
	}
	set, err := runBody(t, c, "application/json", `{"workers": [{"name": "a", "state": 1}, {"name": "b", "state": "2"}, {"name": "c", "state": 9}, {"name": "d", "state": 0}]}`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range set.Metrics {
		state, ok := m.Labels["state"]
		if !ok {
			state = "(none)"
		}
		got[m.Labels["name"]] = state
	}
	want := map[string]string{"a": "running", "b": "stopped", "c": "unknown", "d": "(none)"}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("%s: state %q, want %q", name, got[name], state)
		}
	}
	regex := model.Collector{Name: "r", Decoder: model.DecoderConfig{Type: "text"}, Transform: model.TransformConfig{Type: "regex"},
		Metrics: []model.MetricRule{{Name: "disk", Type: model.GaugeMetricType, Expression: `(\d+) (\w+)`, Labels: []model.LabelRule{{Name: "mount", Expression: "2", ValueMap: map[string]string{"root": "/"}}}}}}
	set, err = runBody(t, regex, "text/plain", "5 root\n7 data\n")
	if err != nil || set.Metrics[0].Labels["mount"] != "/" || set.Metrics[1].Labels["mount"] != "data" {
		t.Fatalf("%v %+v", err, set)
	}
	for name, rule := range map[string]model.MetricRule{
		"static":            {Name: "x", Expression: ".x", Labels: []model.LabelRule{{Name: "l", Value: "v", ValueMap: map[string]string{"v": "w"}}}},
		"required to empty": {Name: "x", Expression: ".x", Labels: []model.LabelRule{{Name: "l", Expression: ".l", Required: true, ValueMap: map[string]string{"a": ""}}}},
		"blank key":         {Name: "x", Expression: ".x", Labels: []model.LabelRule{{Name: "l", Expression: ".l", ValueMap: map[string]string{" a": "b"}}}},
		"rule without name": {Expression: "^x$", Labels: []model.LabelRule{{Name: "l", Expression: "l", ValueMap: map[string]string{"a": "b"}}}},
	} {
		transformType := "jq"
		if name == "rule without name" {
			transformType = "prometheus"
		}
		x := model.Collector{Name: "c", Transform: model.TransformConfig{Type: transformType}}
		if err := CheckMetricRule(&x, &rule); err == nil || !strings.Contains(err.Error(), "value_map") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Two rules of one name may map a label only alike: its series are shared.
func TestLabelValueMapsOfOneNameMustAgree(t *testing.T) {
	rule := func(values map[string]string) model.MetricRule {
		return model.MetricRule{Name: "worker_up", Expression: "1", Labels: []model.LabelRule{{Name: "state", Expression: ".state", ValueMap: values}}}
	}
	differ := model.Collector{Name: "c", Metrics: []model.MetricRule{rule(map[string]string{"1": "running"}), rule(map[string]string{"1": "up"})}}
	if err := CheckLabelValueMapsAgree(&differ); err == nil || !strings.Contains(err.Error(), "rule 1 and another in rule 2") {
		t.Fatalf("err=%v", err)
	}
	alike := model.Collector{Name: "c", Metrics: []model.MetricRule{rule(map[string]string{"1": "running"}), rule(map[string]string{"1": "running"})}}
	if err := CheckLabelValueMapsAgree(&alike); err != nil {
		t.Fatal(err)
	}
	named := differ
	named.Metrics = []model.MetricRule{rule(map[string]string{"1": "running"}), rule(map[string]string{"1": "up"})}
	named.Metrics[1].Name = "worker_other"
	if err := CheckLabelValueMapsAgree(&named); err != nil {
		t.Fatal(err)
	}
}

// A rule whose expression gives text or an object says what it got in the
// scrape's error, the object by its kind and size rather than its content.
func TestValueErrorsNameTheValue(t *testing.T) {
	c := model.Collector{Name: "v", Decoder: model.DecoderConfig{Type: "json"}, Transform: model.TransformConfig{Type: "jq"},
		Metrics: []model.MetricRule{{Name: "state", Type: model.GaugeMetricType, Expression: ".state", ErrorMode: model.ErrorModeFail}}}
	for body, want := range map[string]string{
		`{"state": "n/a"}`: `metric "state": value "n/a" is not a number; map text to numbers with value_map`,
		`{"state": {"a": [1, 2, {"b": "x"}], "c": 1}}`: `metric "state": value is an object with 2 keys, not a number`,
		`{"state": [1, 2, 3]}`:                         `metric "state": value is an array of 3 items, not a number`,
	} {
		_, err := runBody(t, c, "application/json", body)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "map[") || strings.Contains(err.Error(), "strconv") {
			t.Errorf("%s: err=%v, want %q", body, err, want)
		}
	}
	c.Metrics[0].ValueMap = map[string]float64{"up": 1}
	if _, err := runBody(t, c, "application/json", `{"state": {"a": 1}}`); err == nil || !strings.Contains(err.Error(), "value is an object with 1 key, which is neither text value_map can look up nor a number") {
		t.Errorf("err=%v", err)
	}
}

// ruleTextValue gives a text the value ruleValue gives it, or the error it
// gives it in the same words, for rules with and without value_map, its "*"
// and scale: a table of the texts a response holds where a number is
// expected, and random ones made of what numbers are written with.
func TestRuleTextValueAgreesWithRuleValue(t *testing.T) {
	thousandth, double := 0.001, 2.0
	rules := []model.MetricRule{
		{Name: "plain"},
		{Name: "scaled", Scale: &thousandth},
		{Name: "mapped", ValueMap: map[string]float64{"up": 1, "down": 0, "7": 70}},
		{Name: "mapped and scaled", ValueMap: map[string]float64{"up": 1, "1e3": 5}, Scale: &double},
		{Name: "mapped with any", ValueMap: map[string]float64{"up": 1, "*": -1}},
	}
	texts := []string{"1", " 2 ", "\t3\n", "1e3", "0x10", "0b11", "NaN", "nan", "+Inf", "-inf", "Infinity", "", "  ", "up", " up ", "UP", "down", "7", " 7", "1_000", "1,5", "١", "1e400", "-1e400", ".5", "5.", "-0", "abc", "1 2", "0x1p-2", strings.Repeat("9", 400), strings.Repeat("text ", 30), "caf\xc3\xa9", "\xff"}
	random := rand.New(rand.NewPCG(20261002, 3))
	const alphabet = "0123456789+-.eE xXpP_iInNfFaAuUpP \t"
	for range 4000 {
		text := make([]byte, random.IntN(8))
		for i := range text {
			text[i] = alphabet[random.IntN(len(alphabet))]
		}
		texts = append(texts, string(text))
	}
	for _, rule := range rules {
		for _, text := range texts {
			want, wantErr := ruleValue(rule, text)
			got, gotErr := ruleTextValue(rule, text)
			if math.Float64bits(got) != math.Float64bits(want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Fatalf("rule %s, text %q: %v, %v; ruleValue gives %v, %v", rule.Name, text, got, gotErr, want, wantErr)
			}
		}
	}
	t.Logf("%d values compared", len(rules)*len(texts))
}
