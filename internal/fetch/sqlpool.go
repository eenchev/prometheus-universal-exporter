//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A sql collector keeps its connections to a target in a pool, a
// database/sql DB, one for each collector, target and connection settings
// (sqlPoolKey): the account and credential files, the database, the
// options, the TLS settings, the connection settings and the policy, so
// that a change to any of them is a pool of its own. A pool is made at the
// first scrape that needs it and shared by the collector's later scrapes of
// the target; a pool is never shared between collectors.
//
// The limits are of targets, not pools: a target (sqlTargetKey) is the
// collector, the server or Snowflake account, and the connection settings,
// so that the pools of the databases and accounts a collector reaches on one
// server are one target's, while a static target whose connections block
// gives it other settings is a target of its own, with its own limits.
//
//   - max_per_target bounds the connections to a target, across its pools:
//     a scrape, or a keepalive ping, holds one of the target's places
//     (sqlTarget.sem) while it has a connection. A scrape that finds every
//     place taken waits for one within its deadline, counted in the
//     collector's limit waits, and fails naming the limit when the deadline
//     comes first, counted in its limit failures. The connections kept idle
//     hold no place, so a scrape that would make a connection while the
//     target's places taken and its other pools' idle connections come to
//     more than the limit first closes enough of those idle connections
//     (makeRoom): the connections open to the target, in use or idle, are
//     held to the limit. A reload that changes the settings makes another
//     target, so that the old one's pools drain under the old limit. Each
//     pool is held to the limit too (DB.SetMaxOpenConns). A target keeps no
//     more pools than max_per_target, a pool being of no use without a
//     connection, and no more than sqlMaxPoolsPerTarget without a limit: a
//     scrape that needs another pool at the limit closes the target's pool
//     used longest ago that no scrape is using, and fails, counted in the
//     limit failures, when every one is in use (roomForPoolLocked).
//   - max_idle_per_target is how many connections a target keeps idle
//     between scrapes, across its pools: a pool keeps no more
//     (SetMaxIdleConns), and a connection given back to a target whose pools
//     keep that many idle already is closed (giveBack). idle_timeout is how
//     long an idle one is kept (SetConnMaxIdleTime) and max_lifetime how
//     long one is used at all (SetConnMaxLifetime); 0 keeps none idle, keeps
//     them while the collector exists, and uses them for ever.
//   - max_targets bounds the targets a collector has pools open for: a
//     scrape of another target at the limit closes the pools of the target
//     used longest ago that no scrape is using, and fails, counted in the
//     limit failures, when every target is in use.
//   - keepalive check tcp sets TCP keepalives at the interval on every
//     connection the exporter dials, so that one a firewall or a dead host
//     dropped is found broken. check ping asks the database instead: every
//     interval each idle connection of the pool is pinged, and a connection
//     a ping finds broken is closed by database/sql when the driver says
//     so; each failed ping is counted in the collector's keepalive failures.
//     Each ping takes one of the target's places at max_per_target without
//     waiting, the connections pinged being as many as the places free, and
//     the round is skipped for the interval when the scrapes hold all of
//     them; it is given the interval to answer in, so that it holds the
//     place no longer than until the next is due, and one that does not
//     answer by then is a failed ping. It never makes a connection
//     (sqlNoConnect): it pings those the pool keeps idle or none. A ping
//     is no use of the connection to idle_timeout: database/sql counts a
//     connection's idle time from when it was last given back, a ping's
//     giving back too, so with an idle_timeout the pool keeps when a scrape
//     last had each connection (sqlIdleUse), and the keepalive closes the
//     connection it took instead of pinging it when no scrape has had it
//     for idle_timeout.
//
// The credential and TLS files are read each time a connection is made, so
// a password or a certificate replaced on disk is used by the next
// connection. A reload closes the pools of collectors it removes and those
// whose settings it changes (follow); the exporter's shutdown closes all
// of them (closeAll). A closed pool is forgotten at once, so the next
// scrape makes a new one, but the scrapes using it then, a statement in
// progress or a scrape waiting at its max_per_target, finish on it: its
// database/sql DB is closed when the last of them is done. A driver's Close
// of a connection may wait on the network, so connections are closed apart
// from the cache's lock and from a scrape past its deadline; and a
// statement's end, which a driver may not bound, is bounded by the scrape's
// deadline (end).

// sqlPoolKey is what a pool is kept by: the collector, the target, the
// database with its placeholders filled, and the collector's settings.
type sqlPoolKey struct {
	collector string
	address   sqlAddress
	database  string
	base      sqlBase
}

// sqlTargetKey is what a target's limits are kept by: the collector, the
// server or account, and the connection settings.
type sqlTargetKey struct {
	collector string
	address   sqlAddress
	conns     sqlConnSettings
}

// sqlTarget is a target's limits and pools, kept while one of its pools'
// DB is open, a pool a reload closed while scrapes were using it among
// them, so that a pool made for the target in the meantime shares its
// places with the scrapes finishing on the old one.
type sqlTarget struct {
	key sqlTargetKey
	// sem is the target's places at max_per_target, held by a scrape or a
	// keepalive ping while it has a connection; nil without a limit.
	sem chan struct{}
	// pools is the target's pools whose DB is open, and open how many of
	// them are not closed; under the cache's lock.
	pools map[*sqlPool]struct{}
	open  int
	// giving is held while a connection is given back to one of the
	// target's pools to be kept idle (giveBack), from the count of the idle
	// connections to the connection's return, so that two given back at once
	// are counted one after the other.
	giving sync.Mutex
}

