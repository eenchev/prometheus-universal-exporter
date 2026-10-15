//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshfile reads reuse their connections, as grpc calls do (grpcconn.go): one
// SSH connection, with one SFTP session on it, per host and port, account,
// credential files, known_hosts file, keepalive settings and target policy,
// kept across scrapes and shared by the scrapes reaching it at the same
// time. It is made at the first scrape that needs it, and made again:
//
//   - when one of its files changes on disk: a rotated key or password, or
//     a known_hosts line replaced, gets a new connection at the next scrape;
//   - when it has ended: a scrape that finds it broken, or that loses it
//     during its read, connects again once, within its own deadline.
//
// A connection can die without a word: a host that lost power, a firewall or
// a NAT that dropped its entry, sends neither FIN nor RST. So every
// keepalive_interval it sends a keepalive@openssh.com request, which any
// SSH server answers, if only to say it does not know it; when
// keepalive_count_max of them in a row go unanswered within an interval, the
// connection is closed, and the next scrape makes a new one. A connection
// nothing has used for idle_timeout is closed too; with idle_timeout 0 it is
// kept as long as a collector uses it.
//
// Each connection notes the collectors whose scrapes used it. When the
// per-collector state follows a configuration (follow), a collector that is
// gone, or whose connection settings changed, stops being one of them, and a
// connection left without any is closed: as soon as no read holds it, since
// a read in progress is not cut off.
//
// A connection is made apart from the scrape that asked for it first: the
// scrapes reaching it while it is made each wait for it within their own
// deadline, and the making goes on when the first gives up, bounded by the
// larger of its deadline and sshConnectTimeout, so a connection made for
// nobody any more is kept for the next scrape.
//
// A collector with max_open_connections keeps its connections to itself,
// so that its limit counts them alone. A scrape that needs a new one at the
// limit closes the collector's connection no read has used for longest; when
// none is free, it gives up the making of one no scrape waits for any more,
// whose dial is cancelled; and when every one is in use, it waits within its
// deadline for one to be free. A connection being made is given up too at
// the exporter's shutdown, and, once no scrape waits for it, when a reload
// leaves it to no collector or a file of it changed; one whose making was
// given up, or whose collector a reload removed, is not counted as made.
//
// A read cannot be cancelled: pkg/sftp takes no context. A scrape whose
// deadline comes during its read returns at once, closes the files the read
// has open, and gives the read sshAbandonGrace to return; when it does not,
// the connection is dropped, which ends the read, and the scrapes on it then
// connect again.

// sshSettings are what a connection depends on besides the host: who it
// authenticates as and with what, how its host key is verified, how it is
// kept alive and closed, and the policy its address was checked against.
type sshSettings struct {
	username, privateKeyFile, passphraseFile, passwordFile, knownHostsFile string
	keepaliveInterval                                                      time.Duration
	keepaliveCountMax                                                      int
	idleTimeout                                                            time.Duration
	// policy is the collector's allowed_targets and denied_targets, which
	// every connection is checked against when it is made. Policies are
	// interned, so collectors with the same lists share connections.
	policy *targetPolicy
	// owner is the collector whose connection it is, when the collector
	// sets max_open_connections: its connections are its own, so its limit
	// counts them alone. Empty, a connection is shared.
	owner string
}

// sshConnKey is what a connection is kept by: address is what is dialed,
// host:port, and host the name known_hosts_file and the policy match.
type sshConnKey struct {
	address, host string
	sshSettings
}

// sshConn is one kept connection.
type sshConn struct {
	key sshConnKey
	// stamp is the connection's files as they were on disk when it was made
	// (sshFilesStamp).
	stamp string
	// ready is closed once the connection is made or has failed; err is the
	// failure. client and sftp are set before ready is closed.
	ready  chan struct{}
	err    error
	client *ssh.Client
	sftp   *sftp.Client
	// listingSessionsKept says the host did not close a listing's session
	// on the connection in time (sftpLister.hostKeepsSessions).
	listingSessionsKept atomic.Bool
	// ended is closed when the connection has ended, however it ended.
	ended chan struct{}
	// users are the collectors whose scrapes used the connection.
	users map[string]bool
	// maker is the collector whose scrape started making the connection,
	// which it is counted as made by; makerGone says a reload removed that
	// collector while the connection was made, so it is not counted.
	maker     string
	makerGone bool
	// calls is how many reads hold the connection now. retired says the
	// cache has forgotten it, so it is closed when the last of them ends;
	// givenUp that its making was cancelled (giveUpLocked); closed that it
	// has been.
	calls    int
	retired  bool
	givenUp  bool
	closed   bool
	lastUsed time.Time
	idle     *time.Timer
	// cancel cancels the making of the connection, which then fails.
	cancel context.CancelFunc
	// stage is how far the making of the connection has got (sshStage*),
	// for the error of a scrape that stops waiting for it.
	stage atomic.Int32
}

// The stages of making a connection.
const (
	sshStageConnect int32 = iota
	sshStageHandshake
	sshStageSession
)

// sshConnCache is the kept connections, and how many each collector made.
type sshConnCache struct {
	mu       sync.Mutex
	entries  map[sshConnKey]*sshConn
	connects map[string]uint64
	// owned are, of each collector with max_open_connections, its
	// connections open or being made, those the cache has forgotten but a
	// read still holds among them.
	owned map[string]map[*sshConn]bool
	// making are the connections being made, those the cache has forgotten
	// among them, which the shutdown gives up.
	making map[*sshConn]bool
	// freed is closed, and replaced, whenever a connection is released by
	// its last read or closed, which wakes the scrapes waiting at their
	// collector's max_open_connections.
	freed chan struct{}
	// limitWaits and limitFailures count, of each collector with
	// max_open_connections, the scrapes that waited for room at the limit,
	// and those that failed there.
	limitWaits, limitFailures map[string]uint64
}

var sshConns = newSSHConnCache()

