//go:build !select_request_types || request_type_kubernetes

package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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

func init() {
	registerRequestType(&RequestType{
		Name:           RequestTypeKubernetes,
		Fields:         []string{"resource", "group", "version", "namespaces", "all_namespaces", "label_selector", "field_selector", "max_objects", "api_server", "tls", "bearer_token_file", "max_response_bytes"},
		Overrides:      []string{"timeout", PathParamPrefix},
		TargetFields:   []string{"timeout"},
		Validate:       validateKubernetesRequest,
		Fetch:          fetchKubernetes,
		OptionalTarget: true,
		CheckTarget: func(c *model.Collector, target string, static bool) error {
			if strings.TrimSpace(target) == "" {
				return nil
			}
			if static {
				return fmt.Errorf("names a target, %q, which a kubernetes collector takes none of: it lists what collector %q's request names; leave target out", target, c.Name)
			}
			return fmt.Errorf("target %q is not taken: a kubernetes collector lists what its configuration names, so a probe of collector %q leaves target out", target, c.Name)
		},
		Label: func(_ string, c *model.Collector, _ RequestOverrides) (string, error) {
			return kubernetesLabel(c), nil
		},
		Method:     func(*model.Collector, RequestOverrides) string { return kubernetesMethod },
		Display:    strings.TrimSpace,
		Stage:      "kubernetes",
		CheckFiles: checkKubernetesFiles,
	})
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

var (
	kubernetesName  = regexp.MustCompile(model.KubernetesNamePattern)
	kubernetesGroup = regexp.MustCompile(model.KubernetesGroupPattern)
)

func validateKubernetesRequest(x *model.Collector) error {
	r := &x.Request
	switch {
	case r.Resource == "":
		return fmt.Errorf("collector %q has no request.resource; name the plural of the objects to list, such as pods or deployments, as kubectl api-resources lists it", x.Name)
	case strings.Contains(r.Resource, "/"):
		return fmt.Errorf("collector %q request.resource %q names a subresource; a listing is of a resource, such as pods, whose status every object carries", x.Name, r.Resource)
	case len(r.Resource) > 63 || !kubernetesName.MatchString(r.Resource):
		return fmt.Errorf("collector %q request.resource %q is not a resource's plural name, lower case letters, digits and hyphens, such as deployments", x.Name, r.Resource)
	}
	if r.Group == "core" || r.Group == "v1" {
		return fmt.Errorf("collector %q request.group is %q; the core group, of pods, services and nodes, has no name: leave group out", x.Name, r.Group)
	}
	if r.Group != "" && (len(r.Group) > 253 || !kubernetesGroup.MatchString(r.Group)) {
		return fmt.Errorf("collector %q request.group %q is not an API group, such as apps or cert-manager.io; leave it out for the core group", x.Name, r.Group)
	}
	if r.Version != "" && (len(r.Version) > 63 || !kubernetesName.MatchString(r.Version)) {
		return fmt.Errorf("collector %q request.version %q is not an API version, such as v1 or v1beta1", x.Name, r.Version)
	}
	if r.Group == "" && r.Resource == "secrets" {
		return fmt.Errorf("collector %q lists secrets, which a kubernetes collector may not: their values would pass through the transforms, and could be shown by their errors and by a debug report; a metric of a Secret's existence or age comes from another resource, or from an exporter made for it", x.Name)
	}
	if r.AllNamespaces && len(r.Namespaces) > 0 {
		return fmt.Errorf("collector %q sets both request.namespaces and request.all_namespaces; list namespaces, or every namespace, not both", x.Name)
	}
	for i, ns := range r.Namespaces {
		if len(ns) > 63 || !kubernetesName.MatchString(ns) {
			return fmt.Errorf("collector %q request.namespaces entry %q is not a namespace's name", x.Name, ns)
		}
		if slices.Contains(r.Namespaces[:i], ns) {
			return fmt.Errorf("collector %q request.namespaces lists %q twice; its objects would be listed twice", x.Name, ns)
		}
	}
	for key, selector := range map[string]string{"label_selector": r.LabelSelector, "field_selector": r.FieldSelector} {
		if selector != "" && strings.TrimSpace(selector) == "" {
			return fmt.Errorf("collector %q request.%s is written with blanks alone; leave it out to list every object", x.Name, key)
		}
	}
	if r.MaxObjects < 0 {
		return fmt.Errorf("collector %q request.max_objects must not be negative; leave it out for %d", x.Name, DefaultKubernetesMaxObjects)
	}
	if r.APIServer == "" {
		if r.BearerTokenFile != "" || r.TLS != (model.TLSConfig{}) {
			return fmt.Errorf("collector %q sets request.tls or request.bearer_token_file without request.api_server; the API server of the exporter's own cluster is reached with the pod's service account, its token and CA, and these are for one named by api_server", x.Name)
		}
		return nil
	}
	if _, err := parseKubernetesAPIServer(r.APIServer); err != nil {
		return fmt.Errorf("collector %q request.api_server %w", x.Name, err)
	}
	if r.TLS.InsecureSkipVerify {
		return fmt.Errorf("collector %q sets request.tls.insecure_skip_verify; the API server is sent a token that reads the cluster, so its certificate is always verified: give its CA in tls.ca_file", x.Name)
	}
	if (r.TLS.CertFile == "") != (r.TLS.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	if r.BearerTokenFile == "" && r.TLS.CertFile == "" {
		return fmt.Errorf("collector %q names request.api_server with no credential; set bearer_token_file, or tls.cert_file and tls.key_file", x.Name)
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
	if c.Request.Version == "" {
		return "v1"
	}
	return c.Request.Version
}

// kubernetesMaxObjects is the most objects a listing of c may hold.
func kubernetesMaxObjects(c *model.Collector) int {
	if c.Request.MaxObjects == 0 {
		return DefaultKubernetesMaxObjects
	}
	return c.Request.MaxObjects
}

// kubernetesAPIVersion is the group and version as an object's apiVersion
// writes them: v1 for the core group, apps/v1 for apps.
func kubernetesAPIVersion(c *model.Collector) string {
	if c.Request.Group == "" {
		return kubernetesVersion(c)
	}
	return c.Request.Group + "/" + kubernetesVersion(c)
}

// kubernetesPath is the path of the listing of c in namespace, or across
// the cluster for "": /api/v1/... for the core group, /apis/group/version/...
// for the others.
func kubernetesPath(c *model.Collector, namespace string) string {
	prefix := "/apis/" + c.Request.Group + "/" + kubernetesVersion(c)
	if c.Request.Group == "" {
		prefix = "/api/" + kubernetesVersion(c)
	}
	if namespace != "" {
		prefix += "/namespaces/" + url.PathEscape(namespace)
	}
	return prefix + "/" + c.Request.Resource
}

// kubernetesLabel is the url label of a listing: the API server and the
// path of the listing across the cluster, kubernetes: standing for the
// server of the exporter's own cluster. Which namespaces are listed is the
// collector's, as its record type is a dns collector's.
func kubernetesLabel(c *model.Collector) string {
	if c.Request.APIServer == "" {
		return "kubernetes:" + kubernetesPath(c, "")
	}
	server, _ := parseKubernetesAPIServer(c.Request.APIServer)
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
	if c.Request.APIServer != "" {
		server, err := parseKubernetesAPIServer(c.Request.APIServer)
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
	case c.Request.AllNamespaces:
		return []string{""}, nil
	case len(c.Request.Namespaces) > 0:
		return c.Request.Namespaces, nil
	case c.Request.APIServer != "":
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
			return nil, fmt.Errorf("the token to list %s with: %w", c.Request.Resource, err)
		}
	}
	// The deadline of the scrape bounds the listing, every page of it.
	client, err := HTTPClient(TransportSettings{TLS: conn.tls}, false, 0)
	if err != nil {
		return nil, fmt.Errorf("the API server's CA or the client certificate: %w", err)
	}
	limit := responseLimit(c)
	maxObjects := kubernetesMaxObjects(c)
	doc := kubernetesDocument{Items: []json.RawMessage{}}
	total := int64(0)
	for _, namespace := range namespaces {
		listing := kubeapi.Listing{
			Client: client, Server: conn.server, Path: kubernetesPath(c, namespace), Token: token, What: c.Request.Resource,
			LabelSelector: c.Request.LabelSelector, FieldSelector: c.Request.FieldSelector, MaxPageBytes: limit,
			Sent: func(req *http.Request) {
				traceRequest(ctx, req.Method, req.URL.String(), req.Header, "", false)
			},
			Answered: func(outcome string) { traceOutcome(ctx, outcome) },
		}
		err := listing.List(ctx, func(raw []byte) (string, error) {
			var page kubernetesList
			if err := json.Unmarshal(raw, &page); err != nil {
				return "", fmt.Errorf("listing %s at %s: the answer is not a list: %w", c.Request.Resource, conn.server+listing.Path, err)
			}
			doc.Kind, doc.APIVersion = strings.TrimSuffix(page.Kind, "List"), page.APIVersion
			for _, item := range page.Items {
				if doc.Count++; doc.Count > maxObjects {
					return "", model.MarkError(fmt.Errorf("the listing of %s holds more than the %d objects request.max_objects allows; narrow it with namespaces, label_selector or field_selector, or raise max_objects", c.Request.Resource, maxObjects), model.ErrLimitExceeded)
				}
				item, err := withoutManagedFields(item)
				if err != nil {
					return "", fmt.Errorf("listing %s: an object is not a JSON object: %w", c.Request.Resource, err)
				}
				if total += int64(len(item)) + 1; total > limit {
					return "", model.MarkError(fmt.Errorf("the objects of the listing of %s come to more than the response limit, %d bytes; narrow the listing, or raise max_response_bytes", c.Request.Resource, limit), model.ErrLimitExceeded)
				}
				doc.Items = append(doc.Items, item)
			}
			return page.Metadata.Continue, nil
		})
		if err != nil {
			return nil, kubernetesFailure(ctx, c, namespace, err)
		}
	}
	if doc.Kind == "" {
		doc.APIVersion = kubernetesAPIVersion(c)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, model.MarkError(fmt.Errorf("the document of the listing of %s is %d bytes, more than the response limit, %d bytes; narrow the listing, or raise max_response_bytes", c.Request.Resource, len(body), limit), model.ErrLimitExceeded)
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

// withoutManagedFields is an object without its metadata.managedFields,
// which say which client set which field: often the largest part of an
// object, and of no use to a metric. The rest is kept as it was sent.
func withoutManagedFields(item json.RawMessage) (json.RawMessage, error) {
	if !bytes.Contains(item, []byte(`"managedFields"`)) {
		return item, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(item, &object); err != nil {
		return nil, err
	}
	// A metadata that is missing or no object has no managed fields to
	// take out, and the object is left as it was sent.
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(object["metadata"], &metadata)
	if metadata["managedFields"] == nil {
		return item, nil
	}
	delete(metadata, "managedFields")
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	object["metadata"] = raw
	return json.Marshal(object)
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
	what := fmt.Sprintf("listing %s (%s) %s answered %s: %s", c.Request.Resource, kubernetesAPIVersion(c), where, status.Status, message)
	switch status.Code {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s; the API server does not take the token: the service account's, or bearer_token_file's with api_server, is missing, expired or of another cluster", what)
	case http.StatusForbidden:
		return fmt.Errorf("%s; the exporter's service account may not list them: grant it list on %s in group %q, with the chart's kubernetesAccess.rules (docs/KUBERNETES.md#permissions)", what, c.Request.Resource, c.Request.Group)
	case http.StatusNotFound:
		return fmt.Errorf("%s; the API server has no such resource: check resource, its plural name, group and version against kubectl api-resources, and that a custom resource's definition is installed; a cluster-scoped resource, such as nodes, is listed with all_namespaces: true", what)
	}
	return errors.New(what)
}

// checkKubernetesFiles reads the files a collector naming api_server
// reaches it with, as a scrape reads them, for --dry-run: the token file
// and the tls block's. Those of the exporter's own service account are its
// pod's, which a check run elsewhere does not have, and are not read.
func checkKubernetesFiles(c *model.Collector, t *model.StaticTarget) []string {
	if t != nil || c.Request.APIServer == "" {
		return nil
	}
	var warnings []string
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
