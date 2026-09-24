package fetch

import (
	"strings"
	"testing"
)

// Prometheus service discovery produces __address__ as host:port, with no
// scheme, and the chart's monitors pass it through unchanged as the probe's
// target. A target without a scheme is therefore the normal case, not an edge
// case, and it means http.

func TestATargetWithoutASchemeMeansHTTP(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{"10.0.0.5:8080", "http://10.0.0.5:8080/status"},
		{"legacy.example:8080", "http://legacy.example:8080/status"},
		{"legacy.example", "http://legacy.example/status"},
		{"[fd00::5]:9000", "http://[fd00::5]:9000/status"},
		{"  10.0.0.5:8080  ", "http://10.0.0.5:8080/status"},
		{"10.0.0.5:8080/base", "http://10.0.0.5:8080/base/status"},
		// A target that names its scheme keeps it.
		{"https://secure.example:8443", "https://secure.example:8443/status"},
		{"http://legacy.example:8080", "http://legacy.example:8080/status"},
	}
	c := pathCollector("/status")
	for _, test := range tests {
		t.Run(test.target, func(t *testing.T) {
			u, err := ResolveRequestURL(test.target, &c, RequestOverrides{})
			if err != nil {
				t.Fatal(err)
			}
			if got := u.String(); got != test.want {
				t.Fatalf("%q resolved to %q, want %q", test.target, got, test.want)
			}
		})
	}
}

// Credentials in a scheme-less target still reach the request, and are still
// redacted wherever the target is shown.
func TestCredentialsInASchemeLessTarget(t *testing.T) {
	c := pathCollector("/status")
	u, err := ResolveRequestURL("operator:s3cret@10.0.0.5:8080", &c, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if password, _ := u.User.Password(); u.User.Username() != "operator" || password != "s3cret" {
		t.Fatalf("credentials were lost: %v", u.User)
	}
	if shown := safeTarget("operator:s3cret@10.0.0.5:8080"); strings.Contains(shown, "s3cret") || !strings.HasPrefix(shown, "http://") {
		t.Fatalf("safeTarget=%q", shown)
	}
}

// safeTarget runs on error paths, so it must survive anything a caller sends.
// A target it cannot parse is withheld rather than echoed, since the password
// in it could not be found to redact.
func TestSafeTargetNeverPanics(t *testing.T) {
	for _, raw := range []string{"", "10.0.0.5:8080", "http://[::1", "user:pa ss@%zz:1", "://", "legacy.example:8080"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("safeTarget(%q) panicked: %v", raw, r)
				}
			}()
			if shown := safeTarget(raw); strings.Contains(shown, "pa ss") {
				t.Fatalf("safeTarget(%q)=%q leaked the password", raw, shown)
			}
		}()
	}
}

// allowed_schemes still governs the result: a bare target is http, so a
// collector that only allows https rejects it rather than upgrading it.
func TestABareTargetIsSubjectToAllowedSchemes(t *testing.T) {
	c := pathCollector("/status")
	c.Request.AllowedSchemes = []string{"https"}
	if _, err := ResolveRequestURL("10.0.0.5:8080", &c, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), `scheme "http" is not allowed`) {
		t.Fatalf("err=%v", err)
	}
	if _, err := ResolveRequestURL("https://10.0.0.5:8443", &c, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
}