func newSSHConnCache() *sshConnCache {
	return &sshConnCache{entries: map[sshConnKey]*sshConn{}, connects: map[string]uint64{}, owned: map[string]map[*sshConn]bool{}, making: map[*sshConn]bool{}, freed: make(chan struct{}), limitWaits: map[string]uint64{}, limitFailures: map[string]uint64{}}
}

// sshConnectTimeout is the least time making a connection is given: the TCP
// connection, the SSH handshake with authentication, and the start of the
// SFTP session. The first scrape's deadline is given when it is later.
const sshConnectTimeout = 30 * time.Second

// sshAbandonGrace is how long a read whose scrape's deadline came is given to
// return before its connection is dropped. It is a variable for the tests.
var sshAbandonGrace = 2 * time.Second

// sshConnEvent, set by tests, is told what happens to connections: "made",
// "keepalive" (a keepalive answered), "keepalive failed", "closed: "
// followed by why, "given up: " followed by why for a connection whose
// making is cancelled, and "waiting: max_open_connections" for a scrape that
// waits for room, each with the address of the connection.
var sshConnEvent atomic.Pointer[func(event, address string)]

// beforeSSHRead, set by tests, runs when a read of one file is about to
// start on its connection.
var beforeSSHRead atomic.Pointer[func()]

func sshEvent(event, address string) {
	if hook := sshConnEvent.Load(); hook != nil {
		(*hook)(event, address)
	}
}

// get returns the connection for key, making it when there is none, when
// the one kept has ended, or when one of its files changed since it was
// made, and holds it for the caller, who calls release when its read has
// ended. reused says it was made before this scrape asked for it. limit is
// the collector's max_open_connections, which a key with an owner is held
// to.
func (c *sshConnCache) get(ctx context.Context, key sshConnKey, collector string, limit int) (entry *sshConn, reused bool, err error) {
	stamp := sshFilesStamp(key.sshSettings)
	waited := false
	c.mu.Lock()
	for {
		entry = c.entries[key]
		if entry != nil && (entry.stamp != stamp || isClosedChannel(entry.ended)) {
			delete(c.entries, key)
			c.retireLocked(entry, "replaced")
			entry = nil
		}
		if entry != nil {
			break
		}
		if key.owner == "" || limit <= 0 || c.makeRoomLocked(key.owner, limit) {
			entry = c.startLocked(ctx, key, stamp, collector)
			break
		}
		if !waited {
			waited = true
			c.limitWaits[key.owner]++
		}
		freed := c.freed
		c.mu.Unlock()
		sshEvent("waiting: max_open_connections", key.address)
		select {
		case <-freed:
		case <-ctx.Done():
			c.mu.Lock()
			c.limitFailures[key.owner]++
			taken := c.takenLocked(key.owner)
			c.mu.Unlock()
			return nil, false, fmt.Errorf("collector %q has request.max_open_connections, %d, connections (%s), and none was free before the scrape's deadline: %w; raise request.max_open_connections, or the scrape's timeout", key.owner, limit, taken, ctx.Err())
		}
		c.mu.Lock()
	}
	entry.calls++
	entry.users[collector] = true
	entry.lastUsed = time.Now()
	if entry.idle != nil {
		entry.idle.Stop()
	}
	reused = isClosedChannel(entry.ready)
	c.mu.Unlock()
	if !reused {
		select {
		case <-entry.ready:
		case <-ctx.Done():
			c.release(entry)
			return nil, false, entry.unfinished(ctx.Err())
		}
	}
	if entry.err != nil {
		c.release(entry)
		return nil, false, entry.err
	}
	return entry, reused, nil
}

// startLocked starts making a connection for key, apart from the scrape
// that asks for it, ctx: the scrapes waiting for it each wait within their
// own deadline, and the making is bounded by the larger of ctx's deadline
// and sshConnectTimeout, whoever still waits.
func (c *sshConnCache) startLocked(ctx context.Context, key sshConnKey, stamp, collector string) *sshConn {
	entry := &sshConn{key: key, stamp: stamp, ready: make(chan struct{}), ended: make(chan struct{}), users: map[string]bool{}, maker: collector, lastUsed: time.Now()}
	c.entries[key] = entry
	c.making[entry] = true
	if key.owner != "" {
		if c.owned[key.owner] == nil {
			c.owned[key.owner] = map[*sshConn]bool{}
		}
		c.owned[key.owner][entry] = true
	}
	deadline := time.Now().Add(sshConnectTimeout)
	if d, ok := ctx.Deadline(); ok && d.After(deadline) {
		deadline = d
	}
	dialCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	entry.cancel = cancel
	go func() {
		defer cancel()
		c.dial(dialCtx, entry, collector)
	}()
	return entry
}

