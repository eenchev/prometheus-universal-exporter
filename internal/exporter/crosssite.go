package exporter

import (
	"fmt"
	"net/http"
	"strings"
)

// A browser attaches the exporter's Basic Auth credential, once it has been
// typed in, to every request to the exporter, those a page of another site
// makes included: a link, an image, a form or a redirect there is enough. So
// such a page could make a logged-in browser probe a target of its choosing
// through a collector that holds a credential, sending the credential to it
// (unless the collector's target policy keeps it to its own targets), reload
// the configuration with a form posted to /-/reload, or open the debug page
// on a target of its choosing. Without Basic Auth the same page could still
// reach an exporter on the visitor's own network, at localhost or on an
// intranet, which the page's own site cannot.
//
// A browser says where each request comes from in Sec-Fetch-Site: the
// exporter's own pages are same-origin, an address typed in or a bookmark
// none, and every other page cross-site, or same-site for another port or
// subdomain of the same site, which is no more the exporter's than another
// site is. Every endpoint but /health and /ready refuses the last two:
// Prometheus, curl and every other client that is not a browser send no
// Sec-Fetch-Site and are let through, as are the exporter's own pages. The
// header is one a page cannot set or remove (a forbidden header name), so
// what it says is the browser's word.
//
// A browser too old to send Sec-Fetch-Site is let through: its Origin is
// not compared with the request's own host, since a reverse proxy in front
// of the exporter may rewrite the Host, and the comparison would refuse
// the exporter's own pages behind it.

// crossSiteSources are the values of Sec-Fetch-Site that a page other than
// the exporter's own sent.
var crossSiteSources = map[string]bool{"cross-site": true, "same-site": true}

// sameOriginOnly refuses, with 403, a request the browser says a page of
// another site or of another origin of the same site made, before next sees
// it or its credential is checked.
func sameOriginOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); crossSiteSources[site] {
			http.Error(w, fmt.Sprintf("the browser says this request comes from a page of another site (Sec-Fetch-Site: %s); the exporter answers its own pages and clients that are not browsers only. Open the address directly instead", site), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
