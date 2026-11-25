//go:build !select_request_types || request_type_icmp

package fetch

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The icmp request type pings a host. Like ping, it sends ICMP echo
// requests. It hands the transforms a JSON document with how many replies
// came back and how long each took. A rule can then alert on a host that
// stopped answering, or on its round trips. The other types can reach a
// host only through a connection to one of its ports.
//
//	request:
//	  type: icmp
//	  count: 3            # echoes a scrape sends
//	  interval: 1s        # between two echoes
//	  reply_timeout: 1s   # how long the last echo's reply is waited for
//	  payload_bytes: 56   # bytes of data each echo carries
//
// The probe's target is a host (a name or an address) with no port. The
// name is looked up at scrape time. The first address of the family that
// request.ip_family asks for (or of either family) is pinged. As with an
// ntp request, the target policy checks the name before the lookup and
// the address on the socket, which is connected to that address.
//
// The echoes go on an unprivileged ICMP datagram socket (SOCK_DGRAM,
// IPPROTO_ICMP). No raw socket is used, so neither root nor CAP_NET_RAW is
// needed:
//   - Linux opens this socket for a process whose group is in
//     net.ipv4.ping_group_range. This is a namespaced sysctl that Docker,
//     containerd and systemd set to every group.
//   - macOS opens it for every process.
//
// If the system refuses the socket, the scrape fails and the error names
// the sysctl. Linux sets the echo's identifier to the socket's own and
// delivers only the replies to it. So the exporter tells its echoes apart
// by their sequence and a random token in their data. It skips a reply
// that holds neither, such as a late reply to an echo of another scrape.
//
// The echoes are sent interval apart. Replies are read as they arrive and
// each is matched to its echo. After the last echo is sent, its reply is
// waited for up to reply_timeout. The scrape then answers with what came,
// or at its deadline. A host that answered none is not a failure: the
// document says it sent count and received 0, a loss of 1, and a rule
// alerts on that. The scrape fails only when it cannot ask: a name that
// does not resolve, an address the target policy refuses, or a socket the
// system does not open.
//
// The type pings on Linux and macOS, and imports nothing outside the
// standard library.

// ICMPConfig is an icmp collector's own request keys: how many echoes a
// scrape sends (3 by default), how far apart (1s), how long the last
// echo's reply is waited for (1s), and how many bytes of data each echo
// carries (56, as with ping).
type ICMPConfig struct {
	Count        int            `yaml:"count,omitempty"`
	Interval     model.Duration `yaml:"interval,omitempty"`
	ReplyTimeout model.Duration `yaml:"reply_timeout,omitempty"`
	PayloadBytes *int           `yaml:"payload_bytes,omitempty"`
	// AllAddresses pings every address of the target's name of the
	// families ip_family takes, at most icmpMaxAddresses of them, at once,
	// each in the document's addresses, the top of it of all of them.
	AllAddresses bool `yaml:"all_addresses,omitempty"`
}

// icmpRequest is an icmp collector's own request keys, its defaults
// filled in once validated.
func icmpRequest(c *model.Collector) ICMPConfig {
	own, _ := c.Request.Own.(ICMPConfig)
	return own
}

// The bounds and defaults of the keys.
const (
	icmpDefaultCount        = 3
	icmpMaxCount            = 100
	icmpDefaultInterval     = time.Second
	icmpMinInterval         = 10 * time.Millisecond
	icmpDefaultReplyTimeout = time.Second
	icmpMinReplyTimeout     = 10 * time.Millisecond
	icmpDefaultPayload      = 56
	// icmpMaxPayload is the most data an echo carries that fits an IPv6
	// datagram of the 1280 bytes every link carries, its 40 and the 8 of
	// ICMP's header taken off: no echo is fragmented.
	icmpMaxPayload = 1232
	// icmpToken is the bytes of the random token each echo's data begins
	// with, the least payload_bytes may be.
	icmpToken = 8
	// icmpTimeout bounds a scrape with no deadline of its own.
	icmpTimeout = 30 * time.Second
	// icmpMethod is the http_method label of a scrape.
	icmpMethod = "PING"
	// icmpMaxAddresses is how many addresses of a name all_addresses
	// pings, the first of them in the resolver's order.
	icmpMaxAddresses = 16
)

