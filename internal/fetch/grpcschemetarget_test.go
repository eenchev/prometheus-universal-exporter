//go:build linux && (!select_request_types || request_type_grpc)

package fetch

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// grpcSchemeTargetEnv makes TestGRPCSchemeTargetHelperProcess probe the
// target it names.
const grpcSchemeTargetEnv = "PUE_TEST_GRPC_SCHEME_TARGET"

// grpcSchemeTargetPolicyEnv makes the helper's collector allow the target's
// host in allowed_targets, which puts the exporter's dialer in charge when
// there is no proxy.
const grpcSchemeTargetPolicyEnv = "PUE_TEST_GRPC_SCHEME_TARGET_POLICY"

// A target whose host is the name of one of grpc-go's resolvers — unix,
// unix-abstract, passthrough — is host:port to the exporter, and was handed
// to grpc-go as written, which read it as a URL of that scheme: unix:8443 was
// the unix socket 8443, unix-abstract:8443 the abstract socket 8443, dialled
// by grpc-go itself whenever the exporter's dialer was not in charge, behind
// a proxy or with no allowed_targets. grpc-go is now always handed
// dns:///host:port, so unix-abstract and passthrough are names looked up over
// DNS, and behind a proxy the name the proxy is asked to CONNECT to. The host
// unix is refused before anything is dialled: grpc-go dials the address
// unix:8443, which is what a proxied dns:/// target becomes, as a unix socket
// whatever resolver produced it.
//
// grpc-go reads the proxy from the environment once per process, so each
// case is probed by a process of its own: this test binary, run as
// TestGRPCSchemeTargetHelperProcess with the environment of the case, in a
// directory holding the unix socket unix:<port> would name.
func TestGRPCATargetNamedLikeAResolverIsAHostName(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var reached atomic.Int32
	port := listenAbstract(t, &reached)
	countAccepts(&reached, listenUnix(t, filepath.Join(dir, port)))
	proxy := startConnectRecorder(t)
	for _, scheme := range []string{"unix", "unix-abstract", "passthrough"} {
		target := scheme + ":" + port
		for _, proxied := range []bool{false, true} {
			for _, policy := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s proxied=%v allowed_targets=%v", target, proxied, policy), func(t *testing.T) {
					reached.Store(0)
					asked := len(proxy.lines())
					proxyURL := ""
					if proxied {
						proxyURL = "http://" + proxy.addr
					}
					helper := exec.Command(binary, "-test.run=^TestGRPCSchemeTargetHelperProcess$", "-test.count=1")
					helper.Dir = dir
					helper.Env = append(os.Environ(), grpcSchemeTargetEnv+"="+target, grpcSchemeTargetPolicyEnv+"="+strconv.FormatBool(policy),
						"HTTPS_PROXY="+proxyURL, "https_proxy="+proxyURL, "HTTP_PROXY=", "http_proxy=", "NO_PROXY=", "no_proxy=")
					out, err := helper.CombinedOutput()
					if err != nil {
						t.Fatalf("the helper failed: %v\n%s", err, out)
					}
					if !strings.Contains(string(out), "probe failed") {
						t.Fatalf("the probe of %s did not fail:\n%s", target, out)
					}
					if n := reached.Load(); n != 0 {
						t.Fatalf("a unix socket was reached %d times:\n%s", n, out)
					}
					got := proxy.lines()[asked:]
					switch {
					case scheme == "unix":
						if !strings.Contains(string(out), "names the host unix") || len(got) != 0 {
							t.Fatalf("unix was not refused before anything was dialled; the proxy was asked %q:\n%s", got, out)
						}
					case proxied:
						if want := "CONNECT " + target + " "; len(got) == 0 || !strings.HasPrefix(got[0], want) {
							t.Fatalf("the proxy was asked %q, want %q:\n%s", got, want, out)
						}
					}
				})
			}
		}
	}
}

// TestGRPCSchemeTargetHelperProcess is not a test of its own: run as a
// process by TestGRPCATargetNamedLikeAResolverIsAHostName, it probes the
// target the environment names and prints how the probe ended.
func TestGRPCSchemeTargetHelperProcess(t *testing.T) {
	target := os.Getenv(grpcSchemeTargetEnv)
	if target == "" {
		t.Skip("only runs as the helper process of another test")
	}
	c := grpcCollector()
	if os.Getenv(grpcSchemeTargetPolicyEnv) == "true" {
		host, _, _ := strings.Cut(target, ":")
		c.Request.AllowedTargets = []string{host}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := FetchCollector(ctx, target, validGRPC(t, c), RequestOverrides{}, nil); err != nil {
		fmt.Println("probe failed:", err)
		return
	}
	fmt.Println("probe succeeded")
}

// listenAbstract listens on an abstract unix socket named by a free port
// number, as unix-abstract:<port> would name it, and counts what reaches it.
func listenAbstract(t *testing.T, reached *atomic.Int32) string {
	t.Helper()
	for range 20 {
		port := strconv.Itoa(20000 + rand.IntN(40000))
		listener, err := net.Listen("unix", "@"+port)
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = listener.Close() })
		countAccepts(reached, listener)
		return port
	}
	t.Fatal("no abstract unix socket could be listened on")
	return ""
}

func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func countAccepts(reached *atomic.Int32, listener net.Listener) {
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			reached.Add(1)
			_ = conn.Close()
		}
	}()
}

// connectRecorder is a proxy that records the request line of each
// connection and closes it.
type connectRecorder struct {
	addr     string
	mu       sync.Mutex
	received []string
}

func startConnectRecorder(t *testing.T) *connectRecorder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	r := &connectRecorder{addr: listener.Addr().String()}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				r.mu.Lock()
				r.received = append(r.received, strings.TrimSpace(line))
				r.mu.Unlock()
				_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()
	return r
}

func (r *connectRecorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.received...)
}
