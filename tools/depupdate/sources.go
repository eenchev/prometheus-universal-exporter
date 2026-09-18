package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// tagPageLimit bounds how far the tag listing is followed. The official images
// publish thousands of tags; this is enough to reach all of them and still
// terminate if a registry ever returns a cyclic Link header.
const tagPageLimit = 50

// httpClient is shared so every request carries the same timeout.
var httpClient = &http.Client{Timeout: 60 * time.Second}

// The service endpoints are variables so the tests can exercise the pagination
// and error handling against a stub registry without reaching the network.
var (
	dockerAuthBase = "https://auth.docker.io"
	registryBase   = "https://registry-1.docker.io"
	pypiBase       = "https://pypi.org"
)

// getJSON decodes a JSON response and returns the response headers, which the
// tag listing needs for its Link header. It deliberately does not return the
// response itself: the body is consumed and closed here, so no caller can leak
// it.
func getJSON(ctx context.Context, endpoint string, header http.Header, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.Header, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.Header, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	return resp.Header, nil
}

// dockerToken fetches the anonymous pull token the Docker registry requires
// even for public images.
func dockerToken(ctx context.Context, repo string) (string, error) {
	endpoint := dockerAuthBase + "/token?service=registry.docker.io&scope=" +
		url.QueryEscape("repository:"+repo+":pull")
	var body struct {
		Token string `json:"token"`
	}
	if _, err := getJSON(ctx, endpoint, nil, &body); err != nil {
		return "", err
	}
	if body.Token == "" {
		return "", fmt.Errorf("the registry returned no pull token for %s", repo)
	}
	return body.Token, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// dockerTags lists every tag of a public image, following the registry's Link
// header across pages.
func dockerTags(ctx context.Context, repo string) ([]string, error) {
	token, err := dockerToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	endpoint := registryBase + "/v2/" + repo + "/tags/list?n=1000"
	var tags []string
	for page := 0; page < tagPageLimit && endpoint != ""; page++ {
		var body struct {
			Tags []string `json:"tags"`
		}
		responseHeader, err := getJSON(ctx, endpoint, header, &body)
		if err != nil {
			return nil, err
		}
		tags = append(tags, body.Tags...)
		endpoint = ""
		if match := nextLink.FindStringSubmatch(responseHeader.Get("Link")); match != nil {
			base, err := url.Parse(registryBase)
			if err != nil {
				return nil, fmt.Errorf("unusable registry address %q: %w", registryBase, err)
			}
			next, err := url.Parse(strings.TrimSpace(match[1]))
			if err != nil {
				return nil, fmt.Errorf("the registry returned an unusable Link header %q: %w", match[1], err)
			}
			endpoint = base.ResolveReference(next).String()
		}
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("the registry listed no tags for %s", repo)
	}
	return tags, nil
}

// pypiVersions lists the releases of a package that pip can still install.
func pypiVersions(ctx context.Context, pkg string) ([]string, error) {
	var body struct {
		Releases map[string][]pypiFile `json:"releases"`
	}
	endpoint := pypiBase + "/pypi/" + url.PathEscape(pkg) + "/json"
	if _, err := getJSON(ctx, endpoint, nil, &body); err != nil {
		return nil, err
	}
	versions := pypiCandidates(body.Releases)
	if len(versions) == 0 {
		return nil, fmt.Errorf("PyPI listed no installable release of %s", pkg)
	}
	return versions, nil
}
