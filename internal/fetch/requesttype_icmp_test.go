//go:build !select_request_types || request_type_icmp

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// icmpCollector is a validated icmp collector, as edit makes it.
func icmpCollector(t *testing.T, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "ping", Request: model.RequestConfig{Type: RequestTypeICMP}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// pingOrSkip pings target through c, skipping the test where the system
// opens no ICMP datagram socket for the test's group.
func pingOrSkip(t *testing.T, c *model.Collector, target string) (icmpDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT)) {
		t.Skipf("the system opens no ICMP datagram socket here: %v", err)
	}
	if err != nil {
		return icmpDocument{}, err
	}
	var doc icmpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if resp.Headers.Get("Content-Type") != "application/json" || !resp.NoStatus {
		t.Errorf("headers %v, no status %t", resp.Headers, resp.NoStatus)
	}
	return doc, nil
}

// A collector of no keys sends 3 echoes 1s apart, waits 1s for the last
// reply, and carries 56 bytes of data in each, as ping does.
func TestAnICMPCollectorsDefaults(t *testing.T) {
	r := icmpRequest(icmpCollector(t))
	if r.Count != 3 || time.Duration(r.Interval) != time.Second || time.Duration(r.ReplyTimeout) != time.Second || *r.PayloadBytes != 56 {
		t.Errorf("defaults %+v, payload %d", r, *r.PayloadBytes)
	}
	label, err := RequestLabelFor("gw.example", icmpCollector(t), RequestOverrides{})
	if err != nil || label != "icmp://gw.example" || RequestMethodFor(icmpCollector(t), RequestOverrides{}) != "PING" {
		t.Errorf("label %q, %v", label, err)
	}
}

// A ping of loopback is answered: every echo comes back, the loss is 0,
// and each round trip is in the document with their minimum, average,
// maximum and deviation.
func TestAnICMPCollectorPingsLoopback(t *testing.T) {
	c := icmpCollector(t, func(c *model.Collector) {
		c.Request.Own = ICMPConfig{Count: 3, Interval: model.Duration(10 * time.Millisecond), ReplyTimeout: model.Duration(time.Second)}
	})
	doc, err := pingOrSkip(t, c, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Sent != 3 || doc.Received != 3 || doc.LossRatio != 0 || len(doc.RTTsSeconds) != 3 || doc.RTTMin == nil || *doc.RTTMin > *doc.RTTMax || doc.Family != 4 || doc.Address != "127.0.0.1" || doc.PayloadBytes != 56 {
		t.Errorf("document %+v", doc)
	}
}

// A host that answers no echo is no failure: the document says how many
// were sent and that none came back, a loss of 1, with no round trip,
// once reply_timeout has passed after the last; a reply to another
// echo, a duplicate and one of another token are not counted.
func TestAnICMPHostThatAnswersNothingIsALossOfOne(t *testing.T) {
	own := ICMPConfig{Count: 2, Interval: model.Duration(10 * time.Millisecond), ReplyTimeout: model.Duration(100 * time.Millisecond)}
	r := icmpRequest(icmpCollector(t, func(c *model.Collector) { c.Request.Own = own }))
	client, host := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = host.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := host.Read(buf); err != nil {
				return
			}
		}
	}()
	began := time.Now()
	doc, err := icmpPing(context.Background(), client, netip.MustParseAddr("192.0.2.1"), r)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Sent != 2 || doc.Received != 0 || doc.LossRatio != 1 || len(doc.RTTsSeconds) != 0 || doc.RTTMin != nil || time.Since(began) > 3*time.Second {
		t.Errorf("document %+v after %v", doc, time.Since(began))
	}
	// A host that answers the first echo twice and the second with the
	// data of another scrape: one reply, one duplicate.
	client2, host2 := net.Pipe()
	t.Cleanup(func() { _ = client2.Close(); _ = host2.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := host2.Read(buf)
			if err != nil {
				return
			}
			echo := append([]byte(nil), buf[:n]...)
			echo[0] = 0
			if echo[7] == 1 {
				_, _ = host2.Write(echo)
				_, _ = host2.Write(echo)
			} else {
				echo[8] ^= 0xff
				_, _ = host2.Write(echo)
			}
		}
	}()
	doc, err = icmpPing(context.Background(), client2, netip.MustParseAddr("192.0.2.1"), r)
	if err != nil || doc.Sent != 2 || doc.Received != 1 || doc.Duplicates != 1 || doc.LossRatio != 0.5 {
		t.Errorf("document %+v, %v", doc, err)
	}
}

