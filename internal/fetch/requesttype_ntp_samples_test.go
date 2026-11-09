//go:build !select_request_types || request_type_ntp

package fetch

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/tier"
)

// samples and sample_interval are held to their bounds, and default to 1
// and 2s; a target names at most 8 servers by commas, none empty or twice,
// and its label names each.
func TestNTPSamplesAndServersAreChecked(t *testing.T) {
	r := ntpRequest(ntpCollector(t))
	if r.Samples != 1 || time.Duration(r.SampleInterval) != 2*time.Second {
		t.Errorf("defaults %+v", r)
	}
	for _, tc := range []struct {
		own  NTPConfig
		want string
	}{
		{NTPConfig{Samples: 9}, "request.samples is 9"},
		{NTPConfig{Samples: -1}, "request.samples is -1"},
		{NTPConfig{SampleInterval: model.Duration(500 * time.Millisecond)}, "request.sample_interval is 500ms"},
	} {
		if err := ValidateRequest(&model.Collector{Name: "clock", Request: model.RequestConfig{Type: RequestTypeNTP, Own: tc.own}}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v", tc.own, err)
		}
	}
	c := ntpCollector(t)
	for target, want := range map[string]string{
		"a,b,c,d,e,f,g,h,i": "names 9 servers; a scrape asks at most 8",
		"a,,b":              "an empty server between its commas",
		"a, A:123":          "names a:123 twice",
		"a,b/x":             "has a path",
	} {
		if err := CheckTarget(c, target, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", target, err, want)
		}
	}
	if got, err := RequestLabelFor("a.example, b.example:1123", c, RequestOverrides{}); err != nil || got != "ntp://a.example:123,b.example:1123" {
		t.Errorf("label %q, %v", got, err)
	}
}

// With samples, a server is asked that many times, sample_interval apart:
// its document is of the reply of the least delay, with how many were sent
// and answered and the population standard deviation of their offsets; a
// request a later one follows that is not answered before that one is due
// is unanswered; and each request is one of the trace.
func TestAnNTPServerIsSampled(t *testing.T) {
	tier.Slow(t)
	var n atomic.Int32
	// The first answered after 60ms, the second not at all, the third and
	// fourth at once; 5, -, 7 and 6 seconds ahead.
	s := newNTPStub(t, func(request []byte) [][]byte {
		i := n.Add(1)
		switch i {
		case 1:
			time.Sleep(60 * time.Millisecond)
			return [][]byte{goodNTPReply(5 * time.Second).bytes(request)}
		case 2:
			return nil
		case 3:
			return [][]byte{goodNTPReply(7 * time.Second).bytes(request)}
		}
		return [][]byte{goodNTPReply(6 * time.Second).bytes(request)}
	})
	c := ntpCollector(t, func(c *model.Collector) {
		c.Request.Own = NTPConfig{Samples: 4, SampleInterval: model.Duration(time.Second)}
	})
	ctx, trace := WithRequestTrace(context.Background())
	start := time.Now()
	resp, err := FetchCollector(ctx, s.addr, c, RequestOverrides{Timeout: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc ntpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 3*time.Second || took > 5*time.Second {
		t.Errorf("four samples a second apart took %s", took)
	}
	if doc.Samples != 4 || doc.SamplesAnswered != 3 || !ntpNear(doc.JitterSeconds, math.Sqrt(2.0/3), 0.05) {
		t.Errorf("samples %d, answered %d, jitter %g", doc.Samples, doc.SamplesAnswered, doc.JitterSeconds)
	}
	// The third and fourth were answered at once; either may have the
	// least delay, the first's 60ms neither.
	if !ntpNear(doc.OffsetSeconds, 7, 0.05) && !ntpNear(doc.OffsetSeconds, 6, 0.05) {
		t.Errorf("the offset of the least delay is %g", doc.OffsetSeconds)
	}
	requests := trace.Requests()
	if len(requests) != 4 || !requests[1].Failed || !strings.Contains(requests[1].Outcome, "no reply from "+s.addr) || requests[3].Failed {
		t.Errorf("traced %+v", requests)
	}
}

// A target of several servers asks each at once: the document has each
// one's in servers, in the target's order, one that answers no time with
// its error alone, how many answered, and the median and spread of their
// offsets; when none answers, the scrape fails with the first's error.
func TestAnNTPTargetOfSeveralServers(t *testing.T) {
	ahead := newNTPStub(t, ntpAnswering(func() ntpReply { return goodNTPReply(5 * time.Second) }))
	behind := newNTPStub(t, ntpAnswering(func() ntpReply { return goodNTPReply(-time.Second) }))
	kissing := newNTPStub(t, ntpAnswering(func() ntpReply {
		r := goodNTPReply(0)
		r.stratum, r.referenceID = 0, [4]byte{'D', 'E', 'N', 'Y'}
		return r
	}))
	c := ntpCollector(t)
	resp, err := FetchCollector(context.Background(), ahead.addr+","+kissing.addr+","+behind.addr, c, RequestOverrides{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc ntpServersDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 3 || doc.ServersAnswered != 2 || doc.OffsetMedianSeconds == nil || !ntpNear(*doc.OffsetMedianSeconds, 2, 0.05) || !ntpNear(*doc.OffsetSpreadSeconds, 6, 0.05) {
		t.Fatalf("document %s", resp.Body)
	}
	if !ntpNear(doc.Servers[0].OffsetSeconds, 5, 0.05) || doc.Servers[0].Error != "" || !strings.Contains(doc.Servers[1].Error, "Kiss-o'-Death, DENY") || doc.Servers[1].Stratum != 0 || doc.Servers[1].Host != "127.0.0.1" {
		t.Errorf("servers %+v", doc.Servers)
	}
	_, err = FetchCollector(context.Background(), kissing.addr+","+kissing.addr[:strings.LastIndex(kissing.addr, ":")]+":1", c, RequestOverrides{Timeout: 2 * time.Second}, nil)
	if err == nil || !strings.Contains(err.Error(), "Kiss-o'-Death, DENY") {
		t.Errorf("none answering: %v", err)
	}
}
