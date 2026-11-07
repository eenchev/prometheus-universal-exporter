//go:build !select_request_types || request_type_kubernetes

package fetch

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/kubeapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The kubernetes request type lists Kubernetes objects of one resource,
// built in or custom, and hands them to the transforms as a JSON document,
// so that metrics of the kind kube-state-metrics gives, and of custom
// resources it knows nothing of, come from configuration alone:
//
//	request:
//	  type: kubernetes
//	  group: apps                 # the API group; left out for the core group
//	  version: v1                 # v1 when left out
//	  resource: deployments       # required: the resource's plural name
//	  namespaces: [shop, billing] # the exporter's own namespace when left out
//	  label_selector: app.kubernetes.io/part-of=shop
//
// The objects are those the API server lists: of the namespaces named, of
// the exporter's own namespace when none is, or, with all_namespaces, of
// the whole cluster, which is how a cluster-scoped resource such as nodes
// is listed. The listing is the API's REST one over HTTPS, a page of 100
// objects at a time with the continue token of each, a page over the
// response limit asked for again with half as many, made with the
// standard library alone (internal/kubeapi), as the target allocator lists
// EndpointSlices: there is no client library behind it.
//
// The API server is the one of the cluster the exporter runs in, reached
// with its pod's service account: the address Kubernetes gives every pod,
// the token, read again at every scrape so a rotated one is used, and the
// cluster's CA, which the server's certificate must be signed by. Outside
// a cluster, api_server names it, with tls.ca_file and bearer_token_file,
// or a client certificate, for the credentials, each read again at every
// scrape too.
//
// A collector reads what its configuration names and nothing else: a probe
// takes no target, and a static target names none, so whoever can reach
// /probe cannot have the exporter list what its service account may read
// beyond that. The core group's secrets are refused at load: a metrics
// exporter has no business holding them, and a transform's errors and a
// debug report could show them. Every object's metadata.managedFields,
// large and of no use to a metric, is taken out of the document.
//
// This file imports nothing outside the standard library and the
// repository's own packages.

// KubernetesConfig is a kubernetes collector's own request keys: the
// objects listed, by the plural name of their resource, its API group, ""
// for the core group, and its version, v1 when left out; which of them are
// listed, by namespace and by selector; how many a listing may hold, 0 for
// the default; and the API server's URL when the exporter does not list
// its own cluster's; whether it lists the objects' metadata alone; and
// what each node's kubelet is asked for, of a listing of nodes.
type KubernetesConfig struct {
	Resource      string   `yaml:"resource,omitempty"`
	Group         string   `yaml:"group,omitempty"`
	Version       string   `yaml:"version,omitempty"`
	Namespaces    []string `yaml:"namespaces,omitempty"`
	AllNamespaces bool     `yaml:"all_namespaces,omitempty"`
	LabelSelector string   `yaml:"label_selector,omitempty"`
	FieldSelector string   `yaml:"field_selector,omitempty"`
	MaxObjects    int      `yaml:"max_objects,omitempty"`
	APIServer     string   `yaml:"api_server,omitempty"`
	// MetadataOnly lists the objects' metadata alone, a
	// PartialObjectMetadataList (docs/KUBERNETES.md#metadata-alone).
	MetadataOnly bool `yaml:"metadata_only,omitempty"`
	// Kubelet is what each node's kubelet is asked for, stats/summary, of a
	// listing of nodes (docs/KUBERNETES.md#kubelet-statistics);
	// KubeletViaAPIServer asks it through the API server's proxy of the
	// node, and KubeletCAFile is the CA the kubelets' certificates are
	// verified against when they are asked directly.
	Kubelet             string `yaml:"kubelet,omitempty"`
	KubeletViaAPIServer bool   `yaml:"kubelet_via_api_server,omitempty"`
	KubeletCAFile       string `yaml:"kubelet_ca_file,omitempty"`
}

// kubernetesRequest is a kubernetes collector's own request keys: the zero
// KubernetesConfig of a collector built in code whose request has not been
// validated, which sets them (ValidateRequest).
func kubernetesRequest(c *model.Collector) KubernetesConfig {
	own, _ := c.Request.Own.(KubernetesConfig)
	return own
}

// kubernetesShownQuery says which pairs of a listing's request a debug
// report shows the values of (ConfiguredQuery): limit, the page size the
// exporter asks for, and the selectors as the collector's file writes them.
// continue, the API server's token for the next page, stays masked.
func kubernetesShownQuery(c *model.Collector, name, value string) bool {
	own := kubernetesRequest(c)
	switch name {
	case "limit":
		_, err := strconv.ParseUint(value, 10, 31)
		return err == nil
	case "labelSelector":
		return value == own.LabelSelector && writtenAsSent(c, value, "label_selector")
	case "fieldSelector":
		return value == own.FieldSelector && writtenAsSent(c, value, "field_selector")
	}
	return false
}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeKubernetes,
		Fields:       []string{"tls", "bearer_token_file", "max_response_bytes"},
		Config:       KubernetesConfig{},
		KeyRules:     kubernetesKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    kubernetesRequester{},
		ShownQuery:   kubernetesShownQuery,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		SchemaRule:     kubernetesSchemaRule,
		OptionalTarget: true,
		// A kubernetes collector lists what its configuration names, and
		// takes no target at all.
		NoTarget:   true,
		AnswerHint: "Paste the objects listed as the JSON document the exporter makes of them, as the Response of a run shows it",
		JSONAnswer: true,
		Stage:      "kubernetes",
		CheckFiles: checkKubernetesFiles,
	})
}

// kubernetesRequester is the kubernetes request type's Requester.
type kubernetesRequester struct{}

// Validate checks a kubernetes collector's request and fills in its defaults.
func (kubernetesRequester) Validate(c *model.Collector) error {
	return validateKubernetesRequest(c)
}

// Fetch performs one request of a kubernetes collector.
func (kubernetesRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, forwarded http.Header) (*HTTPResponse, error) {
	return fetchKubernetes(ctx, target, c, overrides, forwarded)
}

// CheckTarget checks a kubernetes collector's target.
func (kubernetesRequester) CheckTarget(c *model.Collector, target string, static bool) error {
	if strings.TrimSpace(target) == "" {
		return nil
	}
	if static {
		return fmt.Errorf("names a target, %q, which a kubernetes collector takes none of: it lists what collector %q's request names; leave target out", target, c.Name)
	}
	return fmt.Errorf("target %q is not taken: a kubernetes collector lists what its configuration names, so a probe of collector %q leaves target out", target, c.Name)
}

// Label is the url label of a kubernetes request.
func (kubernetesRequester) Label(_ string, c *model.Collector, _ RequestOverrides) (string, error) {
	return kubernetesLabel(c), nil
}

