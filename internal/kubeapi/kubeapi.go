// Package kubeapi lists objects from a Kubernetes API server over its REST
// API, with nothing but the standard library: the target allocator's
// kubernetes discovery lists EndpointSlices with it, and the exporter's
// kubernetes request type lists objects of any kind. There is no client
// library behind it, and none is needed: a listing is a GET of a path,
// authenticated with a bearer token, answered with a JSON list whose
// metadata.continue names the next page.
//
// It reads what a pod is given to reach the API server of its cluster: the
// address in KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT, and the
// service account's token, CA and namespace in the files Kubernetes mounts.
// The token is read again at every listing, so a rotated one is used; the
// caller builds the HTTP client, which verifies the server against the CA,
// so this package has no TLS settings of its own.
package kubeapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ServiceAccountDir is where Kubernetes mounts a pod's service account: its
// token, the cluster's CA and the pod's namespace.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// The files of a service account directory.
const (
	TokenFile     = "token"
	CAFile        = "ca.crt"
	NamespaceFile = "namespace"
)

// PageSize is how many objects one request of a listing asks for; the rest
// follow with the answer's continue token. 100 keeps a page of the largest
// objects a listing meets, EndpointSlices of 100 endpoints, at about 10 MiB.
const PageSize = 100

// InClusterServer is the URL of the API server of the cluster the process
// runs in, from the environment Kubernetes gives every pod.
func InClusterServer() (string, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return "", ErrNotInCluster
	}
	return "https://" + net.JoinHostPort(host, port), nil
}

// ErrNotInCluster is InClusterServer's answer outside a pod. Each caller
// words what it means for its own reader.
var ErrNotInCluster = errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")

// OwnNamespace is the namespace of the pod whose service account is mounted
// at dir, from its namespace file.
func OwnNamespace(dir string) (string, error) {
	path := filepath.Join(dir, NamespaceFile)
	raw, err := os.ReadFile(path) // #nosec G304 -- the service account's own file
	if err != nil {
		return "", err
	}
	namespace := strings.TrimSpace(string(raw))
	if namespace == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return namespace, nil
}

// ReadToken reads a bearer token file, refusing an empty one.
func ReadToken(path string) (string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- a token file the configuration names
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// Listing is one listing of a path of an API server, every page of it.
type Listing struct {
	// Client sends the requests, and verifies the server.
	Client *http.Client
	// Server is the API server's URL, scheme, host and port, and Path the
	// list path, such as /api/v1/namespaces/db/pods.
	Server, Path string
	// Token is sent as the bearer token of every page; "" sends none, for a
	// client that authenticates with its certificate.
	Token string
	// What names the objects in errors, such as "EndpointSlices".
	What string
	// LabelSelector and FieldSelector are sent as they are; "" sends none.
	LabelSelector, FieldSelector string
	// PageSize is the limit asked for each page; 0 is PageSize.
	PageSize int
	// MaxPageBytes bounds the body of each page; a larger one fails the
	// listing with an error that is ErrPageTooLarge.
	MaxPageBytes int64
	// Sent, when set, is told of each page's request before it is sent,
	// and Answered of how it ended: the status line, or the error.
	Sent     func(req *http.Request)
	Answered func(outcome string)
}

// ErrPageTooLarge is the error of a page over MaxPageBytes.
var ErrPageTooLarge = errors.New("page too large")

// StatusError is an API server's answer other than 200 OK: its status and
// the start of its body, which for an API error is a Status object whose
// message says what was refused.
type StatusError struct {
	What, Address string
	Code          int
	Status        string
	Body          string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("listing %s at %s answered %s: %s", e.What, e.Address, e.Status, e.Body)
}

// pageTooLarge is the error of a page over the bound, ErrPageTooLarge
// with the bound in words.
type pageTooLarge struct {
	what  string
	bound int64
}

func (e *pageTooLarge) Error() string {
	return fmt.Sprintf("a page of %s is larger than %d bytes", e.what, e.bound)
}

func (e *pageTooLarge) Is(target error) bool { return target == ErrPageTooLarge }

// List asks for every page of the listing in turn, handing each page's body
// to page, which returns the page's continue token, "" after the last.
func (l *Listing) List(ctx context.Context, page func(raw []byte) (next string, err error)) error {
	next := ""
	for {
		raw, err := l.page(ctx, next)
		if err != nil {
			return err
		}
		if next, err = page(raw); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
	}
}

// page asks for one page of the listing.
func (l *Listing) page(ctx context.Context, next string) ([]byte, error) {
	size := l.PageSize
	if size <= 0 {
		size = PageSize
	}
	query := url.Values{"limit": {strconv.Itoa(size)}}
	if l.LabelSelector != "" {
		query.Set("labelSelector", l.LabelSelector)
	}
	if l.FieldSelector != "" {
		query.Set("fieldSelector", l.FieldSelector)
	}
	if next != "" {
		query.Set("continue", next)
	}
	address := l.Server + l.Path
	// The address is the API server's: from the pod's environment, which
	// Kubernetes sets, or from the configuration.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"?"+query.Encode(), nil) // #nosec G704 -- the cluster's API server
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if l.Token != "" {
		req.Header.Set("Authorization", "Bearer "+l.Token)
	}
	if l.Sent != nil {
		l.Sent(req)
	}
	resp, err := l.Client.Do(req) // #nosec G704 -- the cluster's API server
	if err != nil {
		l.answered("error: " + err.Error())
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	l.answered(resp.Status)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &StatusError{What: l.What, Address: address, Code: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(body))}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, l.MaxPageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > l.MaxPageBytes {
		return nil, &pageTooLarge{what: l.What, bound: l.MaxPageBytes}
	}
	return raw, nil
}

func (l *Listing) answered(outcome string) {
	if l.Answered != nil {
		l.Answered(outcome)
	}
}
