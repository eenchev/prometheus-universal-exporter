//go:build (!select_request_types || request_type_http) && !unix

package fetch

// maxUnixSocketPath is the longest request.unix_socket the exporter takes
// where the system is no Unix, which no release is built for: Windows's
// AF_UNIX sun_path is 108 bytes, as Linux's is, less the NUL that ends it.
const maxUnixSocketPath = 107