// Method is the http_method label of a kubernetes request.
func (kubernetesRequester) Method(*model.Collector, RequestOverrides) string { return kubernetesMethod }

// Display renders a kubernetes target for logs and labels.
func (kubernetesRequester) Display(target string) string {
	return strings.TrimSpace(target)
}

// kubernetesMethod is the http_method label of a listing.
const kubernetesMethod = "LIST"

// kubernetesTimeout bounds a listing with no deadline of its own, as
// dnsLookupTimeout does a lookup.
const kubernetesTimeout = 30 * time.Second

// DefaultKubernetesMaxObjects is how many objects a listing may hold when
// request.max_objects is left out; one more fails the scrape.
const DefaultKubernetesMaxObjects = 10000

// kubernetesServiceAccountDir is where the pod's service account is
// mounted. Tests point it at a directory of their own.
var kubernetesServiceAccountDir = kubeapi.ServiceAccountDir

// kubernetesNamePattern is what a kubernetes collector's request.resource,
// request.version and each of its request.namespaces are made of: a DNS
// label, lower case (RFC 1123), of at most 63 characters, as Kubernetes
// names them. The exporter and the schema hold them to it alike.
const kubernetesNamePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`

// kubernetesGroupPattern is what a kubernetes collector's request.group is
// made of: a DNS subdomain, lower case, of at most 253 characters.
const kubernetesGroupPattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`

var (
	kubernetesName  = regexp.MustCompile(kubernetesNamePattern)
	kubernetesGroup = regexp.MustCompile(kubernetesGroupPattern)
)

func validateKubernetesRequest(x *model.Collector) error {
	r := &x.Request
	own := kubernetesRequest(x)
	switch {
	case own.Resource == "":
		return fmt.Errorf("collector %q has no request.resource; name the plural of the objects to list, such as pods or deployments, as kubectl api-resources lists it", x.Name)
	case strings.Contains(own.Resource, "/"):
		return fmt.Errorf("collector %q request.resource %q names a subresource; a listing is of a resource, such as pods, whose status every object carries", x.Name, own.Resource)
	case len(own.Resource) > 63 || !kubernetesName.MatchString(own.Resource):
		return fmt.Errorf("collector %q request.resource %q is not a resource's plural name, lower case letters, digits and hyphens, such as deployments", x.Name, own.Resource)
	}
	if own.Group == "core" || own.Group == "v1" {
		return fmt.Errorf("collector %q request.group is %q; the core group, of pods, services and nodes, has no name: leave group out", x.Name, own.Group)
	}
	if own.Group != "" && (len(own.Group) > 253 || !kubernetesGroup.MatchString(own.Group)) {
		return fmt.Errorf("collector %q request.group %q is not an API group, such as apps or cert-manager.io; leave it out for the core group", x.Name, own.Group)
	}
	if own.Version != "" && (len(own.Version) > 63 || !kubernetesName.MatchString(own.Version)) {
		return fmt.Errorf("collector %q request.version %q is not an API version, such as v1 or v1beta1", x.Name, own.Version)
	}
	if own.Group == "" && own.Resource == "secrets" && !own.MetadataOnly {
		return fmt.Errorf("collector %q lists secrets whole, which a kubernetes collector may not: their values would pass through the transforms, and could be shown by their errors and by a debug report; set request.metadata_only: true to list their metadata alone, their names, labels and ages", x.Name)
	}
	if err := validateKubelet(x, own); err != nil {
		return err
	}
	if own.AllNamespaces && len(own.Namespaces) > 0 {
		return fmt.Errorf("collector %q sets both request.namespaces and request.all_namespaces; list namespaces, or every namespace, not both", x.Name)
	}
	for i, ns := range own.Namespaces {
		if len(ns) > 63 || !kubernetesName.MatchString(ns) {
			return fmt.Errorf("collector %q request.namespaces entry %q is not a namespace's name", x.Name, ns)
		}
		if slices.Contains(own.Namespaces[:i], ns) {
			return fmt.Errorf("collector %q request.namespaces lists %q twice; its objects would be listed twice", x.Name, ns)
		}
	}
	selectors := map[string]string{"label_selector": own.LabelSelector, "field_selector": own.FieldSelector}
	for _, key := range model.InFileOrder([]string{"label_selector", "field_selector"}, r.Order) {
		if selector := selectors[key]; selector != "" && strings.TrimSpace(selector) == "" {
			return fmt.Errorf("collector %q request.%s is written with blanks alone; leave it out to list every object", x.Name, key)
		}
	}
	if own.MaxObjects < 0 {
		return fmt.Errorf("collector %q request.max_objects must not be negative; leave it out for %d", x.Name, DefaultKubernetesMaxObjects)
	}
	if own.APIServer == "" {
		if r.BearerTokenFile != "" || r.TLS != (model.TLSConfig{}) {
			return fmt.Errorf("collector %q sets request.tls or request.bearer_token_file without request.api_server; the API server of the exporter's own cluster is reached with the pod's service account, its token and CA, and these are for one named by api_server", x.Name)
		}
		return nil
	}
	if _, err := parseKubernetesAPIServer(own.APIServer); err != nil {
		return fmt.Errorf("collector %q request.api_server %w", x.Name, err)
	}
	if r.TLS.InsecureSkipVerify {
		return fmt.Errorf("collector %q sets request.tls.insecure_skip_verify; the API server is sent a token that reads the cluster, so its certificate is always verified: give its CA in tls.ca_file", x.Name)
	}
	if (r.TLS.CertFile == "") != (r.TLS.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	// The debug page's playground makes no request, and may name no file.
	if r.BearerTokenFile == "" && r.TLS.CertFile == "" && !x.RequestNotMade {
		return fmt.Errorf("collector %q names request.api_server with no credential; set bearer_token_file, or tls.cert_file and tls.key_file", x.Name)
	}
	return nil
}

// kubeletSummary is the one request.kubelet there is: a kubelet's Summary
// API.
const kubeletSummary = "stats/summary"

// validateKubelet checks request.kubelet and the keys that go with it.
func validateKubelet(x *model.Collector, own KubernetesConfig) error {
	if own.Kubelet == "" {
		switch {
		case own.KubeletViaAPIServer:
			return fmt.Errorf("collector %q sets request.kubelet_via_api_server without request.kubelet; it says how the nodes' kubelets are asked, which only a collector with kubelet: %s asks", x.Name, kubeletSummary)
		case own.KubeletCAFile != "":
			return fmt.Errorf("collector %q sets request.kubelet_ca_file without request.kubelet; it is the CA of the nodes' kubelets, which only a collector with kubelet: %s asks", x.Name, kubeletSummary)
		}
		return nil
	}
	switch {
	case own.Kubelet != kubeletSummary:
		return fmt.Errorf("collector %q request.kubelet is %q; the kubelet is asked for %s, its Summary API, alone", x.Name, own.Kubelet, kubeletSummary)
	case own.Group != "" || own.Resource != "nodes":
		return fmt.Errorf("collector %q sets request.kubelet with resource %q; the kubelets asked are those of the nodes listed, so list nodes, of the core group", x.Name, own.Resource)
	case !own.AllNamespaces:
		return fmt.Errorf("collector %q sets request.kubelet without request.all_namespaces; nodes are of no namespace, and are listed across the cluster: set all_namespaces: true", x.Name)
	case own.MetadataOnly:
		return fmt.Errorf("collector %q sets both request.kubelet and request.metadata_only; a kubelet is asked at the address its Node's status names, which its metadata does not hold", x.Name)
	case own.KubeletViaAPIServer && own.KubeletCAFile != "":
		return fmt.Errorf("collector %q sets both request.kubelet_via_api_server and request.kubelet_ca_file; through the API server, the kubelets are not reached by the exporter, and their certificates are the API server's to verify", x.Name)
	}
	return nil
}

// parseKubernetesAPIServer reads request.api_server: an https URL, with a
// path when the API server is behind one, such as a proxy's, and nothing
// else. It is returned without a trailing slash.
func parseKubernetesAPIServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return "", fmt.Errorf("%q is not a URL: %w", raw, err)
	case u.Scheme != "https":
		return "", fmt.Errorf("%q is not an https URL; the API server is sent a token, which is never sent in the clear", raw)
	case u.Host == "":
		return "", fmt.Errorf("%q names no host", raw)
	case u.User != nil:
		return "", fmt.Errorf("%q has a user in it; credentials are bearer_token_file's or a client certificate's", raw)
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(u.Host, ":"):
		return "", fmt.Errorf("%q has a query or fragment; give the scheme, host, port and path alone, such as https://k8s.example.com:6443", raw)
	}
	return "https://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// kubernetesVersion is the version of c, v1 when left out.
