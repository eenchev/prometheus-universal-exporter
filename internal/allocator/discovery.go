package allocator

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// DiscoveryConfig is one discovery source of a probe job's or a
// discovered_targets entry's discovery list: exactly one of FileSD, HTTPSD,
// Kubernetes and DNSSD, with what the allocator makes of its answer.
type DiscoveryConfig struct {
	// Name tells the source apart in the metrics and logs; it defaults to
	// the source's kind and its place in the list, such as http_sd_0.
	Name       string            `yaml:"name"`
	FileSD     *FileSDConfig     `yaml:"file_sd"`
	HTTPSD     *HTTPSDConfig     `yaml:"http_sd"`
	Kubernetes *KubernetesConfig `yaml:"kubernetes"`
	DNSSD      *DNSSDConfig      `yaml:"dns_sd"`
	// Labels sets a label of each discovered target to the value of one
	// of the source's own labels, keyed by the label set: a meta label
	// such as __meta_kubernetes_namespace, which would otherwise not reach
	// a static target, or a group label under another name.
	Labels map[string]string `yaml:"labels"`
	// MaxTargets bounds the targets one answer may hold; an answer with
	// more is refused as a failure. 0 takes --discovery.max-targets.
	MaxTargets int `yaml:"max_targets"`
}

// FileSDConfig reads Prometheus file_sd files: JSON (.json) or YAML (.yml,
// .yaml) lists of target groups.
type FileSDConfig struct {
	// Files are paths or patterns, relative to the directory of the file
	// that names them; a pattern matching nothing gives no targets.
	Files           []string       `yaml:"files"`
	RefreshInterval model.Duration `yaml:"refresh_interval"`
}

// HTTPSDConfig asks a URL for target groups as Prometheus's HTTP service
// discovery does.
type HTTPSDConfig struct {
	URL             string         `yaml:"url"`
	RefreshInterval model.Duration `yaml:"refresh_interval"`
	Timeout         model.Duration `yaml:"timeout"`
	// BearerTokenFile holds a token sent as Authorization: Bearer, read
	// again at every refresh so a rotated token is used.
	BearerTokenFile string          `yaml:"bearer_token_file"`
	TLS             model.TLSConfig `yaml:"tls"`
}

// KubernetesConfig lists EndpointSlices from the API server of the cluster
// the allocator runs in.
type KubernetesConfig struct {
	// Namespaces are listed one by one; none lists the allocator's own.
	Namespaces []string `yaml:"namespaces"`
	// AllNamespaces lists the EndpointSlices of every namespace, which
	// needs a ClusterRole.
	AllNamespaces bool `yaml:"all_namespaces"`
	// LabelSelector is sent as written, a Kubernetes label selector.
	LabelSelector string `yaml:"label_selector"`
	// Port is the endpoint port each target is at, by its name or number.
	Port string `yaml:"port"`
	// IncludeNotReady takes the endpoints that are not ready too.
	IncludeNotReady bool `yaml:"include_not_ready"`
	// AddressType is the addressType of the EndpointSlices taken: IPv4,
	// the default, IPv6, or any, which takes FQDN slices too. A dual-stack
	// Service has a slice of each family, so taking both would give each
	// pod twice.
	AddressType     string         `yaml:"address_type"`
	RefreshInterval model.Duration `yaml:"refresh_interval"`
	Timeout         model.Duration `yaml:"timeout"`
}

// DNSSDConfig looks names up in the DNS as Prometheus's dns_sd_configs
// does: SRV records, each a target at its target host and port, or A or
// AAAA records, each a target at its address and Port.
type DNSSDConfig struct {
	// Names are looked up as absolute names, so the search domains of the
	// system's resolver are never appended to them.
	Names []string `yaml:"names"`
	// Type is SRV, the default, A or AAAA.
	Type string `yaml:"type"`
	// Port is the port of an A or AAAA source's targets; an SRV record
	// carries its own.
	Port int `yaml:"port"`
	// Server, host:port, is asked instead of the system's nameservers.
	Server          string         `yaml:"server"`
	RefreshInterval model.Duration `yaml:"refresh_interval"`
	Timeout         model.Duration `yaml:"timeout"`
}

