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
	"cmp"
	"context"
	"encoding/json"
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
	"time"
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

// UserAgent is the User-Agent every request is sent with, so the API
// server's audit log, and a FlowSchema of its priority and fairness, can
// tell the program apart from other clients of its service account. Each
// program sets it to its name and version as it starts; a test's requests
// are sent with the name alone.
var UserAgent = "prometheus-universal-exporter"

// The Accept of a listing: JSON, or, with metadata alone, the
// PartialObjectMetadataList the API server answers in place of the objects,
// with no fallback, so a server that cannot answer it refuses (406) rather
// than sending the objects whole.
const (
	AcceptJSON         = "application/json"
	AcceptMetadataList = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
)

// PageSize is how many objects one request of a listing asks for at first;
// the rest follow with the answer's continue token. 100 keeps a page of the
// largest objects a listing commonly meets, EndpointSlices of 100
// endpoints, at about 10 MiB. A page over the listing's bound is asked for
// again with half as many objects, so larger objects make smaller pages
// rather than a failed listing.
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
	// Accept is the Accept every page is asked with: AcceptJSON when "".
	Accept string
	// PageSize is the limit asked for the first page; 0 is PageSize.
	PageSize int
	// MaxPageBytes bounds the body of each page. A page over it is asked
	// for again, with the same continue token, with half the limit, down
	// to 1, and the rest of the listing keeps the smaller limit; a page of
	// one object over it fails the listing with an error that is
	// ErrPageTooLarge.
	MaxPageBytes int64
	// Prepare, when set, is given each page's request before it is sent, a
	// page asked for again included, and returns the request to send, which
	// may carry a context of its own, and what to tell of how it ended.
	Prepare func(req *http.Request) (*http.Request, Sent)
	// Retry, when set, is asked of every answer other than 200 OK, with the
	// number of times the page was asked for again so far, whether to ask
	// for it again, and after how long: it may, for an API server's 429 of
	// its priority and fairness, wait as its Retry-After asks. The wait is
	// bounded by the listing's context.
	Retry func(ctx context.Context, answer *StatusError, header http.Header, retried int) (wait time.Duration, again bool)
	// ResumeExpired has a listing whose continue token the API server no
	// longer has, its 410 Expired, go on: with the token the Status
	// carries for the rest of the listing as it is now (an inconsistent
	// continue), or, the Status carrying none, from the first page again,
	// once, Restart told first, so what the pages before were handed is
	// put away. A listing that expires again fails.
	ResumeExpired bool
	Restart       func()
	// Smaller, when set, is told of each smaller limit the listing falls
	// to, after a page over MaxPageBytes.
	Smaller func(size int)
}

// Sent is what a request's Prepare is told of it: Answered of how it ended,
// the status line, the status line and that the page was over the bound,
// or the error; and Waiting of the wait before it is asked for again
// (Listing.Retry), with why. Either may be nil.
type Sent struct {
	Answered func(outcome string)
	Waiting  func(wait time.Duration, why string)
}

// ErrPageTooLarge is the error of a page of one object over MaxPageBytes:
// that object cannot be listed under the bound.
var ErrPageTooLarge = errors.New("page too large")

// ErrTooLarge is the error of an answer to Get over its bound.
var ErrTooLarge = errors.New("answer too large")

// StatusError is an API server's answer other than 200 OK: its status and
// the start of its body, which for an API error is a Status object whose
// message says what was refused.
type StatusError struct {
	What, Address string
	Code          int
	Status        string
	Body          string
	// Retried is how many times the request was asked for again before
	// this answer (Listing.Retry).
	Retried int
	// get says it answered a Get, which is no listing.
	get bool
}

func (e *StatusError) Error() string {
	if e.get {
		return fmt.Sprintf("the %s at %s answered %s: %s", e.What, e.Address, e.Status, e.Body)
	}
	return fmt.Sprintf("listing %s at %s answered %s: %s", e.What, e.Address, e.Status, e.Body)
}

// pageTooLarge is the error of a page over the bound, ErrPageTooLarge
// with the bound in words. List asks again for a smaller page, so only
// that of a page of one object, which cannot be made smaller, is returned.
type pageTooLarge struct {
	what  string
	bound int64
}

func (e *pageTooLarge) Error() string {
	return fmt.Sprintf("a page of a single one of the %s, the least a listing can ask for, is larger than %d bytes", e.what, e.bound)
}

func (e *pageTooLarge) Is(target error) bool { return target == ErrPageTooLarge }

// tooLarge is the error of an answer to Get over its bound, ErrTooLarge.
type tooLarge struct {
	what  string
	bound int64
}

func (e *tooLarge) Error() string {
	return fmt.Sprintf("the %s answered more than %d bytes", e.what, e.bound)
}

func (e *tooLarge) Is(target error) bool { return target == ErrTooLarge }