// icmpLookup looks a name's addresses up. Tests answer of their own.
var icmpLookup = net.DefaultResolver.LookupNetIP

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeICMP,
		Fields:       []string{"allowed_targets", "denied_targets", "ip_family"},
		Config:       ICMPConfig{},
		KeyRules:     icmpKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    icmpRequester{},
		JSONAnswer:   true,
		TargetHint:   func(*model.Collector) (string, string) { return "Host", "host" },
		AnswerHint:   "Paste the JSON document the exporter makes of the replies, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "icmp",
	})
}

// icmpRequester is the icmp request type's Requester.
type icmpRequester struct{ parsedTarget[icmpTarget] }

// icmpTarget reads an icmp target: label returns the url label of a
// request of it.
type icmpTarget struct{}

func (icmpTarget) label(target string) (string, error) {
	host, err := parseICMPTarget(target)
	if err != nil {
		return "", err
	}
	return "icmp://" + host, nil
}

// Validate checks an icmp collector's request and fills in its defaults.
func (icmpRequester) Validate(c *model.Collector) error {
	r := icmpRequest(c)
	if err := checkIPFamily(c.Request.IPFamily); err != nil {
		return fmt.Errorf("collector %q request.%w", c.Name, err)
	}
	switch {
	case r.Count == 0:
		r.Count = icmpDefaultCount
	case r.Count < 0 || r.Count > icmpMaxCount:
		return fmt.Errorf("collector %q request.count is %d; it is how many echoes a scrape sends, from 1 to %d", c.Name, r.Count, icmpMaxCount)
	}
	switch {
	case r.Interval == 0:
		r.Interval = model.Duration(icmpDefaultInterval)
	case time.Duration(r.Interval) < icmpMinInterval:
		return fmt.Errorf("collector %q request.interval is %s; it is how long apart the echoes are sent, at least %s, as ping lets a user other than root send them", c.Name, time.Duration(r.Interval), icmpMinInterval)
	}
	switch {
	case r.ReplyTimeout == 0:
		r.ReplyTimeout = model.Duration(icmpDefaultReplyTimeout)
	case time.Duration(r.ReplyTimeout) < icmpMinReplyTimeout:
		return fmt.Errorf("collector %q request.reply_timeout is %s; it is how long the last echo's reply is waited for, at least %s", c.Name, time.Duration(r.ReplyTimeout), icmpMinReplyTimeout)
	}
	if r.PayloadBytes == nil {
		n := icmpDefaultPayload
		r.PayloadBytes = &n
	} else if n := *r.PayloadBytes; n < icmpToken || n > icmpMaxPayload {
		return fmt.Errorf("collector %q request.payload_bytes is %d; it is how many bytes of data an echo carries, from %d, the token it is matched by, to %d, which no link fragments", c.Name, n, icmpToken, icmpMaxPayload)
	}
	c.Request.Own = r
	return nil
}

// Fetch performs one request of an icmp collector.
func (icmpRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchICMP(ctx, target, c, overrides)
}

// Method is the http_method label of an icmp request.
func (icmpRequester) Method(*model.Collector, RequestOverrides) string { return icmpMethod }

// parseICMPTarget reads a target: a host name or an address, an IPv6 one
// in brackets or not, and nothing else.
func parseICMPTarget(raw string) (string, error) {
	target := strings.TrimSpace(raw)
	switch {
	case target == "":
		return "", ErrMissingTarget
	case strings.Contains(target, "://"):
		return "", fmt.Errorf("target %q has a scheme; an icmp target is a host, such as gateway.example.com or 192.0.2.1", raw)
	case strings.ContainsAny(target, "/?#@"):
		return "", fmt.Errorf("target %q has a path, query or user in it; an icmp target is only a host", raw)
	}
	host := strings.TrimSuffix(strings.TrimPrefix(target, "["), "]")
	if _, err := netip.ParseAddr(host); err != nil && strings.Contains(host, ":") {
		return "", fmt.Errorf("target %q has a port or is no address; an icmp target is a host, which ICMP reaches without a port", raw)
	}
	return canonicalHost(host)
}

