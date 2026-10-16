//go:build !select_request_types || request_type_http

package exporter

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The exporter's Basic Auth compares SHA-256 digests of the presented and
// the configured credential (credentialsMatch), so the comparison takes the
// same time whatever their lengths: subtle.ConstantTimeCompare on the
// credentials themselves returned at once when the lengths differed, which
// told a client the configured password's length.

// Whatever the lengths of the presented and the configured username and
// password, and whichever of them is wrong, the comparison is two calls of
// the constant-time compare, both always made, each on two slices of the
// same 32 bytes.
func TestCredentialsAreComparedAsDigestsOfOneLength(t *testing.T) {
	lengths := []int{0, 1, 7, 64, 4096}
	for _, configured := range lengths[1:] {
		want, err := config.WebAuthDigests(&model.ExporterBasicAuth{Enabled: true, Username: strings.Repeat("u", configured), Password: strings.Repeat("p", configured)})
		if err != nil {
			t.Fatal(err)
		}
		for _, presentedUser := range lengths {
			for _, presentedPassword := range lengths {
				var calls [][2]int
				compare := func(x, y []byte) int {
					calls = append(calls, [2]int{len(x), len(y)})
					return subtle.ConstantTimeCompare(x, y)
				}
				got := credentialsMatch(compare, strings.Repeat("u", presentedUser), strings.Repeat("p", presentedPassword), want)
				if len(calls) != 2 || calls[0] != [2]int{32, 32} || calls[1] != [2]int{32, 32} {
					t.Fatalf("configured %d, presented %d and %d: compared %v", configured, presentedUser, presentedPassword, calls)
				}
				if right := presentedUser == configured && presentedPassword == configured; got != right {
					t.Fatalf("configured %d, presented %d and %d: matched %v", configured, presentedUser, presentedPassword, got)
				}
			}
		}
	}
}

// Only the configured username with the configured password is let in: a
// wrong username, a wrong password, both, either left empty, no credential,
// one a prefix or an extension of the right one, and one of a megabyte are
// refused 401 with the challenge, from the configuration as from files.
func TestExporterBasicAuthLetsInOnlyTheCredential(t *testing.T) {
	server, err := webAuthServer(t, &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("s", 1<<20)
	for _, tc := range []struct {
		user, password string
		want           int
	}{
		{"exporter", "s3cret", http.StatusOK},
		{"other", "s3cret", http.StatusUnauthorized},
		{"exporter", "wrong", http.StatusUnauthorized},
		{"other", "wrong", http.StatusUnauthorized},
		{"", "s3cret", http.StatusUnauthorized},
		{"exporter", "", http.StatusUnauthorized},
		{"", "", http.StatusUnauthorized},
		{"exporter", "s3cre", http.StatusUnauthorized},
		{"exporter", "s3crett", http.StatusUnauthorized},
		{"exporte", "s3cret", http.StatusUnauthorized},
		{"exporter", long, http.StatusUnauthorized},
		{long, "s3cret", http.StatusUnauthorized},
	} {
		if got := selfMetricsAs(t, server, tc.user, tc.password); got != tc.want {
			t.Errorf("%.20q/%.20q: %d, want %d", tc.user, tc.password, got, tc.want)
		}
	}
	// A request with no credential at all, and one whose Authorization is
	// not Basic, get the challenge.
	for _, header := range []http.Header{nil, {"Authorization": {"Bearer s3cret"}}} {
		r := probeOnce(t, server, "/self-metrics", header)
		if r.Code != http.StatusUnauthorized || !strings.Contains(r.Header().Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%v: %d, WWW-Authenticate %q", header, r.Code, r.Header().Get("WWW-Authenticate"))
		}
	}

	// A password of a megabyte is let in when it is the one configured.
	server, err = webAuthServer(t, &model.ExporterBasicAuth{Enabled: true, Username: "exporter", Password: long})
	if err != nil {
		t.Fatal(err)
	}
	if got := selfMetricsAs(t, server, "exporter", long); got != http.StatusOK {
		t.Fatalf("the long password was refused: %d", got)
	}
	if got := selfMetricsAs(t, server, "exporter", long[1:]); got != http.StatusUnauthorized {
		t.Fatalf("a shorter password was let in: %d", got)
	}
}
