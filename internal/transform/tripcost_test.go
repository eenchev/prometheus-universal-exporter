package transform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/expr"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector reads the timings of its requests when a jq or yq text of
// it, whichever key holds it, names $timings, and whenever it runs Python;
// otherwise its requests need not be timed.
func TestReadsTimingsIsWhereARuleOrAScriptMayReadThem(t *testing.T) {
	for name, test := range map[string]struct {
		collector model.Collector
		reads     bool
	}{
		"no rule reads them":       {model.Collector{Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "up", Expression: "$status"}, {Name: "ssl", Expression: "$tls != null"}}}, false},
		"an expression":            {model.Collector{Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "up", Expression: "$timings.total"}}}, true},
		"an items expression":      {model.Collector{Transform: model.TransformConfig{Type: "yq"}, Metrics: []model.MetricRule{{Name: "phase", Items: "$timings | to_entries[]", Expression: ".value"}}}, true},
		"a label":                  {model.Collector{Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "up", Expression: "1", Labels: []model.LabelRule{{Name: "slow", Expression: "$timings.total > 1"}}}}}, true},
		"a transform label":        {model.Collector{Transform: model.TransformConfig{Type: "jq", Labels: map[string]string{"dns": "$timings.dns"}}}, true},
		"a Python script":          {model.Collector{Transform: model.TransformConfig{Type: "python", Script: "emit('up', 1)"}}, true},
		"a Python pre-script":      {model.Collector{Transform: model.TransformConfig{Type: "jq", PreScript: "data = data"}}, true},
		"text of no rule names it": {model.Collector{Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "up", Expression: "1", Description: "not $timings"}}}, true},
		"the remote address":       {model.Collector{Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "ip", Expression: "1", Labels: []model.LabelRule{{Name: "ip", Expression: "$remote_ip"}}}}}, true},
		"the HTTP version alone":   {model.Collector{Transform: model.TransformConfig{Type: "jq"}, Metrics: []model.MetricRule{{Name: "v", Expression: "$http_version | tonumber"}}}, false},
	} {
		if got := ReadsTimings(&test.collector); got != test.reads {
			t.Errorf("%s: ReadsTimings is %v", name, got)
		}
	}
}

// The variables of a response's request are made only for a program that
// may read one, and once: a program that names none of $timings, $tls,
// $url and $redirects runs with them null, and leaves them unmade.
func TestTheTripsVariablesAreMadeOnlyForAProgramThatReadsThem(t *testing.T) {
	r := tripResponse()
	r.Trip.URL = &url.URL{Scheme: "https", Host: "www.example.com", Path: "/"}
	ctx := withResponseVariables(context.Background(), r)
	vars := ctx.Value(responseVariablesKey{}).(*responseVariables)
	status, err := expr.CompileJQProgram("$status")
	if err != nil {
		t.Fatal(err)
	}
	values := vars.values(status, nil)
	if values[1] != http.StatusOK || values[3] != nil || vars.read.Timings != nil || vars.read.URL != nil {
		t.Errorf("a program reading $status alone made the trip's variables: %v, %+v", values, vars.read)
	}
	timings, err := expr.CompileJQProgram("$timings.total")
	if err != nil {
		t.Fatal(err)
	}
	values = vars.values(timings, nil)
	if total, _ := values[3].(map[string]any)["total"].(float64); total != 0.05 || values[5] != "https://www.example.com/" {
		t.Errorf("a program reading $timings ran with %v", values)
	}
	made := reflect.ValueOf(vars.read.Timings).Pointer()
	if again := reflect.ValueOf(vars.values(timings, nil)[3]).Pointer(); again != made {
		t.Error("the trip's variables were made again for the second program")
	}
}

// The request line writes response.timings, response.tls, response.url,
// response.redirects, response.http_version and response.remote_ip from the
// trip, as json.Marshal writes the values jq
// reads: of no trip, of one over plain HTTP with phases it did not have,
// and of one over TLS whose certificate's names need escaping.
func TestThePythonRequestWritesTheTripAsJSONWould(t *testing.T) {
	connect, tlsPhase, firstByte := 1500*time.Microsecond, 30*time.Nanosecond, 2*time.Second
	for name, trip := range map[string]*fetch.HTTPTrip{
		"none":  nil,
		"plain": {Connect: &connect, FirstByte: &firstByte, Total: 3 * time.Second, ProtoMajor: 1, ProtoMinor: 0, RemoteIP: netip.MustParseAddr("203.0.113.7"), URL: &url.URL{Scheme: "http", Host: "x", Path: "/a", RawQuery: "token=s3cret&b=<c>"}},
		"tls": {DNS: &connect, Connect: &connect, TLS: &tlsPhase, FirstByte: &firstByte, Total: 4 * time.Second, Redirects: 3, URL: &url.URL{Scheme: "https", Host: "x"}, ProtoMajor: 2, RemoteIP: netip.MustParseAddr("2001:db8::7"),
			TLSConnection: &fetch.TLSConnection{TLSVersion: "TLS 1.3", CipherSuite: "TLS_AES_128_GCM_SHA256", ServerName: "x", Verified: true, NotBefore: 1700000000, NotAfter: 1800000000, SubjectCN: "a <b> & \"c\"", IssuerCN: "\u2028é"}},
	} {
		var nul bool
		got := string(appendPythonTrip(nil, trip, &nul))
		var want []byte
		for _, field := range []struct {
			key   string
			value any
		}{{`,"timings":`, trip.JQTimings()}, {`,"tls":`, trip.JQTLS()}, {`,"url":`, trip.JQURL()}, {`,"redirects":`, trip.JQRedirects()}, {`,"http_version":`, trip.JQHTTPVersion()}, {`,"remote_ip":`, trip.JQRemoteIP()}} {
			value, err := json.Marshal(field.value)
			if err != nil {
				t.Fatal(err)
			}
			want = append(append(want, field.key...), value...)
		}
		if got != string(want) {
			t.Errorf("%s:\n%s\nwant\n%s", name, got, want)
		}
	}
}