// sqlMaxPoolsPerTarget is how many pools a target without a max_per_target
// keeps open at most: each database or account a probe names is a pool, a
// DB with goroutines of its own, and a target with a max_per_target keeps no
// more pools than it may have connections.
const sqlMaxPoolsPerTarget = 64

// poolsPerTarget is how many pools a target of settings s keeps open at most.
func poolsPerTarget(s sqlConnSettings) int {
	if s.maxPerTarget > 0 {
		return s.maxPerTarget
	}
	return sqlMaxPoolsPerTarget
}

// sqlPool is one pool of connections.
type sqlPool struct {
	key     sqlPoolKey
	driver  *sqlDriver
	options map[string]string
	db      *sql.DB
	// target is the target whose limits the pool is held to.
	target *sqlTarget
	// inUse is how many scrapes are using the pool, lastUsed when one
	// last ended, and closed whether the pool has been closed; all three
	// under the cache's lock.
	inUse    int
	lastUsed time.Time
	closed   bool
	// uncounted says the pool was closed for a collector the
	// configuration no longer has, or at the shutdown: the connections its
	// last scrapes make are no longer the collector's to count. Under the
	// cache's lock.
	uncounted bool
	// stop ends the pool's keepalive pings.
	stop chan struct{}
	// made counts the connections made.
	made atomic.Uint64
	// idle is when a scrape last had each connection, for a pool that
	// pings with an idle_timeout; nil for any other.
	idle *sqlIdleUse
	// sockets is the network connections each connection dialed, by the
	// driver's connection as idle keeps them, those of a connection whose
	// network connections are all closed forgotten when the pool makes
	// another (keepSockets).
	socketsMu sync.Mutex
	sockets   map[any]*sqlSockets
}

// sqlSockets is the network connections one connection of a pool dialed
// through the exporter's dialer, so that a connection whose statement's end
// outlives the scrape's deadline can be closed under its driver (end): a
// driver whose end takes no context and sets no deadline, as
// go-sql-driver/mysql's Rollback (transaction.go, exec("ROLLBACK")), returns
// once its network connection is closed. The connections are kept as the
// dialer gives them, so that a driver that looks into its connection
// (mysql's *net.TCPConn and syscall.Conn) finds what it did before; those
// the driver closed are forgotten at its next dial.
type sqlSockets struct {
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

var errSQLAbandoned = errors.New("the connection was closed: its statement's end outlived the scrape's deadline")

// wrap is dial, its connections kept in s.
func (s *sqlSockets) wrap(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			_ = conn.Close()
			return nil, errSQLAbandoned
		}
		s.conns = append(slices.DeleteFunc(s.conns, socketClosed), conn)
		return conn, nil
	}
}

// open says one of the network connections is open.
func (s *sqlSockets) open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.conns, func(conn net.Conn) bool { return !socketClosed(conn) })
}

// close closes the network connections, and refuses any dialed later.
func (s *sqlSockets) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, conn := range s.conns {
		_ = conn.Close()
	}
	s.conns = nil
}

// socketClosed says conn is closed: a closed network connection fails
// SyscallConn's Control. One that is no syscall.Conn is taken as open.
func socketClosed(conn net.Conn) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return true
	}
	return raw.Control(func(uintptr) {}) != nil
}

// keepSockets keeps the network connections of the pool's new connection
// dc, and forgets those of connections whose network connections are all
// closed: the connections database/sql closed.
func (p *sqlPool) keepSockets(dc any, s *sqlSockets) {
	p.socketsMu.Lock()
	defer p.socketsMu.Unlock()
	for k, kept := range p.sockets {
		if !kept.open() {
			delete(p.sockets, k)
		}
	}
	if s.open() {
		if p.sockets == nil {
			p.sockets = map[any]*sqlSockets{}
		}
		p.sockets[dc] = s
	}
}

// socketsOf is the network connections of the scrape's connection, nil when
// none is known: a driver that dials by itself (vertica).
func (p *sqlPool) socketsOf(conn *sql.Conn) *sqlSockets {
	var s *sqlSockets
	_ = conn.Raw(func(dc any) error {
		p.socketsMu.Lock()
		s = p.sockets[sqlDriverConn(dc)]
		p.socketsMu.Unlock()
		return nil
	})
	return s
}

// sqlIdleUse is when a scrape last had each connection of a pool, by the
// driver's connection, which database/sql hands out guarded (Conn.Raw,
// sqlDriverConn): every driver's connection is a pointer, so it is one key
// for as long as it is open. An entry older than the idle_timeout is the
// connection's end, so the keepalive forgets it then (prune): what
// database/sql closed leaves nothing behind for long.
type sqlIdleUse struct {
	timeout time.Duration
	mu      sync.Mutex
	used    map[any]time.Time
}

// mark records that a scrape has conn now.
func (u *sqlIdleUse) mark(conn *sql.Conn) {
	_ = conn.Raw(func(dc any) error {
		u.mu.Lock()
		u.used[sqlDriverConn(dc)] = time.Now()
		u.mu.Unlock()
		return nil
	})
}

// expired says no scrape has had dc for the idle_timeout; one no scrape
// ever had, such as a connection the keepalive made itself, has expired.
func (u *sqlIdleUse) expired(dc any) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	used, ok := u.used[dc]
	return !ok || time.Since(used) >= u.timeout
}

// forget forgets every connection, when the pool is closed.
func (u *sqlIdleUse) forget() {
	u.mu.Lock()
	defer u.mu.Unlock()
	clear(u.used)
}