func kubernetesVersion(c *model.Collector) string {
	if kubernetesRequest(c).Version == "" {
		return "v1"
	}
	return kubernetesRequest(c).Version
}

// kubernetesMaxObjects is the most objects a listing of c may hold.
func kubernetesMaxObjects(c *model.Collector) int {
	if kubernetesRequest(c).MaxObjects == 0 {
		return DefaultKubernetesMaxObjects
	}
	return kubernetesRequest(c).MaxObjects
}

// kubernetesAPIVersion is the group and version as an object's apiVersion
// writes them: v1 for the core group, apps/v1 for apps.
func kubernetesAPIVersion(c *model.Collector) string {
	if kubernetesRequest(c).Group == "" {
		return kubernetesVersion(c)
	}
	return kubernetesRequest(c).Group + "/" + kubernetesVersion(c)
}

// kubernetesPath is the path of the listing of c in namespace, or across
// the cluster for "": /api/v1/... for the core group, /apis/group/version/...
// for the others.
func kubernetesPath(c *model.Collector, namespace string) string {
	prefix := "/apis/" + kubernetesRequest(c).Group + "/" + kubernetesVersion(c)
	if kubernetesRequest(c).Group == "" {
		prefix = "/api/" + kubernetesVersion(c)
	}
	if namespace != "" {
		prefix += "/namespaces/" + url.PathEscape(namespace)
	}
	return prefix + "/" + kubernetesRequest(c).Resource
}

// kubernetesLabel is the url label of a listing: the API server and the
// path of the listing across the cluster, kubernetes: standing for the
// server of the exporter's own cluster. Which namespaces are listed is the
// collector's, as its record type is a dns collector's.
func kubernetesLabel(c *model.Collector) string {
	if kubernetesRequest(c).APIServer == "" {
		return "kubernetes:" + kubernetesPath(c, "")
	}
	server, _ := parseKubernetesAPIServer(kubernetesRequest(c).APIServer)
	return server + kubernetesPath(c, "")
}

// kubernetesConnection is how a scrape reaches the API server: its URL,
// the TLS settings of the client, and the file of the token, "" for none.
type kubernetesConnection struct {
	server    string
	tls       model.TLSConfig
	tokenFile string
}

func kubernetesConnectionOf(c *model.Collector) (kubernetesConnection, error) {
	if kubernetesRequest(c).APIServer != "" {
		server, err := parseKubernetesAPIServer(kubernetesRequest(c).APIServer)
		return kubernetesConnection{server: server, tls: c.Request.TLS, tokenFile: c.Request.BearerTokenFile}, err
	}
	server, err := kubeapi.InClusterServer()
	if err != nil {
		return kubernetesConnection{}, fmt.Errorf("%w: the exporter does not run in a Kubernetes pod, so it cannot reach its cluster's API server; set request.api_server to list another's", err)
	}
	return kubernetesConnection{
		server:    server,
		tls:       model.TLSConfig{CAFile: filepath.Join(kubernetesServiceAccountDir, kubeapi.CAFile)},
		tokenFile: filepath.Join(kubernetesServiceAccountDir, kubeapi.TokenFile),
	}, nil
}

// kubernetesNamespaces are the namespaces a scrape lists, [""] for every
// namespace: those named, or the exporter's own.
func kubernetesNamespaces(c *model.Collector) ([]string, error) {
	switch {
	case kubernetesRequest(c).AllNamespaces:
		return []string{""}, nil
	case len(kubernetesRequest(c).Namespaces) > 0:
		return kubernetesRequest(c).Namespaces, nil
	case kubernetesRequest(c).APIServer != "":
		return nil, fmt.Errorf("collector %q names request.api_server and no namespace; the exporter's own namespace is that of its pod, in its own cluster, so list request.namespaces, or set all_namespaces", c.Name)
	}
	namespace, err := kubeapi.OwnNamespace(kubernetesServiceAccountDir)
	if err != nil {
		return nil, fmt.Errorf("the exporter's own namespace cannot be read from its service account (%w); list request.namespaces", err)
	}
	return []string{namespace}, nil
}

// kubernetesDocument is a listing as the transforms read it.
type kubernetesDocument struct {
	// Kind is the objects' kind, Deployment, and APIVersion their group
	// and version, apps/v1, as the API server named the list.
	Kind       string `json:"kind"`
	APIVersion string `json:"api_version"`
	Count      int    `json:"count"`
	// Items are the objects in the order listed: namespace by namespace,
	// as request.namespaces names them, each as the API server sends it
	// but for its metadata.managedFields.
	Items []json.RawMessage `json:"items"`
}

// kubernetesList is the part of a page the listing reads.
type kubernetesList struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []json.RawMessage `json:"items"`
}

