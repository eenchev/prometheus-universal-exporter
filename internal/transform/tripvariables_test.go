package transform

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/decode"
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// tripResponse is an answer to a request over plain HTTP on a new
// connection to an IP address: a connection, no lookup, no handshake.
func tripResponse() *fetch.HTTPResponse {
	connect, firstByte := 2*time.Millisecond, 30*time.Millisecond
	return &fetch.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"items": [{"name": "a"}, {"name": "b"}]}`),
		Trip:       &fetch.HTTPTrip{Connect: &connect, FirstByte: &firstByte, Total: 50 * time.Millisecond},
	}
}

// tripSeries runs the collector's rules on r and gives its series by name
// and labels, in the exposition's words.
func tripSeries(t *testing.T, c model.Collector, r *fetch.HTTPResponse) string {
	t.Helper()
	c.Request.Type, c.Limits.MaxMetrics = fetch.RequestTypeHTTP, 100
	if c.Decoder.Type == "" {
		c.Decoder.Type = "json"
	}
	d, err := decode.Decode(r, &c)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Transform(context.Background(), d, r, &c, "python3")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, m := range set.Metrics {
		b.WriteString(m.Name)
		for _, name := range model.SortedKeys(m.Labels) {
			fmt.Fprintf(&b, " %s=%s", name, m.Labels[name])
		}
		fmt.Fprintf(&b, " %s\n", strconv.FormatFloat(m.Value, 'f', -1, 64))
	}
	return b.String()
}

// $timings is bound in every jq and yq program, the expression of a rule,
// its items and a label, each per item too, and is Python's
// response.timings: seconds, null for a phase the request did not have, and
// null whole for an answer no HTTP request brought.
func TestTimingsAreBoundInEveryProgram(t *testing.T) {
	rules := []model.MetricRule{
		{Name: "total", Expression: "$timings.total"},
		{Name: "first_byte", Expression: "$timings.first_byte"},
		{Name: "connect", Expression: "$timings.connect"},
		{Name: "no_lookup", Expression: "if $timings.dns == null and $timings.tls == null then 1 else 0 end"},
		{Name: "per_item", Items: ".items[] | select($timings.total > 0)", Expression: "$timings.total * 1000", Labels: []model.LabelRule{{Name: "name", Expression: ".name"}, {Name: "phases", Expression: "$timings | keys | join(\",\")"}}},
		{Name: "absent", Expression: "if $timings == null then 1 else 0 end"},
	}
	want := "total 0.05\nfirst_byte 0.03\nconnect 0.002\nno_lookup 1\n" +
		"per_item name=a phases=connect,dns,first_byte,tls,total 50\nper_item name=b phases=connect,dns,first_byte,tls,total 50\nabsent 0\n"
	for _, kind := range []string{"jq", "yq"} {
		c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: kind}, Metrics: rules}
		if got := tripSeries(t, c, tripResponse()); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", kind, got, want)
		}
		c.Metrics = rules[len(rules)-1:]
		bare := tripResponse()
		bare.Trip = nil
		if got := tripSeries(t, c, bare); got != "absent 1\n" {
			t.Errorf("%s without a trip: %s", kind, got)
		}
	}

	requirePython(t)
	c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: "python", Script: `t = response.timings
metric(name='total', value=t['total'])
metric(name='nulls', value=sum(1 for k in ('dns', 'tls') if t[k] is None))
metric(name='phases', value=len(t))
`}}
	if got := tripSeries(t, c, tripResponse()); got != "total 0.05\nnulls 2\nphases 5\n" {
		t.Errorf("python: %s", got)
	}
	c.Transform.Script = "metric(name='absent', value=1 if response.timings is None else 0)\n"
	bare := tripResponse()
	bare.Trip = nil
	if got := tripSeries(t, c, bare); got != "absent 1\n" {
		t.Errorf("python without a trip: %s", got)
	}
}

// $tls is the TLS connection as jq, yq and Python read it, null over plain
// HTTP and for an answer no HTTP request brought.
func TestTLSIsBoundInEveryProgram(t *testing.T) {
	secure := tripResponse()
	secure.Trip.TLSConnection = &fetch.TLSConnection{TLSVersion: "TLS 1.3", CipherSuite: "TLS_AES_128_GCM_SHA256", ServerName: "www.example.com", Verified: true, NotBefore: 1759276800, NotAfter: 1767225599, SubjectCN: "www.example.com", IssuerCN: "Example Issuing CA"}
	rules := []model.MetricRule{
		{Name: "expiry", Expression: "$tls.not_after"},
		{Name: "verified", Expression: "if $tls.verified then 1 else 0 end"},
		{Name: "info", Expression: "1", Labels: []model.LabelRule{{Name: "version", Expression: "$tls.tls_version"}, {Name: "cipher", Expression: "$tls.cipher_suite"}, {Name: "sni", Expression: "$tls.server_name"}, {Name: "subject", Expression: "$tls.subject_cn"}, {Name: "issuer", Expression: "$tls.issuer_cn"}}},
		{Name: "valid_for", Expression: "$tls.not_after - $tls.not_before"},
	}
	want := "expiry 1767225599\nverified 1\n" +
		"info cipher=TLS_AES_128_GCM_SHA256 issuer=Example Issuing CA sni=www.example.com subject=www.example.com version=TLS 1.3 1\nvalid_for 7948799\n"
	plain := []model.MetricRule{{Name: "plain", Expression: "if $tls == null then 1 else 0 end"}}
	for _, kind := range []string{"jq", "yq"} {
		c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: kind}, Metrics: rules}
		if got := tripSeries(t, c, secure); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", kind, got, want)
		}
		c.Metrics = plain
		bare := tripResponse()
		if got := tripSeries(t, c, bare); got != "plain 1\n" {
			t.Errorf("%s over plain HTTP: %s", kind, got)
		}
		bare.Trip = nil
		if got := tripSeries(t, c, bare); got != "plain 1\n" {
			t.Errorf("%s without a trip: %s", kind, got)
		}
	}

	requirePython(t)
	c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: "python", Script: `metric(name='expiry', value=response.tls['not_after'], labels={'issuer': response.tls['issuer_cn']})