// List asks for every page of the listing in turn, handing each page's body
// to page, which returns the page's continue token, "" after the last.
//
// A page over MaxPageBytes is not handed to page, so what page counts, the
// objects and their bytes, counts each object once; the same page is asked
// for again with half the limit. The API server sends whole objects, and
// how large they are is up to the cluster: EndpointSlices of a raised
// --max-endpoints-per-slice, or custom resources with large specs, make a
// page of PageSize larger than a bound that fits the common case, and
// failing such a listing would fail it at every refresh. The smaller limit
// is kept for the rest of the listing, the following objects being likely
// as large, and not remembered for the next: a listing starts at PageSize
// again, so no state outlives it, at the cost of at most seven refused
// requests (100, 50, 25, 12, 6, 3, 1) each read no further than the bound.
// Every request, the ones asked again included, is bounded by ctx.
func (l *Listing) List(ctx context.Context, page func(raw []byte) (next string, err error)) error {
	size := l.PageSize
	if size <= 0 {
		size = PageSize
	}
	next, restarted := "", false
	for {
		raw, err := l.page(ctx, next, size)
		if errors.Is(err, ErrPageTooLarge) && size > 1 {
			size /= 2
			if l.Smaller != nil {
				l.Smaller(size)
			}
			continue
		}
		var status *StatusError
		if l.ResumeExpired && next != "" && errors.As(err, &status) && status.Code == http.StatusGone {
			if token := expiredContinue(status.Body); token != "" {
				next = token
				continue
			}
			if !restarted {
				restarted, next = true, ""
				if l.Restart != nil {
					l.Restart()
				}
				continue
			}
		}
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

// expiredContinue is the continue token of a 410 Expired's Status, with
// which the API server lets a listing whose token it no longer has go on
// with what is there now; "" when it has none.
func expiredContinue(body string) string {
	var status struct {
		Reason   string `json:"reason"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(body), &status) != nil || status.Reason != "Expired" {
		return ""
	}
	return status.Metadata.Continue
}

// page asks for one page of the listing, of at most size objects.
func (l *Listing) page(ctx context.Context, next string, size int) ([]byte, error) {
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
	r := request{client: l.Client, address: l.Server + l.Path, query: query.Encode(), token: l.Token, accept: l.Accept, what: l.What, prepare: l.Prepare, retry: l.Retry}
	return r.get(ctx, l.MaxPageBytes, func() error {
		return &pageTooLarge{what: l.What, bound: l.MaxPageBytes}
	}, func(status string) string {
		return fmt.Sprintf("%s, a page of %d larger than %d bytes", status, size, l.MaxPageBytes)
	})
}

// Get is one GET of a path of an API server, or of a kubelet, that is no
// listing: its body, answered 200 OK and of at most MaxBytes, as a node's
// /stats/summary is asked for.
type Get struct {
	Client *http.Client
	// Address is the URL asked for, Token sent as Lists' is, and What
	// names what answers in errors, such as "kubelet of node a".
	Address, Token, What string
	MaxBytes             int64
	Prepare              func(req *http.Request) (*http.Request, Sent)
}

// Do asks for g's address. An answer over MaxBytes is an error that is
// ErrTooLarge; one other than 200 OK a StatusError.
func (g Get) Do(ctx context.Context) ([]byte, error) {
	r := request{client: g.Client, address: g.Address, token: g.Token, accept: AcceptJSON, what: g.What, prepare: g.Prepare, isGet: true}
	return r.get(ctx, g.MaxBytes, func() error {
		return &tooLarge{what: g.What, bound: g.MaxBytes}
	}, func(status string) string {
		return fmt.Sprintf("%s, larger than %d bytes", status, g.MaxBytes)
	})
}

// request is one GET, asked for again as retry says.
type request struct {
	client                *http.Client
	address, query, token string
	accept, what          string
	prepare               func(req *http.Request) (*http.Request, Sent)
	retry                 func(ctx context.Context, answer *StatusError, header http.Header, retried int) (time.Duration, bool)
	isGet                 bool
}

// statusBodyBytes is how much of an answer other than 200 OK is kept: a
// Status's message, and the continue token of a 410 Expired.
const statusBodyBytes = 4096

func (r request) get(ctx context.Context, maxBytes int64, over func() error, overOutcome func(status string) string) ([]byte, error) {
	address := r.address
	if r.query != "" {
		address += "?" + r.query
	}
	for retried := 0; ; retried++ {
		// The address is the API server's: from the pod's environment,
		// which Kubernetes sets, or from the configuration; or a kubelet's,
		// from the API server's Node.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil) // #nosec G704 -- the cluster's API server
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", cmp.Or(r.accept, AcceptJSON))
		req.Header.Set("User-Agent", UserAgent)
		if r.token != "" {
			req.Header.Set("Authorization", "Bearer "+r.token)
		}
		var sent Sent
		if r.prepare != nil {
			req, sent = r.prepare(req)
		}
		answered := func(outcome string) {
			if sent.Answered != nil {
				sent.Answered(outcome)
			}
		}
		resp, err := r.client.Do(req) // #nosec G704 -- the cluster's API server
		if err != nil {
			answered("error: " + err.Error())
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			answered(resp.Status)
			body, _ := io.ReadAll(io.LimitReader(resp.Body, statusBodyBytes))
			_ = resp.Body.Close()
			status := &StatusError{What: r.what, Address: r.address, Code: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(body)), Retried: retried, get: r.isGet}
			if r.retry == nil {
				return nil, status
			}
			wait, again := r.retry(ctx, status, resp.Header, retried)
			if !again {
				return nil, status
			}
			if sent.Waiting != nil {
				sent.Waiting(wait, resp.Status)
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, fmt.Errorf("%w, while waiting %s to ask again: %w", status, wait, ctx.Err())
			case <-timer.C:
			}
			continue
		}
		// The body is closed without reading it to its end: the rest of an
		// answer over the bound may be of any size, and reading it to reuse
		// the connection could take longer than asking again on another.
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		_ = resp.Body.Close()
		if err != nil {
			answered(resp.Status + ", then the body broke off: " + err.Error())
			return nil, err
		}
		if int64(len(raw)) > maxBytes {
			answered(overOutcome(resp.Status))
			return nil, over()
		}
		answered(resp.Status)
		return raw, nil
	}
}