// kubernetesConcurrency is how many of a scrape's requests are sent at
// once: the listings of its namespaces, and with kubelet, the requests of
// its nodes' kubelets. A collector of many namespaces takes about as long
// as its slowest few rather than as all of them added up, and no API
// server or kubelet is asked more of at once than a handful of clients
// would ask.
const kubernetesConcurrency = 4

// kubernetesThrottleRetries is how many times a page the API server's
// priority and fairness refused with 429 is asked for again, each after
// the wait its Retry-After asks for, within the scrape's deadline.
const kubernetesThrottleRetries = 3

// kubernetesThrottleWait is the wait before asking again for a page
// answered 429 without a Retry-After the exporter can read.
const kubernetesThrottleWait = time.Second

// lastAppliedAnnotation is kubectl apply's copy of the configuration it
// applied, which of a Secret holds its values; a listing of metadata alone
// takes it out of every object.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// fetchKubernetes lists a scrape's objects and answers them as JSON.
func fetchKubernetes(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	if strings.TrimSpace(target) != "" {
		return nil, fmt.Errorf("target %q is not taken: a kubernetes collector lists what its configuration names", target)
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, kubernetesTimeout)
		defer cancel()
	}
	conn, err := kubernetesConnectionOf(c)
	if err != nil {
		return nil, err
	}
	namespaces, err := kubernetesNamespaces(c)
	if err != nil {
		return nil, err
	}
	token := ""
	if conn.tokenFile != "" {
		if token, err = kubeapi.ReadToken(conn.tokenFile); err != nil {
			return nil, fmt.Errorf("the token to list %s with: %w", kubernetesRequest(c).Resource, err)
		}
	}
	// The deadline of the scrape bounds the listing, every page of it.
	client, err := HTTPClient(TransportSettings{TLS: conn.tls}, false, 0)
	if err != nil {
		return nil, fmt.Errorf("the API server's CA or the client certificate: %w", err)
	}
	limit := responseLimit(c)
	l := &kubernetesListing{c: c, conn: conn, client: client, token: token, limit: limit, maxObjects: kubernetesMaxObjects(c), items: make([][]json.RawMessage, len(namespaces))}
	err = kubernetesEach(ctx, len(namespaces), func(ctx context.Context, i int) error {
		if err := l.list(ctx, i, namespaces[i]); err != nil {
			return kubernetesFailure(ctx, c, namespaces[i], err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var body []byte
	if kubernetesRequest(c).Kubelet != "" {
		body, err = l.kubeletDocument(ctx)
	} else {
		body, err = l.document()
	}
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, model.MarkError(fmt.Errorf("the document of the listing of %s is %d bytes, more than the response limit, %d bytes; narrow the listing, or raise max_response_bytes", kubernetesRequest(c).Resource, len(body), limit), model.ErrLimitExceeded)
	}
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
		Target:     target,
		Collector:  c.Name,
		Duration:   time.Since(start),
	}, nil
}

// kubernetesEach runs do for 0 to n-1, kubernetesConcurrency at a time. The
// first error ends the others, whose context it cancels, and is the one
// returned.
func kubernetesEach(ctx context.Context, n int, do func(ctx context.Context, i int) error) error {
	if n == 1 {
		return do(ctx, 0)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		first error
		wg    sync.WaitGroup
	)
	turns := make(chan struct{}, kubernetesConcurrency)
	started := 0
	for i := range n {
		select {
		case turns <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		started++
		go func() {
			defer wg.Done()
			defer func() { <-turns }()
			if err := do(ctx, i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if first == nil && started < n {
		// The scrape's own context ended before every one was begun.
		return context.Cause(ctx)
	}
	return first
}

// kubernetesListing is what a scrape's listings gather, from each
// namespace at once: the objects of each, in the order request.namespaces
// names the namespaces, and, across them, the count and the bytes, which
// max_objects and the response limit bound.
type kubernetesListing struct {
	c                *model.Collector
	conn             kubernetesConnection
	client           *http.Client
	token            string
	limit            int64
	maxObjects       int
	mu               sync.Mutex
	items            [][]json.RawMessage
	count            int
	total            int64
	kind, apiVersion string
}

// list lists namespace, the i-th, into l.items[i].
func (l *kubernetesListing) list(ctx context.Context, i int, namespace string) error {
	own := kubernetesRequest(l.c)
	listing := kubeapi.Listing{
		Client: l.client, Server: l.conn.server, Path: kubernetesPath(l.c, namespace), Token: l.token, What: own.Resource,
		LabelSelector: own.LabelSelector, FieldSelector: own.FieldSelector, MaxPageBytes: l.limit,
		Prepare: kubernetesPrepare(ctx), Retry: kubernetesRetry, ResumeExpired: true,
		// A listing begun again puts away what it had listed.
		Restart: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			for _, item := range l.items[i] {
				l.count--
				l.total -= int64(len(item)) + 1
			}
			l.items[i] = nil
		},
	}
	if own.MetadataOnly {
		listing.Accept = kubeapi.AcceptMetadataList
	}
	return listing.List(ctx, func(raw []byte) (string, error) {
		var page kubernetesList
		if err := json.Unmarshal(raw, &page); err != nil {
			// The API server's address is the operator's, as below.
			return "", model.WithOperatorDetail(
				fmt.Errorf("listing %s at %s: the answer is not a list: %w", own.Resource, l.conn.server+listing.Path, err),
				fmt.Sprintf("listing %s: the answer is not a list: %s", own.Resource, err))
		}
		// A listing of metadata alone is never taken as anything else: a
		// server, or what stands before it, that answers the objects
		// whole would hand a Secret's values to the transforms.
		if own.MetadataOnly && page.Kind != "PartialObjectMetadataList" {
			return "", fmt.Errorf("listing %s: the API server answered a %s, not the PartialObjectMetadataList of their metadata alone asked for", own.Resource, cmp.Or(page.Kind, "list of no kind"))
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.kind, l.apiVersion = strings.TrimSuffix(page.Kind, "List"), page.APIVersion
		for _, item := range page.Items {
			if l.count++; l.count > l.maxObjects {
				return "", model.MarkError(fmt.Errorf("the listing of %s holds more than the %d objects request.max_objects allows; narrow it with namespaces, label_selector or field_selector, or raise max_objects", own.Resource, l.maxObjects), model.ErrLimitExceeded)
			}
			item, err := trimObject(item, own.MetadataOnly)
			if err != nil {
				return "", fmt.Errorf("listing %s: an object is not a JSON object: %w", own.Resource, err)
			}
			if l.total += int64(len(item)) + 1; l.total > l.limit {
				return "", model.MarkError(fmt.Errorf("the objects of the listing of %s come to more than the response limit, %d bytes; narrow the listing, or raise max_response_bytes", own.Resource, l.limit), model.ErrLimitExceeded)
			}
			l.items[i] = append(l.items[i], item)
		}
		return page.Metadata.Continue, nil
	})
}

// objects are the objects listed, namespace by namespace.
func (l *kubernetesListing) objects() []json.RawMessage {
	out := []json.RawMessage{}
	for _, items := range l.items {
		out = append(out, items...)
	}
	return out
}

// document is the listing as the transforms read it.
func (l *kubernetesListing) document() ([]byte, error) {
	doc := kubernetesDocument{Kind: l.kind, APIVersion: l.apiVersion, Items: l.objects()}
	doc.Count = len(doc.Items)
	switch {
	case kubernetesRequest(l.c).MetadataOnly:
		// What the API server names a list of metadata, whether or not it
		// had objects to list.
		doc.Kind, doc.APIVersion = "PartialObjectMetadata", "meta.k8s.io/v1"
	case doc.Kind == "":
		doc.APIVersion = kubernetesAPIVersion(l.c)
	}
	return json.Marshal(doc)
}

// kubernetesPrepare records each request of a scrape in its trace, when it
// has one, at its own place (tracedAt), since the requests of a scrape are
// sent at once; sends the trace context with it under
// otlp.traces.propagate; and, of an exported trace, times its phases for
// its span (TracedRequest.Phases).
func kubernetesPrepare(ctx context.Context) func(req *http.Request) (*http.Request, kubeapi.Sent) {
	return func(req *http.Request) (*http.Request, kubeapi.Sent) {
		// With otlp.traces.propagate, each request carries the trace
		// context as an HTTP target's does: the API server takes a
		// traceparent (its APIServerTracing), and its spans are then
		// children of the request's. No header of the request is the
		// operator's, so there is none to leave as it is.
		span := propagateTrace(ctx, req.Header, false)
		at := traceRequestAt(ctx, req.Method, req.URL.String(), req.Header, "", false)
		at.spanID(span)
		var trip *tripTrace
		if at.t != nil && spanTraced(ctx) {
			trip = newTripTrace(ctx, time.Now())
			trip.at = &at
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trip.hooks()))
		}
		return req, kubeapi.Sent{
			// kubeapi tells a request that got no answer by this
			// beginning of its own.
			Answered: func(outcome string) {
				failed := strings.HasPrefix(outcome, "error: ")
				if failed {
					trip.recordPhases()
				}
				at.outcome(outcome, failed)
			},
			Waiting: func(wait time.Duration, why string) {
				at.wait(fmt.Sprintf("then waited %s, as the answer's Retry-After asked", shownWait(wait)))
			},
		}
	}
}

// kubernetesRetry asks again for a page the API server's priority and
// fairness refused with 429 Too Many Requests: after the wait its
// Retry-After asks for, a second when it has none the exporter can read,
// at most kubernetesThrottleRetries times, and not when the wait would end
// after the scrape's deadline.
func kubernetesRetry(ctx context.Context, answer *kubeapi.StatusError, header http.Header, retried int) (time.Duration, bool) {
	if answer.Code != http.StatusTooManyRequests || retried >= kubernetesThrottleRetries {
		return 0, false
	}
	wait := kubernetesThrottleWait
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if asked, ok := parseRetryAfter(value, time.Now()); ok {
			wait = asked
		}
	}
	if deadline, has := ctx.Deadline(); has && time.Until(deadline) <= wait {
		return 0, false
	}
	return wait, true
}

// trimObject is an object without its metadata.managedFields, which say
// which client set which field: often the largest part of an object, and
// of no use to a metric; and, of a listing of metadata alone, with its
// kind, apiVersion and metadata alone, whatever else an answer carried,
// and without kubectl apply's copy of what it applied, which of a Secret
// holds its values. The rest is kept as it was sent.
func trimObject(item json.RawMessage, metadataOnly bool) (json.RawMessage, error) {
	if !metadataOnly && !bytes.Contains(item, []byte(`"managedFields"`)) {
		return item, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(item, &object); err != nil {
		return nil, err
	}
	changed := false
	if metadataOnly {
		for key := range object {
			if key != "kind" && key != "apiVersion" && key != "metadata" {
				delete(object, key)
				changed = true
			}
		}
	}
	// A metadata that is missing or no object has nothing to take out, and
	// the object is left as it was sent.
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	if metadata["managedFields"] != nil {
		delete(metadata, "managedFields")
		changed = true
	}
	if metadataOnly && metadata["annotations"] != nil {
		var annotations map[string]json.RawMessage
		if json.Unmarshal(metadata["annotations"], &annotations) == nil && annotations[lastAppliedAnnotation] != nil {
			delete(annotations, lastAppliedAnnotation)
			raw, err := json.Marshal(annotations)
			if err != nil {
				return nil, err
			}
			metadata["annotations"], changed = raw, true
		}
	}
	if !changed {
		return item, nil
	}
	if metadata != nil {
		raw, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		object["metadata"] = raw
	}
	return json.Marshal(object)
}

// The kubelet of each node of a collector with request.kubelet answers what
// no object of the API holds: what each pod, container and volume uses of
// the node's CPU, memory and file systems, ephemeral storage among them, as
// its Summary API, /stats/summary, has it. The collector lists the nodes,
// as its keys name them, and asks each node's kubelet, kubernetesConcurrency
// at a time: at the address of its Node, InternalIP first, and the port
// the Node says its kubelet serves on, with the token the listing is made
// with, the kubelet's certificate verified against kubelet_ca_file, the
// cluster's CA when it is left out; or, with kubelet_via_api_server,
// through the API server's proxy of the node, for a cluster whose kubelets'
// certificates no CA the exporter has signs. The first needs get on
// nodes/stats, the second get on nodes/proxy, which lets its holder do
// all a kubelet does, so the first is the one to choose where it works.

// kubeletSummaryVersion is the version of the kubelet's Summary API, as the
// document of a collector with request.kubelet names it.
const kubeletSummaryVersion = "stats/v1alpha1"

// kubeletDocument is the document of a collector with request.kubelet: each
// node's summary, as its kubelet answered it, in the order the nodes were
// listed, and the nodes whose kubelet answered none, and why.
type kubeletDocument struct {
	Kind       string            `json:"kind"`
	APIVersion string            `json:"api_version"`
	Count      int               `json:"count"`
	Items      []json.RawMessage `json:"items"`
	Failed     []kubeletFailure  `json:"failed"`
}

// kubeletFailure is a node whose kubelet answered no summary. Error says
// why in the words a probe's caller is shown, which name no address.
type kubeletFailure struct {
	Node  string `json:"node"`
	Error string `json:"error"`
}

// kubeletNode is what the request of a node's kubelet needs of its Node.
type kubeletNode struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
		DaemonEndpoints struct {
			KubeletEndpoint struct {
				Port int `json:"Port"`
			} `json:"kubeletEndpoint"`
		} `json:"daemonEndpoints"`
	} `json:"status"`
}

// kubeletAddressTypes are the addresses of a Node a kubelet is asked at, in
// the order they are tried: the one inside the cluster first, as the API
// server's own --kubelet-preferred-address-types has it by default.
var kubeletAddressTypes = []string{"InternalIP", "Hostname", "ExternalIP"}

// kubeletDefaultPort is the kubelet's port when its Node names none.
const kubeletDefaultPort = 10250

// address is where n's kubelet is asked directly, "" when the Node has no
// address to ask it at.
func (n kubeletNode) address() string {
	for _, kind := range kubeletAddressTypes {
		for _, a := range n.Status.Addresses {
			if a.Type == kind && a.Address != "" {
				port := n.Status.DaemonEndpoints.KubeletEndpoint.Port
				if port <= 0 || port > 65535 {
					port = kubeletDefaultPort
				}
				return "https://" + net.JoinHostPort(a.Address, strconv.Itoa(port))
			}
		}
	}
	return ""
}

// kubeletDocument asks the kubelet of every node listed for its summary.
// The nodes whose kubelet answered none are in the document's failed, so
// one node down leaves the others' metrics; when every kubelet fails, the
// scrape fails as the first did.
func (l *kubernetesListing) kubeletDocument(ctx context.Context) ([]byte, error) {
	own := kubernetesRequest(l.c)
	objects := l.objects()
	nodes := make([]kubeletNode, len(objects))
	for i, raw := range objects {
		if err := json.Unmarshal(raw, &nodes[i]); err != nil {
			return nil, fmt.Errorf("listing nodes: a node is not a Node: %w", err)
		}
	}
	client := l.client
	if !own.KubeletViaAPIServer {
		var err error
		if client, err = HTTPClient(TransportSettings{TLS: kubeletTLS(l.c, l.conn)}, false, 0); err != nil {
			return nil, fmt.Errorf("the kubelets' CA or the client certificate: %w", err)
		}
	}
	// A kubelet that does not answer, a node gone dark, is given up on a
	// little before the scrape's deadline, so the others' summaries are
	// answered with it among the failed, rather than the scrape failing
	// at its deadline for one node.
	asking, stop := context.WithDeadline(ctx, kubeletDeadline(ctx))
	defer stop()
	summaries := make([]json.RawMessage, len(nodes))
	failures := make([]error, len(nodes))
	var total int64
	var mu sync.Mutex
	err := kubernetesEach(ctx, len(nodes), func(ctx context.Context, i int) error {
		node := nodes[i]
		get := kubeapi.Get{Client: client, Token: l.token, What: "kubelet of node " + node.Metadata.Name, MaxBytes: l.limit, Prepare: kubernetesPrepare(asking)}
		if own.KubeletViaAPIServer {
			get.Address = l.conn.server + "/api/v1/nodes/" + url.PathEscape(node.Metadata.Name) + "/proxy/" + own.Kubelet
		} else if address := node.address(); address != "" {
			get.Address = address + "/" + own.Kubelet
		} else {
			failures[i] = errKubeletNoAddress
			return nil
		}
		raw, err := get.Do(asking)
		if err == nil && !json.Valid(raw) {
			err = errKubeletNotJSON
		}
		if err != nil {
			failures[i] = err
			// The scrape's own end, or a sibling's limit, ends them all:
			// what failed is the scrape, not the node.
			if ctx.Err() != nil {
				return err
			}
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if total += int64(len(raw)) + 1; total > l.limit {
			return model.MarkError(fmt.Errorf("the summaries of the nodes' kubelets come to more than the response limit, %d bytes; narrow the nodes with label_selector or field_selector, or raise max_response_bytes", l.limit), model.ErrLimitExceeded)
		}
		summaries[i] = raw
		return nil
	})
	if err != nil {
		if errors.Is(err, model.ErrLimitExceeded) {
			return nil, err
		}
		// The scrape ended before the kubelets were asked: its caller
		// left, or its time ran out before the kubelets' deadline.
		return nil, model.WithOperatorDetail(fmt.Errorf("asking the nodes' kubelets for their %s: %w", own.Kubelet, err),
			fmt.Sprintf("asking the nodes' kubelets for their %s: %s", own.Kubelet, context.Cause(ctx)))
	}
	doc := kubeletDocument{Kind: "Summary", APIVersion: kubeletSummaryVersion, Items: []json.RawMessage{}, Failed: []kubeletFailure{}}
	for i, node := range nodes {
		if failures[i] == nil && summaries[i] == nil {
			// None is left unasked but by the scrape's end, which fails it
			// above; this is that rule kept.
			failures[i] = context.DeadlineExceeded
		}
		if failures[i] != nil {
			doc.Failed = append(doc.Failed, kubeletFailure{Node: node.Metadata.Name, Error: kubeletShown(failures[i])})
			continue
		}
		doc.Items = append(doc.Items, summaries[i])
	}
	doc.Count = len(doc.Items)
	if len(nodes) > 0 && len(doc.Items) == 0 {
		return nil, kubeletFailed(ctx, own, failures[0])
	}
	return json.Marshal(doc)
}

// kubeletDeadline is when a scrape gives up on the kubelets that have not
// answered: a tenth of its time left before its deadline, at most a
// second, so the document of those that did is answered in time.
func kubeletDeadline(ctx context.Context) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(kubernetesTimeout)
	}
	return deadline.Add(-min(time.Until(deadline)/10, time.Second))
}

