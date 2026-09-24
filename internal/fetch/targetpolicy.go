package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector's request.allowed_targets and request.denied_targets say which
// hosts its requests may reach, whoever names the target: a probe's target
// parameter comes from whoever can reach /probe, and a redirect from the
// target itself. Each entry is a host name, a glob of one, such as
// *.example.com, an IP address or a CIDR network, such as 10.0.0.0/8.
//
// A request is refused when its host, or an address it resolves to, is
// denied, or when allowed_targets is set and neither its host nor every
// address it resolves to is allowed. Names are checked against the host as
// written, addresses against what it resolves to: before the request, again
// on every redirect, and once more against the address each connection is
// actually made to, so a name that resolves elsewhere between the check and
// the connection is caught too. Behind a proxy the exporter connects to the
// proxy rather than the target, and the addresses checked are those the
// exporter resolves the target's name to itself.

// TargetRefusedError is a request refused by the collector's
// allowed_targets or denied_targets. The exporter answers it 403.
type TargetRefusedError struct {
	Host   string
	Reason string
}

func (e *TargetRefusedError) Error() string {
	return fmt.Sprintf("target %s refused: %s", e.Host, e.Reason)
}

// ErrTargetRefused matches every TargetRefusedError with errors.Is.
var ErrTargetRefused = errors.New("target refused")

// Is makes every TargetRefusedError match ErrTargetRefused.
func (e *TargetRefusedError) Is(target error) bool { return target == ErrTargetRefused }

// targetPolicy is a compiled pair of lists.
type targetPolicy struct {
	allowNames, denyNames []string
	allowNets, denyNets   []netip.Prefix
}

var hostGlob = regexp.MustCompile(`^[a-z0-9*?_]([a-z0-9*?_.-]*[a-z0-9*?_])?$`)

// compileTargetPolicy reads the two lists, nil when both are empty.
func compileTargetPolicy(allowed, denied []string) (*targetPolicy, error) {
	if len(allowed) == 0 && len(denied) == 0 {
		return nil, nil
	}
	p := &targetPolicy{}
	for _, list := range []struct {
		key   string
		items []string
		names *[]string
		nets  *[]netip.Prefix
	}{
		{"allowed_targets", allowed, &p.allowNames, &p.allowNets},
		{"denied_targets", denied, &p.denyNames, &p.denyNets},
	} {
		for _, raw := range list.items {
			entry := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
			entry = strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]")
			switch {
			case entry == "":
				return nil, fmt.Errorf("request.%s has an empty entry", list.key)
			case strings.Contains(entry, "/"):
				prefix, err := netip.ParsePrefix(entry)
				if err != nil {
					return nil, fmt.Errorf("request.%s entry %q is not a CIDR network such as 10.0.0.0/8: %w", list.key, raw, err)
				}
				*list.nets = append(*list.nets, prefix.Masked())
			default:
				if addr, err := netip.ParseAddr(entry); err == nil {
					addr = addr.Unmap()
					*list.nets = append(*list.nets, netip.PrefixFrom(addr, addr.BitLen()))
					continue
				}
				if !hostGlob.MatchString(entry) {
					return nil, fmt.Errorf("request.%s entry %q is not a host name, a glob of one such as *.example.com, an IP address or a CIDR network; give the host alone, without a scheme, port or path", list.key, raw)
				}
				*list.names = append(*list.names, entry)
			}
		}
	}
	return p, nil
}

var targetPolicies sync.Map // "allowed\x00denied" -> *targetPolicy

// policyOf is the collector's compiled policy, nil when it sets none. The
// lists were checked at load, so an error here cannot happen.
func policyOf(c *model.Collector) *targetPolicy {
	if len(c.Request.AllowedTargets) == 0 && len(c.Request.DeniedTargets) == 0 {
		return nil
	}
	key := strings.Join(c.Request.AllowedTargets, "\x01") + "\x00" + strings.Join(c.Request.DeniedTargets, "\x01")
	if p, ok := targetPolicies.Load(key); ok {
		return p.(*targetPolicy)
	}
	p, err := compileTargetPolicy(c.Request.AllowedTargets, c.Request.DeniedTargets)
	if err != nil || p == nil {
		return nil
	}
	targetPolicies.Store(key, p)
	return p
}

// validateTargetPolicy checks a collector's lists at load.
func validateTargetPolicy(c *model.Collector) error {
	if _, err := compileTargetPolicy(c.Request.AllowedTargets, c.Request.DeniedTargets); err != nil {
		return fmt.Errorf("collector %q %w", c.Name, err)
	}
	return nil
}

func matchesName(globs []string, host string) (string, bool) {
	for _, glob := range globs {
		if ok, _ := path.Match(glob, host); ok {
			return glob, true
		}
	}
	return "", false
}

