package fetch

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// request.rate_limit bounds how fast an http or graphite collector asks one
// origin — scheme, host and port — of its targets: requests_per_second on
// average, and burst at once after a pause. It is there for a target that
// answers badly when asked too often, such as an old appliance, which many
// probes, static targets, retries and pages can otherwise ask at the same
// moment.
//
// Every request sent counts, where it is sent: each attempt of a retry, each
// page, each hop of a followed redirect and each request of a login, the
// last two against the origin they go to, which is the target's own when
// they stay on it. A request takes a token from its bucket, waiting for one
// within its deadline; one that would have to wait past its deadline is
// refused at once rather than left to time out, since the wait could only
// end in the same failure later, with the target asked nothing.
//
// The buckets are kept by collector and origin, and by the limit itself: two
// collectors asking the same host each have their own, so one collector's
// traffic never slows another's, and a static target with a rate_limit of
// its own has its own bucket for its origin, beside the one the collector's
// other targets there share. A configuration that changes a limit starts
// the new limit from a full bucket.
//
// A bucket that has filled up again is as good as a new one, so it is
// dropped once it has; at most rateLimitMaxBuckets are kept, the least
// recently used dropped first beyond that, so a collector probed for many
// targets keeps no more than that.

// rateLimitMaxBuckets is the most buckets kept at once. Beyond it the least
// recently used is dropped, which forgets the requests it counted: the bound
// is on memory, and is far above the origins a deployment asks within the
// time a bucket takes to fill.
var rateLimitMaxBuckets = 10000