// prune forgets the connections no scrape has had for the idle_timeout.
func (u *sqlIdleUse) prune() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for dc, used := range u.used {
		if time.Since(used) >= u.timeout {
			delete(u.used, dc)
		}
	}
}

// sqlPoolCache is the pools, and the counts of each collector's
// connections, keepalive failures, limit waits and failures, and the rows
// of its last statement.
type sqlPoolCache struct {
	mu      sync.Mutex
	pools   map[sqlPoolKey]*sqlPool
	targets map[sqlTargetKey]*sqlTarget
	// draining is the pools closed while scrapes were using them, until the
	// last is done, whose connections are still the collector's.
	draining                                               map[*sqlPool]struct{}
	connects, keepaliveFailures, limitWaits, limitFailures map[string]uint64
	rows                                                   map[string]int
}

var sqlPools = newSQLPoolCache()

func newSQLPoolCache() *sqlPoolCache {
	return &sqlPoolCache{pools: map[sqlPoolKey]*sqlPool{}, targets: map[sqlTargetKey]*sqlTarget{}, draining: map[*sqlPool]struct{}{}, connects: map[string]uint64{}, keepaliveFailures: map[string]uint64{}, limitWaits: map[string]uint64{}, limitFailures: map[string]uint64{}, rows: map[string]int{}}
}

// sqlPoolEvent, set by tests, is told what happens to pools and their
// connections: "made" (a connection), "keepalive" (a ping answered),
// "keepalive failed", "keepalive skipped" (every place at max_per_target
// taken), "idle closed" (a connection the keepalive closed for its
// idle_timeout), "room made" (an idle connection closed for a scrape at
// max_per_target), "closed: " and why for a pool closed, "released" (its DB
// closed, the last scrape using it done), "waiting: max_per_target" for a
// scrape that waits for a connection, and "end abandoned" for a statement
// whose end outlived the scrape's deadline, each with the target's
// host:port.
var sqlPoolEvent atomic.Pointer[func(event, address string)]

func sqlEvent(event, address string) {
	if hook := sqlPoolEvent.Load(); hook != nil {
		(*hook)(event, address)
	}
}

// sqlConnectHook, set by tests, makes connections in place of the
// driver: it is given the driver's name and what the driver would have
// been given.
var sqlConnectHook atomic.Pointer[func(ctx context.Context, driverName string, spec *sqlConnectSpec) (driver.Conn, error)]

// sqlNoConnect, a value of a context, says that a connection is to be taken
// from those a pool keeps idle and none made: the pool's connector refuses
// to make one, with errSQLNoIdle, which database/sql gives back as it is.
type sqlNoConnect struct{}

var errSQLNoIdle = errors.New("the pool has no idle connection")

// noConnect is ctx, under which a pool makes no connection.
func noConnect(ctx context.Context) context.Context {
	return context.WithValue(ctx, sqlNoConnect{}, true)
}

// acquire is the pool of key, made now when there is none, with one more
// scrape using it.
func (c *sqlPoolCache) acquire(key sqlPoolKey, options map[string]string) (*sqlPool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pools[key]; p != nil {
		p.inUse++
		return p, nil
	}
	s := key.base.conns
	tkey := sqlTargetKey{collector: key.collector, address: key.address, conns: s}
	t := c.targets[tkey]
	if limit := s.maxTargets; limit > 0 && (t == nil || t.open == 0) {
		if err := c.roomForTargetLocked(key.collector, limit); err != nil {
			return nil, err
		}
	}
	if t == nil {
		t = &sqlTarget{key: tkey, pools: map[*sqlPool]struct{}{}}
		if s.maxPerTarget > 0 {
			t.sem = make(chan struct{}, s.maxPerTarget)
		}
		c.targets[tkey] = t
	} else if limit := poolsPerTarget(s); t.open >= limit {
		if err := c.roomForPoolLocked(t, limit); err != nil {
			return nil, err
		}
		// Closing the target's last pool forgot the target.
		c.targets[tkey] = t
	}
	p := &sqlPool{key: key, driver: sqlDriverTable[key.base.driver], options: options, target: t, stop: make(chan struct{}), inUse: 1}
	t.pools[p] = struct{}{}
	t.open++
	p.db = sql.OpenDB(&sqlConnector{cache: c, pool: p})
	p.db.SetMaxOpenConns(s.maxPerTarget)
	p.db.SetMaxIdleConns(s.maxIdle)
	p.db.SetConnMaxIdleTime(s.idleTimeout)
	p.db.SetConnMaxLifetime(s.maxLifetime)
	if s.check == sqlCheckPing && s.keepalive > 0 {
		if s.idleTimeout > 0 {
			p.idle = &sqlIdleUse{timeout: s.idleTimeout, used: map[any]time.Time{}}
		}
		go c.keepalive(p, s.keepalive)
	}
	c.pools[key] = p
	return p, nil
}

// roomForTargetLocked makes room for one more target of a collector at its
// max_targets, limit: it closes the pools of the target used longest ago
// that no scrape is using, when the collector has limit targets with pools
// open, and fails, counted, when every one of them is in use.
func (c *sqlPoolCache) roomForTargetLocked(collector string, limit int) error {
	busy := map[*sqlTarget]bool{}
	last := map[*sqlTarget]time.Time{}
	for k, p := range c.pools {
		if k.collector != collector {
			continue
		}
		busy[p.target] = busy[p.target] || p.inUse > 0
		if p.lastUsed.After(last[p.target]) {
			last[p.target] = p.lastUsed
		}
	}
	if len(busy) < limit {
		return nil
	}
	var oldest *sqlTarget
	for t, inUse := range busy {
		if !inUse && (oldest == nil || last[t].Before(last[oldest])) {
			oldest = t
		}
	}
	if oldest == nil {
		c.limitFailures[collector]++
		return fmt.Errorf("collector %q has request.connections.max_targets, %d, targets with connections open, each in use by a scrape; raise max_targets", collector, limit)
	}
	for _, p := range c.pools {
		if p.target == oldest {
			c.closeLocked(p, "max_targets", true)
		}
	}
	return nil
}