// The defaults and bounds of a discovery source.
const (
	// DefaultMaxDiscoveredTargets is --discovery.max-targets's default.
	DefaultMaxDiscoveredTargets = 10000
	// DefaultFileSDRefresh is how often file_sd files are read again by
	// default: the allocator's own watch interval.
	DefaultFileSDRefresh = time.Minute
	// DefaultHTTPSDRefresh is Prometheus's own for HTTP SD.
	DefaultHTTPSDRefresh = time.Minute
	// DefaultKubernetesRefresh is how often EndpointSlices are listed.
	DefaultKubernetesRefresh = 30 * time.Second
	// DefaultDNSSDRefresh is Prometheus's own for DNS service discovery.
	DefaultDNSSDRefresh = 30 * time.Second
	// DefaultDNSSDType is the record type a dns_sd source asks for by
	// default.
	DefaultDNSSDType = "SRV"
	// DefaultDiscoveryTimeout bounds an http_sd request, a Kubernetes
	// listing or a dns_sd source's lookups, or the source's
	// refresh_interval when that is shorter.
	DefaultDiscoveryTimeout = 10 * time.Second
	// DefaultAddressType is the addressType of the EndpointSlices a
	// kubernetes source takes by default.
	DefaultAddressType = "IPv4"
	// minDiscoveryRefresh is the shortest refresh_interval taken.
	minDiscoveryRefresh = time.Second
	// maxDiscoveryAnswerBytes bounds an http_sd answer, a file_sd file and
	// a page of a Kubernetes listing.
	maxDiscoveryAnswerBytes = 16 << 20
	// AddressPlaceholder stands for a discovered target's address in a
	// target_template.
	AddressPlaceholder = "{{address}}"
)

var (
	// discoveryName is what a source may be called: it is a label value,
	// and a word in logs.
	discoveryName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	// dnsLabel is a Kubernetes namespace's name.
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// portName is a Kubernetes port's name.
	portName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// kind is the source's kind: file_sd, http_sd, kubernetes or dns_sd.
func (d *DiscoveryConfig) kind() string {
	switch {
	case d.FileSD != nil:
		return "file_sd"
	case d.HTTPSD != nil:
		return "http_sd"
	case d.Kubernetes != nil:
		return "kubernetes"
	case d.DNSSD != nil:
		return "dns_sd"
	}
	return ""
}

// refresh is how often the source is asked again.
func (d *DiscoveryConfig) refresh() time.Duration {
	var set model.Duration
	var def time.Duration
	switch {
	case d.FileSD != nil:
		set, def = d.FileSD.RefreshInterval, DefaultFileSDRefresh
	case d.HTTPSD != nil:
		set, def = d.HTTPSD.RefreshInterval, DefaultHTTPSDRefresh
	case d.Kubernetes != nil:
		set, def = d.Kubernetes.RefreshInterval, DefaultKubernetesRefresh
	case d.DNSSD != nil:
		set, def = d.DNSSD.RefreshInterval, DefaultDNSSDRefresh
	}
	if set == 0 {
		return def
	}
	return time.Duration(set)
}

// reservedTargetLabels are what a discovery source's labels may not set:
// job is the scrape configuration's, instance the allocator's (a probe's
// target) or Prometheus's, and static_target the static targets endpoint's.
var reservedTargetLabels = map[string]bool{"job": true, "instance": true, "static_target": true}

// checkDiscovery checks a discovery list, filling each source's name. base
// is the directory of the file it is in, against which a relative path is
// taken; each path is rewritten so.
func checkDiscovery(list []DiscoveryConfig, base string) error {
	names := map[string]bool{}
	for i := range list {
		d := &list[i]
		kinds := 0
		for _, set := range []bool{d.FileSD != nil, d.HTTPSD != nil, d.Kubernetes != nil, d.DNSSD != nil} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			return fmt.Errorf("discovery source %d must set exactly one of file_sd, http_sd, kubernetes and dns_sd", i)
		}
		if d.Name == "" {
			d.Name = d.kind() + "_" + strconv.Itoa(i)
		}
		if !discoveryName.MatchString(d.Name) {
			return fmt.Errorf("discovery source %d has the name %q; a name is letters, digits, _, . and -, not starting with . or -", i, d.Name)
		}
		if names[d.Name] {
			return fmt.Errorf("discovery source %q is named twice; give each source its own name", d.Name)
		}
		names[d.Name] = true
		if err := d.check(base); err != nil {
			return fmt.Errorf("discovery source %q: %w", d.Name, err)
		}
	}
	return nil
}