var (
	errKubeletNoAddress = errors.New("its Node has no InternalIP, Hostname or ExternalIP address to ask its kubelet at")
	errKubeletNotJSON   = errors.New("its kubelet's answer is not JSON")
)

// kubeletTLS is how a kubelet is reached directly: the listing's client
// certificate, if any, and kubelet_ca_file, or the CA the API server is
// verified against when it is left out.
func kubeletTLS(c *model.Collector, conn kubernetesConnection) model.TLSConfig {
	settings := conn.tls
	if file := kubernetesRequest(c).KubeletCAFile; file != "" {
		settings.CAFile = file
	}
	return settings
}

// kubeletShown is why a node's kubelet answered no summary, as the document
// shows it: its status, or that the request failed and, when it did,
// that the scrape's time ran out; never the address, which is the
// operator's.
func kubeletShown(err error) string {
	var status *kubeapi.StatusError
	switch {
	case errors.As(err, &status):
		return "the kubelet answered " + status.Status
	case errors.Is(err, kubeapi.ErrTooLarge):
		return "the kubelet's summary is larger than the response limit"
	case errors.Is(err, errKubeletNoAddress), errors.Is(err, errKubeletNotJSON):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the request to the kubelet failed: " + context.DeadlineExceeded.Error()
	}
	return "the request to the kubelet failed"
}