// roomForPoolLocked makes room for one more pool of a target at limit pools
// (poolsPerTarget): it closes the target's pool used longest ago that no
// scrape is using, and fails, counted, when every one of them is in use.
func (c *sqlPoolCache) roomForPoolLocked(t *sqlTarget, limit int) error {
	var oldest *sqlPool
	for q := range t.pools {
		if !q.closed && q.inUse == 0 && (oldest == nil || q.lastUsed.Before(oldest.lastUsed)) {
			oldest = q
		}
	}
	if oldest == nil {
		c.limitFailures[t.key.collector]++
		if t.key.conns.maxPerTarget > 0 {
			return fmt.Errorf("collector %q has request.connections.max_per_target, %d, pools open to %s, one for each database and account scraped there, each in use by a scrape, and a target keeps no more pools than it may have connections; raise max_per_target", t.key.collector, limit, t.key.address.hostPort)
		}
		return fmt.Errorf("collector %q has %d pools open to %s, one for each database and account scraped there, each in use by a scrape, the most a target without request.connections.max_per_target keeps; scrape fewer of its databases and accounts at once, or set max_per_target", t.key.collector, limit, t.key.address.hostPort)
	}
	c.closeLocked(oldest, "pools per target", true)
	return nil
}

// release is a scrape done with its pool, which closes a pool closed while
// it was in use when the scrape is the last using it.
func (c *sqlPoolCache) release(p *sqlPool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.inUse--
	p.lastUsed = time.Now()
	if p.closed && p.inUse == 0 {
		delete(c.draining, p)
		c.closeDBLocked(p)
	}
}

// closeLocked forgets a pool, so that the next scrape makes a new one, and
// closes it: at once when no scrape is using it, and otherwise when the
// last of them is done (release), its idle connections closed now and each
// in use when it is given back, while the scrapes using it, waiting at its
// max_per_target among them, finish on it. counted says the connections
// they make are still the collector's to count.
func (c *sqlPoolCache) closeLocked(p *sqlPool, why string, counted bool) {
	if p.closed {
		return
	}
	p.closed = true
	p.uncounted = !counted
	p.target.open--
	if c.pools[p.key] == p {
		delete(c.pools, p.key)
	}
	close(p.stop)
	sqlEvent("closed: "+why, p.key.address.hostPort)
	if p.inUse == 0 {
		c.closeDBLocked(p)
		return
	}
	// database/sql closes the idle connections in SetMaxIdleConns, each
	// driver's Close, which may wait on the network, called there: apart,
	// not under the cache's lock.
	go p.db.SetMaxIdleConns(0)
	if counted {
		c.draining[p] = struct{}{}
	}
}

// closeDBLocked closes a pool's DB, and forgets its target with the last
// of the target's pools.
func (c *sqlPoolCache) closeDBLocked(p *sqlPool) {
	t := p.target
	delete(t.pools, p)
	if len(t.pools) == 0 && c.targets[t.key] == t {
		delete(c.targets, t.key)
	}
	closeSQLDB(p)
}

// closeSQLDB closes a pool's DB, apart: a driver's Close of a connection may
// wait on the network.
func closeSQLDB(p *sqlPool) {
	go func() {
		_ = p.db.Close()
		sqlEvent("released", p.key.address.hostPort)
	}()
}

// wait takes one of the places of the pool's target, at its
// max_per_target, waiting within ctx for one to be free.
func (c *sqlPoolCache) wait(ctx context.Context, p *sqlPool) error {
	sem := p.target.sem
	select {
	case sem <- struct{}{}:
		return nil
	default:
	}
	c.mu.Lock()
	if !p.uncounted {
		c.limitWaits[p.key.collector]++
	}
	c.mu.Unlock()
	sqlEvent("waiting: max_per_target", p.key.address.hostPort)
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		if !p.uncounted {
			c.limitFailures[p.key.collector]++
		}
		c.mu.Unlock()
		return fmt.Errorf("collector %q has request.connections.max_per_target, %d, connections to %s, and none was free before the scrape's deadline: %w; raise max_per_target, or the scrape's timeout", p.key.collector, cap(sem), p.key.address.hostPort, ctx.Err())
	}
}

// keepalive pings the idle connections of the pool every interval, until
// the pool is closed. It forgets the connections no scrape has had for the
// idle_timeout at each interval, whether it pings or not, and all of them
// when the pool is closed.
func (c *sqlPoolCache) keepalive(p *sqlPool, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			if p.idle != nil {
				p.idle.forget()
			}
			return
		case <-ticker.C:
		}
		if p.idle != nil {
			p.idle.prune()
		}
		if p.db.Stats().Idle == 0 {
			continue
		}
		round := keepaliveOnce(c, p, interval)
		if round.skipped {
			sqlEvent("keepalive skipped", p.key.address.hostPort)
			continue
		}
		for range round.closed {
			sqlEvent("idle closed", p.key.address.hostPort)
		}
		if round.failed > 0 {
			c.mu.Lock()
			if !p.closed {
				c.keepaliveFailures[p.key.collector] += uint64(round.failed)
			}
			c.mu.Unlock()
		}
		for range round.failed {
			sqlEvent("keepalive failed", p.key.address.hostPort)
		}
		for range round.pinged {
			sqlEvent("keepalive", p.key.address.hostPort)
		}
	}
}

