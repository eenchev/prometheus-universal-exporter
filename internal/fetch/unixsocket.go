//go:build !select_request_types || request_type_http

package fetch

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
