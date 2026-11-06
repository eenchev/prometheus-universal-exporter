//go:build !select_request_types || request_type_http || request_type_tcp

package fetch

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// request.unix_socket, of an http collector (unixsocket.go) and of a tcp
// one (requesttype_tcp.go), is checked alike in every build that has either
// type, and its pattern is the schema's in every build.

// maxUnixSocketPath is the longest request.unix_socket the system the
// exporter runs on can connect to, in bytes: sun_path, the path's room in
// the socket address, less the NUL that ends it. That is 107 on Linux, and
// on Windows, whose sun_path is 108 bytes too, and 103 on macOS and the
// BSDs, whose sun_path is 104; Go's net package refuses a longer path only
// when it dials.
const maxUnixSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// validateUnixSocket checks a collector's request.unix_socket when
// the configuration loads: other than "", the key left out, it is an
// absolute path with no placeholder or control character, no longer than
// the system's socket addresses hold (maxUnixSocketPath), and the
// collector names no proxy. A path too long would load and then fail every
// request when it dials, so it is refused here. The schema leaves the
// length alone: the limit is the system's, 107 bytes on Linux and 103 on
// macOS, and no one length would be right for both.
func validateUnixSocket(c *model.Collector, socket string) error {
	if socket == "" {
		return nil
	}
	switch {
	case strings.TrimSpace(socket) == "":
		return fmt.Errorf("collector %q request.unix_socket is nothing but blanks; write the absolute path of the socket, or leave the key out", c.Name)
	case HasPathParams(socket) || strings.Contains(socket, "{{"):
		return fmt.Errorf("collector %q request.unix_socket has a placeholder; the socket is the configuration's alone, and no probe parameter sets it", c.Name)
	case strings.IndexFunc(socket, unicode.IsControl) >= 0:
		return fmt.Errorf("collector %q request.unix_socket has a control character in it, almost always the line break an environment variable ended with", c.Name)
	case !strings.HasPrefix(socket, "/"):
		return fmt.Errorf("collector %q request.unix_socket %q is not an absolute path; write it from the root, such as /var/run/docker.sock, since a relative one would depend on the directory the exporter was started in", c.Name, socket)
	case len(socket) > maxUnixSocketPath:
		return fmt.Errorf("collector %q request.unix_socket is %d bytes long, and a Unix socket's path may be at most %d bytes on %s; use a shorter path to the socket, such as a symbolic link to it in a shorter directory", c.Name, len(socket), maxUnixSocketPath, runtime.GOOS)
	case c.Request.ProxyURL != "" || c.Request.ProxyBasicAuthFile != nil:
		return fmt.Errorf("collector %q sets request.unix_socket and request.proxy_url or proxy_basic_auth_file; its requests go to the socket, never through a proxy, so leave the proxy keys out", c.Name)
	}
	return nil
}
