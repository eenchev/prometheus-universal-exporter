package kubeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/kubetest"
)

// items are n objects named obj-0 and on.
func items(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"metadata": map[string]any{"name": fmt.Sprintf("obj-%d", i)}}
	}
	return out
}

// names lists every page of l, the names of the objects in the order
// listed.
func names(t *testing.T, l *Listing) ([]string, error) {
	t.Helper()
	var got []string
	err := l.List(context.Background(), func(raw []byte) (string, error) {
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, item := range page.Items {
			got = append(got, item.Metadata.Name)
		}
		return page.Metadata.Continue, nil
	})
	return got, err
}

// A listing asks for pages of the page size, follows each continue token
// to the last page, sends the token, both selectors and an Accept of JSON,
// and tells of each request and how it was answered.
func TestAListingFollowsEveryPage(t *testing.T) {
	s := kubetest.New(t)
	s.Serve("/api/v1/namespaces/shop/pods", kubetest.List{Kind: "PodList", APIVersion: "v1", Items: items(7)})
	var sent, answered []string
	l := &Listing{
		Client: s.Client(), Server: s.URL, Path: "/api/v1/namespaces/shop/pods", Token: "token-1", What: "pods",
		LabelSelector: "app=shop", FieldSelector: "status.phase=Running", PageSize: 3, MaxPageBytes: 1 << 20,
		Sent:     func(req *http.Request) { sent = append(sent, req.URL.Query().Get("continue")) },
		Answered: func(outcome string) { answered = append(answered, outcome) },
	}
	got, err := names(t, l)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"obj-0", "obj-1", "obj-2", "obj-3", "obj-4", "obj-5", "obj-6"}; !slices.Equal(got, want) {
		t.Errorf("listed %v, want %v", got, want)
	}
	requests := s.Requests()
	if len(requests) != 3 || !slices.Equal(sent, []string{"", "3", "6"}) || !slices.Equal(answered, []string{"200 OK", "200 OK", "200 OK"}) {
		t.Fatalf("requests %+v, sent %v, answered %v", requests, sent, answered)
	}
	for _, r := range requests {
		if r.Query.Get("limit") != "3" || r.Query.Get("labelSelector") != "app=shop" || r.Query.Get("fieldSelector") != "status.phase=Running" || r.Authorization != "Bearer token-1" {
			t.Errorf("request %+v", r)
		}
	}

	// Without a page size, the default; without selectors, none sent.
	l = &Listing{Client: s.Client(), Server: s.URL, Path: "/api/v1/namespaces/shop/pods", Token: "token-1", What: "pods", MaxPageBytes: 1 << 20}
	if _, err := names(t, l); err != nil {
		t.Fatal(err)
	}
	last := s.Requests()[len(s.Requests())-1]
	if last.Query.Get("limit") != "100" || last.Query.Has("labelSelector") || last.Query.Has("fieldSelector") {
		t.Errorf("request %+v", last)
	}
}

// An answer other than 200 is a StatusError with the status and the start
// of the body; a page over the bound is ErrPageTooLarge; without a token,
// no Authorization is sent.
func TestAListingsFailuresSayWhat(t *testing.T) {
	s := kubetest.New(t)
	s.Serve("/apis/apps/v1/deployments", kubetest.List{Kind: "DeploymentList", APIVersion: "apps/v1", Items: items(50)})
	s.Forbid("/api/v1/nodes")
	for path, code := range map[string]int{"/api/v1/nodes": 403, "/apis/example.com/v1/widgets": 404} {
		_, err := names(t, &Listing{Client: s.Client(), Server: s.URL, Path: path, Token: "token-1", What: "things", MaxPageBytes: 1 << 20})
		var status *StatusError
		if !errors.As(err, &status) || status.Code != code || !strings.Contains(err.Error(), "listing things at "+s.URL+path+" answered ") || !strings.Contains(status.Body, `"kind":"Status"`) {
			t.Errorf("%s: %v", path, err)
		}
	}
	_, err := names(t, &Listing{Client: s.Client(), Server: s.URL, Path: "/apis/apps/v1/deployments", Token: "token-1", What: "deployments", MaxPageBytes: 100})
	if !errors.Is(err, ErrPageTooLarge) || err.Error() != "a page of deployments is larger than 100 bytes" {
		t.Errorf("a page over the bound: %v", err)
	}
	_, err = names(t, &Listing{Client: s.Client(), Server: s.URL, Path: "/apis/apps/v1/deployments", What: "deployments", MaxPageBytes: 1 << 20})
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 401 || s.Requests()[len(s.Requests())-1].Authorization != "" {
		t.Errorf("without a token: %v, sent %+v", err, s.Requests()[len(s.Requests())-1])
	}
}

// The in-cluster server is the environment's, and outside a pod there is
// none; the namespace and the token are read from their files, and an
// empty one is refused.
func TestTheInClusterConfigurationIsReadFromThePod(t *testing.T) {
	s := kubetest.New(t)
	s.InCluster(t)
	if server, err := InClusterServer(); err != nil || server != s.URL {
		t.Errorf("server %q, %v; want %q", server, err, s.URL)
	}
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := InClusterServer(); !errors.Is(err, ErrNotInCluster) {
		t.Errorf("outside a pod: %v", err)
	}
	if ns, err := OwnNamespace(s.Dir); err != nil || ns != "monitoring" {
		t.Errorf("namespace %q, %v", ns, err)
	}
	if token, err := ReadToken(filepath.Join(s.Dir, TokenFile)); err != nil || token != "token-1" {
		t.Errorf("token %q, %v", token, err)
	}
	s.Write(t, NamespaceFile, " \n")
	s.Write(t, TokenFile, "\n")
	if _, err := OwnNamespace(s.Dir); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("an empty namespace: %v", err)
	}
	if _, err := ReadToken(filepath.Join(s.Dir, TokenFile)); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("an empty token: %v", err)
	}
}
