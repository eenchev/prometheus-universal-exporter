//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A probe's sql target that ends in a colon, with no port after it, fails
// the scrape saying what to write, and makes no connection; an IPv6
// address keeps its brackets wherever the target is written again, without
// a port too, as a collector whose driver names none has it.
func TestASQLTargetWithAnEmptyPortIsRefusedAndIPv6KeepsItsBrackets(t *testing.T) {
	f := newSQLFixture(t)
	c := f.collector(t, "db", SQLDriverPostgres, "q")
	for _, target := range []string{"db.example:", "[::1]:", "[2001:db8::1]:"} {
		want := `target "` + target + `" ends in a colon with no port after it; write the port after the colon, or leave the colon out for the driver's port`
		if _, err := FetchCollector(context.Background(), target, c, RequestOverrides{}, nil); err == nil || err.Error() != want {
			t.Errorf("%s: %v\nwant %s", target, err, want)
		}
	}
	if opened := f.db.Opened(); opened != 0 {
		t.Errorf("%d connections made to targets with no port", opened)
	}
	unknown := &model.Collector{Name: "x", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: "db2"}}
	for target, want := range map[string]string{"[::1]": "[::1]", "[2001:DB8::1]": "[2001:db8::1]", "[::1]:7": "[::1]:7", "db.example": "db.example"} {
		if address, err := parseSQLTarget(unknown, target); err != nil || address.hostPort != want {
			t.Errorf("%s: %q %v, want %q", target, address.hostPort, err, want)
		}
	}
}

// Every target but one ending in a colon, and an IPv6 address written
// without a port for a driver that names none, is read as it was: the same
// address or the same error, over a table of the ways a target is written,
// for a driver with a port and one without.
func TestASQLTargetIsReadAsItWasButForAnEmptyPort(t *testing.T) {
	f := newSQLFixture(t)
	collectors := []*model.Collector{
		f.collector(t, "db", SQLDriverPostgres, "q"),
		{Name: "x", Request: model.RequestConfig{Type: RequestTypeSQL, Driver: "db2"}},
	}
	var targets []string
	for _, host := range []string{"db.example", "DB.Example.", "10.0.0.1", "[::1]", "::1", "[2001:db8::1]", "[fe80::1%eth0]", "", " db ", "a:b:c", "x@y", "h/p", "[::1"} {
		targets = append(targets, host)
		for _, port := range []string{"5432", "0", "65536", "05432", "x", "-1", " 1"} {
			targets = append(targets, host+":"+port)
		}
	}
	for _, c := range collectors {
		for _, target := range targets {
			got, err := parseSQLTarget(c, target)
			want, wantErr := parseSQLTargetAsItWas(c, target)
			if strings.HasSuffix(strings.TrimSpace(target), ":") || want.port == "" && strings.Contains(want.host, ":") {
				continue
			}
			if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Errorf("%s %q: %+v %v, as it was %+v %v", c.Request.Driver, target, got, err, want, wantErr)
			}
		}
	}
}

// parseSQLTargetAsItWas is parseSQLTarget before a target ending in a colon
// was refused and an IPv6 address without a port kept its brackets.
func parseSQLTargetAsItWas(c *model.Collector, raw string) (sqlAddress, error) {
	raw = strings.TrimSpace(raw)
	// A collector that was never validated may name no driver this build
	// has; its target is then read as a host and a port, the port left
	// out where it names none.
	d := sqlDriverOf(c)
	if d == nil {
		d = &sqlDriver{port: func(map[string]string) string { return "" }}
	}
	if strings.ContainsAny(raw, "/?#@") {
		return sqlAddress{}, fmt.Errorf("target %q has a scheme, path, query or user in it; a sql target is only the server, host or host:port: the account is request.username and the database request.database", raw)
	}
	if d.account {
		account := strings.ToLower(raw)
		if !sqlAccount.MatchString(account) {
			return sqlAddress{}, fmt.Errorf("target %q is not a Snowflake account identifier; write the account as myorg-myaccount, or its locator as xy12345.eu-west-1", raw)
		}
		host := d.host(account)
		port := d.port(c.Request.Options)
		return sqlAddress{host: host, port: port, hostPort: net.JoinHostPort(host, port), account: account}, nil
	}
	host, port := raw, d.port(c.Request.Options)
	if h, p, err := net.SplitHostPort(raw); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		host = raw[1 : len(raw)-1]
	} else if strings.Count(raw, ":") == 1 {
		return sqlAddress{}, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:5432", raw)
	}
	if host == "" {
		return sqlAddress{}, fmt.Errorf("target %q names no host; a sql target is host or host:port", raw)
	}
	if n, err := strconv.Atoi(port); port != "" && (err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port) {
		return sqlAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return sqlAddress{}, err
	}
	hostPort := canonical
	if port != "" {
		hostPort = net.JoinHostPort(canonical, port)
	}
	return sqlAddress{host: canonical, port: port, hostPort: hostPort}, nil
}