// kubeletFailed words the failure of a scrape no kubelet answered: what
// the first said, which is the operator's, and what to do about it.
func kubeletFailed(ctx context.Context, own KubernetesConfig, err error) error {
	shown := kubeletShown(err)
	hint := ""
	var status *kubeapi.StatusError
	if errors.As(err, &status) {
		switch {
		case status.Code == http.StatusForbidden && own.KubeletViaAPIServer:
			hint = "; the exporter's service account may not ask the kubelets through the API server: grant it get on nodes/proxy, with the chart's kubernetesAccess.kubelet: proxy (docs/KUBERNETES.md#kubelet-statistics)"
		case status.Code == http.StatusForbidden:
			hint = "; the exporter's service account may not read the kubelets' statistics: grant it get on nodes/stats, with the chart's kubernetesAccess.kubelet: stats (docs/KUBERNETES.md#kubelet-statistics)"
		case status.Code == http.StatusUnauthorized:
			hint = "; the kubelets do not take the token: their webhook authentication may be off, which a kubelet of --authentication-token-webhook=false has"
		}
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		hint = "; no CA the exporter has signs the kubelets' certificates: give theirs in request.kubelet_ca_file, or set kubelet_via_api_server (docs/KUBERNETES.md#kubelet-statistics)"
	}
	failure := fmt.Errorf("no node's kubelet answered its %s; the first: %w%s", own.Kubelet, err, hint)
	if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
		failure = fmt.Errorf("%w: %w", failure, ctx.Err())
	}
	return model.WithOperatorDetail(failure, "no node's kubelet answered its "+own.Kubelet+"; the first: "+shown+hint)
}

