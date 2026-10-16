//go:build !select_request_types || request_type_http || request_type_graphite || request_type_grpc

package fetch

import (
	"net/http"
	"strings"
	"testing"
)

// forwardedAsItWas is what a probe forwarded of a forward_headers entry
// before the configuration checked the entries: the exporter's
// forwardableHeaders as it was, with its list of the headers never
// forwarded, which now lives here. "" is nothing forwarded.
func forwardedAsItWas(entry string) string {
	never := map[string]bool{
		"Accept-Encoding": true, "Authorization": true, "Connection": true, "Content-Length": true, "Host": true,
		"Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	}
	name := http.CanonicalHeaderKey(strings.TrimSpace(entry))
	if name == "" || never[name] || strings.HasPrefix(name, "Proxy-") {
		return ""
	}
	return name
}

// A forward_headers entry is refused when the configuration loads exactly
// when a probe could never forward it: one the forwarding as it was drops,
// whatever its case and the blanks around it, and one that is no header
// name, which a probe's header_<name> parameter, canonicalised, could never
// match a name Go sends. Every other entry loads, and the list of the
// headers never forwarded is the one the forwarding used.
func TestAForwardHeadersEntryIsRefusedExactlyWhenItIsNeverForwarded(t *testing.T) {
	entries := []string{"", " ", "\t", "X-Tenant", " x-tenant ", "X Tenant", "X:Tenant", "X-Ténant", "Hosts", "X-Host", "Proxy", "Proxy-", "proxy-x", "PROXY-CONNECTION", "Te", "TE", "te", "Tee"}
	for _, name := range append(UnforwardableHeaderNames(), "Accept-Language", "User-Agent", "X-Forwarded-For", "Cookie") {
		entries = append(entries, name, strings.ToLower(name), strings.ToUpper(name), " "+name+"\t")
	}
	for _, entry := range entries {
		err := checkForwardHeaders([]string{entry})
		forwarded := forwardedAsItWas(entry)
		token := strings.TrimSpace(entry) != "" && !strings.ContainsFunc(strings.TrimSpace(entry), func(r rune) bool { return r <= ' ' || r == ':' || r >= 0x7f })
		if want := forwarded != "" && token; (err == nil) != want {
			t.Errorf("forward_headers [%q]: the check says %v, and a probe forwarded %q", entry, err, forwarded)
		}
	}
	if len(UnforwardableHeaderNames()) != 11 || UnforwardableReason("Proxy-Anything") == "" {
		t.Errorf("the headers never forwarded are %v and those starting %s", UnforwardableHeaderNames(), UnforwardablePrefix)
	}
}
