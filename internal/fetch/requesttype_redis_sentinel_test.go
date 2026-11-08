//go:build request_type_redis

package fetch

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/redistest"
)

// redisSentinelOf is a stand-in Sentinel that knows mymaster as the
// server at address, and what SENTINEL MASTER answers of it.
func redisSentinelOf(t *testing.T, address string) *redistest.Server {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := redistest.New(t)
	sentinel.Reply("SENTINEL GET-MASTER-ADDR-BY-NAME mymaster", "*2\r\n"+redistest.Bulk(host)+redistest.Bulk(port))
	var master strings.Builder
	fields := []string{"name", "mymaster", "ip", host, "port", port, "flags", "master", "num-slaves", "2", "num-other-sentinels", "2", "quorum", "2", "failover-timeout", "180000"}
	master.WriteString("*16\r\n")
	for _, field := range fields {
		master.WriteString(redistest.Bulk(field))
	}
	sentinel.Reply("SENTINEL MASTER mymaster", master.String())
	return sentinel
}

// withSentinel has a redis collector ask its target, a Sentinel, for the
// primary mymaster.
func withSentinel(s RedisSentinel) func(*model.Collector) {
	return func(c *model.Collector) { editRedis(c, func(r *RedisConfig) { r.Sentinel = &s }) }
}