// dial makes the connection of entry, counting it as collector's unless its
// making was given up, and hands it to the scrapes waiting for it; when none
// is left, it is kept for the next as a connection a read released, or
// closed when the cache has forgotten it meanwhile. A connection made after
// its making was given up, at the exporter's shutdown, at a reload that left
// it to no collector, or once no scrape waited for it, belongs to no
// collector's count, as does one whose collector a reload removed; one the
// cache forgot while a scrape waited for it is counted, since that scrape
// reads on it.
func (c *sshConnCache) dial(ctx context.Context, entry *sshConn, collector string) {
	client, session, err := dialSSH(ctx, entry.key, &entry.stage)
	c.mu.Lock()
	delete(c.making, entry)
	if err != nil {
		entry.err = err
		if c.entries[entry.key] == entry {
			delete(c.entries, entry.key)
		}
		entry.retired, entry.closed = true, true
		c.disownLocked(entry)
		close(entry.ready)
		close(entry.ended)
		c.mu.Unlock()
		return
	}
	entry.client, entry.sftp = client, session
	if !entry.givenUp && !entry.makerGone {
		c.connects[collector]++
	}
	close(entry.ready)
	c.mu.Unlock()
	sshEvent("made", entry.key.address)
	go c.watch(entry)
	if entry.key.keepaliveInterval > 0 {
		go c.keepalive(entry)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.calls == 0 {
		c.unheldLocked(entry)
	}
}

// unfinished is the error of a scrape whose deadline came while it waited
// for the connection to be made, saying how far the making had got.
func (entry *sshConn) unfinished(err error) error {
	switch entry.stage.Load() {
	case sshStageHandshake:
		return fmt.Errorf("the SSH handshake with %s did not finish: %w", entry.key.address, err)
	case sshStageSession:
		return fmt.Errorf("starting the SFTP session on %s: %w", entry.key.address, err)
	}
	return fmt.Errorf("connecting to %s: %w", entry.key.address, err)
}

// makeRoomLocked makes room for one more connection of owner under limit:
// it closes the owner's connections no read holds, the one used longest ago
// first, and then gives up the making of those no scrape waits for, until
// fewer than limit are left, and says whether that was done. A connection
// given up is counted until its dial has ended, which wakes the scrapes
// waiting for room. Every connection held by a read, or waited for, stays.
func (c *sshConnCache) makeRoomLocked(owner string, limit int) bool {
	for len(c.owned[owner]) >= limit {
		var oldest, unwaited *sshConn
		ending := 0
		for entry := range c.owned[owner] {
			made := isClosedChannel(entry.ready)
			switch {
			case !made && entry.givenUp:
				ending++
			case !made && entry.calls == 0:
				if unwaited == nil || entry.lastUsed.Before(unwaited.lastUsed) {
					unwaited = entry
				}
			case entry.calls == 0 && !entry.retired && entry.client != nil && (oldest == nil || entry.lastUsed.Before(oldest.lastUsed)):
				oldest = entry
			}
		}
		switch {
		case len(c.owned[owner])-ending < limit:
			return false
		case oldest != nil:
			if c.entries[oldest.key] == oldest {
				delete(c.entries, oldest.key)
			}
			oldest.retired = true
			c.closeLocked(oldest, "max_open_connections")
		case unwaited != nil:
			c.giveUpLocked(unwaited, "max_open_connections")
		default:
			return false
		}
	}
	return true
}

// giveUpLocked cancels the making of a connection: the cache forgets it, and
// its dial fails, or closes it uncounted when it was made all the same.
func (c *sshConnCache) giveUpLocked(entry *sshConn, why string) {
	if c.entries[entry.key] == entry {
		delete(c.entries, entry.key)
	}
	entry.retired, entry.givenUp = true, true
	entry.cancel()
	sshEvent("given up: "+why, entry.key.address)
}

// takenLocked says what holds the connections of owner, for the error of a
// scrape that found none free at its max_open_connections: those open and
// held by a read, those being made, for a scrape waiting for them or not,
// and those given up but not ended yet.
func (c *sshConnCache) takenLocked(owner string) string {
	var held, free, waited, making, ending int
	for entry := range c.owned[owner] {
		switch {
		case !isClosedChannel(entry.ready) && entry.givenUp:
			ending++
		case !isClosedChannel(entry.ready) && entry.calls > 0:
			waited++
		case !isClosedChannel(entry.ready):
			making++
		case entry.calls > 0:
			held++
		default:
			free++
		}
	}
	var parts []string
	for _, part := range []struct {
		n    int
		what string
	}{{held, "open and held by a read"}, {free, "open and just freed"}, {waited, "being made for a scrape waiting for it"}, {making, "being made"}, {ending, "being given up"}} {
		if part.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.n, part.what))
		}
	}
	return strings.Join(parts, ", ")
}

// disownLocked takes a connection that is closed, or was never made, off
// its owner's count, and wakes the scrapes waiting for room.
func (c *sshConnCache) disownLocked(entry *sshConn) {
	if owned := c.owned[entry.key.owner]; owned[entry] {
		delete(owned, entry)
		if len(owned) == 0 {
			delete(c.owned, entry.key.owner)
		}
	}
	c.wakeLocked()
}

// wakeLocked wakes the scrapes waiting for a connection to be free.
func (c *sshConnCache) wakeLocked() {
	close(c.freed)
	c.freed = make(chan struct{})
}

// release ends a read's hold on a connection. A connection the cache has
// forgotten is closed when no read holds it, and given up when it is still
// being made and no scrape waits for it any more; one it keeps waits
// idle_timeout for the next read.
func (c *sshConnCache) release(entry *sshConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.calls--
	entry.lastUsed = time.Now()
	if entry.calls > 0 {
		return
	}
	if !isClosedChannel(entry.ready) {
		if entry.retired && !entry.givenUp {
			c.giveUpLocked(entry, "unused")
		}
		// A connection still being made is otherwise seen to when it is
		// made (dial).
		return
	}
	c.unheldLocked(entry)
}

// unheldLocked sees to a made connection no read holds: closed when the
// cache has forgotten it, its idle timer started when the cache keeps it,
// and the scrapes waiting at their collector's max_open_connections woken.
func (c *sshConnCache) unheldLocked(entry *sshConn) {
	if entry.retired {
		c.closeLocked(entry, "unused")
		return
	}
	if idle := entry.key.idleTimeout; idle > 0 {
		if entry.idle == nil {
			entry.idle = time.AfterFunc(idle, func() { c.idleEnded(entry) })
		} else {
			entry.idle.Reset(idle)
		}
	}
	c.wakeLocked()
}

// idleEnded closes a connection nothing has used for idle_timeout.
func (c *sshConnCache) idleEnded(entry *sshConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[entry.key] != entry || entry.calls > 0 {
		return
	}
	if rest := entry.key.idleTimeout - time.Since(entry.lastUsed); rest > 0 {
		entry.idle.Reset(rest)
		return
	}
	delete(c.entries, entry.key)
	entry.retired = true
	c.closeLocked(entry, "idle")
}

// drop forgets a connection and closes it now, whatever reads hold it: one
// that keepalives found dead, one a read lost, or one a read left stalled.
// The reads on it fail, and the next scrapes make a new one.
func (c *sshConnCache) drop(entry *sshConn, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[entry.key] == entry {
		delete(c.entries, entry.key)
	}
	entry.retired = true
	c.closeLocked(entry, why)
}