// kubernetesFailure words a failed listing: an API server's refusal with
// what to do about it, the commonest being a permission the exporter's
// service account lacks; a listing cut short by the scrape's deadline
// wraps the context's error.
func kubernetesFailure(ctx context.Context, c *model.Collector, namespace string, err error) error {
	var status *kubeapi.StatusError
	if !errors.As(err, &status) {
		if errors.Is(err, kubeapi.ErrPageTooLarge) {
			return model.MarkError(fmt.Errorf("%w, the response limit; narrow the listing, or raise max_response_bytes", err), model.ErrLimitExceeded)
		}
		// A request that failed names the API server's URL, which is the
		// operator's — the cluster's own address, or api_server's — as is
		// the address the exporter connected from: a probe's caller is told
		// the request failed, and whether the scrape's deadline ended it.
		var request *url.Error
		if errors.As(err, &request) {
			ended := ""
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				ended = ": " + context.DeadlineExceeded.Error()
			case errors.Is(err, context.Canceled):
				ended = ": " + context.Canceled.Error()
			}
			err = model.WithOperatorDetail(err, fmt.Sprintf("listing %s: the request to the API server failed%s; why is in the exporter's log", kubernetesRequest(c).Resource, ended))
		}
		if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
			return fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return err
	}
	where := "across the cluster"
	if namespace != "" {
		where = "in the namespace " + namespace
	}
	message := status.Body
	var apiStatus struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(status.Body), &apiStatus) == nil && apiStatus.Message != "" {
		message = apiStatus.Message
	}
	// What the API server said is the operator's: it names the service
	// account, the namespaces and the resources it may not list, and of a
	// body that is no Status, anything a proxy before it answered. A probe's
	// caller is told the listing, the status and what to do about it.
	listing := fmt.Sprintf("listing %s (%s) %s answered %s", kubernetesRequest(c).Resource, kubernetesAPIVersion(c), where, status.Status)
	what, told := listing+": "+message, listing+"; what it said is in the exporter's log"
	var hint string
	switch status.Code {
	case http.StatusUnauthorized:
		hint = "; the API server does not take the token: the service account's, or bearer_token_file's with api_server, is missing, expired or of another cluster"
	case http.StatusForbidden:
		hint = fmt.Sprintf("; the exporter's service account may not list them: grant it list on %s in group %q, with the chart's kubernetesAccess.rules (docs/KUBERNETES.md#permissions)", kubernetesRequest(c).Resource, kubernetesRequest(c).Group)
	case http.StatusTooManyRequests:
		hint = fmt.Sprintf("; its priority and fairness had no seat for the listing, which was asked for again %d times, each after the wait its Retry-After asked for, and is not when that wait would end after the scrape's deadline: the API server is busy, or the exporter's FlowSchema gets it too few seats", status.Retried)
	case http.StatusGone:
		hint = "; the listing's continue token expired, and so did it again when the listing started over: the objects change faster than they can be listed, so narrow the listing with label_selector or field_selector"
	case http.StatusNotAcceptable:
		hint = "; the API server does not answer the metadata alone of the objects, a PartialObjectMetadataList, which every API server since Kubernetes 1.15 answers: a proxy before it may refuse the Accept"
	case http.StatusNotFound:
		hint = "; the API server has no such resource: check resource, its plural name, group and version against kubectl api-resources, and that a custom resource's definition is installed; a cluster-scoped resource, such as nodes, is listed with all_namespaces: true"
	}
	if message == "" {
		return errors.New(listing + hint)
	}
	return model.WithOperatorDetail(errors.New(what+hint), told+hint)
}

// checkKubernetesFiles reads the files a collector reaches what it asks
// with, as a scrape reads them, for --dry-run: kubelet_ca_file, and of a
// collector naming api_server, the token file and the tls block's. Those of the exporter's own service account are its
// pod's, which a check run elsewhere does not have, and are not read.
func checkKubernetesFiles(c *model.Collector, t *model.StaticTarget) []string {
	if t != nil {
		return nil
	}
	var warnings []string
	if file := kubernetesRequest(c).KubeletCAFile; file != "" {
		if _, err := tlsConfig(model.TLSConfig{CAFile: file}); err != nil {
			warnings = append(warnings, fmt.Sprintf("collector %q: request.kubelet_ca_file: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", c.Name, err))
		}
	}
	if kubernetesRequest(c).APIServer == "" {
		return warnings
	}
	if c.Request.BearerTokenFile != "" {
		if _, err := kubeapi.ReadToken(c.Request.BearerTokenFile); err != nil {
			warnings = append(warnings, fmt.Sprintf("collector %q: request.bearer_token_file: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", c.Name, err))
		}
	}
	if _, err := tlsConfig(c.Request.TLS); err != nil {
		warnings = append(warnings, fmt.Sprintf("collector %q: request.tls: %v; a scrape would fail on it now (the files are read at each scrape, so one put in place later is used then)", c.Name, err))
	}
	return warnings
}