func (d *DiscoveryConfig) check(base string) error {
	if d.MaxTargets < 0 {
		return errors.New("max_targets must not be negative; leave it out, or 0, for --discovery.max-targets")
	}
	for _, name := range model.SortedKeys(d.Labels) {
		from := d.Labels[name]
		switch {
		case !model.ValidLabelName(name):
			return fmt.Errorf("labels sets %q, which is not a label name", name)
		case strings.HasPrefix(name, "__"):
			return fmt.Errorf("labels sets %q; a label starting with __ is Prometheus's own", name)
		case reservedTargetLabels[name]:
			return fmt.Errorf("labels sets %s, which the allocator, the exporter or Prometheus sets on every target", name)
		case !model.ValidLabelName(from):
			return fmt.Errorf("labels sets %s from %q, which is not a label name", name, from)
		}
	}
	if d.refresh() < minDiscoveryRefresh {
		return fmt.Errorf("refresh_interval %s is under the least, %s", d.refresh(), minDiscoveryRefresh)
	}
	switch {
	case d.FileSD != nil:
		return d.FileSD.check(base)
	case d.HTTPSD != nil:
		return d.HTTPSD.check(base, d.refresh())
	case d.DNSSD != nil:
		return d.DNSSD.check(d.refresh())
	default:
		return d.Kubernetes.check(d.refresh())
	}
}

func (f *FileSDConfig) check(base string) error {
	if len(f.Files) == 0 {
		return errors.New("file_sd.files must not be empty")
	}
	for i, file := range f.Files {
		if strings.TrimSpace(file) == "" {
			return errors.New("file_sd.files has an empty entry")
		}
		if _, err := filepath.Match(file, ""); err != nil {
			return fmt.Errorf("file_sd.files entry %q is not a pattern: %w", file, err)
		}
		switch strings.ToLower(filepath.Ext(file)) {
		case ".json", ".yml", ".yaml":
		default:
			return fmt.Errorf("file_sd.files entry %q must end in .json, .yml or .yaml, which tells how its files are read", file)
		}
		f.Files[i] = resolvePath(base, file)
	}
	return nil
}

func (h *HTTPSDConfig) check(base string, refresh time.Duration) error {
	u, err := url.Parse(h.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("http_sd.url %q must be an absolute http or https URL", h.URL)
	}
	if u.User != nil {
		return errors.New("http_sd.url must not carry credentials; give a bearer_token_file")
	}
	if err := checkTimeout("http_sd", h.Timeout, refresh); err != nil {
		return err
	}
	if err := fetch.CheckClientTLSKeys(h.TLS); err != nil {
		return fmt.Errorf("http_sd.tls %w", err)
	}
	if h.TLS != (model.TLSConfig{}) && u.Scheme != "https" {
		return errors.New("http_sd.tls is set for an http URL, which never uses it")
	}
	h.BearerTokenFile = resolvePath(base, h.BearerTokenFile)
	h.TLS.CAFile = resolvePath(base, h.TLS.CAFile)
	h.TLS.CertFile = resolvePath(base, h.TLS.CertFile)
	h.TLS.KeyFile = resolvePath(base, h.TLS.KeyFile)
	return nil
}