// retireLocked marks a connection the cache has just forgotten, and closes
// it unless a read holds it, or gives up its making unless a scrape waits
// for it; release closes it, or gives it up, then.
func (c *sshConnCache) retireLocked(entry *sshConn, why string) {
	entry.retired = true
	switch {
	case entry.calls > 0:
	case !isClosedChannel(entry.ready):
		c.giveUpLocked(entry, why)
	default:
		c.closeLocked(entry, why)
	}
}

// closeLocked closes a connection once. Closing the SSH connection closes
// its socket, which ends whatever waits on it at once. The SFTP session is
// closed after it, apart: its close waits for a request being sent, which
// the socket's close ends.
func (c *sshConnCache) closeLocked(entry *sshConn, why string) {
	if entry.closed || entry.client == nil {
		return
	}
	entry.closed = true
	if entry.idle != nil {
		entry.idle.Stop()
	}
	c.disownLocked(entry)
	_ = entry.client.Close()
	go func() { _ = entry.sftp.Close() }()
	sshEvent("closed: "+why, entry.key.address)
}

// watch notes a connection that ended, closed by either side or broken, so
// no scrape is handed it again.
func (c *sshConnCache) watch(entry *sshConn) {
	_ = entry.client.Wait()
	close(entry.ended)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[entry.key] == entry {
		delete(c.entries, entry.key)
	}
	entry.retired = true
	c.closeLocked(entry, "ended")
}

// keepalive sends a keepalive request every keepalive_interval, and drops
// the connection when keepalive_count_max in a row go unanswered within an
// interval.
func (c *sshConnCache) keepalive(entry *sshConn) {
	interval, most := entry.key.keepaliveInterval, entry.key.keepaliveCountMax
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	missed := 0
	for {
		select {
		case <-ticker.C:
		case <-entry.ended:
			return
		}
		answered := make(chan error, 1)
		go func() {
			_, _, err := entry.client.SendRequest("keepalive@openssh.com", true, nil)
			answered <- err
		}()
		wait := time.NewTimer(interval)
		select {
		case err := <-answered:
			if err == nil {
				missed = 0
				sshEvent("keepalive", entry.key.address)
			} else {
				missed++
			}
		case <-wait.C:
			missed++
		case <-entry.ended:
			wait.Stop()
			return
		}
		wait.Stop()
		if missed > 0 {
			sshEvent("keepalive failed", entry.key.address)
		}
		if missed >= most {
			c.drop(entry, "keepalive")
			return
		}
	}
}

// follow forgets, of every connection, the collectors that the
// configuration in force no longer has, or has with other connection
// settings, and closes the connections no collector is left using. A
// collector's settings are its own and those of each of its static
// targets, whose credentials may replace its own. Two reloads close
// together may be followed in the other order; the connection the older
// configuration's following closes is then made again at the next scrape
// that needs it.
func (c *sshConnCache) follow(collectors []model.Collector, targets []model.StaticTarget) {
	wanted := map[string]map[sshSettings]bool{}
	byName := map[string]*model.Collector{}
	for i := range collectors {
		col := &collectors[i]
		if col.Request.Type != RequestTypeSSHFile {
			continue
		}
		byName[col.Name] = col
		wanted[col.Name] = map[sshSettings]bool{sshSettingsOf(col, RequestOverrides{}): true}
	}
	for i := range targets {
		if col := byName[targets[i].Collector]; col != nil {
			wanted[col.Name][sshSettingsOf(col, TargetOverrides(&targets[i]))] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		for user := range entry.users {
			if !wanted[user][key.sshSettings] {
				delete(entry.users, user)
			}
		}
		if len(entry.users) == 0 {
			delete(c.entries, key)
			if !isClosedChannel(entry.ready) && entry.calls == 0 {
				c.giveUpLocked(entry, "reload")
				continue
			}
			c.retireLocked(entry, "reload")
		}
	}
	for entry := range c.making {
		if wanted[entry.maker] == nil {
			entry.makerGone = true
		}
	}
	for _, counts := range []map[string]uint64{c.connects, c.limitWaits, c.limitFailures} {
		for name := range counts {
			if wanted[name] == nil {
				delete(counts, name)
			}
		}
	}
}

// closeAll closes every connection, for the exporter's shutdown, gives up
// those being made, those the cache has forgotten among them, and forgets
// how many each collector made and its counts at max_open_connections: the
// counts are of the exporter's life, as every other self-metric's, and a
// test's exporter is a new life.
func (c *sshConnCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if !isClosedChannel(entry.ready) {
			c.giveUpLocked(entry, "shutdown")
			continue
		}
		delete(c.entries, key)
		entry.retired = true
		c.closeLocked(entry, "shutdown")
	}
	for entry := range c.making {
		if !entry.givenUp {
			c.giveUpLocked(entry, "shutdown")
		}
	}
	c.connects = map[string]uint64{}
	c.limitWaits, c.limitFailures = map[string]uint64{}, map[string]uint64{}
}

// collectorConnections reports the connections a collector's scrapes use
// that are open now, and how many its scrapes have made.
func (c *sshConnCache) collectorConnections(collector string) (open int, made uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.users[collector] && entry.client != nil && !entry.closed {
			open++
		}
	}
	return open, c.connects[collector]
}

// limitCounts reports how many of a collector's scrapes waited for room at
// its max_open_connections, and how many failed there.
func (c *sshConnCache) limitCounts(collector string) (waited, failed uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitWaits[collector], c.limitFailures[collector]
}

