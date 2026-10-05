//go:build !select_request_types || request_type_grpc

package fetch

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// How a call's connection is made again: by the retries of a call that found
// none, and after a connection that stopped answering without a word.

// protosetCollector calls GetStats with descriptors from a file, so a probe
// is one call and nothing else.
func protosetCollector(t *testing.T) model.Collector {
	t.Helper()
	c := grpcCollector()
	c.Request.Descriptors, c.Request.ProtosetFile = "protoset", grpctest.WriteProtoset(t, filepath.Join(t.TempDir(), "queue.pb"), false)
	return c
}

// unusedAddr is a local address nothing listens on, and nothing of these
// tests has listened on before (grpctest.Listen).
func unusedAddr(t *testing.T) string {
	t.Helper()
	listener := grpctest.Listen(t)
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

// waitsAnHour makes a connection that could not be made wait an hour before
// it tries again by itself.
var waitsAnHour = grpc.WithConnectParams(grpc.ConnectParams{
	Backoff:           backoff.Config{BaseDelay: time.Hour, Multiplier: 1, MaxDelay: time.Hour},
	MinConnectTimeout: 20 * time.Second,
})

// connectionsWaitAnHour gives the connections the test's calls make an hour
// to wait before they try again by themselves, where the exporter's wait
// five seconds at most (grpcReconnect). In a test of what ends that wait
// nothing else then makes the connection again, however long the machine
// takes over the test: a server that came back is reached because a call
// ended the wait, or not at all.
func connectionsWaitAnHour(t *testing.T) {
	t.Helper()
	previous := grpcReconnect
	t.Cleanup(func() { grpcReconnect = previous })
	grpcReconnect = waitsAnHour
}

// callsWaitForAConnection gives a call a minute to wait for a connection
// that had failed to be made, where the exporter gives it a second
// (reconnectWait). The second is for a target that stays down; a test of a
// server that came back would fail by it on a machine too busy to connect
// within one. The minute bounds a connection that is never made.
func callsWaitForAConnection(t *testing.T) {
	t.Helper()
	previous := reconnectWait
	t.Cleanup(func() { reconnectWait = previous })
	reconnectWait = time.Minute
}

// keptConnectionState is the state of the connection the exporter keeps for
// the collector's calls to a plaintext target, and whether it keeps one.
func keptConnectionState(addr string, c *model.Collector) (connectivity.State, bool) {
	grpcConns.mu.Lock()
	defer grpcConns.mu.Unlock()
	entry := grpcConns.entries[grpcConnKey{dial: addr, policy: policyOf(c)}]
	if entry == nil {
		return connectivity.Idle, false
	}
	return entry.conn.GetState(), true
}

// tcpRelay forwards TCP connections to a backend and counts them, so a test
// can tell a call on a kept connection from one on a new connection. goSilent
// makes the connections open at that moment swallow everything from then on
// while staying open, as a connection does whose peer lost power or whose NAT
// entry was dropped: no FIN, no RST. Connections made afterwards are
// forwarded as usual.
type tcpRelay struct {
	addr string
	// answerDelay holds back what the server sends, as a slow path back does.
	answerDelay time.Duration

	mu       sync.Mutex
	silenced chan struct{}
	conns    []net.Conn
	accepted int
	// closed counts the connections the client has closed.
	closed int
}

func startTCPRelay(t *testing.T, backend string) *tcpRelay {
	t.Helper()
	return startSlowTCPRelay(t, backend, 0)
}

// startSlowTCPRelay is startTCPRelay with everything the server sends held
// back by answerDelay.
func startSlowTCPRelay(t *testing.T, backend string, answerDelay time.Duration) *tcpRelay {
	t.Helper()
	listener := grpctest.Listen(t)
	r := &tcpRelay{addr: listener.Addr().String(), answerDelay: answerDelay, silenced: make(chan struct{})}
	t.Cleanup(func() {
		_ = listener.Close()
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, conn := range r.conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", backend)
			if err != nil {
				_ = client.Close()
				continue
			}
			r.mu.Lock()
			r.conns = append(r.conns, client, server)
			r.accepted++
			silenced := r.silenced
			r.mu.Unlock()
			forward := func(to, from net.Conn, fromClient bool) {
				buf := make([]byte, 32<<10)
				for {
					n, err := from.Read(buf)
					if err != nil {
						if fromClient {
							r.mu.Lock()
							r.closed++
							r.mu.Unlock()
							_ = to.Close()
						}
						return
					}
					if !fromClient && r.answerDelay > 0 {
						time.Sleep(r.answerDelay)
					}
					select {
					case <-silenced:
					default:
						_, _ = to.Write(buf[:n])
					}
				}
			}
			go forward(server, client, true)
			go forward(client, server, false)
		}
	}()
	return r
}

func (r *tcpRelay) goSilent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.silenced)
	r.silenced = make(chan struct{})
}