// The target policy holds the host, and the address the socket is
// connected to.
func TestAnICMPCollectorKeepsToItsTargetLists(t *testing.T) {
	c := icmpCollector(t, func(c *model.Collector) { c.Request.DeniedTargets = []string{"127.0.0.0/8", "::1/128"} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := FetchCollector(ctx, "127.0.0.1", c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a denied address: %v", err)
	}
	if _, err := FetchCollector(ctx, "localhost", c, RequestOverrides{}, nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a name of a denied address: %v", err)
	}
}

// An IPv6 address is pinged on an ICMPv6 socket: opening one, which a
// system without IPv6 or without ping sockets refuses, fails or succeeds,
// and never panics, as making the IPv4 socket address of it first did.
func TestAnICMPSocketToAnIPv6Address(t *testing.T) {
	conn, err := icmpDial(netip.MustParseAddr("::1"))
	if err == nil {
		_ = conn.Close()
	}
}

// An echo is type 8 with a checksum over the whole message, or type 128
// for ICMPv6, whose checksum the system works out; a reply is matched by
// its type, its code and its data, and an IPv4 reply that carries its IP
// header, as macOS reads one, is read past it.
func TestICMPEchoesAndReplies(t *testing.T) {
	data := []byte("tokentok-and-more")
	echo := icmpEcho(false, 7, data)
	if echo[0] != 8 || icmpChecksum(echo) != 0 || echo[7] != 7 {
		t.Errorf("echo %x, checksum over it %x", echo, icmpChecksum(echo))
	}
	if v6 := icmpEcho(true, 1, data); v6[0] != 128 || v6[2] != 0 || v6[3] != 0 {
		t.Errorf("ICMPv6 echo %x", v6)
	}
	reply := append([]byte{0, 0, 0, 0, 0, 9, 0, 7}, data...)
	if seq, ok := icmpReply(reply, false, data); !ok || seq != 7 {
		t.Errorf("a reply: %d %v", seq, ok)
	}
	header := append([]byte{0x45}, make([]byte, 19)...)
	if seq, ok := icmpReply(append(header, reply...), false, data); !ok || seq != 7 {
		t.Errorf("a reply after its IP header: %d %v", seq, ok)
	}
	if _, ok := icmpReply(append([]byte{0, 0, 0, 0, 0, 9, 0, 7}, "other-token-data!"...), false, data); ok {
		t.Error("a reply of another token was taken")
	}
	if _, ok := icmpReply(append([]byte{3, 1, 0, 0, 0, 0, 0, 0}, data...), false, data); ok {
		t.Error("an unreachable was taken as a reply")
	}
	if seq, ok := icmpReply(append([]byte{129, 0, 0, 0, 0, 9, 0, 2}, data...), true, data); !ok || seq != 2 {
		t.Errorf("an ICMPv6 reply: %d %v", seq, ok)
	}
	// A datagram whose IP header says it is 60 bytes and is 20: no reply,
	// where reading past it panicked.
	if _, ok := icmpReply(append([]byte{0x4f}, make([]byte, 19)...), false, data); ok {
		t.Error("a datagram shorter than the header it claims was taken as a reply")
	}
}

// A target is a host, and the address picked is of the family ip_family
// asks for.
func TestICMPTargetsAndFamilies(t *testing.T) {
	for raw, want := range map[string]string{"gw.example": "gw.example", "192.0.2.1": "192.0.2.1", "[2001:db8::1]": "2001:db8::1", "2001:db8::1": "2001:db8::1"} {
		if host, err := parseICMPTarget(raw); err != nil || host != want {
			t.Errorf("%s: %q, %v", raw, host, err)
		}
	}
	for raw, want := range map[string]string{"gw.example:7": "has a port", "http://gw": "has a scheme", "gw/x": "has a path", "": "target"} {
		if _, err := parseICMPTarget(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", raw, err, want)
		}
	}
	ctx := context.Background()
	if a, err := icmpAddresses(ctx, "::1", "prefer_ip4"); err != nil || len(a) != 1 || a[0] != netip.MustParseAddr("::1") {
		t.Errorf("an IPv6 address preferring IPv4: %v, %v", a, err)
	}
	if _, err := icmpAddresses(ctx, "::1", "ip4"); err == nil || !strings.Contains(err.Error(), "has no IPv4 address, and request.ip_family is ip4") {
		t.Errorf("an IPv6 address of ip4: %v", err)
	}
}

// The keys are held to their bounds when the configuration loads.
func TestICMPKeysAreChecked(t *testing.T) {
	seven, big := 7, 1233
	for _, c := range []struct {
		own  ICMPConfig
		want string
	}{
		{ICMPConfig{Count: -1}, "request.count is -1"},
		{ICMPConfig{Count: 101}, "request.count is 101"},
		{ICMPConfig{Interval: model.Duration(time.Millisecond)}, "request.interval is 1ms"},
		{ICMPConfig{ReplyTimeout: model.Duration(time.Millisecond)}, "request.reply_timeout is 1ms"},
		{ICMPConfig{PayloadBytes: &seven}, "request.payload_bytes is 7"},
		{ICMPConfig{PayloadBytes: &big}, "request.payload_bytes is 1233"},
	} {
		x := &model.Collector{Name: "ping", Request: model.RequestConfig{Type: RequestTypeICMP, Own: c.own}}
		if err := ValidateRequest(x); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.own, err, c.want)
		}
	}
	x := &model.Collector{Name: "ping", Request: model.RequestConfig{Type: RequestTypeICMP, IPFamily: "ip5"}}
	if err := ValidateRequest(x); err == nil || !strings.Contains(err.Error(), `ip_family "ip5"`) {
		t.Errorf("ip_family ip5: %v", err)
	}
}