// size reports how many connections are kept, for tests.
func (c *sshConnCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// session runs read on a connection for key, with a fileSource of root on it,
// within ctx. A read that loses its connection is run once more, on a new
// one, while ctx lasts. shown is what a debug probe's request section shows
// of the read.
func (c *sshConnCache) session(ctx context.Context, key sshConnKey, collector string, limit int, root, shown string, read func(*sftpSource) error) error {
	for attempt := 0; ; attempt++ {
		entry, reused, err := c.get(ctx, key, collector, limit)
		traceSSH(ctx, key, shown, reused, err)
		if err != nil {
			return err
		}
		src := &sftpSource{client: entry.sftp, conn: entry.client, hostKeepsSessions: &entry.listingSessionsKept, root: root}
		done := make(chan error, 1)
		go func() {
			if hook := beforeSSHRead.Load(); hook != nil {
				(*hook)()
			}
			done <- src.run(func() error { return read(src) })
		}()
		select {
		case err = <-done:
			c.release(entry)
		case <-ctx.Done():
			c.abandon(entry, src, done)
			err = fmt.Errorf("reading %s: %w", shown, ctx.Err())
			traceOutcome(ctx, "error: "+err.Error())
			return err
		}
		if err != nil && attempt == 0 && ctx.Err() == nil && entry.lost(err) {
			c.drop(entry, "lost")
			traceOutcome(ctx, "the connection was lost: "+err.Error())
			continue
		}
		if err != nil {
			traceOutcome(ctx, "error: "+err.Error())
		} else {
			traceOutcome(ctx, "read")
		}
		return err
	}
}

// directory reads a directory on a connection for key as session reads a
// file, and hands answer what was read: all of it, or, when ctx ended
// first, the files read by then (awaitDirectory).
func (c *sshConnCache) directory(ctx context.Context, key sshConnKey, collector string, limit int, root, shown string, read func(*sftpSource, *directoryProgress) error, answer func(*DirectoryRead)) error {
	for attempt := 0; ; attempt++ {
		entry, reused, err := c.get(ctx, key, collector, limit)
		traceSSH(ctx, key, shown, reused, err)
		if err != nil {
			return err
		}
		src := &sftpSource{client: entry.sftp, conn: entry.client, hostKeepsSessions: &entry.listingSessionsKept, root: root}
		progress := &directoryProgress{}
		done := make(chan error, 1)
		go func() { done <- src.run(func() error { return read(src, progress) }) }()
		abandoned := false
		result, err := awaitDirectory(ctx, root, progress, done, func() {
			abandoned = true
			c.abandon(entry, src, done)
		})
		if !abandoned {
			c.release(entry)
			if err != nil && attempt == 0 && ctx.Err() == nil && entry.lost(err) {
				c.drop(entry, "lost")
				traceOutcome(ctx, "the connection was lost: "+err.Error())
				continue
			}
		}
		if err != nil {
			traceOutcome(ctx, "error: "+err.Error())
			return err
		}
		traceOutcome(ctx, fmt.Sprintf("read %d files", len(result.Files)))
		answer(result)
		return nil
	}
}

// abandon is a scrape giving up on its read: the files the read has open
// are closed, and the read is given sshAbandonGrace to return, after which
// the connection is dropped. The connection is released when the read
// returns.
func (c *sshConnCache) abandon(entry *sshConn, src *sftpSource, done <-chan error) {
	src.abandon()
	go func() {
		grace := time.NewTimer(sshAbandonGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
			c.drop(entry, "stalled")
			<-done
		}
		c.release(entry)
	}()
}

// lost says a read's error is its connection ending under it.
func (entry *sshConn) lost(err error) bool {
	return errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || isClosedChannel(entry.ended)
}

// traceSSH records a read in a debug probe's request section: the host, the
// account, the file or directory, and whether the connection was kept from
// an earlier scrape or made for this one.
func traceSSH(ctx context.Context, key sshConnKey, shown string, reused bool, err error) {
	connection := "new"
	if reused {
		connection = "reused"
	}
	traceRequest(ctx, localFileMethod, shown, http.Header{"Ssh-User": {key.username}, "Ssh-Connection": {connection}}, "", false)
	if err != nil {
		traceOutcome(ctx, "error: "+err.Error())
	}
}

func isClosedChannel(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// sshFilesStamp describes a connection's files as they are on disk now.
func sshFilesStamp(s sshSettings) string {
	var b strings.Builder
	for _, path := range []string{s.privateKeyFile, s.passphraseFile, s.passwordFile, s.knownHostsFile} {
		b.WriteByte('|')
		if path == "" {
			continue
		}
		b.WriteString(path)
		st, err := os.Stat(path)
		if err != nil {
			b.WriteString(":missing")
			continue
		}
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(st.Size(), 10))
	}
	return b.String()
}

// dialSSH makes a connection: TCP to the address, checked against the
// policy as it is made, the SSH handshake, with the host key verified
// against known_hosts_file and the account authenticated, and the SFTP
// session, all within ctx, or sshConnectTimeout when ctx has no deadline.
// stage is set to each stage as it starts.
func dialSSH(ctx context.Context, key sshConnKey, stage *atomic.Int32) (*ssh.Client, *sftp.Client, error) {
	config, hostKeyFailure, err := sshClientConfig(key)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sshConnectTimeout)
		defer cancel()
	}
	if key.policy != nil {
		guard := &policyGuard{policy: key.policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "ssh", Host: key.address}); err != nil {
			return nil, nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", key.address)
	if err != nil {
		if errors.Is(err, ErrTargetRefused) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("connecting to %s: %w", key.address, err)
	}
	// The handshake and the start of the session take no context: the
	// socket's deadline bounds them, and is brought forward when ctx ends.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	stage.Store(sshStageHandshake)
	clientConn, channels, requests, err := ssh.NewClientConn(conn, key.address, config)
	if err != nil {
		_ = conn.Close()
		return nil, nil, sshHandshakeError(ctx, key, err, *hostKeyFailure)
	}
	client := ssh.NewClient(clientConn, channels, requests)
	stage.Store(sshStageSession)
	session, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("starting the SFTP session on %s: %w", key.address, ctx.Err())
		}
		return nil, nil, fmt.Errorf("the host %s took the connection of user %q but did not start an SFTP session: %w; the account needs the sftp subsystem, which OpenSSH's Subsystem sftp line enables", key.address, key.username, err)
	}
	if !stop() {
		// ctx ended just as the session started: the socket's deadline is
		// past, and the connection of no use.
		_ = session.Close()
		_ = client.Close()
		return nil, nil, fmt.Errorf("connecting to %s over SSH: %w", key.address, ctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	return client, session, nil
}

// sshHandshakeError says in plain words why a handshake failed: the host
// key, which hostKeyFailure holds when the check refused it, the
// authentication, or the deadline.
func sshHandshakeError(ctx context.Context, key sshConnKey, err error, hostKeyFailure error) error {
	switch {
	case hostKeyFailure != nil:
		return hostKeyFailure
	case errors.Is(err, errSSHInteractiveRounds):
		return fmt.Errorf("the host %s refused user %q: it asked keyboard-interactive questions in more than %d rounds, and the password in %s is given in %d at most; check request.username and the credential files, and that the host's keyboard-interactive questions ask for the password alone", key.address, key.username, sshInteractiveRounds, key.passwordFile, sshInteractiveRounds)
	case strings.Contains(err.Error(), "unable to authenticate"):
		tried := sshMethodsTried(err)
		message := fmt.Sprintf("the host %s refused user %q: authentication failed with %s", key.address, key.username, strings.Join(sshMethods(key.sshSettings), " and "))
		if len(tried) > 0 {
			message += " (the SSH methods tried: " + strings.Join(tried, ", ") + ")"
		}
		message += "; check request.username and the credential files"
		if slices.Contains(tried, "keyboard-interactive") {
			message += ", and that the host's keyboard-interactive questions ask for the password alone: they are answered with it, and those the host echoes with the username"
		}
		return errors.New(message)
	case ctx.Err() != nil:
		return fmt.Errorf("the SSH handshake with %s did not finish: %w", key.address, ctx.Err())
	}
	return fmt.Errorf("the SSH handshake with %s failed: %w", key.address, err)
}

// sshMethodsTried are the SSH authentication methods a refused handshake
// tried, as x/crypto's error lists them, without "none", which asks the host
// for the methods it takes and offers nothing.
func sshMethodsTried(err error) []string {
	_, rest, found := strings.Cut(err.Error(), "attempted methods [")
	if !found {
		return nil
	}
	list, _, _ := strings.Cut(rest, "]")
	return slices.DeleteFunc(strings.Fields(list), func(method string) bool { return method == "none" })
}

// sshMethods names the credentials a connection authenticates with, in order.
func sshMethods(s sshSettings) []string {
	var methods []string
	if s.privateKeyFile != "" {
		methods = append(methods, "the private key in "+s.privateKeyFile)
	}
	if s.passwordFile != "" {
		methods = append(methods, "the password in "+s.passwordFile)
	}
	return methods
}

// sshClientConfig reads a connection's files, as they are now, into the
// configuration of its handshake: the private key, decrypted with its
// passphrase, and the password, tried in that order, the password as SSH's
// password method, or, to a host that does not take that method, as the
// answer to keyboard-interactive questions, which a host that takes
// passwords through PAM alone asks (sshInteractiveFallback); and the
// known_hosts lines, which the host key must be among. The host key
// algorithms of the lines known for the host are asked for first, so that a
// host offering several keys presents one the file has; when the host
// offers none of them, it presents another, which is refused as not the one
// known_hosts_file holds. When the check refuses the host's key, the
// returned failure holds why, in the words of sshHostKeyError.
func sshClientConfig(key sshConnKey) (*ssh.ClientConfig, *error, error) {
	var methods []ssh.AuthMethod
	var callback ssh.ClientAuthCallback
	if key.privateKeyFile != "" {
		signer, err := sshSigner(key.privateKeyFile, key.passphraseFile)
		if err != nil {
			return nil, nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if key.passwordFile != "" {
		password, err := readSSHPassword(key.passwordFile)
		if err != nil {
			return nil, nil, err
		}
		methods = append(methods, ssh.Password(password))
		callback = sshInteractiveFallback(key.privateKeyFile != "", ssh.KeyboardInteractive(sshAnswers(key.username, password)))
	}
	known, err := readKnownHosts(key.knownHostsFile)
	if err != nil {
		return nil, nil, err
	}
	failure := new(error)
	config := &ssh.ClientConfig{
		User:         key.username,
		Auth:         methods,
		AuthCallback: callback,
		HostKeyCallback: func(hostname string, remote net.Addr, hostKey ssh.PublicKey) error {
			if err := known.verify(hostname, remote, hostKey); err != nil {
				*failure = sshHostKeyError(key, hostKey, err, known)
				return *failure
			}
			return nil
		},
		HostKeyAlgorithms: knownHostKeyAlgorithms(known, key.address),
	}
	return config, failure, nil
}

// sshInteractiveFallback picks keyboard-interactive, answered with the
// password, as the next method of a handshake once the host has been offered
// every other method of the connection it takes, and only when the host
// neither takes nor has refused the password method: a host that takes both,
// as OpenSSH with PAM does by default, would otherwise be given a wrong
// password twice, which counts twice towards its MaxAuthTries, a PAM lockout
// or fail2ban. Otherwise x/crypto picks from the configured methods, the
// private key and the password, in that order.
func sshInteractiveFallback(hasKey bool, interactive ssh.AuthMethod) ssh.ClientAuthCallback {
	own := []string{"password"}
	if hasKey {
		own = []string{"publickey", "password"}
	}
	return func(ctx *ssh.ClientAuthContext) (ssh.AuthMethod, error) {
		done := func(method string) bool {
			return slices.Contains(ctx.TriedMethods, method) || slices.Contains(ctx.PartialSuccessMethods, method)
		}
		for _, method := range own {
			if slices.Contains(ctx.AllowedMethods, method) && !done(method) {
				return nil, nil
			}
		}
		if slices.Contains(ctx.AllowedMethods, "password") || done("password") || done("keyboard-interactive") || !slices.Contains(ctx.AllowedMethods, "keyboard-interactive") {
			return nil, nil
		}
		return interactive, nil
	}
}

// sshInteractiveRounds is the most rounds of keyboard-interactive questions
// a connection answers with the password; a round without a question sends
// nothing, and is not counted.
const sshInteractiveRounds = 3

// errSSHInteractiveRounds ends keyboard-interactive at a host that asks
// more rounds of questions than sshInteractiveRounds.
var errSSHInteractiveRounds = errors.New("the host asked more rounds of keyboard-interactive questions than are answered")

// sshAnswers answers a host's keyboard-interactive questions: those it does
// not echo, which is how a host asks for a secret, with the password, and
// those it echoes, which is how it asks for a name, with the username; in
// sshInteractiveRounds rounds at most, after which the method fails.
func sshAnswers(username, password string) ssh.KeyboardInteractiveChallenge {
	rounds := 0
	return func(_, _ string, questions []string, echos []bool) ([]string, error) {
		if len(questions) > 0 {
			rounds++
		}
		if rounds > sshInteractiveRounds {
			return nil, errSSHInteractiveRounds
		}
		answers := make([]string, len(questions))
		for i := range questions {
			answers[i] = password
			if i < len(echos) && echos[i] {
				answers[i] = username
			}
		}
		return answers, nil
	}
}

// readSSHPassword reads the password in passwordFile.
func readSSHPassword(passwordFile string) (string, error) {
	password, err := ReadCredentialFile(passwordFile)
	if err != nil {
		return "", fmt.Errorf("reading request.password_file: %w", err)
	}
	if password == "" {
		return "", fmt.Errorf("request.password_file %s is empty", passwordFile)
	}
	return password, nil
}

// sshSigner reads a private key, decrypting it with the passphrase in
// passphraseFile when it is encrypted.
func sshSigner(keyFile, passphraseFile string) (ssh.Signer, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading request.private_key_file: %w", err)
	}
	// x/crypto reads no encrypted PKCS#8 key, and would call it a key of a
	// type it does not know.
	if block, _ := pem.Decode(data); block != nil && block.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, fmt.Errorf("request.private_key_file %s is an encrypted PKCS#8 key, which cannot be read; convert it to OpenSSH's format with ssh-keygen -p -f %s, which asks for its passphrase and a new one, or write it unencrypted", keyFile, keyFile)
	}
	signer, err := ssh.ParsePrivateKey(data)
	var encrypted *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &encrypted) && passphraseFile == "":
		return nil, fmt.Errorf("request.private_key_file %s is encrypted; name the file of its passphrase in request.passphrase_file", keyFile)
	case errors.As(err, &encrypted):
		passphrase, err := ReadCredentialFile(passphraseFile)
		if err != nil {
			return nil, fmt.Errorf("reading request.passphrase_file: %w", err)
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("request.private_key_file %s could not be decrypted with the passphrase in %s: %w", keyFile, passphraseFile, err)
		}
	case err != nil:
		return nil, fmt.Errorf("request.private_key_file %s is not a private key: %w; give an OpenSSH, PKCS#1, PKCS#8 or SEC 1 private key", keyFile, err)
	}
	return signer, nil
}