// connections is how many connections were made, and how many of them the
// client has closed.
func (r *tcpRelay) connections() (accepted, closed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted, r.closed
}

// probeGRPC probes target with a deadline and reports how long it took.
func probeGRPC(target string, c *model.Collector, deadline time.Duration, overrides RequestOverrides) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	_, err := FetchCollector(ctx, target, c, overrides, nil)
	return time.Since(start), err
}

// A retry connects again. The server is down at the first attempt and comes
// back once that attempt has failed: the retry reaches it. grpc-go fails
// every call at once, without dialing, while a connection that was refused
// waits to try again, so without the retry ending that wait the retries
// would all be answered by the first refusal. With reflection the refused
// connection fails the reflection question, before any call, and that is
// retried the same way.
//
// The connection would wait an hour by itself, so only the retry makes it
// again, and the retry has a minute to: the server comes back when the
// first attempt is seen to have failed, however long after that the test
// gets to start it.
func TestGRPCARetryConnectsAgainAndReachesAServerThatCameBack(t *testing.T) {
	connectionsWaitAnHour(t)
	callsWaitForAConnection(t)
	for _, descriptors := range []string{"protoset", "reflection"} {
		t.Run(descriptors, func(t *testing.T) {
			addr := unusedAddr(t)
			c := grpcCollector()
			if descriptors == "protoset" {
				c = protosetCollector(t)
			}
			c.Request.Retry.Attempts = 3
			c.Request.Retry.Backoff = model.Duration(200 * time.Millisecond)
			checked := validGRPC(t, c)
			failed := make(chan error, 1)
			go func() {
				_, err := FetchCollector(context.Background(), addr, checked, RequestOverrides{}, nil)
				failed <- err
			}()
			testutil.WaitFor(t, "the first attempt to find no server", func() bool {
				state, kept := keptConnectionState(addr, checked)
				return kept && state == connectivity.TransientFailure
			})
			server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer, Addr: addr})
			if err := <-failed; err != nil {
				t.Fatalf("the probe failed although the server came back within its retries: %v", err)
			}
			if n := len(server.Calls()); n != 1 {
				t.Fatalf("the server answered %d calls, want 1", n)
			}
		})
	}
}

// toldOfResets is a connection that says when its wait to connect again has
// been ended.
type toldOfResets struct {
	*grpc.ClientConn
	reset chan struct{}
}

func (c toldOfResets) ResetConnectBackoff() {
	c.ClientConn.ResetConnectBackoff()
	select {
	case c.reset <- struct{}{}:
	default:
	}
}