// sqlKeepaliveRound is what one keepalive round did: how many connections it
// pinged, how many pings failed, how many connections it closed for their
// idle_timeout, and whether it was skipped, taking no connection.
type sqlKeepaliveRound struct {
	pinged, failed, closed int
	skipped                bool
}

// keepaliveOnce pings every idle connection of the pool, within interval.
// database/sql hands out the connection given back last first, so the round
// takes all the idle connections before it gives any back: each held with
// one of the target's places at max_per_target, taken without waiting, as
// many as there are places free. The connections are pinged at once, so
// that one that does not answer takes none of the others' interval, and
// given back as a scrape's are (giveBack). The round is skipped when it
// takes none: no place free, or no idle connection left by the time it
// takes one, since it makes none.
func keepaliveOnce(c *sqlPoolCache, p *sqlPool, interval time.Duration) (round sqlKeepaliveRound) {
	ctx, cancel := context.WithTimeout(noConnect(context.Background()), interval)
	defer cancel()
	sem := p.target.sem
	var conns []*sql.Conn
	defer func() {
		for _, conn := range conns {
			c.giveBack(p, conn)
			if sem != nil {
				<-sem
			}
		}
	}()
take:
	for n := p.db.Stats().Idle; len(conns) < n; {
		if sem != nil {
			select {
			case sem <- struct{}{}:
			default:
				break take
			}
		}
		conn, err := p.db.Conn(ctx)
		if err != nil {
			if sem != nil {
				<-sem
			}
			if !errors.Is(err, errSQLNoIdle) {
				round.failed++
			}
			break
		}
		conns = append(conns, conn)
	}
	if len(conns) == 0 {
		round.skipped = round.failed == 0
		return round
	}
	closed := make([]bool, len(conns))
	errs := make([]error, len(conns))
	var pings sync.WaitGroup
	for i, conn := range conns {
		pings.Go(func() { closed[i], errs[i] = pingConn(ctx, p, conn) })
	}
	pings.Wait()
	for i := range conns {
		switch {
		case closed[i]:
			round.closed++
		case errs[i] != nil:
			round.failed++
		default:
			round.pinged++
		}
	}
	return round
}

// pingConn is the keepalive's ping of one connection. With an idle_timeout
// it closes the connection instead, saying so, when no scrape has had it for
// the idle_timeout, so that the ping does not keep it. database/sql discards
// a connection whose ping, or the function given Conn.Raw, fails with
// driver.ErrBadConn.
func pingConn(ctx context.Context, p *sqlPool, conn *sql.Conn) (closed bool, err error) {
	err = conn.Raw(func(dc any) (err error) {
		// The ping runs the driver in a goroutine of the keepalive's, where
		// a panic of the driver would end the process: it is a failed ping
		// instead, and the connection is discarded. It is recovered here,
		// inside Raw, so that database/sql still unlocks the connection.
		defer func() {
			if r := recover(); r != nil {
				err = errors.Join(sqlDriverPanic(p.driver.name, r), driver.ErrBadConn)
			}
		}()
		dc = sqlDriverConn(dc)
		if p.idle != nil && p.idle.expired(dc) {
			closed = true
			return driver.ErrBadConn
		}
		if pinger, ok := dc.(driver.Pinger); ok {
			return pinger.Ping(ctx)
		}
		return nil
	})
	if closed {
		return true, nil
	}
	return false, err
}

// sqlResult is what a statement returned: its rows, as the transforms read
// them, its columns, and the rows written as JSON.
type sqlResult struct {
	rows    []any
	columns []string
	shown   []byte
}

// query runs c's statement with args on a connection of the pool of key,
// within ctx.
func (c *sqlPoolCache) query(ctx context.Context, key sqlPoolKey, col *model.Collector, args []any) (*sqlResult, error) {
	p, err := c.acquire(key, col.Request.Options)
	if err != nil {
		traceSQL(ctx, key, len(args), false, err)
		return nil, err
	}
	sem := p.target.sem
	if sem != nil {
		if err := c.wait(ctx, p); err != nil {
			c.release(p)
			traceSQL(ctx, key, len(args), false, err)
			return nil, err
		}
		c.makeRoom(ctx, p)
	}
	// giveUp gives back the scrape's place and its use of the pool, once its
	// connection is back in the pool or closed.
	giveUp := func() {
		if sem != nil {
			<-sem
		}
		c.release(p)
	}
	before := p.made.Load()
	conn, err := p.db.Conn(ctx)
	if err != nil {
		giveUp()
		err = sqlDeadline(ctx, "connecting", err)
		traceSQL(ctx, key, len(args), false, err)
		return nil, err
	}
	if p.idle != nil {
		p.idle.mark(conn)
	}
	traceSQL(ctx, key, len(args), p.made.Load() == before, nil)
	result, finish, err := p.statement(ctx, conn, col, args)
	c.end(ctx, p, conn, finish, giveUp)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if !p.uncounted {
		c.rows[col.Name] = len(result.rows)
	}
	c.mu.Unlock()
	return result, nil
}