// knownHosts is a known_hosts file read: the check of a host key, and,
// each as ssh.PublicKey.Marshal writes it, the keys of its @cert-authority
// lines, those of them whose line names every host with a bare *, and the
// keys of its @revoked lines.
type knownHosts struct {
	check                         ssh.HostKeyCallback
	authorities, anyHost, revoked map[string]bool
}

// readKnownHosts reads a known_hosts file.
func readKnownHosts(file string) (*knownHosts, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading request.known_hosts_file: %w", err)
	}
	lines := 0
	known := &knownHosts{authorities: map[string]bool{}, anyHost: map[string]bool{}, revoked: map[string]bool{}}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines++
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[0] != "@cert-authority" && fields[0] != "@revoked" {
				continue
			}
			marked, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[2:], " ")))
			if err != nil {
				continue
			}
			switch {
			case fields[0] == "@revoked":
				known.revoked[string(marked.Marshal())] = true
			case fields[1] == "*":
				known.anyHost[string(marked.Marshal())] = true
				fallthrough
			default:
				known.authorities[string(marked.Marshal())] = true
			}
		}
	}
	if lines == 0 {
		return nil, fmt.Errorf("request.known_hosts_file %s holds no host key; add the host's line, as ssh-keyscan writes it", file)
	}
	known.check, err = knownhosts.New(file)
	if err != nil {
		return nil, fmt.Errorf("request.known_hosts_file %s cannot be read as known_hosts lines: %w", file, err)
	}
	return known, nil
}