// icmpDocument is a scrape's replies as the transforms read them: an RTT
// key is left out when no reply came.
type icmpDocument struct {
	Host    string `json:"host"`
	Address string `json:"address"`
	// Family is 4 or 6.
	Family       int       `json:"family"`
	Sent         int       `json:"sent"`
	Received     int       `json:"received"`
	LossRatio    float64   `json:"loss_ratio"`
	RTTsSeconds  []float64 `json:"rtts_seconds"`
	RTTMin       *float64  `json:"rtt_min_seconds,omitempty"`
	RTTAvg       *float64  `json:"rtt_avg_seconds,omitempty"`
	RTTMax       *float64  `json:"rtt_max_seconds,omitempty"`
	RTTStddev    *float64  `json:"rtt_stddev_seconds,omitempty"`
	Duplicates   int       `json:"duplicates"`
	PayloadBytes int       `json:"payload_bytes"`
	// Addresses holds, with all_addresses, each address pinged, in the
	// resolver's order. They are the addresses in AddressesFound of the
	// families ip_family takes. Address and Family are then "" and 0, and
	// the remaining fields cover all addresses together.
	Addresses      []icmpAddressResult `json:"addresses,omitempty"`
	AddressesFound int                 `json:"addresses_found,omitempty"`
	// DurationSeconds is the whole scrape, the lookup included.
	DurationSeconds float64 `json:"duration_seconds"`
}

// icmpAddressResult is the ping of one address of all_addresses.
type icmpAddressResult struct {
	Address     string    `json:"address"`
	Family      int       `json:"family"`
	Sent        int       `json:"sent"`
	Received    int       `json:"received"`
	LossRatio   float64   `json:"loss_ratio"`
	RTTsSeconds []float64 `json:"rtts_seconds"`
	RTTMin      *float64  `json:"rtt_min_seconds,omitempty"`
	RTTAvg      *float64  `json:"rtt_avg_seconds,omitempty"`
	RTTMax      *float64  `json:"rtt_max_seconds,omitempty"`
	RTTStddev   *float64  `json:"rtt_stddev_seconds,omitempty"`
	Duplicates  int       `json:"duplicates"`
}

// fetchICMP pings a scrape's host and answers what came back as JSON.
func fetchICMP(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	host, err := parseICMPTarget(target)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, icmpTimeout)
		defer cancel()
	}
	// The host's name now; the address the echoes go to once the socket is
	// connected to it, as an ntp request's is checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "icmp", Host: host}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	candidates, err := icmpAddresses(ctx, host, c.Request.IPFamily)
	if err != nil {
		return nil, err
	}
	r := icmpRequest(c)
	var doc icmpDocument
	if !r.AllAddresses {
		at := traceRequestAt(ctx, icmpMethod, "icmp://"+host, http.Header{"Icmp-Address": {candidates[0].String()}, "Icmp-Count": {strconv.Itoa(r.Count)}}, "", false)
		if doc, err = icmpPingAddress(ctx, host, candidates[0], r, at); err != nil {
			return nil, err
		}
	} else {
		if doc, err = icmpPingAll(ctx, host, candidates, r); err != nil {
			return nil, err
		}
	}
	doc.Host, doc.DurationSeconds = host, time.Since(start).Seconds()
	return jsonAnswer(doc, target, c, start)
}