// A call that finds its connection trying to connect, rather than waiting
// to, reaches a server that came back all the same. grpc-go reports the
// failure before in both cases, and ending the wait of a connection that is
// trying does nothing: when the attempt it found then fails, begun while the
// server was down, the connection waits again, an hour here, and the call
// that ended its wait once went on to fail with the server up. The wait is
// ended again for as long as the failure is reported.
//
// The order is the test's own: the connection's second attempt is held open
// until the call has ended the wait, and only then fails; the target is up
// from the third attempt on.
func TestGRPCACallThatFindsTheConnectionTryingReachesAServerThatCameBack(t *testing.T) {
	callsWaitForAConnection(t)
	server := grpctest.Start(t, grpctest.Options{Answer: statsAnswer})
	var attempts atomic.Int32
	trying, fail := make(chan struct{}), make(chan struct{})
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		switch attempts.Add(1) {
		case 1:
			return nil, errors.New("connection refused")
		case 2:
			close(trying)
			select {
			case <-fail:
			case <-ctx.Done():
			}
			return nil, errors.New("connection refused")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Addr)
	}
	conn, err := grpc.NewClient("passthrough:///down.test:1", grpc.WithTransportCredentials(insecure.NewCredentials()), waitsAnHour, grpc.WithContextDialer(dial))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.Connect()
	testutil.WaitFor(t, "the first attempt to fail", func() bool { return conn.GetState() == connectivity.TransientFailure })
	// The connection tries again, as it does by itself when its wait is over.
	conn.ResetConnectBackoff()
	<-trying
	told := toldOfResets{conn, make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconnectNow(context.Background(), told)
	}()
	<-told.reset
	close(fail)
	<-done
	if state := conn.GetState(); state != connectivity.Ready {
		t.Fatalf("after the call waited for it the connection is %s, after %d attempts: the server that came back was not reached", state, attempts.Load())
	}
}

// reconnectNowWas is reconnectNow as it was, the wait ended once.
func reconnectNowWas(ctx context.Context, conn reconnecting) {
	if conn.GetState() != connectivity.TransientFailure {
		return
	}
	conn.ResetConnectBackoff()
	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn.WaitForStateChange(wait, connectivity.TransientFailure)
}

// scriptedConnection is a connection in a state, which is made when its wait
// has been ended madeAfter times, 0 for never, and counts what is asked of
// it.
type scriptedConnection struct {
	state     connectivity.State
	madeAfter int
	resets    int
	waits     int
}

func (c *scriptedConnection) GetState() connectivity.State { return c.state }

func (c *scriptedConnection) ResetConnectBackoff() {
	c.resets++
	if c.resets == c.madeAfter {
		c.state = connectivity.Ready
	}
}

func (c *scriptedConnection) WaitForStateChange(ctx context.Context, from connectivity.State) bool {
	c.waits++
	if c.state != from {
		return true
	}
	<-ctx.Done()
	return false
}

// Ending the wait again changes nothing for a call that does not find its
// connection failed, for one whose connection is made when its wait is
// ended, and for one whose time is already over: against reconnectNow as it
// was, each asks the same of the connection and leaves it in the same state.
// What changed is the connection that stays failed: its wait was ended once
// in the second the call waits, and is ended four times, a quarter of a
// second apart.
func TestGRPCEndingAConnectionsWaitAgainLeavesOtherCallsAsTheyWere(t *testing.T) {
	over, cancel := context.WithCancel(context.Background())
	cancel()
	for _, state := range []connectivity.State{connectivity.Idle, connectivity.Connecting, connectivity.Ready, connectivity.TransientFailure, connectivity.Shutdown} {
		for name, ctx := range map[string]context.Context{"in time": context.Background(), "out of time": over} {
			for _, madeAfter := range []int{0, 1} {
				if state == connectivity.TransientFailure && madeAfter == 0 && ctx.Err() == nil {
					// The connection that stays failed, below.
					continue
				}
				was, now := &scriptedConnection{state: state, madeAfter: madeAfter}, &scriptedConnection{state: state, madeAfter: madeAfter}
				reconnectNowWas(ctx, was)
				reconnectNow(ctx, now)
				if *was != *now {
					t.Errorf("%s, %s, made after %d: the connection is left %+v, and was %+v", state, name, madeAfter, *now, *was)
				}
				if failed := state == connectivity.TransientFailure; (now.resets == 1) != failed || (now.waits == 1) != failed {
					t.Errorf("%s, %s, made after %d: %d ends of the wait and %d waits, want one of each only for a connection that failed", state, name, madeAfter, now.resets, now.waits)
				}
			}
		}
	}
	synctest.Test(t, func(t *testing.T) {
		for madeAfter, wantResets := range map[int]int{0: 4, 2: 2, 4: 4} {
			conn := &scriptedConnection{state: connectivity.TransientFailure, madeAfter: madeAfter}
			began := time.Now()
			reconnectNow(context.Background(), conn)
			wantTook, wantState := reconnectWait, connectivity.TransientFailure
			if madeAfter > 0 {
				wantTook, wantState = time.Duration(madeAfter-1)*reconnectAgain, connectivity.Ready
			}
			if took := time.Since(began); conn.resets != wantResets || took != wantTook || conn.state != wantState {
				t.Errorf("a connection made after %d ends of its wait: %d ends in %s, left %s; want %d in %s, left %s", madeAfter, conn.resets, took, conn.state, wantResets, wantTook, wantState)
			}
		}
	})
}