// verify checks the key a host presented against the file, and, where
// x/crypto's check refuses a host certificate that OpenSSH takes, as
// OpenSSH does: a certificate whose authority has an @cert-authority line
// naming every host with a bare * is taken on every port, as OpenSSH's *
// matches [host]:port, while x/crypto takes a bare * for port 22 alone; and
// a certificate no @cert-authority line trusts for the host is checked again
// as the plain key it certifies, which a line of the host's may hold
// (OpenSSH's "no matching CA found, retry with plain key"), unless its
// authority is @revoked.
func (k *knownHosts) verify(address string, remote net.Addr, hostKey ssh.PublicKey) error {
	err := k.check(address, remote, hostKey)
	cert, ok := hostKey.(*ssh.Certificate)
	if err == nil || !ok || !isNoAuthority(err) {
		return err
	}
	authority := cert.SignatureKey.Marshal()
	if host, port, splitErr := net.SplitHostPort(address); splitErr == nil && port != sshDefaultPort && k.anyHost[string(authority)] {
		return k.check(net.JoinHostPort(host, sshDefaultPort), remote, hostKey)
	}
	if !k.revoked[string(authority)] && !k.authorities[string(cert.Key.Marshal())] && k.check(address, remote, cert.Key) == nil {
		return nil
	}
	return err
}

// isNoAuthority says a host certificate was refused because no
// @cert-authority line trusts its authority for the host; x/crypto says so
// in words alone.
func isNoAuthority(err error) bool {
	return strings.Contains(err.Error(), "ssh: no authorities for hostname")
}