func (k *KubernetesConfig) check(refresh time.Duration) error {
	if k.AllNamespaces && len(k.Namespaces) > 0 {
		return errors.New("kubernetes sets both namespaces and all_namespaces; list namespaces, or take them all")
	}
	seen := map[string]bool{}
	for _, ns := range k.Namespaces {
		if !dnsLabel.MatchString(ns) || len(ns) > 63 {
			return fmt.Errorf("kubernetes.namespaces has %q, which is not a namespace's name", ns)
		}
		if seen[ns] {
			return fmt.Errorf("kubernetes.namespaces lists %q twice", ns)
		}
		seen[ns] = true
	}
	if err := checkLabelSelector(k.LabelSelector); err != nil {
		return fmt.Errorf("kubernetes.label_selector %q: %w", k.LabelSelector, err)
	}
	if _, _, err := k.port(); err != nil {
		return err
	}
	switch k.AddressType {
	case "", "IPv4", "IPv6", "any":
	default:
		return fmt.Errorf("kubernetes.address_type %q must be IPv4, IPv6 or any", k.AddressType)
	}
	return checkTimeout("kubernetes", k.Timeout, refresh)
}

func (n *DNSSDConfig) check(refresh time.Duration) error {
	if len(n.Names) == 0 {
		return errors.New("dns_sd.names must not be empty")
	}
	seen := map[string]bool{}
	for i, raw := range n.Names {
		name, err := dnsSDName(raw)
		if err != nil {
			return fmt.Errorf("dns_sd.names entry %w", err)
		}
		if seen[name] {
			return fmt.Errorf("dns_sd.names lists %q twice", name)
		}
		seen[name] = true
		n.Names[i] = name
	}
	if n.Type == "" {
		n.Type = DefaultDNSSDType
	}
	switch n.Type {
	case "SRV":
		if n.Port != 0 {
			return errors.New("dns_sd.port is set for type SRV, whose records carry their own ports; leave it out")
		}
	case "A", "AAAA":
		if n.Port < 1 || n.Port > 65535 {
			return fmt.Errorf("dns_sd.port must be a port from 1 to 65535 for type %s, whose records carry no port", n.Type)
		}
	default:
		return fmt.Errorf("dns_sd.type %q must be SRV, A or AAAA", n.Type)
	}
	if n.Server != "" {
		server, err := fetch.ParseDNSServer(n.Server)
		if err != nil {
			return fmt.Errorf("dns_sd.server %w", err)
		}
		n.Server = server
	}
	return checkTimeout("dns_sd", n.Timeout, refresh)
}

// dnsSDName reads a name of dns_sd.names: a DNS name, such as
// _metrics._tcp.example.com, in lower case without a trailing dot, so that
// a name written twice in two spellings is told, and every replica asks for
// and labels its targets with the same one.
func dnsSDName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("is empty")
	}
	if strings.ContainsAny(name, "/?#@:[]") {
		return "", fmt.Errorf("%q is not a DNS name; the server is dns_sd.server and the port dns_sd.port", raw)
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return "", fmt.Errorf("%q is an IP address, not a name to look up", raw)
	}
	canonical, err := fetch.CanonicalDNSName(name)
	var refused *fetch.TargetRefusedError
	if errors.As(err, &refused) {
		return "", fmt.Errorf("%q is not a DNS name: %s", raw, refused.Reason)
	}
	return canonical, err
}

// port is the port the targets are at: its name, or its number.
func (k *KubernetesConfig) port() (name string, number int, err error) {
	if n, convErr := strconv.Atoi(k.Port); convErr == nil {
		if n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("kubernetes.port %d is not a port from 1 to 65535", n)
		}
		return "", n, nil
	}
	if !portName.MatchString(k.Port) || len(k.Port) > 15 {
		return "", 0, fmt.Errorf("kubernetes.port %q is neither a port's name, as the Service names it, nor its number", k.Port)
	}
	return k.Port, 0, nil
}

// takes reports whether the source takes an EndpointSlice of addressType:
// every one with any, and otherwise the one family it names, IPv4 unless
// address_type says otherwise.
func (k *KubernetesConfig) takes(addressType string) bool {
	want := k.AddressType
	if want == "" {
		want = DefaultAddressType
	}
	return want == "any" || addressType == want
}