// rateLimitNow and rateLimitSleep are the clock and the wait, which tests
// replace to count tokens without sleeping.
var (
	rateLimitNow   = time.Now
	rateLimitSleep = func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

// errRateLimitRefused matches every rateLimitError: a request its
// rate_limit would have delayed past its deadline, which a retry would only
// meet again, closer to the deadline.
var errRateLimitRefused = errors.New("refused by request.rate_limit")

// rateLimitError is a request refused by request.rate_limit, or by an entry
// of rate_limits, before it was sent.
type rateLimitError struct {
	limit  string
	origin string
	wait   time.Duration
	left   time.Duration
	// rule is the name of the rate_limits entry that refused the request,
	// and origin then the host it was for; "" for request.rate_limit.
	rule string
}

func (e *rateLimitError) Error() string {
	if e.rule != "" {
		return fmt.Sprintf("rate limit of %s of rate_limits entry %q for %s would delay the request past its deadline: it would wait %s for its turn, and %s is left; ask the entry's hosts less often, raise the entry's rate_limit, or the timeout", e.limit, e.rule, e.origin, e.wait.Round(time.Millisecond), e.left.Round(time.Millisecond))
	}
	return fmt.Sprintf("rate limit of %s for %s would delay the request past its deadline: it would wait %s for its turn, and %s is left; ask the target less often, raise request.rate_limit, or the timeout", e.limit, e.origin, e.wait.Round(time.Millisecond), e.left.Round(time.Millisecond))
}

// Is makes every rateLimitError match errRateLimitRefused.
func (e *rateLimitError) Is(target error) bool { return target == errRateLimitRefused }

// rateLimitSetting is the limit in force for one trip's requests, carried in
// its context, so the requests the trip makes beyond the first — retries,
// pages, redirects, logins — wait on it wherever they are sent. Every http
// and graphite trip carries one, perSecond 0 when the collector has no
// limit of its own, since the entries of rate_limits apply to its requests
// all the same; no other type's trip carries one, and their requests take
// no turn under either.
type rateLimitSetting struct {
	collector string
	perSecond float64
	burst     int
}

type rateLimitContextKey struct{}

// RateLimitBurst is the burst of l in force: its own, or 1.
func RateLimitBurst(l *model.RateLimitConfig) int {
	if l == nil || l.Burst == nil {
		return 1
	}
	return *l.Burst
}

// describeRateLimit is how a limit is named in messages.
func describeRateLimit(perSecond float64, burst int) string {
	rate := strconv.FormatFloat(perSecond, 'g', -1, 64)
	unit := "requests"
	if perSecond == 1 {
		unit = "request"
	}
	if burst == 1 {
		return fmt.Sprintf("%s %s per second", rate, unit)
	}
	return fmt.Sprintf("%s %s per second, %d at once", rate, unit, burst)
}

// withRateLimit returns ctx carrying the rate limit in force for a request
// of c: a static target's own, or the collector's, or none, with which the
// requests still take their turns under rate_limits.
func withRateLimit(ctx context.Context, c *model.Collector, overrides RequestOverrides) context.Context {
	l := c.Request.RateLimit
	if overrides.RateLimit != nil {
		l = overrides.RateLimit
	}
	setting := &rateLimitSetting{collector: c.Name}
	if l != nil {
		setting.perSecond, setting.burst = l.RequestsPerSecond, RateLimitBurst(l)
	}
	return context.WithValue(ctx, rateLimitContextKey{}, setting)
}

// validateRateLimit checks request.rate_limit and fills in its burst.
func validateRateLimit(c *model.Collector) error {
	l := c.Request.RateLimit
	if l == nil {
		return nil
	}
	if err := checkRateLimit(l); err != nil {
		return fmt.Errorf("collector %q request.rate_limit%w", c.Name, err)
	}
	if l.Burst == nil {
		burst := 1
		l.Burst = &burst
	}
	return nil
}

// checkRateLimit checks the values of a rate_limit block, a collector's or a
// static target's, without writing into it. Its error begins with the key it
// is about, after the block's name.
func checkRateLimit(l *model.RateLimitConfig) error {
	if math.IsNaN(l.RequestsPerSecond) || math.IsInf(l.RequestsPerSecond, 0) || l.RequestsPerSecond <= 0 {
		return fmt.Errorf(".requests_per_second is %v; it is required, and is a number of requests a second greater than 0, such as 5, or 0.5 for one request every 2 seconds", l.RequestsPerSecond)
	}
	if l.Burst != nil && *l.Burst < 1 {
		return fmt.Errorf(".burst is %d; it is how many requests may be sent at once after a pause, at least 1, and 1 when left out", *l.Burst)
	}
	return nil
}

// requestOrigin is the scheme, host and port u is sent to, the port filled
// in when the URL leaves it out, so two spellings of one origin share a
// bucket.
func requestOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// rateLimitWait is how long a request waited for its turns, and what a
// debug report says of the waits.
type rateLimitWait struct {
	waited time.Duration
	note   string
}

// rateLimitTurn is a bucket a request takes a turn from: its collector's,
// under request.rate_limit, or that of the entry of rate_limits its host
// matches, rule then naming the entry and host the host.
type rateLimitTurn struct {
	set  *rateLimiterSet
	key  rateLimitKey
	rule string
	host string
}

// limit is how the turn's limit is named in messages: by its rate, and the
// origin or the entry and the host it is for.
func (t rateLimitTurn) limit() string {
	if t.rule != "" {
		return fmt.Sprintf("the rate limit of %s of rate_limits entry %q for %s", describeRateLimit(t.key.perSecond, t.key.burst), t.rule, t.host)
	}
	return fmt.Sprintf("the rate limit of %s for %s", describeRateLimit(t.key.perSecond, t.key.burst), t.key.origin)
}

// rateLimitTurns are the buckets a request to u under setting takes a turn
// from, in order: its collector's, when it has a limit, then the entry of
// rate_limits its host matches, when one does.
func rateLimitTurns(setting *rateLimitSetting, u *url.URL) []rateLimitTurn {
	var turns []rateLimitTurn
	if setting.perSecond > 0 {
		turns = append(turns, rateLimitTurn{set: rateLimits, key: rateLimitKey{collector: setting.collector, origin: requestOrigin(u), perSecond: setting.perSecond, burst: setting.burst}})
	}
	if rule, host := matchHostRateLimit(u); rule != nil {
		turns = append(turns, rateLimitTurn{set: hostRateLimits, key: rule.key, rule: rule.key.collector, host: host})
	}
	return turns
}

// waitRateLimit waits for the turns of a request to u under the rate limits
// that apply to it — the one ctx carries and the entry of rate_limits its
// host matches — one after the other, and returns how long it waited:
// nothing without a limit, or with a token to spare in each. A wait that
// would end past ctx's deadline is not begun, and the request is refused
// with a rateLimitError; the turns it took under the others are given back,
// since it is not sent.
func waitRateLimit(ctx context.Context, u *url.URL) (rateLimitWait, error) {
	setting, ok := ctx.Value(rateLimitContextKey{}).(*rateLimitSetting)
	if !ok {
		return rateLimitWait{}, nil
	}
	// A debug run's requests take their turns as any other's, so a debug
	// probe asks the target no faster than the limit, but count in no
	// self-metric of the collector's or of rate_limits' (§ 42.17).
	_, debug := ctx.Value(requestTraceKey{}).(*RequestTrace)
	var out rateLimitWait
	var taken []rateLimitTurn
	for _, turn := range rateLimitTurns(setting, u) {
		wait, err := takeRateLimitTurn(ctx, turn, !debug)
		if err != nil {
			for _, t := range taken {
				t.set.giveBack(t.key)
			}
			return rateLimitWait{}, err
		}
		taken = append(taken, turn)
		if wait > 0 {
			out.waited += wait
			out.note = joinWaits(out.note, fmt.Sprintf("sent after waiting %s for its turn under %s", wait.Round(time.Millisecond), turn.limit()))
		}
	}
	return out, nil
}

// takeRateLimitTurn waits for the request's turn in one bucket, read
// against the clock as it is after the turns taken before it, and returns
// how long it waited.
func takeRateLimitTurn(ctx context.Context, turn rateLimitTurn, counted bool) (time.Duration, error) {
	deadline, hasDeadline := ctx.Deadline()
	wait, err := turn.set.reserve(turn.key, rateLimitNow(), deadline, hasDeadline, counted)
	if err != nil {
		var refused *rateLimitError
		if turn.rule != "" && errors.As(err, &refused) {
			refused.rule, refused.origin = turn.rule, turn.host
		}
		return 0, err
	}
	if wait == 0 {
		return 0, nil
	}
	if err := rateLimitSleep(ctx, wait); err != nil {
		// The turn not taken is given back, so a trip cut short does not
		// slow those after it.
		turn.set.giveBack(turn.key)
		return 0, fmt.Errorf("the wait of %s for its turn under %s was cut short: %w", wait.Round(time.Millisecond), turn.limit(), err)
	}
	return wait, nil
}

// traceRateLimitWait records on the last request recorded, the one just
// waited for, how long it waited for its turns and why. recordedBefore says
// it was recorded before the wait, a redirect being recorded before it is
// checked: its duration then starts after the wait, which is no time the
// target took.
func traceRateLimitWait(ctx context.Context, w rateLimitWait, recordedBefore bool) {
	if w.waited <= 0 {
		return
	}
	t, ok := ctx.Value(requestTraceKey{}).(*RequestTrace)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := len(t.requests); n > 0 {
		r := &t.requests[n-1]
		r.Wait = joinWaits(r.Wait, w.note)
		if recordedBefore {
			r.started = r.started.Add(w.waited)
		}
	}
}

// joinWaits adds a wait to those a request has recorded.
func joinWaits(recorded, wait string) string {
	if recorded == "" {
		return wait
	}
	return recorded + "; " + wait
}

// rateLimitKey names a bucket: a collector's requests to an origin under one
// limit. For an entry of rate_limits (hostratelimit.go), collector is the
// entry's name, under which its waits are counted, and origin its hosts, so
// an entry a reload changes in any way has a new bucket.
type rateLimitKey struct {
	collector string
	origin    string
	perSecond float64
	burst     int
}

// rateBucket is a token bucket: tokens, at most burst, as of last. Tokens
// go below zero for the turns already promised to requests waiting.
type rateBucket struct {
	key    rateLimitKey
	tokens float64
	last   time.Time
}

// full says whether the bucket has filled up again by now, when it is as
// good as a new one.
func (b *rateBucket) full(now time.Time) bool {
	return b.tokens+now.Sub(b.last).Seconds()*b.key.perSecond >= float64(b.key.burst)
}

// rateLimiterSet is every bucket, most recently used first, and the waits
// and refusals of each collector.
type rateLimiterSet struct {
	mu      sync.Mutex
	buckets map[rateLimitKey]*list.Element
	order   *list.List
	waited  map[string]uint64
	refused map[string]uint64
}

var rateLimits = newRateLimiterSet()

func newRateLimiterSet() *rateLimiterSet {
	return &rateLimiterSet{buckets: map[rateLimitKey]*list.Element{}, order: list.New(), waited: map[string]uint64{}, refused: map[string]uint64{}}
}

// reserve takes a turn for a request under key at now: at once, when the
// bucket has a token, or after the wait it returns. A turn that would come
// at or past deadline is not taken. counted says the wait or the refusal
// is counted for the collector.
func (s *rateLimiterSet) reserve(key rateLimitKey, now, deadline time.Time, hasDeadline, counted bool) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The oldest buckets that have filled up again are dropped first: they
	// hold nothing a new bucket would not.
	for back := s.order.Back(); back != nil; back = s.order.Back() {
		b := back.Value.(*rateBucket)
		if !b.full(now) {
			break
		}
		s.remove(back)
	}
	var b *rateBucket
	if element, ok := s.buckets[key]; ok {
		b = element.Value.(*rateBucket)
		s.order.MoveToFront(element)
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens = math.Min(float64(key.burst), b.tokens+elapsed.Seconds()*key.perSecond)
			b.last = now
		}
	} else {
		b = &rateBucket{key: key, tokens: float64(key.burst), last: now}
		s.buckets[key] = s.order.PushFront(b)
		for len(s.buckets) > rateLimitMaxBuckets {
			s.remove(s.order.Back())
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0, nil
	}
	wait := time.Duration(math.Ceil((1 - b.tokens) / key.perSecond * float64(time.Second)))
	if hasDeadline && !now.Add(wait).Before(deadline) {
		if counted {
			s.refused[key.collector]++
		}
		return 0, &rateLimitError{limit: describeRateLimit(key.perSecond, key.burst), origin: key.origin, wait: wait, left: max(deadline.Sub(now), 0)}
	}
	b.tokens--
	if counted {
		s.waited[key.collector]++
	}
	return wait, nil
}