// statement runs c's statement with args on conn, within ctx, and reads its
// rows; finish ends it, which query bounds by the scrape's deadline (end).
func (p *sqlPool) statement(ctx context.Context, conn *sql.Conn, col *model.Collector, args []any) (result *sqlResult, finish func(), err error) {
	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	// The statement has a context of its own, which ends it when its rows
	// are not all read: closing the rows of a statement still running reads
	// the rest of them with mysql and pgx (mysqlRows.Close, pgconn's
	// ResultReader.Close), up to the scrape's deadline, where a statement
	// whose context has ended is abandoned by the driver, its connection
	// closed with it.
	stmtCtx, stop := context.WithCancel(ctx)
	rows, done, err := p.driver.run(stmtCtx, conn, col.Request.Statement, args, timeout)
	if err != nil {
		err = p.sqlMessageLimit(col, err)
		err = p.sqlDeadline(ctx, "running the statement", err)
		traceOutcome(ctx, "error: "+err.Error())
		return nil, stop, err
	}
	finish = func() { done(); stop() }
	result, err = readSQLRows(rows, sqlMaxRows(col), responseLimit(col))
	if err != nil {
		// Ended before done closes the rows.
		stop()
		if p.driver.rowsError != nil {
			err = p.driver.rowsError(err)
		}
		err = p.sqlMessageLimit(col, err)
		err = p.sqlDeadline(ctx, "reading the rows", err)
		traceOutcome(ctx, "error: "+err.Error())
		return nil, finish, err
	}
	traceOutcome(ctx, fmt.Sprintf("returned %d rows of the columns %s", len(result.rows), strings.Join(result.columns, ", ")))
	return result, finish, nil
}

// sqlMessageLimit is err in the words of the response limit when it is the
// driver's refusal of a message from the server larger than the limit allows
// (sqlDriver.messageTooLarge): a row too large for the limit, refused before
// it is read.
func (p *sqlPool) sqlMessageLimit(col *model.Collector, err error) error {
	if p.driver.messageTooLarge == nil || !p.driver.messageTooLarge(err) {
		return err
	}
	return model.MarkError(model.Errorf("the server sent a message larger than the response limit of %d bytes and the %d bytes the protocol is allowed beside it, as a row too large for the limit is; return smaller values, or raise request.max_response_bytes or limits.max_response_bytes: %w",
		model.Size(responseLimit(col)), sqlMessageSlack, err), model.ErrLimitExceeded)
}

// sqlEndGrace is how long the end of a statement is waited for when the
// scrape's deadline has passed by the time it begins.
const sqlEndGrace = 100 * time.Millisecond

// end ends a scrape's statement, finish (closing its rows and rolling its
// transaction back), gives its connection back, and then its place and its
// use of the pool, giveUp. The statement's end is bounded by the scrape's
// deadline, or sqlEndGrace when that has passed already: a driver whose end
// takes no context and sets no deadline (go-sql-driver/mysql's Rollback,
// transaction.go) would hold the scrape for as long as the server does not
// answer. When the bound comes first the scrape goes on, with the rows it
// read or its failure, and the connection's network connections are closed
// (sqlSockets), which ends the driver's wait; the connection is then
// closed, never given back, and the place and the pool given up only after
// that, so that a connection still open holds its place at max_per_target.
// A connection whose network connections are not known, of a driver that
// dials by itself (vertica), holds its place until the driver returns.
func (c *sqlPoolCache) end(ctx context.Context, p *sqlPool, conn *sql.Conn, finish, giveUp func()) {
	rest := func(abandoned bool) {
		if p.idle != nil {
			p.idle.mark(conn)
		}
		if abandoned {
			discardSQLConn(conn)
		} else {
			c.giveBack(p, conn)
		}
		giveUp()
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		finish()
		rest(false)
		return
	}
	sockets := p.socketsOf(conn)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		finish()
	}()
	timer := time.NewTimer(max(time.Until(deadline), sqlEndGrace))
	defer timer.Stop()
	select {
	case <-ended:
		rest(false)
		return
	case <-timer.C:
	}
	select {
	case <-ended:
		rest(false)
		return
	default:
	}
	if sockets != nil {
		sockets.close()
	}
	sqlEvent("end abandoned", p.key.address.hostPort)
	go func() {
		<-ended
		rest(true)
	}()
}

// discardSQLConn closes conn instead of giving it back to its pool:
// database/sql closes a connection the function given Raw fails with
// driver.ErrBadConn.
func discardSQLConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// giveBack gives a connection back to its pool, to be kept idle, or closes
// it when its target's pools keep max_idle_per_target idle already: the
// limit is of the target, across its pools, while database/sql holds each
// pool to it alone (SetMaxIdleConns), which is enough for a target of one
// pool. The idle connections are counted and the connection given back
// under the target's giving lock, so that one given back in the meantime is
// counted neither twice nor not at all; a connection closed is closed after
// it, a driver's Close possibly waiting on the network. A connection of a
// closed pool is closed: the pool's idle connections are closed apart
// (closeLocked), and one given back before that would otherwise be kept,
// and taken by a scrape still waiting on the pool.
func (c *sqlPoolCache) giveBack(p *sqlPool, conn *sql.Conn) {
	t := p.target
	limit := t.key.conns.maxIdle
	c.mu.Lock()
	closed, single := p.closed, t.open < 2
	var pools []*sqlPool
	if !closed && !single && limit > 0 {
		pools = make([]*sqlPool, 0, len(t.pools))
		for q := range t.pools {
			if !q.closed {
				pools = append(pools, q)
			}
		}
	}
	c.mu.Unlock()
	switch {
	case closed:
		discardSQLConn(conn)
		return
	case single || limit <= 0:
		_ = conn.Close()
		return
	}
	t.giving.Lock()
	idle := 0
	for _, q := range pools {
		idle += q.db.Stats().Idle
	}
	keep := idle < limit
	if keep {
		_ = conn.Close()
	}
	t.giving.Unlock()
	if !keep {
		discardSQLConn(conn)
	}
}