// timeout is how long one ask of the source may take: its timeout, or
// when that is unset DefaultDiscoveryTimeout, cut to its refresh_interval
// when that is shorter, so an ask never outlasts the interval to the next
// one. Only a timeout written longer than the interval is refused, as a
// mistake; a short interval alone needs no timeout written beside it.
func (d *DiscoveryConfig) timeout() time.Duration {
	var set model.Duration
	switch {
	case d.HTTPSD != nil:
		set = d.HTTPSD.Timeout
	case d.Kubernetes != nil:
		set = d.Kubernetes.Timeout
	case d.DNSSD != nil:
		set = d.DNSSD.Timeout
	}
	if set != 0 {
		return time.Duration(set)
	}
	return min(DefaultDiscoveryTimeout, d.refresh())
}

func checkTimeout(kind string, set model.Duration, refresh time.Duration) error {
	switch {
	case set < 0:
		return fmt.Errorf("%s.timeout must not be negative", kind)
	case time.Duration(set) > refresh:
		return fmt.Errorf("%s.timeout %s is longer than its refresh_interval %s", kind, time.Duration(set), refresh)
	}
	return nil
}

// resolvePath is path taken against base when it is relative; "" stays "".
func resolvePath(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

var (
	// selectorKey is a label key: an optional DNS subdomain prefix and a
	// name of at most 63 characters.
	selectorKey = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	// selectorValue is a label value: empty, or as a key's name.
	selectorValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
	// setRequirement is "key in (a, b)" or "key notin (a, b)".
	setRequirement = regexp.MustCompile(`^(\S+)\s+(in|notin)\s*\(([^()]*)\)$`)
)

// checkLabelSelector refuses a label selector the API server would refuse,
// so the mistake is told at load rather than at every listing: a comma
// separated list of key, !key, key=value, key==value, key!=value,
// key in (values) and key notin (values).
func checkLabelSelector(selector string) error {
	if strings.TrimSpace(selector) == "" {
		return nil
	}
	for _, req := range splitSelector(selector) {
		req = strings.TrimSpace(req)
		if req == "" {
			return errors.New("it has an empty requirement")
		}
		if m := setRequirement.FindStringSubmatch(req); m != nil {
			if !selectorKey.MatchString(m[1]) {
				return fmt.Errorf("%q is not a label key", m[1])
			}
			for _, v := range strings.Split(m[3], ",") {
				if !selectorValue.MatchString(strings.TrimSpace(v)) {
					return fmt.Errorf("%q is not a label value", strings.TrimSpace(v))
				}
			}
			continue
		}
		key, value, op := req, "", ""
		for _, candidate := range []string{"!=", "==", "="} {
			if k, v, found := strings.Cut(req, candidate); found {
				key, value, op = k, v, candidate
				break
			}
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if op == "" {
			key = strings.TrimSpace(strings.TrimPrefix(key, "!"))
		}
		if !selectorKey.MatchString(key) {
			return fmt.Errorf("%q is not a label key", key)
		}
		if !selectorValue.MatchString(value) {
			return fmt.Errorf("%q is not a label value", value)
		}
	}
	return nil
}

// splitSelector splits a selector at the commas outside parentheses.
func splitSelector(selector string) []string {
	var parts []string
	depth, start := 0, 0
	for i, c := range selector {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, selector[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, selector[start:])
}

// checkTargetTemplate refuses a target_template without the address.
func checkTargetTemplate(template string) error {
	if template != "" && !strings.Contains(template, AddressPlaceholder) {
		return fmt.Errorf("target_template %q has no %s, which stands for each discovered target's address", template, AddressPlaceholder)
	}
	return nil
}

// fromTemplate is the target of the discovered address under template.
func fromTemplate(template, address string) string {
	if template == "" {
		return address
	}
	return strings.ReplaceAll(template, AddressPlaceholder, address)
}
