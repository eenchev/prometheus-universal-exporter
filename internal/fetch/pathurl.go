//go:build !select_request_types || request_type_localfile || request_type_s3file || request_type_sshfile || request_type_sql || request_type_ftp

package fetch

import (
	"net/url"
	"strings"
)

// pathURL is the URL of a path read over scheme from host, as the url label
// of a file, an object or a database read and a debug report's request line
// show it: the path escaped as a URL's path is, so a space in a file's name
// is %20 and a ? or a # in it stays part of the path rather than reading as
// a query or a fragment, with {{param_...}} placeholders kept as they are
// written, as requestLabel keeps them.
func pathURL(scheme, host, p string) string {
	u := url.URL{Scheme: scheme, Host: host, Path: p}
	return strings.NewReplacer("%7B%7B", "{{", "%7D%7D", "}}").Replace(u.String())
}