// makeRoom closes idle connections of the other pools of p's target, which
// hold none of its places, as many as the places taken and those idle
// connections come to more than max_per_target, so that the connection the
// scrape may make next keeps the connections open to the target within the
// limit. The places taken are read before the idle connections: a scrape
// gives its connection back before its place, so a connection on its way
// back is counted once or twice, never not at all. It may close one too
// many, when p has an idle connection the scrape takes, but never makes one
// (sqlNoConnect). The connections are closed apart, a driver's Close
// possibly waiting on the network, and waited for within the scrape's
// deadline: the scrape makes no connection past it.
func (c *sqlPoolCache) makeRoom(ctx context.Context, p *sqlPool) {
	t := p.target
	c.mu.Lock()
	if len(t.pools) < 2 {
		c.mu.Unlock()
		return
	}
	others := make([]*sqlPool, 0, len(t.pools)-1)
	for q := range t.pools {
		if q != p && !q.closed {
			others = append(others, q)
		}
	}
	c.mu.Unlock()
	excess := len(t.sem) - cap(t.sem)
	for _, q := range others {
		excess += q.db.Stats().Idle
	}
	var closing sync.WaitGroup
	closes := false
	for _, q := range others {
		for excess > 0 && q.db.Stats().Idle > 0 {
			conn, err := q.db.Conn(noConnect(ctx))
			if err != nil {
				break
			}
			excess--
			closes = true
			closing.Go(func() {
				discardSQLConn(conn)
				sqlEvent("room made", q.key.address.hostPort)
			})
		}
	}
	if !closes {
		return
	}
	closed := make(chan struct{})
	go func() {
		closing.Wait()
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
	}
}

// sqlDeadline says of a failure that the scrape's deadline came first, when
// it did: a driver reports a cancelled statement in words of its own.
func sqlDeadline(ctx context.Context, doing string, err error) error {
	if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
		return fmt.Errorf("%s: the scrape's deadline came first (%w): %w", doing, ctx.Err(), err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: the scrape's deadline came first: %w", doing, err)
	}
	return fmt.Errorf("%s: %w", doing, err)
}

// sqlDeadline is the package's sqlDeadline, which also says the deadline
// came first when the server reports that it ended the statement at the
// time limit the driver set it to, the scrape's time left: the server's
// clock may end it a moment before the scrape's context ends.
func (p *sqlPool) sqlDeadline(ctx context.Context, doing string, err error) error {
	if ctx.Err() == nil && p.driver.timedOut != nil && p.driver.timedOut(err) {
		return fmt.Errorf("%s: the scrape's deadline came first (%w): %w", doing, context.DeadlineExceeded, err)
	}
	return sqlDeadline(ctx, doing, err)
}

// traceSQL records a statement in a debug probe's request section: the
// driver, server and database as the url label shows them, the account,
// the database with its placeholders filled, whether the connection was
// kept from an earlier scrape, and how many values were bound. Neither the
// values, which come from the probe, nor a credential is shown.
func traceSQL(ctx context.Context, key sqlPoolKey, args int, reused bool, err error) {
	connection := "new"
	if reused {
		connection = "reused"
	}
	database := key.database
	if sqlDriverTable[key.base.driver].serviceName {
		database = key.base.serviceName
	}
	header := http.Header{
		"Sql-Driver":     {key.base.driver},
		"Sql-User":       {key.base.username},
		"Sql-Database":   {database},
		"Sql-Connection": {connection},
		"Sql-Args":       {strconv.Itoa(args)},
	}
	traceRequest(ctx, sqlMethod, key.base.driver+"://"+key.address.hostPort+"/"+database, header, "", false)
	if err != nil {
		traceOutcome(ctx, "error: "+err.Error())
	}
}