// kubernetesSchemaRule is what a kubernetes collector's request is held to, as
// the exporter holds it (validateKubernetesRequest): resource,
// written with something other than "", and not the core group's secrets
// but with metadata_only; kubelet only of the core group's nodes, with
// all_namespaces and without metadata_only, and kubelet_via_api_server and
// kubelet_ca_file only beside it, not both;
// namespaces or all_namespaces, not both; bearer_token_file only beside
// api_server, and beside api_server a credential, bearer_token_file or
// tls.cert_file, and no insecure_skip_verify true. "" is a key left out
// to the exporter, so each is "written" when it is something else
// (SchemaWrittenKey).
func kubernetesSchemaRule() map[string]any {
	rule := SchemaWrittenKey("resource")
	coreGroup := map[string]any{"anyOf": []any{map[string]any{"not": map[string]any{"required": []string{"group"}}}, map[string]any{"properties": map[string]any{"group": map[string]any{"const": ""}}}}}
	rule["allOf"] = []any{
		map[string]any{"if": map[string]any{"allOf": []any{coreGroup, map[string]any{"properties": map[string]any{"resource": map[string]any{"const": "secrets"}}}}}, "then": map[string]any{"properties": map[string]any{"metadata_only": map[string]any{"const": true}}, "required": []string{"metadata_only"}}},
		map[string]any{"if": SchemaWrittenKey("kubelet"), "then": map[string]any{
			"allOf": []any{coreGroup},
			"properties": map[string]any{
				"resource": map[string]any{"const": "nodes"}, "all_namespaces": map[string]any{"const": true}, "metadata_only": map[string]any{"not": map[string]any{"const": true}},
			},
			"required": []string{"all_namespaces"},
			"not":      map[string]any{"allOf": []any{map[string]any{"properties": map[string]any{"kubelet_via_api_server": map[string]any{"const": true}}, "required": []string{"kubelet_via_api_server"}}, SchemaWrittenKey("kubelet_ca_file")}},
		}, "else": map[string]any{
			"properties": map[string]any{"kubelet_via_api_server": map[string]any{"not": map[string]any{"const": true}}},
			"not":        SchemaWrittenKey("kubelet_ca_file"),
		}},
		map[string]any{
			"if":   map[string]any{"properties": map[string]any{"all_namespaces": map[string]any{"const": true}}, "required": []string{"all_namespaces"}},
			"then": map[string]any{"not": map[string]any{"properties": map[string]any{"namespaces": map[string]any{"minItems": 1}}, "required": []string{"namespaces"}}},
		},
		map[string]any{"if": map[string]any{"not": SchemaWrittenKey("api_server")}, "then": map[string]any{"not": SchemaWrittenKey("bearer_token_file")}},
		map[string]any{"if": SchemaWrittenKey("api_server"), "then": map[string]any{
			"anyOf":      []any{SchemaWrittenKey("bearer_token_file"), map[string]any{"properties": map[string]any{"tls": SchemaWrittenKey("cert_file")}, "required": []string{"tls"}}},
			"properties": map[string]any{"tls": map[string]any{"properties": map[string]any{"insecure_skip_verify": map[string]any{"not": map[string]any{"const": true}}}}},
		}},
	}
	return rule
}

// kubernetesKeyRules is what the configuration schema says of a kubernetes
// collector's own keys.
func kubernetesKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"resource":               {"type": "string", "pattern": SchemaOptionalPattern(kubernetesNamePattern), "maxLength": 63, "description": "kubernetes, required: the plural name of the resource listed, as kubectl api-resources lists it, such as pods, deployments or certificates. The core group's secrets are refused. See docs/KUBERNETES.md."},
		"group":                  {"type": "string", "pattern": SchemaOptionalPattern(kubernetesGroupPattern), "maxLength": 253, "not": map[string]any{"enum": []string{"core", "v1"}}, "description": "kubernetes: the resource's API group, such as apps or cert-manager.io; left out for the core group, of pods, services and nodes, which has no name."},
		"version":                {"type": "string", "pattern": SchemaOptionalPattern(kubernetesNamePattern), "maxLength": 63, "description": "kubernetes: the resource's API version, such as v1 or v1beta1. Defaults to v1."},
		"namespaces":             {"items": map[string]any{"type": "string", "pattern": kubernetesNamePattern, "maxLength": 63}, "description": "kubernetes: the namespaces listed, each at its own path, in this order. Left out, with all_namespaces false, the exporter's own namespace, from its service account."},
		"all_namespaces":         {"description": "kubernetes: list the resource across the cluster, every namespace's objects, or a cluster-scoped resource's, such as nodes. Not with namespaces. Defaults to false."},
		"label_selector":         {"not": SchemaOnlyBlanks(), "description": "kubernetes: the label selector sent with the listing, such as app.kubernetes.io/part-of=shop,tier!=cache; every object when left out."},
		"field_selector":         {"not": SchemaOnlyBlanks(), "description": "kubernetes: the field selector sent with the listing, such as status.phase=Running, of the fields the resource can be selected by; every object when left out."},
		"max_objects":            {"minimum": 0, "description": "kubernetes: the most objects a listing may hold; one more fails the scrape. Defaults to 10000."},
		"metadata_only":          {"description": "kubernetes: list the objects' metadata alone, their names, namespaces, labels, annotations and times, as a PartialObjectMetadataList, without kubectl's last-applied-configuration annotation: smaller pages, and the one way to list the core group's secrets. Not with kubelet. Defaults to false. See docs/KUBERNETES.md#metadata-alone."},
		"kubelet":                {"enum": []string{"", kubeletSummary}, "description": "kubernetes: with resource nodes, all_namespaces and the core group, ask each node's kubelet for its Summary API, stats/summary, what each pod, container and volume uses of CPU, memory and file systems, ephemeral storage among them; the document is the summaries, and the nodes whose kubelet answered none. Needs get on nodes/stats (nodes/proxy with kubelet_via_api_server). See docs/KUBERNETES.md#kubelet-statistics."},
		"kubelet_via_api_server": {"description": "kubernetes, with kubelet: ask the kubelets through the API server's proxy of each node, for a cluster whose kubelets' certificates no CA the exporter has signs; needs get on nodes/proxy, which lets its holder do all a kubelet does. Not with kubelet_ca_file. Defaults to false."},
		"kubelet_ca_file":        {"type": "string", "description": "kubernetes, with kubelet: the CA the kubelets' serving certificates are verified against when each is asked directly, read at every scrape; the CA the API server is verified against when left out, the cluster's in its own cluster."},
		"api_server":             {"type": "string", "pattern": SchemaOptionalPattern(`^https://[^/?#@]+(/[^?#]*)?$`), "description": "kubernetes: the URL of an API server other than the one of the cluster the exporter runs in, https, with a path when it is behind one; reached with tls.ca_file and bearer_token_file, or tls.cert_file and tls.key_file, each read at every scrape. Left out, the exporter's own cluster's, with its pod's service account. See docs/KUBERNETES.md#outside-the-cluster."},
	}
}