func matchesAddr(nets []netip.Prefix, addr netip.Addr) (netip.Prefix, bool) {
	addr = addr.Unmap()
	for _, prefix := range nets {
		if prefix.Contains(addr) {
			return prefix, true
		}
	}
	return netip.Prefix{}, false
}

// checkName applies the name rules to host and says whether its name alone
// allows it, so its addresses need not be.
func (p *targetPolicy) checkName(host string) (nameAllowed bool, err error) {
	if glob, denied := matchesName(p.denyNames, host); denied {
		return false, &TargetRefusedError{Host: host, Reason: fmt.Sprintf("it matches %q in request.denied_targets", glob)}
	}
	_, allowed := matchesName(p.allowNames, host)
	return allowed, nil
}

// checkAddr applies the address rules to one address host resolves to.
func (p *targetPolicy) checkAddr(host string, addr netip.Addr, nameAllowed bool) error {
	if prefix, denied := matchesAddr(p.denyNets, addr); denied {
		return &TargetRefusedError{Host: host, Reason: fmt.Sprintf("its address %s is in %s in request.denied_targets", addr.Unmap(), prefix)}
	}
	if nameAllowed || len(p.allowNames)+len(p.allowNets) == 0 {
		return nil
	}
	if _, allowed := matchesAddr(p.allowNets, addr); !allowed {
		return &TargetRefusedError{Host: host, Reason: fmt.Sprintf("neither it nor its address %s is in request.allowed_targets", addr.Unmap())}
	}
	return nil
}

// needsAddrs says whether host's addresses have to be looked at.
func (p *targetPolicy) needsAddrs(nameAllowed bool) bool {
	return len(p.denyNets) > 0 || (!nameAllowed && len(p.allowNames)+len(p.allowNets) > 0)
}

// check applies the policy to host before a request: its name, then every
// address it resolves to. It returns whether the name alone allowed it.
func (p *targetPolicy) check(ctx context.Context, host string) (bool, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	nameAllowed, err := p.checkName(host)
	if err != nil || !p.needsAddrs(nameAllowed) {
		return nameAllowed, err
	}
	addrs, err := resolveHost(ctx, host)
	if err != nil {
		return nameAllowed, err
	}
	for _, addr := range addrs {
		if err := p.checkAddr(host, addr, nameAllowed); err != nil {
			return nameAllowed, err
		}
	}
	return nameAllowed, nil
}

// resolveHost is host's addresses: the address itself for an IP literal.
var resolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil {
		return []netip.Addr{addr}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolving %s to check it against request.allowed_targets and denied_targets: %w", host, err)
	}
	return addrs, nil
}

// policyGuard carries a request's policy to the connections it makes: the
// hosts it checked, and whether each one's name allowed it.
type policyGuard struct {
	policy *targetPolicy
	mu     sync.Mutex
	hosts  map[string]bool
}

type policyGuardKey struct{}

// withTargetPolicy checks host against the collector's policy and returns a
// context whose connections are checked too; ctx itself when the collector
// sets no policy.
func withTargetPolicy(ctx context.Context, c *model.Collector, host string) (context.Context, error) {
	policy := policyOf(c)
	if policy == nil {
		return ctx, nil
	}
	guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
	if err := guard.check(ctx, host); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, policyGuardKey{}, guard), nil
}

// check applies the policy to a host the request is about to reach, the
// first or one a redirect names, and remembers it for the connections.
func (g *policyGuard) check(ctx context.Context, host string) error {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	nameAllowed, err := g.policy.check(ctx, host)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.hosts[host] = nameAllowed
	g.mu.Unlock()
	return nil
}

// checkRedirect applies a request's policy to the host a redirect leads to.
func checkRedirect(ctx context.Context, host string) error {
	if guard, ok := ctx.Value(policyGuardKey{}).(*policyGuard); ok {
		return guard.check(ctx, host)
	}
	return nil
}

// checkConnection applies a request's policy to the address a connection to
// dialed, host:port, was made to. A dial to a host the request did not
// check is a dial to a proxy, whose target was checked by name and resolved
// address already.
func checkConnection(ctx context.Context, dialed string, conn net.Conn) error {
	guard, ok := ctx.Value(policyGuardKey{}).(*policyGuard)
	if !ok {
		return nil
	}
	host, _, err := net.SplitHostPort(dialed)
	if err != nil {
		host = dialed
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	guard.mu.Lock()
	nameAllowed, checked := guard.hosts[host]
	guard.mu.Unlock()
	if !checked {
		return nil
	}
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	addr, ok := netip.AddrFromSlice(remote.IP)
	if !ok {
		return nil
	}
	return guard.policy.checkAddr(host, addr, nameAllowed)
}

// policyDialer wraps dial so every connection is checked against the policy
// of the request that made it.
func policyDialer(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := checkConnection(ctx, address, conn); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}
