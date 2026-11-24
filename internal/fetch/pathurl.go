//go:build !select_request_types || request_type_localfile || request_type_s3file || request_type_sshfile || request_type_sql || request_type_ftp

package fetch

import (
	"net/url"
	"strings"
)

// pathURL is the URL of a path read over scheme from host, as shown by the
// url label of a file, object or database read and by a debug report's
// request line. The path is escaped as a URL's path is, so a space in a
// file's name is %20, and a ? or # in it stays part of the path instead of
// reading as a query or fragment. {{param_...}} placeholders are kept as
// written, as requestLabel keeps them.
func pathURL(scheme, host, p string) string {
	u := url.URL{Scheme: scheme, Host: host, Path: p}
	return strings.NewReplacer("%7B%7B", "{{", "%7D%7D", "}}").Replace(u.String())
}