metric(name='verified', value=response.tls['verified'])
`}}
	if got := tripSeries(t, c, secure); got != "expiry issuer=Example Issuing CA 1767225599\nverified 1\n" {
		t.Errorf("python: %s", got)
	}
	c.Transform.Script = "metric(name='plain', value=1 if response.tls is None else 0)\n"
	if got := tripSeries(t, c, tripResponse()); got != "plain 1\n" {
		t.Errorf("python over plain HTTP: %s", got)
	}
}

// $url and $redirects are where the request went and how many redirects led
// there, as jq, yq and Python read them, and null for an answer no HTTP
// request brought.
func TestURLAndRedirectsAreBoundInEveryProgram(t *testing.T) {
	redirected := tripResponse()
	final, err := url.Parse("https://www.example.com/health?token=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	redirected.Trip.URL, redirected.Trip.Redirects = final, 2
	rules := []model.MetricRule{
		{Name: "redirects", Expression: "$redirects"},
		{Name: "final", Expression: "1", Labels: []model.LabelRule{{Name: "url", Expression: "$url"}}},
	}
	want := "redirects 2\nfinal url=https://www.example.com/health?token=<redacted> 1\n"
	absent := []model.MetricRule{{Name: "absent", Expression: "if $url == null and $redirects == null then 1 else 0 end"}}
	for _, kind := range []string{"jq", "yq"} {
		c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: kind}, Metrics: rules}
		if got := tripSeries(t, c, redirected); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", kind, got, want)
		}
		c.Metrics = absent
		bare := tripResponse()
		bare.Trip = nil
		if got := tripSeries(t, c, bare); got != "absent 1\n" {
			t.Errorf("%s without a trip: %s", kind, got)
		}
	}

	requirePython(t)
	c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: "python", Script: "metric(name='redirects', value=response.redirects, labels={'url': response.url})\n"}}
	if got := tripSeries(t, c, redirected); got != "redirects url=https://www.example.com/health?token=<redacted> 2\n" {
		t.Errorf("python: %s", got)
	}
	c.Transform.Script = "metric(name='absent', value=1 if response.url is None and response.redirects is None else 0)\n"
	bare := tripResponse()
	bare.Trip = nil
	if got := tripSeries(t, c, bare); got != "absent 1\n" {
		t.Errorf("python without a trip: %s", got)
	}
}

// $http_version and $remote_ip are the version of HTTP the answer came in
// and the address its connection was made to, as jq, yq and Python read
// them, and null for an answer no HTTP request brought, and $remote_ip for
// one whose address is not known.
func TestHTTPVersionAndRemoteIPAreBoundInEveryProgram(t *testing.T) {
	answered := tripResponse()
	answered.Trip.ProtoMajor, answered.Trip.ProtoMinor, answered.Trip.RemoteIP = 2, 0, netip.MustParseAddr("2001:db8::7")
	rules := []model.MetricRule{
		{Name: "version", Expression: "$http_version | tonumber"},
		{Name: "address", Expression: "1", Labels: []model.LabelRule{{Name: "ip", Expression: "$remote_ip"}, {Name: "version", Expression: "$http_version"}}},
	}
	want := "version 2\naddress ip=2001:db8::7 version=2 1\n"
	unknown := tripResponse()
	unknown.Trip.ProtoMajor, unknown.Trip.ProtoMinor = 1, 1
	absent := []model.MetricRule{{Name: "absent", Expression: "if $http_version == null and $remote_ip == null then 1 else 0 end"}}
	for _, kind := range []string{"jq", "yq"} {
		c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: kind}, Metrics: rules}
		if got := tripSeries(t, c, answered); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", kind, got, want)
		}
		c.Metrics = []model.MetricRule{{Name: "unknown", Expression: `if $remote_ip == null and $http_version == "1.1" then 1 else 0 end`}}
		if got := tripSeries(t, c, unknown); got != "unknown 1\n" {
			t.Errorf("%s with no address: %s", kind, got)
		}
		c.Metrics = absent
		bare := tripResponse()
		bare.Trip = nil
		if got := tripSeries(t, c, bare); got != "absent 1\n" {
			t.Errorf("%s without a trip: %s", kind, got)
		}
	}

	requirePython(t)
	c := model.Collector{Name: "trip", Transform: model.TransformConfig{Type: "python", Script: "metric(name='address', value=float(response.http_version), labels={'ip': response.remote_ip})\n"}}
	if got := tripSeries(t, c, answered); got != "address ip=2001:db8::7 2\n" {
		t.Errorf("python: %s", got)
	}
	c.Transform.Script = "metric(name='absent', value=1 if response.http_version is None and response.remote_ip is None else 0)\n"
	bare := tripResponse()
	bare.Trip = nil
	if got := tripSeries(t, c, bare); got != "absent 1\n" {
		t.Errorf("python without a trip: %s", got)
	}
}
