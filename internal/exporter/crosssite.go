package exporter

import (
	"fmt"
	"net/http"
	"strings"
)

// Once the exporter's Basic Auth credential has been typed in, a browser
// attaches it to every request to the exporter, including those a page of
// another site makes: a link, an image, a form or a redirect there is
// enough. So such a page could make a logged-in browser:
//   - probe a target of its choosing through a collector that holds a
//     credential, sending the credential to it (unless the collector's
//     target policy keeps it to its own targets);
//   - reload the configuration with a form posted to /-/reload;
//   - open the debug page on a target of its choosing.
//
// Without Basic Auth the same page could still reach an exporter on the
// visitor's own network, at localhost or on an intranet, which the page's
// own site cannot.
//
// A browser states where each request comes from in Sec-Fetch-Site. The
// exporter's own pages are same-origin, and a typed address or a bookmark is
// none. Every other page is cross-site, or same-site for another port or
// subdomain of the same site, which belongs to the exporter no more than
// another site does. Every endpoint except /health and /ready refuses those
// last two. Prometheus, curl and every other non-browser client send no
// Sec-Fetch-Site and are let through, as are the exporter's own pages. A
// page cannot set or remove the header (a forbidden header name), so its
// value comes from the browser.
//
// A browser too old to send Sec-Fetch-Site is let through. Its Origin is not
// compared with the request's own host, because a reverse proxy in front of
// the exporter may rewrite the Host, and the comparison would then refuse
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
