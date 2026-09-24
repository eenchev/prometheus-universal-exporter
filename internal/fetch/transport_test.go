package fetch

import (
	"net/url"
	"testing"
)

func TestBooleanOverrideParsing(t *testing.T) {
	for _, name := range []string{"follow_redirects", "enable_http2", "insecure_skip_verify"} {
		t.Run(name, func(t *testing.T) {
			absent, err := ParseRequestOverrides(url.Values{})
			if err != nil {
				t.Fatal(err)
			}
			if overrideFor(t, absent, name) != nil {
				t.Fatalf("an absent %s must leave the collector setting in force", name)
			}
			for _, raw := range []string{"true", "false"} {
				parsed, err := ParseRequestOverrides(url.Values{name: {raw}})
				if err != nil {
					t.Fatal(err)
				}
				value := overrideFor(t, parsed, name)
				if value == nil || *value != (raw == "true") {
					t.Fatalf("%s=%s parsed as %v", name, raw, value)
				}
			}
			for _, raw := range []string{"yes", "1", "", "TRUE!"} {
				if _, err := ParseRequestOverrides(url.Values{name: {raw}}); err == nil {
					t.Fatalf("%s=%q was accepted; want a client error", name, raw)
				}
			}
		})
	}
}

func overrideFor(t *testing.T, overrides RequestOverrides, name string) *bool {
	t.Helper()
	switch name {
	case "follow_redirects":
		return overrides.FollowRedirects
	case "enable_http2":
		return overrides.EnableHTTP2
	case "insecure_skip_verify":
		return overrides.InsecureSkipVerify
	}
	t.Fatalf("unknown override %q", name)
	return nil
}
