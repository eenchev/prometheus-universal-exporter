//go:build !select_request_types || request_type_http

package exporter

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every page has one icon, in its head: docs/logo.svg byte for byte, as a
// data: URL, which the pages' Content-Security-Policy lets it load.
func TestEveryPageHasTheLogoAsItsIcon(t *testing.T) {
	logo, err := os.ReadFile(filepath.Join("..", "..", "docs", "logo.svg"))
	if err != nil {
		t.Fatal(err)
	}
	server := landingServer(t, weatherConfig())
	server.SetDebug(DebugFull)
	icon := regexp.MustCompile(`<link rel="icon"[^>]*>`)
	href := regexp.MustCompile(`^<link rel="icon" type="image/svg\+xml" href="data:image/svg\+xml;base64,([A-Za-z0-9+/=]+)">$`)
	for _, path := range []string{"/", "/collectors", "/debug", server.staticTargetsStatusEndpoint()} {
		response := getPath(server, http.MethodGet, path)
		page := response.Body.String()
		head, _, _ := strings.Cut(page, "</head>")
		found := icon.FindAllString(page, -1)
		if len(found) != 1 || !strings.Contains(head, found[0]) {
			t.Errorf("%s has the icons %q, want one in its head", path, found)
			continue
		}
		match := href.FindStringSubmatch(found[0])
		if match == nil {
			t.Errorf("%s: the icon %q is not an SVG data: URL", path, found[0])
			continue
		}
		if svg, err := base64.StdEncoding.DecodeString(match[1]); err != nil || !bytes.Equal(svg, logo) {
			t.Errorf("%s: the icon is not docs/logo.svg (%v)", path, err)
		}
		if policy := response.Header().Get("Content-Security-Policy"); !regexp.MustCompile(`(^|; )img-src data:(;|$)`).MatchString(policy) {
			t.Errorf("%s: the Content-Security-Policy %q does not let the icon load", path, policy)
		}
	}
}