// With request.sentinel, the target is a Sentinel: the scrape asks it which
// server is the primary of the master named, and what it knows of it, on a
// connection of its own, and reads that server, whose INFO the document
// holds beside the Sentinel's view of it, each number a JSON number and the
// address read. Each logs in with its own account: the Sentinel with
// sentinel's, the primary with the collector's.
func TestARedisCollectorAsksItsSentinelForThePrimary(t *testing.T) {
	primary := redistest.New(t)
	primary.SetInfo(redisInfo...)
	primary.RequirePassword("", "server-pass")
	sentinel := redisSentinelOf(t, primary.Addr)
	sentinel.RequirePassword("watcher", "sentinel-pass")
	dir := t.TempDir()
	serverFile, sentinelFile := filepath.Join(dir, "server"), filepath.Join(dir, "sentinel")
	for file, password := range map[string]string{serverFile: "server-pass", sentinelFile: "sentinel-pass"} {
		if err := os.WriteFile(file, []byte(password), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := redisCollector(t, func(c *model.Collector) { c.Request.PasswordFile = serverFile },
		withSentinel(RedisSentinel{Master: "mymaster", Username: "watcher", PasswordFile: sentinelFile}))
	body, err := redisScrape(t, c, sentinel.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info     map[string]map[string]any `json:"info"`
		Sentinel map[string]any            `json:"sentinel"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info["replication"]["role"] != "master" || doc.Sentinel["address"] != primary.Addr || doc.Sentinel["num-slaves"] != 2.0 ||
		doc.Sentinel["flags"] != "master" || doc.Sentinel["name"] != "mymaster" {
		t.Errorf("document %s", body)
	}
	if got := redisSent(sentinel); got != "AUTH watcher sentinel-pass|SENTINEL GET-MASTER-ADDR-BY-NAME mymaster|SENTINEL MASTER mymaster" {
		t.Errorf("the Sentinel was sent %s", got)
	}
	if got := redisSent(primary); got != "AUTH server-pass|INFO" {
		t.Errorf("the primary was sent %s", got)
	}
}

// redisSent is every command a stand-in server was sent, joined by |.
func redisSent(s *redistest.Server) string {
	var sent []string
	for _, command := range s.Commands() {
		sent = append(sent, strings.Join(command, " "))
	}
	return strings.Join(sent, "|")
}

// A master the Sentinel does not know, which it answers with a nil, fails
// the scrape saying so, and an answer that is no address, as does one
// that is no address and port.
func TestARedisSentinelsUnknownMasterFailsTheScrape(t *testing.T) {
	sentinel := redistest.New(t)
	c := redisCollector(t, withSentinel(RedisSentinel{Master: "mymaster"}))
	for reply, want := range map[string]string{
		"*-1\r\n":                      `knows no primary named "mymaster"`,
		"*1\r\n" + redistest.Bulk("x"): "not the primary's address and port",
		"*2\r\n" + redistest.Bulk("h") + redistest.Bulk("0"): "the Sentinel answered an address that is none",
	} {
		sentinel.Reply("SENTINEL GET-MASTER-ADDR-BY-NAME mymaster", reply)
		if _, err := redisScrape(t, c, sentinel.Addr, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %s", reply, err, want)
		}
	}
}

// The primary a Sentinel names is held to the target lists as the target
// is: one the lists refuse is never dialed.
func TestTheRedisPrimaryASentinelNamesIsHeldToTheTargetLists(t *testing.T) {
	primary := redistest.New(t)
	primary.SetInfo(redisInfo...)
	sentinel := redisSentinelOf(t, "169.254.169.254:6379")
	if _, err := redisScrape(t, redisCollector(t, withSentinel(RedisSentinel{Master: "mymaster"})), sentinel.Addr, nil); !errors.Is(err, ErrTargetRefused) || !strings.Contains(err.Error(), "the primary 169.254.169.254:6379 the Sentinel named") {
		t.Errorf("the metadata service as primary: %v", err)
	}
	if got := redisSent(primary); got != "" {
		t.Errorf("a refused primary was sent %s", got)
	}
}

// The Sentinel's replies and the primary's are held to the response limit
// together: a limit both fit alone but not together fails the scrape.
func TestARedisSentinelsRepliesCountTowardTheResponseLimit(t *testing.T) {
	primary := redistest.New(t)
	primary.SetInfo(redistest.Section{Name: "Server", Lines: []string{"redis_version:7.2.4", "x:" + strings.Repeat("y", 150)}})
	sentinel := redisSentinelOf(t, primary.Addr)
	limited := func(limit model.ByteSize) *model.Collector {
		return redisCollector(t, withSentinel(RedisSentinel{Master: "mymaster"}), func(c *model.Collector) { c.Request.MaxResponseBytes = limit })
	}
	if _, err := redisScrape(t, limited(4096), sentinel.Addr, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := redisScrape(t, limited(300), sentinel.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("the Sentinel's replies and INFO past the limit together: %v", err)
	}
}

// request.sentinel names its master, without a blank at an end, a control
// character or a placeholder, and a username beside its password file.
func TestARedisSentinelsKeysAreChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		sentinel RedisSentinel
		want     string
	}{
		"no master":            {RedisSentinel{}, "request.sentinel names no master"},
		"a blank at an end":    {RedisSentinel{Master: "mymaster "}, "has a blank at an end"},
		"a placeholder":        {RedisSentinel{Master: "{{param_m}}"}, "has a placeholder"},
		"a username alone":     {RedisSentinel{Master: "m", Username: "watcher"}, "request.sentinel.username without request.sentinel.password_file"},
		"a username of blanks": {RedisSentinel{Master: "m", Username: "a b", PasswordFile: "/p"}, "request.sentinel.username \"a b\" is not an ACL user's name"},
		"a file of blanks":     {RedisSentinel{Master: "m", PasswordFile: " "}, "request.sentinel.password_file is blanks alone"},
	} {
		c := &model.Collector{Name: "cache", Request: model.RequestConfig{Type: RequestTypeRedis}}
		withSentinel(tc.sentinel)(c)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
}

// --dry-run reads the Sentinel's password file, as a scrape would.
func TestADryRunReadsTheRedisSentinelsPasswordFile(t *testing.T) {
	c := redisCollector(t, withSentinel(RedisSentinel{Master: "m", PasswordFile: filepath.Join(t.TempDir(), "missing")}))
	if warnings := FileWarnings(c, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.sentinel.password_file") {
		t.Errorf("warnings %q", warnings)
	}
}
