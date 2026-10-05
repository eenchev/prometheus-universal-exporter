//go:build !select_request_types || request_type_grpc

package grpctest

import (
	"errors"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
)

// Listen gives no port twice in a process, though each listener is closed
// before the next is asked for and the kernel gives a freed port out again:
// of as many ports as it gives here, taken as the kernel offers them, some
// come twice.
func TestListenGivesNoPortTwice(t *testing.T) {
	const listeners = 3000
	seen := map[string]bool{}
	for range listeners {
		listener := Listen(t)
		addr := listener.Addr().String()
		_ = listener.Close()
		if seen[addr] {
			t.Fatalf("%s was given twice in %d listeners", addr, len(seen)+1)
		}
		seen[addr] = true
	}
	// A server takes its port from Listen too.
	server := Start(t, Options{})
	if seen[server.Addr] {
		t.Fatalf("a server was started on %s, which a listener had before", server.Addr)
	}
}

// fatalSeen is a test that records that it was failed, and with what, where
// the test itself would end.
type fatalSeen struct {
	testing.TB
	with []any
}

func (f *fatalSeen) Helper() {}

func (f *fatalSeen) Fatal(args ...any) {
	f.with = args
	runtime.Goexit()
}

// A server started on the address a stopped one had waits for the address
// while something else holds it, and fails the test on any other error at
// once: whatever was given the port in the meantime has it only for a while.
func TestAnAddressInUseIsWaitedFor(t *testing.T) {
	inUse := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	asked := 0
	listener := listenWhenFree(t, func() (net.Listener, error) {
		if asked++; asked <= 3 {
			return nil, inUse
		}
		return Listen(t), nil
	})
	_ = listener.Close()
	if asked != 4 {
		t.Fatalf("the address was asked for %d times, want until it was free, the fourth", asked)
	}

	denied := errors.New("permission denied")
	failed := &fatalSeen{TB: t}
	asked = 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		listenWhenFree(failed, func() (net.Listener, error) {
			asked++
			return nil, denied
		})
	}()
	<-done
	if len(failed.with) == 0 {
		t.Fatalf("an error that is not of an address in use did not fail the test; the address was asked for %d times", asked)
	}
	if got, _ := failed.with[0].(error); asked != 1 || len(failed.with) != 1 || !errors.Is(got, denied) {
		t.Fatalf("an error that is not of an address in use: asked %d times, the test failed with %v; want once, and that error", asked, failed.with)
	}

	// The address itself: held, it is in use, and Start takes it once free.
	held := Listen(t)
	addr := held.Addr().String()
	if _, err := net.Listen("tcp", addr); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("listening on an address that is held: %v, want it in use", err)
	}
	_ = held.Close()
	if server := Start(t, Options{Addr: addr}); server.Addr != addr {
		t.Fatalf("the server listens on %s, want %s", server.Addr, addr)
	}
}