// icmpPingAddress pings address, of host, as r says, its request recorded
// at at: held to the target lists, a socket opened to it, the echoes sent
// and the replies read.
func icmpPingAddress(ctx context.Context, host string, address netip.Addr, r ICMPConfig, at tracedAt) (icmpDocument, error) {
	// The address is checked against the target lists before a socket is
	// opened, as a connection's address is (checkConnection). The socket is
	// connected to that address only. A refused address is refused whether
	// or not the system would open the socket.
	if err := checkConnection(ctx, net.JoinHostPort(host, "0"), icmpConnectedTo{remote: &net.UDPAddr{IP: address.AsSlice()}}); err != nil {
		at.outcome("error: "+err.Error(), true)
		return icmpDocument{}, err
	}
	conn, err := icmpDial(address)
	if err != nil {
		err = fmt.Errorf("opening an ICMP socket to ping %s: %w", address, err)
		if errors.Is(err, os.ErrPermission) {
			err = fmt.Errorf("%w; the system lets no process of the exporter's group open one: on Linux, set the sysctl net.ipv4.ping_group_range to take the group, such as \"0 2147483647\"", err)
		}
		at.outcome("error: "+err.Error(), true)
		return icmpDocument{}, err
	}
	defer func() { _ = conn.Close() }()
	doc, err := icmpPing(ctx, conn, address, r)
	if err != nil {
		at.outcome("error: "+err.Error(), true)
		return icmpDocument{}, err
	}
	at.outcome(fmt.Sprintf("%d of %d replies", doc.Received, doc.Sent), false)
	at.facts(intFact("icmp.sent", doc.Sent), intFact("icmp.received", doc.Received))
	return doc, nil
}

// icmpPingAll pings all addresses of host at once, each as its own
// request. The document lists each one in addresses, plus the counts,
// round trips and statistics of all of them together. If one fails, the
// scrape fails and the error names it.
func icmpPingAll(ctx context.Context, host string, addresses []netip.Addr, r ICMPConfig) (icmpDocument, error) {
	found := len(addresses)
	addresses = addresses[:min(found, icmpMaxAddresses)]
	docs := make([]icmpDocument, len(addresses))
	errs := make([]error, len(addresses))
	var wg sync.WaitGroup
	var panicked goroutinePanics
	for i, address := range addresses {
		at := traceRequestAt(ctx, icmpMethod, "icmp://"+host, http.Header{"Icmp-Address": {address.String()}, "Icmp-Count": {strconv.Itoa(r.Count)}}, "", false)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer panicked.keep()
			docs[i], errs[i] = icmpPingAddress(ctx, host, address, r, at)
		}()
	}
	wg.Wait()
	panicked.raise()
	if err := errors.Join(errs...); err != nil {
		return icmpDocument{}, err
	}
	all := icmpDocument{PayloadBytes: *r.PayloadBytes, AddressesFound: found, Addresses: make([]icmpAddressResult, len(docs))}
	var rtts []float64
	for i, d := range docs {
		all.Addresses[i] = icmpAddressResult{Address: d.Address, Family: d.Family, Sent: d.Sent, Received: d.Received, LossRatio: d.LossRatio,
			RTTsSeconds: d.RTTsSeconds, RTTMin: d.RTTMin, RTTAvg: d.RTTAvg, RTTMax: d.RTTMax, RTTStddev: d.RTTStddev, Duplicates: d.Duplicates}
		all.Sent += d.Sent
		all.Duplicates += d.Duplicates
		rtts = append(rtts, d.RTTsSeconds...)
	}
	icmpStatistics(&all, rtts)
	return all, nil
}

// icmpConnectedTo is the socket a scrape connects, as the target policy
// reads it: the address it is connected to.
type icmpConnectedTo struct {
	net.Conn
	remote net.Addr
}

// RemoteAddr is the address the socket is connected to.
func (c icmpConnectedTo) RemoteAddr() net.Addr { return c.remote }

// icmpAddresses returns the addresses a host may be pinged at. The first
// one is used without all_addresses. If the host is an address, it is the
// only one. Otherwise they are the name's addresses chosen by ip_family:
// ip4 or ip6 gives that family; prefer_ip4 or prefer_ip6 gives that
// family's first, then the other's; left out, both in the resolver's order.
func icmpAddresses(ctx context.Context, host, family string) ([]netip.Addr, error) {
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a.Unmap())
	} else {
		found, err := icmpLookup(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("looking up %s: %w", host, err)
		}
		for _, a := range found {
			addrs = append(addrs, a.Unmap())
		}
	}
	of := func(four bool) []netip.Addr {
		var out []netip.Addr
		for _, a := range addrs {
			if a.Is4() == four {
				out = append(out, a)
			}
		}
		return out
	}
	var pick []netip.Addr
	switch family {
	case "ip4":
		pick = of(true)
	case "ip6":
		pick = of(false)
	case "prefer_ip4":
		pick = append(of(true), of(false)...)
	case "prefer_ip6":
		pick = append(of(false), of(true)...)
	default:
		pick = addrs
	}
	if len(pick) == 0 {
		return nil, fmt.Errorf("%s has no %s address, and request.ip_family is %s", host, map[string]string{"ip4": "IPv4", "ip6": "IPv6"}[family], family)
	}
	return pick, nil
}

