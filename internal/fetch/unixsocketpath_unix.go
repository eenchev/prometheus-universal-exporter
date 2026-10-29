//go:build (!select_request_types || request_type_http) && unix

package fetch

import "syscall"

// maxUnixSocketPath is the longest request.unix_socket the system the
// exporter runs on can connect to, in bytes: sun_path, the path's room in
// the socket address, less the NUL that ends it. That is 107 on Linux and
// 103 on macOS and the BSDs, whose sun_path is 104 bytes; Go's net package
// refuses a longer path only when it dials.
const maxUnixSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1