// Every retry of a call that found no connection dials: a target that goes
// on refusing is asked once for the first attempt and again for each retry,
// and the probe still fails as UNAVAILABLE.
func TestGRPCEachRetryOfACallWithoutAConnectionDialsAgain(t *testing.T) {
	// A listener that hangs up on every connection, counting them.
	listener := grpctest.Listen(t)
	t.Cleanup(func() { _ = listener.Close() })
	var dials atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = conn.Close()
		}
	}()
	c := protosetCollector(t)
	c.Request.Retry.Attempts = 2
	c.Request.Retry.Backoff = model.Duration(10 * time.Millisecond)
	checked := validGRPC(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx, trace := WithRequestTrace(ctx)
	_, err := FetchCollector(ctx, listener.Addr().String(), checked, RequestOverrides{}, nil)
	if code, _ := GRPCStatusCode(checked, err); code != int(codes.Unavailable) {
		t.Fatalf("err=%v, want UNAVAILABLE", err)
	}
	if n := len(trace.Requests()); n != 3 {
		t.Fatalf("%d attempts, want the call and its 2 retries", n)
	}
	if n := dials.Load(); n < 3 {
		t.Fatalf("the target was dialed %d times by a call and its 2 retries, want each to dial", n)
	}
}

// A connection that dies without a FIN or RST is replaced. grpc-go goes on
// calling it ready, so the probe that finds it dead waits out its deadline;
// that probe drops the connection, and the next one dials a new connection and
// is answered, rather than every later probe waiting out its deadline on the
// dead one.
//
// The probes that are to be answered have a minute, which a probe on the dead
// connection would wait out and fail by: being answered is what shows the
// new connection, so no time is measured.
func TestGRPCAConnectionThatDiesSilentlyIsReplacedAtTheNextProbe(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Answer: statsAnswer})
	relay := startTCPRelay(t, server.Addr)
	checked := validGRPC(t, protosetCollector(t))
	if _, err := probeGRPC(relay.addr, checked, time.Minute, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	relay.goSilent()
	_, err := probeGRPC(relay.addr, checked, 500*time.Millisecond, RequestOverrides{})
	if code, _ := GRPCStatusCode(checked, err); code != int(codes.DeadlineExceeded) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the probe on the dead connection: err=%v, want DEADLINE_EXCEEDED", err)
	}
	took, err := probeGRPC(relay.addr, checked, time.Minute, RequestOverrides{})
	if err != nil {
		t.Fatalf("the probe after the one that found the connection dead failed after %s: %v", took, err)
	}
	if accepted, _ := relay.connections(); accepted != 2 {
		t.Fatalf("%d connections were made, want the first and the one that replaced it", accepted)
	}
	if n := len(server.Calls()); n != 2 {
		t.Fatalf("the server answered %d calls, want 2", n)
	}
	// The dead connection is closed, since no call holds it.
	waitFor(t, "the dropped connection to be closed", func() bool {
		_, closed := relay.connections()
		return closed == 1
	})
}

