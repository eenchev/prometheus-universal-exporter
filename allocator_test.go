package main

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// Allocator mode's command line (allocator.go).

// --allocator.url with a flag that names a configuration file, given
// explicitly, is a command-line error naming both, at a start and with
// --dry-run alike; so is a flag of allocator mode without --allocator.url,
// --allocator.url without --allocator.cache-dir, a URL that is not a plain
// http one, and an --allocator.advertise that is not host:port.
func TestAllocatorModeCommandLineErrors(t *testing.T) {
	dir := t.TempDir()
	mode := []string{"--allocator.url=http://pue-allocator:8080", "--allocator.cache-dir=" + dir}
	for _, c := range []struct {
		args []string
		want string
	}{
		{append(mode, "--config.file=configs/config.example.yaml"), "--allocator.url and --config.file cannot be given together"},
		{append(mode, "--config.file=/etc/prometheus-universal-exporter/config.yaml"), "--allocator.url and --config.file cannot be given together"},
		{append(mode, "--static-targets-file=targets.yaml"), "--allocator.url and --static-targets-file cannot be given together"},
		{append([]string{"--dry-run"}, append(mode, "--static-targets-file=targets.yaml")...), "--allocator.url and --static-targets-file cannot be given together"},
		{[]string{"--allocator.cache-dir=" + dir}, "--allocator.cache-dir is only read in allocator mode"},
		{[]string{"--allocator.token-file=token"}, "--allocator.token-file is only read in allocator mode"},
		{[]string{"--allocator.url=http://pue-allocator:8080"}, "--allocator.url needs --allocator.cache-dir"},
		{[]string{"--allocator.url=https://pue-allocator", "--allocator.cache-dir=" + dir}, "must be the allocator's http:// URL"},
		{append(mode, "--allocator.advertise=10.0.0.1"), "must be host:port"},
	} {
		out := runCLI(t, c.args...)
		if out.code != 2 || !strings.Contains(out.stderr, c.want) || out.stdout != "" {
			t.Errorf("%v: exit=%d stderr=%s stdout=%s", c.args, out.code, out.stderr, out.stdout)
		}
	}
}

// --dry-run in allocator mode checks the bundle in the cache directory, which
// a restart would start with; with none there it fails, saying the
// configuration file is not there.
func TestDryRunInAllocatorModeChecksTheCacheDirectory(t *testing.T) {
	dir := t.TempDir()
	out := runCheckCLI(t, "--allocator.url=http://pue-allocator:8080", "--allocator.cache-dir="+dir)
	if result := out.result(t, "config"); out.code != 1 || result.File != dir+"/config.yaml" || !strings.Contains(strings.Join(result.Errors, " "), "no such file") {
		t.Fatalf("exit=%d\n%s", out.code, out.stdout)
	}
}

// --allocator.advertise left out is the listen address's host when that is
// one address that is not a loopback one, or else the host's first address
// that is not, IPv4 before IPv6, with the listen port.
func TestTheDefaultAdvertisedAddress(t *testing.T) {
	saved := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = saved })
	nets := func(cidrs ...string) func() ([]net.Addr, error) {
		return func() ([]net.Addr, error) {
			var out []net.Addr
			for _, cidr := range cidrs {
				ip, ipNet, err := net.ParseCIDR(cidr)
				if err != nil {
					t.Fatal(err)
				}
				ipNet.IP = ip
				out = append(out, ipNet)
			}
			return out, nil
		}
	}
	for _, c := range []struct {
		listen string
		addrs  []string
		want   string
	}{
		{":8080", []string{"127.0.0.1/8", "::1/128", "fe80::1/64", "fd00::5/64", "10.1.2.3/24"}, "10.1.2.3:8080"},
		{"0.0.0.0:9000", []string{"127.0.0.1/8", "fd00::5/64"}, "[fd00::5]:9000"},
		{"10.9.9.9:8080", []string{"10.1.2.3/24"}, "10.9.9.9:8080"},
		{"127.0.0.1:8080", []string{"127.0.0.1/8", "192.168.1.4/24"}, "192.168.1.4:8080"},
		{":8080", []string{"127.0.0.1/8"}, ""},
	} {
		interfaceAddrs = nets(c.addrs...)
		got, err := defaultAdvertise(c.listen)
		if c.want == "" {
			if err == nil || !strings.Contains(err.Error(), "set --allocator.advertise") {
				t.Errorf("%s %v: %q %v", c.listen, c.addrs, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s %v: %q %v, want %q", c.listen, c.addrs, got, err, c.want)
		}
	}
	interfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("no interfaces") }
	if _, err := defaultAdvertise(":8080"); err == nil {
		t.Error("an error listing the addresses was not reported")
	}
}
