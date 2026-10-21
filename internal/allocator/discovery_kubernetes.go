package allocator

import (
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// serviceAccountDir is where Kubernetes mounts a pod's service account:
// its token, the cluster's CA and the pod's namespace. Tests point it at a
// directory of their own.
var serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// kubernetesPageSize is how many EndpointSlices one listing request asks
// for; the rest follow with the answer's continue token. A page must fit in
// maxDiscoveryAnswerBytes, 16 MiB, or the listing fails at every refresh.
// The API server sends whole slices: the EndpointSlice controller puts up
// to 100 endpoints in one (its --max-endpoints-per-slice default), and an
// endpoint with names of their longest, a 253-byte pod and node name, a
// 63-byte namespace and hostname, an IPv6 address, its uid, zone, hints and
// conditions, is about 1 KiB of JSON, so a full slice with its metadata,
// copied Service labels and managed fields is about 100 KiB. 100 of them are
// about 10 MiB, which fits with room to spare; 500 were about 50 MiB, which
// a large cluster's listing reached. TestAKubernetesPageOfTheWorstCaseFits
// builds such a page. A cluster that raises --max-endpoints-per-slice makes
// larger slices than this allows for.
const kubernetesPageSize = 100

// endpointSliceList is the part of a discovery.k8s.io/v1 EndpointSliceList
// the allocator reads.
type endpointSliceList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []endpointSlice `json:"items"`
}