// waitFor waits for a condition, as long as testutil.WaitFor does: half a
// minute, which bounds what never happens.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	testutil.WaitFor(t, what, condition)
}

// Dropping a connection does not cut off a call it is answering. Two probes
// share the connection; the deadline of one ends while the server is still
// working on the other. The connection is dropped, so a third probe dials a
// new one, but the call in progress is answered on the old connection, which
// is closed only when that call has ended.
func TestGRPCDroppingAConnectionDoesNotCutOffACallBeingAnswered(t *testing.T) {
	slowArrived, answerSlow := make(chan struct{}), make(chan struct{})
	server := grpctest.Start(t, grpctest.Options{Answer: func(ctx context.Context, m, request string) (string, error) {
		switch {
		case strings.Contains(request, "never"):
			<-ctx.Done()
			return "", ctx.Err()
		case strings.Contains(request, "slow"):
			close(slowArrived)
			<-answerSlow
		}
		return statsAnswer(ctx, m, request)
	}})
	// The server learns the call's deadline and gives the call up when it
	// passes, an instant after the probe does. Its word is held back, so that
	// the probe's own deadline is always what ends the call, as it is on a
	// connection that has died.
	relay := startSlowTCPRelay(t, server.Addr, 150*time.Millisecond)
	checked := validGRPC(t, protosetCollector(t))
	message := func(queue string) RequestOverrides {
		m := `{"queue": "` + queue + `"}`
		return RequestOverrides{Message: &m}
	}
	slow := make(chan error, 1)
	go func() {
		_, err := probeGRPC(relay.addr, checked, time.Minute, message("slow"))
		slow <- err
	}()
	<-slowArrived
	if _, err := probeGRPC(relay.addr, checked, 200*time.Millisecond, message("never")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the probe the server never answers: err=%v", err)
	}
	if _, err := probeGRPC(relay.addr, checked, time.Minute, message("next")); err != nil {
		t.Fatal(err)
	}
	if accepted, closed := relay.connections(); accepted != 2 || closed != 0 {
		t.Fatalf("%d connections made and %d closed; want a second one made, and the first kept open for the call it is answering", accepted, closed)
	}
	close(answerSlow)
	if err := <-slow; err != nil {
		t.Fatalf("the call being answered when its connection was dropped failed: %v", err)
	}
	waitFor(t, "the dropped connection to be closed once its last call ended", func() bool {
		_, closed := relay.connections()
		return closed == 1
	})
}