// giveBack returns the turn a request under key did not take.
func (s *rateLimiterSet) giveBack(key rateLimitKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.buckets[key]; ok {
		b := element.Value.(*rateBucket)
		b.tokens = math.Min(float64(key.burst), b.tokens+1)
	}
}

func (s *rateLimiterSet) remove(element *list.Element) {
	delete(s.buckets, element.Value.(*rateBucket).key)
	s.order.Remove(element)
}

// follow drops the buckets and counts of the collectors no longer in force.
func (s *rateLimiterSet) follow(collectors []model.Collector) {
	names := make(map[string]bool, len(collectors))
	for i := range collectors {
		names[collectors[i].Name] = true
	}
	s.keep(func(key rateLimitKey) bool { return names[key.collector] }, names)
}

// keep drops the buckets whose key kept refuses, and the counts of the
// names not among names.
func (s *rateLimiterSet) keep(kept func(rateLimitKey) bool, names map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for element := s.order.Front(); element != nil; {
		next := element.Next()
		if !kept(element.Value.(*rateBucket).key) {
			s.remove(element)
		}
		element = next
	}
	for _, counts := range []map[string]uint64{s.waited, s.refused} {
		for name := range counts {
			if !names[name] {
				delete(counts, name)
			}
		}
	}
}

// reset forgets every bucket and count, at the exporter's shutdown.
func (s *rateLimiterSet) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets, s.order = map[rateLimitKey]*list.Element{}, list.New()
	s.waited, s.refused = map[string]uint64{}, map[string]uint64{}
}

// size is how many buckets are kept.
func (s *rateLimiterSet) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buckets)
}

// CollectorRateLimit reports how many requests of a collector waited for
// their turn under its request.rate_limit, or a static target's, and how
// many it refused for a wait past their deadline.
func CollectorRateLimit(collector string) (waited, refused uint64) {
	rateLimits.mu.Lock()
	defer rateLimits.mu.Unlock()
	return rateLimits.waited[collector], rateLimits.refused[collector]
}