type endpointSlice struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	AddressType string `json:"addressType"`
	Endpoints   []struct {
		Addresses  []string `json:"addresses"`
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
		Hostname  string `json:"hostname"`
		NodeName  string `json:"nodeName"`
		TargetRef *struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"targetRef"`
	} `json:"endpoints"`
	Ports []struct {
		Name string `json:"name"`
		Port *int   `json:"port"`
	} `json:"ports"`
}

// serviceNameLabel is the label an EndpointSlice names its Service by.
const serviceNameLabel = "kubernetes.io/service-name"

// refreshKubernetes lists the EndpointSlices k selects from the API server
// of the cluster the allocator runs in, with the pod's service account: its
// token, read again at every listing so a rotated one is used, and the
// cluster's CA, which the API server's certificate must be signed by. The
// whole listing, every page of every namespace, takes at most timeout.
func refreshKubernetes(ctx context.Context, k *KubernetesConfig, mapping map[string]string, limit int, timeout time.Duration) ([]discovered, error) {
	// The client's own timeout bounds one request alone; a listing of many
	// pages could take many times it.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set: the allocator does not run in a Kubernetes pod, so it cannot reach the API server")
	}
	token, err := readToken(filepath.Join(serviceAccountDir, "token"))
	if err != nil {
		return nil, fmt.Errorf("the service account token: %w", err)
	}
	namespaces := k.Namespaces
	if !k.AllNamespaces && len(namespaces) == 0 {
		raw, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
		if err != nil || strings.TrimSpace(string(raw)) == "" {
			return nil, fmt.Errorf("the allocator's own namespace cannot be read from %s; list kubernetes.namespaces", filepath.Join(serviceAccountDir, "namespace"))
		}
		namespaces = []string{strings.TrimSpace(string(raw))}
	}
	client, err := fetch.HTTPClient(fetch.TransportSettings{TLS: model.TLSConfig{CAFile: filepath.Join(serviceAccountDir, "ca.crt")}}, false, timeout)
	if err != nil {
		return nil, fmt.Errorf("the cluster's CA: %w", err)
	}
	server := "https://" + net.JoinHostPort(host, port)
	paths := []string{"/apis/discovery.k8s.io/v1/endpointslices"}
	if !k.AllNamespaces {
		paths = paths[:0]
		for _, ns := range namespaces {
			paths = append(paths, "/apis/discovery.k8s.io/v1/namespaces/"+url.PathEscape(ns)+"/endpointslices")
		}
	}
	portName, portNumber, _ := k.port()
	var out []discovered
	seen := map[string]bool{}
	for _, path := range paths {
		next := ""
		for {
			list, err := listEndpointSlices(ctx, client, server+path, k.LabelSelector, next, token)
			if err != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil, fmt.Errorf("listing the EndpointSlices took longer than the timeout, %s: %w", timeout, err)
				}
				return nil, err
			}
			for i := range list.Items {
				if !k.takes(list.Items[i].AddressType) {
					continue
				}
				for _, d := range sliceTargets(&list.Items[i], portName, portNumber, k.IncludeNotReady) {
					if seen[d.Address] {
						continue
					}
					seen[d.Address] = true
					d.Labels = mapLabels(d.Labels, mapping)
					out = append(out, d)
					if len(out) > limit {
						return nil, fmt.Errorf("the EndpointSlices hold more than the %d targets max_targets allows", limit)
					}
				}
			}
			if next = list.Metadata.Continue; next == "" {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// listEndpointSlices asks for one page of a listing.
func listEndpointSlices(ctx context.Context, client *http.Client, address, selector, next, token string) (*endpointSliceList, error) {
	query := url.Values{"limit": {strconv.Itoa(kubernetesPageSize)}}
	if selector != "" {
		query.Set("labelSelector", selector)
	}
	if next != "" {
		query.Set("continue", next)
	}
	// The address is the API server's, from the pod's environment, which
	// Kubernetes sets.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"?"+query.Encode(), nil) // #nosec G704 -- the cluster's API server
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req) // #nosec G704 -- the cluster's API server
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("listing EndpointSlices at %s answered %s: %s", address, resp.Status, strings.TrimSpace(string(body)))
	}
	raw, err := readBounded(resp.Body, "a page of EndpointSlices")
	if err != nil {
		return nil, err
	}
	var list endpointSliceList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("listing EndpointSlices at %s: not an EndpointSliceList: %w", address, err)
	}
	return &list, nil
}

// sliceTargets are the targets of one EndpointSlice: each address of each
// endpoint, ready unless includeNotReady, at the port named portName or
// numbered portNumber, with the meta labels of the endpoint and the slice.
// A slice without that port has none.
func sliceTargets(s *endpointSlice, portName string, portNumber int, includeNotReady bool) []discovered {
	port := 0
	for _, p := range s.Ports {
		if p.Port != nil && ((portName != "" && p.Name == portName) || (portNumber != 0 && *p.Port == portNumber)) {
			port, portName = *p.Port, p.Name
			break
		}
	}
	if port == 0 {
		return nil
	}
	base := map[string]string{
		"__meta_kubernetes_namespace":          s.Metadata.Namespace,
		"__meta_kubernetes_endpointslice_name": s.Metadata.Name,
		"__meta_kubernetes_endpoint_port_name": portName,
	}
	if s.AddressType != "" {
		base["__meta_kubernetes_endpointslice_address_type"] = s.AddressType
	}
	if service := s.Metadata.Labels[serviceNameLabel]; service != "" {
		base["__meta_kubernetes_service_name"] = service
	}
	for name, value := range s.Metadata.Labels {
		base["__meta_kubernetes_endpointslice_label_"+sanitizeLabelName(name)] = value
	}
	var out []discovered
	for _, ep := range s.Endpoints {
		// The API leaves ready out when it is unknown, which is to be read
		// as ready.
		ready := ep.Conditions.Ready == nil || *ep.Conditions.Ready
		if !ready && !includeNotReady {
			continue
		}
		labels := make(map[string]string, len(base)+4)
		for name, value := range base {
			labels[name] = value
		}
		labels["__meta_kubernetes_endpoint_ready"] = strconv.FormatBool(ready)
		if ep.Hostname != "" {
			labels["__meta_kubernetes_endpoint_hostname"] = ep.Hostname
		}
		if ep.NodeName != "" {
			labels["__meta_kubernetes_node_name"] = ep.NodeName
		}
		if ep.TargetRef != nil && ep.TargetRef.Kind == "Pod" {
			labels["__meta_kubernetes_pod_name"] = ep.TargetRef.Name
		}
		for _, address := range ep.Addresses {
			out = append(out, discovered{Address: net.JoinHostPort(address, strconv.Itoa(port)), Labels: labels})
		}
	}
	return out
}

// sanitizeLabelName writes every character a label name cannot hold as _,
// as Prometheus writes a Kubernetes label into a meta label's name.
func sanitizeLabelName(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, name)
}