// With all_addresses, every address of the name of the families ip_family
// takes is pinged, each a request of its own: the document has each in
// addresses, in the resolver's order, and at its top the echoes of all of
// them, their round trips and statistics, with no address of its own;
// without it, the first alone, with no addresses.
func TestAnICMPCollectorPingsEveryAddressOfAName(t *testing.T) {
	old := icmpLookup
	icmpLookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.2")}, nil
	}
	t.Cleanup(func() { icmpLookup = old })
	all := func(family string) *model.Collector {
		return icmpCollector(t, func(c *model.Collector) {
			c.Request.IPFamily = family
			c.Request.Own = ICMPConfig{Count: 2, Interval: model.Duration(10 * time.Millisecond), ReplyTimeout: model.Duration(time.Second), AllAddresses: true}
		})
	}
	ctx, trace := WithRequestTrace(context.Background())
	resp, err := FetchCollector(ctx, "pool.example", all("ip4"), RequestOverrides{}, nil)
	if err != nil && strings.Contains(err.Error(), "permission denied") {
		t.Skip("the system opens no ICMP datagram socket here")
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc icmpDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Address != "" || doc.Family != 0 || doc.AddressesFound != 2 || len(doc.Addresses) != 2 || doc.Addresses[0].Address != "127.0.0.1" || doc.Addresses[1].Address != "127.0.0.2" ||
		doc.Sent != 4 || doc.Received != 4 || doc.LossRatio != 0 || len(doc.RTTsSeconds) != 4 || doc.RTTMax == nil || doc.Addresses[1].Sent != 2 || doc.Addresses[1].Received != 2 {
		t.Fatalf("document %+v", doc)
	}
	if requests := trace.Requests(); len(requests) != 2 || requests[0].Header.Get("Icmp-Address") != "127.0.0.1" || requests[1].Outcome != "2 of 2 replies" {
		t.Errorf("traced %+v", requests)
	}
	doc, err = pingOrSkip(t, all(""), "pool.example")
	if err != nil {
		t.Fatal(err)
	}
	if doc.AddressesFound != 3 || len(doc.Addresses) != 3 || doc.Addresses[1].Family != 6 {
		t.Errorf("of both families: %+v", doc)
	}
	first := icmpCollector(t, func(c *model.Collector) {
		c.Request.Own = ICMPConfig{Count: 1, Interval: model.Duration(10 * time.Millisecond), ReplyTimeout: model.Duration(time.Second)}
	})
	doc, err = pingOrSkip(t, first, "pool.example")
	if err != nil || doc.Address != "127.0.0.1" || doc.Addresses != nil || doc.AddressesFound != 0 {
		t.Errorf("without all_addresses: %+v, %v", doc, err)
	}
}