// icmpEcho builds an echo request's ICMP message: type 8 for IPv4 and 128
// for IPv6, code 0, the checksum, an identifier, the sequence, and the
// data, which begins with the scrape's token. The checksum is computed
// only for IPv4; for ICMPv6 the system computes it over the pseudo-header.
// The system replaces the identifier with the socket's own.
func icmpEcho(v6 bool, seq uint16, data []byte) []byte {
	msg := make([]byte, 8+len(data))
	msg[0] = 8
	if v6 {
		msg[0] = 128
	}
	binary.BigEndian.PutUint16(msg[6:], seq)
	copy(msg[8:], data)
	if !v6 {
		binary.BigEndian.PutUint16(msg[2:], icmpChecksum(msg))
	}
	return msg
}

// icmpChecksum is the Internet checksum of b (RFC 1071).
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// icmpReply reads a datagram as the reply to an echo of this scrape. It
// returns the sequence of an echo reply (type 0 or 129) whose data is the
// token and what followed it. On macOS an IPv4 ICMP datagram socket also
// reads the IP header, which is removed.
func icmpReply(b []byte, v6 bool, data []byte) (uint16, bool) {
	if !v6 && len(b) >= 20 && b[0]>>4 == 4 {
		// A header that says it is longer than the datagram is no header
		// of a reply to read, and no slice to take: this runs in the
		// goroutine that reads the socket, where a panic is the process's.
		header := int(b[0]&0x0f) * 4
		if header > len(b) {
			return 0, false
		}
		b = b[header:]
	}
	want := byte(0)
	if v6 {
		want = 129
	}
	if len(b) < 8 || b[0] != want || b[1] != 0 || string(b[8:]) != string(data) {
		return 0, false
	}
	return binary.BigEndian.Uint16(b[6:]), true
}

// icmpPing sends r.Count echoes on conn interval apart, reading the replies
// as they come, until every one has come, reply_timeout after the last was
// sent, or the scrape's deadline.
func icmpPing(ctx context.Context, conn net.Conn, address netip.Addr, r ICMPConfig) (icmpDocument, error) {
	v6 := address.Is6()
	data := make([]byte, *r.PayloadBytes)
	_, _ = rand.Read(data[:icmpToken])
	for i := icmpToken; i < len(data); i++ {
		data[i] = byte(i)
	}
	doc := icmpDocument{Address: address.String(), Family: 4, PayloadBytes: len(data), RTTsSeconds: []float64{}}
	if v6 {
		doc.Family = 6
	}
	type result struct {
		seq uint16
		at  time.Time
	}
	replies := make(chan result, r.Count*2)
	readErr := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	var panicked goroutinePanics
	go func() {
		// A panic of the reader ends the scrape as a read error does, and
		// is raised below, on the scrape's goroutine.
		defer func() {
			if recovered := recover(); recovered != nil {
				panicked.hold(recovered)
				readErr <- net.ErrClosed
			}
		}()
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			switch {
			case icmpLostEcho(err):
				// An error the network answered an echo with, such as
				// host unreachable, is that echo lost; the replies to the
				// others are read on.
				continue
			case err != nil:
				// The socket closed, the deadline, or any other end.
				readErr <- err
				return
			}
			if seq, ok := icmpReply(buf[:n], v6, data); ok {
				select {
				case replies <- result{seq, time.Now()}:
				default:
				}
			}
		}
	}()
	sent := map[uint16]time.Time{}
	got := map[uint16]bool{}
	var rtts []float64
	var wait <-chan time.Time
	next := time.NewTimer(0)
	defer next.Stop()
	collect := func(res result) {
		at, ok := sent[res.seq]
		switch {
		case !ok:
		case got[res.seq]:
			doc.Duplicates++
		default:
			got[res.seq] = true
			rtts = append(rtts, res.at.Sub(at).Seconds())
		}
	}