// The cache hands one connection to every call of its target, and counts
// them. A dropped connection is forgotten at once and closed when the last
// call holding it ends; dropping it again, as the other probes that timed
// out on it do, leaves the connection that replaced it alone.
func TestGRPCADroppedConnectionIsClosedWhenItsLastCallEnds(t *testing.T) {
	cache := &grpcConnCache{entries: map[grpcConnKey]*grpcConnEntry{}}
	key := grpcConnKey{dial: "127.0.0.1:1"}
	now := time.Now()
	get := func() *grpcConnEntry {
		t.Helper()
		entry, err := cache.get(key, now)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	dead := get()
	if again := get(); again != dead {
		t.Fatal("two calls at once did not share the connection")
	}
	cache.drop(key, dead)
	if cache.size() != 0 {
		t.Fatal("the dropped connection is still handed out")
	}
	replacement := get()
	if replacement == dead {
		t.Fatal("the call after the drop got the dropped connection")
	}
	cache.drop(key, dead)
	if again := get(); again != replacement {
		t.Fatal("dropping the dead connection a second time dropped the one that replaced it")
	}
	cache.release(dead)
	if state := dead.conn.GetState(); state == connectivity.Shutdown {
		t.Fatal("the dropped connection was closed while a call still held it")
	}
	cache.release(dead)
	if state := dead.conn.GetState(); state != connectivity.Shutdown {
		t.Fatalf("the dropped connection is %s after its last call ended, want it closed", state)
	}
	cache.release(replacement)
	cache.release(replacement)
	if state := replacement.conn.GetState(); state == connectivity.Shutdown || cache.size() != 1 {
		t.Fatalf("the connection in use was closed or forgotten: %s, %d kept", state, cache.size())
	}
	// A connection with no call in progress is closed as it is dropped.
	cache.drop(key, replacement)
	if state := replacement.conn.GetState(); state != connectivity.Shutdown {
		t.Fatalf("a dropped connection no call holds is %s, want it closed", state)
	}
}

// Only the probe's own deadline, on a connection that was made, drops it. A
// DEADLINE_EXCEEDED the server sent is an answer, on a connection that works;
// and a connection still being made when the deadline ends is making itself
// already, so dropping it would start a slow handshake over at every probe.
func TestGRPCOtherDeadlinesKeepTheConnection(t *testing.T) {
	t.Run("a status the server sent", func(t *testing.T) {
		server := grpctest.Start(t, grpctest.Options{Answer: func(context.Context, string, string) (string, error) {
			return "", status.Error(codes.DeadlineExceeded, "the backend took too long")
		}})
		relay := startTCPRelay(t, server.Addr)
		checked := validGRPC(t, protosetCollector(t))
		for range 2 {
			_, err := probeGRPC(relay.addr, checked, time.Minute, RequestOverrides{})
			if code, _ := GRPCStatusCode(checked, err); code != int(codes.DeadlineExceeded) || !strings.Contains(err.Error(), "the backend took too long") {
				t.Fatalf("err=%v", err)
			}
		}
		if accepted, _ := relay.connections(); accepted != 1 {
			t.Fatalf("%d connections were made, want both calls on one", accepted)
		}
	})
	t.Run("a connection still being made", func(t *testing.T) {
		// A listener that accepts and then says nothing: the HTTP/2
		// handshake never finishes.
		listener := grpctest.Listen(t)
		var mu sync.Mutex
		var held []net.Conn
		t.Cleanup(func() {
			_ = listener.Close()
			mu.Lock()
			defer mu.Unlock()
			for _, conn := range held {
				_ = conn.Close()
			}
		})
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				held = append(held, conn)
				mu.Unlock()
			}
		}()
		checked := validGRPC(t, protosetCollector(t))
		for range 2 {
			if _, err := probeGRPC(listener.Addr().String(), checked, 300*time.Millisecond, RequestOverrides{}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v", err)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(held) != 1 {
			t.Fatalf("%d connections were made, want the second probe to wait for the one being made", len(held))
		}
	})
}

// A reflection question on a connection that died silently is treated as a
// call is: the probe whose deadline ends waiting for it drops the connection,
// and the next probe asks again on a new one rather than joining the question
// that is still waiting on the dead connection.
func TestGRPCAReflectionQuestionOnADeadConnectionIsAskedAgainOnANewOne(t *testing.T) {
	server := grpctest.Start(t, grpctest.Options{Reflection: "v1", Answer: statsAnswer})
	relay := startTCPRelay(t, server.Addr)
	checked := validGRPC(t, grpcCollector())
	if _, err := probeGRPC(relay.addr, checked, time.Minute, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	relay.goSilent()
	// The answer has aged out, so the next probe asks the server again.
	reflectionAnswers.forget(reflectedKey{conn: grpcConnKey{dial: relay.addr, policy: policyOf(checked)}, service: grpctest.Service})
	if _, err := probeGRPC(relay.addr, checked, 500*time.Millisecond, RequestOverrides{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the probe on the dead connection: err=%v", err)
	}
	// A minute: a probe that joined the question on the dead connection would
	// fail with it when the question's own time ran out, well within that.
	// Being answered shows the new connection, so no time is measured.
	took, err := probeGRPC(relay.addr, checked, time.Minute, RequestOverrides{})
	if err != nil {
		t.Fatalf("the probe after the one that found the connection dead failed after %s: %v", took, err)
	}
	if accepted, _ := relay.connections(); accepted != 2 {
		t.Fatalf("%d connections were made, want the first and the one that replaced it", accepted)
	}
}
