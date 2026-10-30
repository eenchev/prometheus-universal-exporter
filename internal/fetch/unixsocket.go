//go:build !select_request_types || request_type_http

package fetch

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// An http collector may send its requests over a Unix domain socket,
// request.unix_socket, in place of a TCP connection to the URL's host, as
// curl --unix-socket does: Docker's API on /var/run/docker.sock, a php-fpm
// or nginx status page served on a socket, a systemd service's. The URL
// keeps every other part of its meaning: its host is the Host header, and
// the TLS server name of an https URL, whose handshake is made over the
// socket; its path and query are the request's. Every request of the
// collector goes to the socket, its login's, its redirects' and its pages'
// included, whatever their host, as they would go through its proxy, which
// the socket stands in place of: the two are refused together, and neither
// the environment's proxy nor the defaults block's applies to it.
//
// The socket is the operator's choice alone. It is a key of the collector,
// which no probe parameter and no static target sets, and it holds no
// placeholder: whoever can reach /probe names the URL, whose host only
// goes into the request, and never the file the exporter connects to.
// So the path is trusted as configuration is, and is no target of
// request.allowed_targets and denied_targets; they hold the URL's host to
// its name as written, and an address written as the host to the address
// rules, since there is no address the request is dialed to (targetpolicy.go).
//
// The socket is part of the transport's settings, so collectors with
// different sockets never share a connection pool, and none shares one
// with a collector that dials TCP. Its path is a file of the exporter's
// side, which a failed probe's caller is not told (transportwithheld.go).

// UnixSocketPattern is what request.unix_socket may be, for the
// configuration's JSON Schema: an absolute path, or "", which is the key
// left out. What else it has to be is the exporter's to refuse.
const UnixSocketPattern = `^$|^/`

// maxUnixSocketPath is the longest request.unix_socket the system the
// exporter runs on can connect to, in bytes: sun_path, the path's room in
// the socket address, less the NUL that ends it. That is 107 on Linux, and
// on Windows, whose sun_path is 108 bytes too, and 103 on macOS and the
// BSDs, whose sun_path is 104; Go's net package refuses a longer path only
// when it dials.
const maxUnixSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// validateUnixSocket checks an http collector's request.unix_socket when
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