// follow closes the pools whose collector the configuration in force no
// longer has, or has with other settings, and forgets the counts of the
// collectors it no longer has, the connections of their pools still
// draining no longer counted. A collector's settings are its own and those
// each of its static targets gives it.
func (c *sqlPoolCache) follow(collectors []model.Collector, targets []model.StaticTarget) {
	wanted := map[string]map[sqlBase]bool{}
	byName := map[string]*model.Collector{}
	for i := range collectors {
		col := &collectors[i]
		if col.Request.Type != RequestTypeSQL || sqlDriverTable[col.Request.Driver] == nil {
			continue
		}
		byName[col.Name] = col
		wanted[col.Name] = map[sqlBase]bool{sqlBaseOf(col, RequestOverrides{}): true}
	}
	for i := range targets {
		if col := byName[targets[i].Collector]; col != nil {
			wanted[col.Name][sqlBaseOf(col, TargetOverrides(&targets[i]))] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.pools {
		if !wanted[key.collector][key.base] {
			c.closeLocked(p, "reload", wanted[key.collector] != nil)
		}
	}
	// A pool an earlier reload closed while scrapes were using it is no
	// longer the collector's to count once its collector is removed.
	for p := range c.draining {
		if wanted[p.key.collector] == nil {
			p.uncounted = true
			delete(c.draining, p)
		}
	}
	for _, counts := range []map[string]uint64{c.connects, c.keepaliveFailures, c.limitWaits, c.limitFailures} {
		for name := range counts {
			if wanted[name] == nil {
				delete(counts, name)
			}
		}
	}
	for name := range c.rows {
		if wanted[name] == nil {
			delete(c.rows, name)
		}
	}
}

// closeAll closes every pool, for the exporter's shutdown, and forgets
// every count: the counts are of the exporter's life, as every other
// self-metric's, and a test's exporter is a new life.
func (c *sqlPoolCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pools {
		c.closeLocked(p, "shutdown", false)
	}
	for p := range c.draining {
		p.uncounted = true
	}
	c.draining = map[*sqlPool]struct{}{}
	c.connects, c.keepaliveFailures = map[string]uint64{}, map[string]uint64{}
	c.limitWaits, c.limitFailures = map[string]uint64{}, map[string]uint64{}
	c.rows = map[string]int{}
}

// collectorConnections reports the connections a collector's pools have
// open now, those a reload closed while scrapes were using them among them,
// and how many it has made.
func (c *sqlPoolCache) collectorConnections(collector string) (open int, made uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.pools {
		if key.collector == collector {
			open += p.db.Stats().OpenConnections
		}
	}
	for p := range c.draining {
		if p.key.collector == collector {
			open += p.db.Stats().OpenConnections
		}
	}
	return open, c.connects[collector]
}

// limitCounts reports how many of a collector's scrapes waited at its
// max_per_target, and how many failed there or at its max_targets.
func (c *sqlPoolCache) limitCounts(collector string) (waited, failed uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitWaits[collector], c.limitFailures[collector]
}

// collectorPools reports what a collector's pools hold.
func (c *sqlPoolCache) collectorPools(collector string) PoolStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	var stats PoolStats
	targets := map[*sqlTarget]bool{}
	for key, p := range c.pools {
		if key.collector == collector {
			targets[p.target] = true
			stats.Idle += p.db.Stats().Idle
		}
	}
	stats.Targets = len(targets)
	stats.KeepaliveFailures = c.keepaliveFailures[collector]
	if rows, ok := c.rows[collector]; ok {
		stats.Rows = &rows
	}
	return stats
}

// size reports how many pools are open, for tests.
func (c *sqlPoolCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pools)
}

// sqlConnector makes the connections of a pool: the credential and TLS
// files read for each, through the pool's driver.
type sqlConnector struct {
	cache *sqlPoolCache
	pool  *sqlPool
}

func (s *sqlConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if ctx.Value(sqlNoConnect{}) != nil {
		return nil, errSQLNoIdle
	}
	p := s.pool
	spec, err := p.spec()
	if err != nil {
		return nil, fmt.Errorf("connecting to %s at %s: %w", p.driver.name, p.key.address.hostPort, err)
	}
	sockets := &sqlSockets{}
	spec.dial = sockets.wrap(spec.dial)
	connect := p.driver.connect
	if hook := sqlConnectHook.Load(); hook != nil {
		connect = func(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
			return (*hook)(ctx, p.driver.name, spec)
		}
	}
	conn, err := sqlConnectRecovering(ctx, p.driver.name, connect, spec)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s at %s: %w", p.driver.name, p.key.address.hostPort, err)
	}
	p.keepSockets(conn, sockets)
	p.made.Add(1)
	s.cache.mu.Lock()
	if !p.uncounted {
		s.cache.connects[p.key.collector]++
	}
	s.cache.mu.Unlock()
	sqlEvent("made", p.key.address.hostPort)
	return guardSQLConn(p.driver.name, conn), nil
}

func (*sqlConnector) Driver() driver.Driver { return sqlDriverStub{} }

// spec is what the pool's next connection is made with, its credential
// and TLS files read now.
func (p *sqlPool) spec() (*sqlConnectSpec, error) {
	base := &p.key.base
	password, key, err := sqlCredentials(base)
	if err != nil {
		return nil, err
	}
	address := p.key.address
	config, digest, err := sqlTLSConfig(p.driver, base, p.options, address.host)
	if err != nil {
		return nil, err
	}
	var keepalive time.Duration
	if base.conns.check == sqlCheckTCP {
		keepalive = base.conns.keepalive
	}
	return &sqlConnectSpec{
		host: address.host, port: address.port, hostPort: address.hostPort, account: address.account,
		username: base.username, password: password, privateKey: key,
		database: p.key.database, serviceName: base.serviceName,
		options: p.options, tls: config, tlsDigest: digest,
		dial:    sqlDial(base.policy, address.host, keepalive),
		appName: sqlAppName,
		limit:   base.limit,
	}, nil
}

// SQLConnectInfo is what a connection of the sql request type is made with,
// as a test's stand-in for the driver is given it (UseSQLTestDriver): the
// driver's name, the target, the account and the password read from its
// file, the database, the options and whether the connection uses TLS.
type SQLConnectInfo struct {
	Driver, Host, Port, Account, Username, Password, Database, ServiceName string
	Options                                                                map[string]string
	TLS                                                                    bool
}

// UseSQLTestDriver makes every connection of the sql request type with
// connect, in place of the drivers, until the function it returns is
// called: for the tests of other packages, which reach a stand-in database
// through the very pools and connectors a real driver is reached through.
func UseSQLTestDriver(connect func(ctx context.Context, info SQLConnectInfo) (driver.Conn, error)) (restore func()) {
	hook := func(ctx context.Context, driverName string, spec *sqlConnectSpec) (driver.Conn, error) {
		return connect(ctx, SQLConnectInfo{
			Driver: driverName, Host: spec.host, Port: spec.port, Account: spec.account,
			Username: spec.username, Password: spec.password, Database: spec.database, ServiceName: spec.serviceName,
			Options: spec.options, TLS: spec.tls != nil,
		})
	}
	sqlConnectHook.Store(&hook)
	return func() { sqlConnectHook.Store(nil) }
}
