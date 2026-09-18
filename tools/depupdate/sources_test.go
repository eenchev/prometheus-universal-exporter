package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubRegistry serves the two calls the Docker registry requires — an anonymous
// pull token, then the tag list — and splits the tags across two pages so the
// Link header handling is exercised.
func stubRegistry(t *testing.T, pages [][]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"stub-token"}`))
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			if got := r.Header.Get("Authorization"); got != "Bearer stub-token" {
				t.Errorf("Authorization=%q, want the pull token", got)
			}
			page := 0
			if r.URL.Query().Get("last") != "" {
				page = 1
			}
			if page+1 < len(pages) {
				w.Header().Set("Link", `</v2/library/golang/tags/list?n=1000&last=`+pages[page][len(pages[page])-1]+`>; rel="next"`)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"library/golang","tags":["` + strings.Join(pages[page], `","`) + `"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestDockerTagsFollowsEveryPage(t *testing.T) {
	server := stubRegistry(t, [][]string{{"1.22-alpine", "1.23-alpine"}, {"1.24-alpine", "latest"}})
	dockerAuthBase, registryBase = server.URL, server.URL
	t.Cleanup(func() { dockerAuthBase, registryBase = "https://auth.docker.io", "https://registry-1.docker.io" })

	tags, err := dockerTags(context.Background(), "library/golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 4 {
		t.Fatalf("tags=%v, want all four across both pages", tags)
	}
	next, err := selectUpdate("1.22", dockerTagCandidates(tags, "-alpine"), policy{AllowMinor: true})
	if err != nil {
		t.Fatal(err)
	}
	if next != "1.24" {
		t.Fatalf("next=%q, want the newest alpine tag from the second page", next)
	}
}

// A source that fails must surface the failure. Reporting "nothing to update"
// would be indistinguishable from being current, and the schedule would go
// quietly stale.
func TestDockerTagsReportsAFailingRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	dockerAuthBase, registryBase = server.URL, server.URL
	t.Cleanup(func() { dockerAuthBase, registryBase = "https://auth.docker.io", "https://registry-1.docker.io" })

	if _, err := dockerTags(context.Background(), "library/golang"); err == nil {
		t.Fatal("a failing registry must be reported as an error")
	}
}

func TestPypiVersionsReadsTheReleaseIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"releases":{"5.3.0":[{"yanked":false}],"5.3.1":[{"yanked":true}],"5.4.0":[{"yanked":false}]}}`))
	}))
	t.Cleanup(server.Close)
	pypiBase = server.URL
	t.Cleanup(func() { pypiBase = "https://pypi.org" })

	versions, err := pypiVersions(context.Background(), "lxml")
	if err != nil {
		t.Fatal(err)
	}
	next, err := selectUpdate("5.3.0", versions, policy{AllowMinor: true})
	if err != nil {
		t.Fatal(err)
	}
	if next != "5.4.0" {
		t.Fatalf("next=%q, want 5.4.0; the yanked 5.3.1 must be ignored", next)
	}
}

// resolve drives the whole table; a pin the Dockerfile no longer declares is
// skipped rather than failing the run.
func TestResolveSkipsArgumentsTheDockerfileNoLongerDeclares(t *testing.T) {
	updates, err := resolve(context.Background(), map[string]string{})
	if err != nil {
		t.Fatalf("an empty Dockerfile should resolve to no updates, got %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("updates=%v, want none", updates)
	}
}