// knownHostKeyAlgorithms are the host key algorithms to ask address for:
// those of the keys the file knows for it, in the order the file lists them,
// and after them every other this client takes, so a host that no longer
// offers the known type presents another and is refused as presenting a key
// that is not the known one. When an @cert-authority line is known for the
// address, the certificate algorithms come first, since a host certificate
// of any key type may be signed by a certificate authority of any; a host
// whose certificate is signed by another authority is still taken by a line
// of its plain key (knownHosts.verify). An @cert-authority line naming every
// host with a bare * asks for the certificate algorithms first only of a
// host the file holds no plain line of: a host it holds one of is asked for
// that line's types first, as OpenSSH asks, so a host that also presents a
// certificate of another authority is taken by its line. None, the client's
// own order, when the file knows no key for the address. They are found by
// asking the check about a key no file holds, whose refusal lists the keys
// known for the address.
func knownHostKeyAlgorithms(k *knownHosts, address string) []string {
	probe, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
	if err != nil {
		return nil
	}
	var refused *knownhosts.KeyError
	listed := errors.As(k.check(address, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, probe), &refused) && len(refused.Want) > 0
	if !listed && len(k.anyHost) == 0 {
		return nil
	}
	var algorithms []string
	add := func(names ...string) {
		for _, name := range names {
			if !slices.Contains(algorithms, name) {
				algorithms = append(algorithms, name)
			}
		}
	}
	supported := ssh.SupportedAlgorithms().HostKeys
	certificates := func() {
		add(slices.DeleteFunc(slices.Clone(supported), func(name string) bool { return !strings.Contains(name, "-cert-v01@openssh.com") })...)
	}
	if !listed {
		certificates()
		add(supported...)
		return algorithms
	}
	for _, known := range refused.Want {
		if k.authorities[string(known.Key.Marshal())] {
			certificates()
		}
	}
	for _, known := range refused.Want {
		if k.authorities[string(known.Key.Marshal())] {
			continue
		}
		switch known.Key.Type() {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			add(known.Key.Type())
		}
	}
	add(supported...)
	return algorithms
}

// sshHostKeyError says why the host's key was refused, and what to do.
func sshHostKeyError(key sshConnKey, hostKey ssh.PublicKey, err error, known *knownHosts) error {
	presented := sshKeyName(hostKey)
	var refused, plain *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	cert, isCert := hostKey.(*ssh.Certificate)
	switch {
	case errors.As(err, &revoked):
		return fmt.Errorf("the host %s presented the key %s, which request.known_hosts_file %s marks @revoked", key.address, presented, key.knownHostsFile)
	case isCert && (revokedCertificate(err) || isNoAuthority(err)) && known.revoked[string(cert.SignatureKey.Marshal())]:
		return fmt.Errorf("the host %s presented a host certificate, %s, signed by the authority %s, which request.known_hosts_file %s marks @revoked", key.address, presented, sshKeyName(cert.SignatureKey), key.knownHostsFile)
	case isCert && revokedCertificate(err):
		return fmt.Errorf("the host %s presented a host certificate of the key %s, which request.known_hosts_file %s marks @revoked", key.address, sshKeyName(cert.Key), key.knownHostsFile)
	case isCert && isNoAuthority(err) && known.revoked[string(cert.Key.Marshal())]:
		return fmt.Errorf("the host %s presented a host certificate, %s, signed by the authority %s, which no @cert-authority line of request.known_hosts_file %s trusts for the host, of the key %s, which the file marks @revoked", key.address, presented, sshKeyName(cert.SignatureKey), key.knownHostsFile, sshKeyName(cert.Key))
	case isCert && isNoAuthority(err) && errors.As(known.check(key.address, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, cert.Key), &plain) && len(plain.Want) > 0:
		return fmt.Errorf("the host %s presented a host certificate, %s, signed by the authority %s, which no @cert-authority line of request.known_hosts_file %s trusts for the host, of the key %s, which is not the one the file holds for the host; add an @cert-authority line of the authority that signs the host's certificates, naming the host as it is scraped, [host]:port or a pattern such as [*]:port for a port other than 22, or, when the host's key changed, replace its line", key.address, presented, sshKeyName(cert.SignatureKey), key.knownHostsFile, sshKeyName(cert.Key))
	case isCert && isNoAuthority(err):
		return fmt.Errorf("the host %s presented a host certificate, %s, signed by the authority %s, which no @cert-authority line of request.known_hosts_file %s trusts for the host, and no line of the file holds the key it certifies; add an @cert-authority line of the authority that signs the host's certificates, naming the host as it is scraped, [host]:port or a pattern such as [*]:port for a port other than 22, or the host's own line", key.address, presented, sshKeyName(cert.SignatureKey), key.knownHostsFile)
	case errors.As(err, &refused) && len(refused.Want) == 0:
		host, port, _ := net.SplitHostPort(key.address)
		return fmt.Errorf("the host %s is not in request.known_hosts_file %s; add its line, which `ssh-keyscan -p %s %s` prints, after checking that the key's fingerprint, %s, is the host's own (ssh-keygen -lf on the host's /etc/ssh/ssh_host_*_key.pub)", key.address, key.knownHostsFile, port, host, presented)
	case errors.As(err, &refused):
		return fmt.Errorf("the host %s presented the key %s, which is not the one request.known_hosts_file %s holds for it; either the host's key changed and its line must be replaced, or something stands between the exporter and the host", key.address, presented, key.knownHostsFile)
	}
	return fmt.Errorf("the key of the host %s could not be checked against request.known_hosts_file %s: %w", key.address, key.knownHostsFile, err)
}

// sshKeyName names a key by its type and SHA-256 fingerprint.
func sshKeyName(key ssh.PublicKey) string {
	return key.Type() + " " + ssh.FingerprintSHA256(key)
}

// revokedCertificate says a host certificate was refused because its key or
// its authority is @revoked; x/crypto says so in words alone.
func revokedCertificate(err error) bool {
	return strings.HasPrefix(err.Error(), "ssh: certificate serial ") && strings.HasSuffix(err.Error(), " revoked")
}