loop:
	for {
		select {
		case <-next.C:
			seq := uint16(len(sent) + 1) //nolint:gosec // G115: at most icmpMaxCount echoes
			// A network with no route to the host is an echo lost, as ping
			// counts it; any other failure to send ends the scrape.
			if _, err := conn.Write(icmpEcho(v6, seq, data)); err != nil && !icmpLostEcho(err) {
				return icmpDocument{}, fmt.Errorf("sending an echo to %s: %w", address, err)
			}
			sent[seq] = time.Now()
			if len(sent) < r.Count {
				next.Reset(time.Duration(r.Interval))
			} else {
				wait = time.After(time.Duration(r.ReplyTimeout))
			}
		case res := <-replies:
			collect(res)
			if len(sent) == r.Count && len(got) == r.Count {
				break loop
			}
		case <-wait:
			break loop
		case <-ctx.Done():
			break loop
		case <-readErr:
			break loop
		}
	}
	panicked.raise()
	// Replies that came with the last are counted too.
	for {
		select {
		case res := <-replies:
			collect(res)
			continue
		default:
		}
		break
	}
	doc.Sent = len(sent)
	icmpStatistics(&doc, rtts)
	return doc, nil
}

// icmpStatistics fills in doc from its Sent and the round trips rtts: the
// replies, the loss, the round trips sorted from the shortest, and their
// minimum, average, maximum and population standard deviation. The
// statistics are left out when there are no round trips.
func icmpStatistics(doc *icmpDocument, rtts []float64) {
	doc.Received = len(rtts)
	if doc.Sent > 0 {
		doc.LossRatio = 1 - float64(doc.Received)/float64(doc.Sent)
	}
	if len(rtts) > 0 {
		slices.Sort(rtts)
		minimum, maximum := rtts[0], rtts[len(rtts)-1]
		var sum, squares float64
		for _, v := range rtts {
			sum += v
		}
		avg := sum / float64(len(rtts))
		for _, v := range rtts {
			squares += (v - avg) * (v - avg)
		}
		stddev := math.Sqrt(squares / float64(len(rtts)))
		doc.RTTMin, doc.RTTAvg, doc.RTTMax, doc.RTTStddev = &minimum, &avg, &maximum, &stddev
	}
	doc.RTTsSeconds = append([]float64{}, rtts...)
}

// icmpLostEcho reports whether err is the network's answer to an echo,
// which loses that echo and no other. This is no route to the network or
// host, or an ICMP error sent back by a router or the host. A connected
// socket reports such an error on its next read or write.
func icmpLostEcho(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ECONNREFUSED)
}

// icmpKeyRules is what the configuration schema says of the type's own
// keys beyond their Go types.
func icmpKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"count":         {"minimum": 0, "maximum": icmpMaxCount, "description": "icmp: how many echo requests a scrape sends, from 1 to 100; 3 when left out. See docs/ICMP.md."},
		"interval":      {"not": SchemaLeastDuration(icmpMinInterval, true), "description": "icmp: how long apart the echoes are sent, at least 10ms; 1s when left out."},
		"reply_timeout": {"not": SchemaLeastDuration(icmpMinReplyTimeout, true), "description": "icmp: how long the last echo's reply is waited for once it is sent, at least 10ms; 1s when left out. A scrape takes about (count - 1) × interval + reply_timeout, within its deadline."},
		"all_addresses": {"description": "icmp: ping every address of the target's name, of the families ip_family takes, at most 16, at once, each in the document's addresses and the top of the document of all of them; the first alone when false, the default. See docs/ICMP.md#every-address."},
		"payload_bytes": {"minimum": icmpToken, "maximum": icmpMaxPayload, "description": "icmp: how many bytes of data each echo carries, from 8 to 1232; 56 when left out, as ping's."},
	}
}
