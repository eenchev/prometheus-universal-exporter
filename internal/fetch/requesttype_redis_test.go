//go:build request_type_redis

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/redistest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// redisCollector is a validated redis collector, as edit makes it.
func redisCollector(t *testing.T, edit ...func(*model.Collector)) *model.Collector {
	t.Helper()
	c := &model.Collector{Name: "cache", Request: model.RequestConfig{Type: RequestTypeRedis}}
	for _, e := range edit {
		e(c)
	}
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// redisScrape is a scrape of target through c, with the probe's
// parameters, and its document as JSON.
func redisScrape(t *testing.T, c *model.Collector, target string, params map[string]string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{Params: params}, nil)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// redisInfo is INFO as a Redis 7 server writes its sections, cut short.
var redisInfo = []redistest.Section{
	{Name: "Server", Lines: []string{"redis_version:7.2.4", "redis_git_sha1:00000000", "redis_mode:standalone", "uptime_in_seconds:86400", "executable:/usr/local/bin/redis-server"}},
	{Name: "Clients", Lines: []string{"connected_clients:12", "blocked_clients:0"}},
	{Name: "Memory", Lines: []string{"used_memory:1048576", "used_memory_human:1.00M", "maxmemory:4194304", "used_memory_peak_perc:55.12%", "mem_fragmentation_ratio:1.25", "maxmemory_policy:noeviction"}},
	{Name: "Stats", Lines: []string{"keyspace_hits:900", "keyspace_misses:100", "total_net_input_bytes:18446744073709551615"}},
	{Name: "Replication", Lines: []string{"role:master", "connected_slaves:1", "slave0:ip=10.0.0.2,port=6379,state=online,offset=1234,lag=0", "master_repl_offset:1240"}},
	{Name: "Keyspace", Lines: []string{"db0:keys=12,expires=3,avg_ttl=1500", "db2:keys=1,expires=0,avg_ttl=0"}},
}

// A redis collector with nothing but its type asks the server for INFO
// and answers its sections as a document: each section by its name in
// lower case, a number as a JSON number written as the server wrote it,
// however large, other text as text, and a line of name=value pairs, a
// keyspace's or a replica's, as an object of them.
func TestARedisCollectorAnswersINFOAsADocument(t *testing.T) {
	server := redistest.New(t)
	server.SetInfo(redisInfo...)
	body, err := redisScrape(t, redisCollector(t), server.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"used_memory":1048576`, `"mem_fragmentation_ratio":1.25`, `"total_net_input_bytes":18446744073709551615`,
		`"redis_version":"7.2.4"`, `"redis_git_sha1":"00000000"`, `"used_memory_peak_perc":"55.12%"`, `"used_memory_human":"1.00M"`,
		`"db0":{"avg_ttl":1500,"expires":3,"keys":12}`, `"slave0":{"ip":"10.0.0.2","lag":0,"offset":1234,"port":6379,"state":"online"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the document has no %s:\n%s", want, body)
		}
	}
	var doc struct {
		Info     map[string]map[string]any `json:"info"`
		Commands map[string]any            `json:"commands"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Info) != 6 || doc.Info["clients"]["connected_clients"] != 12.0 || doc.Info["replication"]["role"] != "master" || doc.Commands != nil {
		t.Errorf("document %s", body)
	}
	if got := server.Commands(); len(got) != 1 || strings.Join(got[0], " ") != "INFO" {
		t.Errorf("sent %q", got)
	}
}

// info_sections asks for each section with an INFO of its own, which every
// version of Redis takes, and the document holds those alone; a section the
// server does not know, which it answers with nothing, fails the scrape.
func TestARedisCollectorAsksForItsSectionsOneByOne(t *testing.T) {
	server := redistest.New(t)
	server.SetInfo(redisInfo...)
	body, err := redisScrape(t, redisCollector(t, func(c *model.Collector) { c.Request.InfoSections = []string{"memory", "keyspace"} }), server.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Info map[string]map[string]any }
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Info) != 2 || doc.Info["memory"]["maxmemory"] != 4194304.0 || doc.Info["keyspace"]["db2"] == nil {
		t.Errorf("document %s", body)
	}
	if got := server.Commands(); len(got) != 2 || strings.Join(got[0], " ") != "INFO memory" || strings.Join(got[1], " ") != "INFO keyspace" {
		t.Errorf("sent %q", got)
	}
	_, err = redisScrape(t, redisCollector(t, func(c *model.Collector) { c.Request.InfoSections = []string{"memroy"} }), server.Addr, nil)
	if err == nil || !strings.Contains(err.Error(), "INFO memroy: the server answered no section") {
		t.Errorf("an unknown section: %v", err)
	}
}

// With password_file the scrape logs in first, AUTH password for the
// default user and AUTH username password for an ACL user, the file read
// at every scrape; a password the server refuses fails the scrape with the
// server's words and never the password, and so does a server that wants
// one when the collector has none.
func TestARedisCollectorLogsInWithItsPasswordFile(t *testing.T) {
	server := redistest.New(t)
	server.SetInfo(redisInfo...)
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := redisScrape(t, redisCollector(t), server.Addr, nil); err != nil {
		t.Fatal(err)
	}
	server.RequirePassword("", "s3cret")
	if _, err := redisScrape(t, redisCollector(t), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "NOAUTH Authentication required") {
		t.Errorf("without a password: %v", err)
	}
	defaultUser := redisCollector(t, func(c *model.Collector) { c.Request.PasswordFile = passwordFile })
	if _, err := redisScrape(t, defaultUser, server.Addr, nil); err != nil {
		t.Fatal(err)
	}
	server.RequirePassword("exporter", "s3cret")
	aclUser := redisCollector(t, func(c *model.Collector) { c.Request.Username, c.Request.PasswordFile = "exporter", passwordFile })
	if _, err := redisScrape(t, aclUser, server.Addr, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordFile, []byte("wrong-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := redisScrape(t, aclUser, server.Addr, nil)
	if err == nil || !strings.Contains(err.Error(), "AUTH exporter ***: the server answered: WRONGPASS") || strings.Contains(err.Error(), "wrong-one") {
		t.Errorf("a wrong password: %v", err)
	}
	var auths []string
	for _, command := range server.Commands() {
		if command[0] == "AUTH" {
			auths = append(auths, strings.Join(command, " "))
		}
	}
	if strings.Join(auths, "|") != "AUTH s3cret|AUTH exporter s3cret|AUTH exporter wrong-one" {
		t.Errorf("logged in with %q", auths)
	}
}

// db selects its database after the login, and db 0, the one a
// connection starts in, sends no SELECT; a database the server does not
// have fails the scrape.
func TestARedisCollectorSelectsItsDatabase(t *testing.T) {
	server := redistest.New(t)
	server.SetInfo(redisInfo...)
	if _, err := redisScrape(t, redisCollector(t, func(c *model.Collector) { c.Request.DB = 3 }), server.Addr, nil); err != nil {
		t.Fatal(err)
	}
	if got := server.Commands(); len(got) != 2 || strings.Join(got[0], " ") != "SELECT 3" {
		t.Errorf("sent %q", got)
	}
	if _, err := redisScrape(t, redisCollector(t, func(c *model.Collector) { c.Request.DB = 20 }), server.Addr, nil); err == nil || !strings.Contains(err.Error(), "SELECT 20: the server answered: ERR DB index is out of range") {
		t.Errorf("a database out of range: %v", err)
	}
	label, err := RequestLabelFor(server.Addr, redisCollector(t, func(c *model.Collector) { c.Request.DB = 3 }), RequestOverrides{})
	if err != nil || label != "redis://"+server.Addr+"/3" {
		t.Errorf("label %q, %v", label, err)
	}
}

// Each command's reply is put under its name, kind by kind: an integer as
// a number, a bulk string as text, nil as null, an array as a list; the
// replies of HGETALL and XINFO STREAM, flat lists of names and values, as
// objects, those of XINFO GROUPS as a list of objects, and CLUSTER INFO's
// lines as an object. A placeholder in an argument is filled from the
// probe, and with commands and no info_sections, no INFO is sent.
func TestARedisCollectorPutsEachCommandsReplyUnderItsName(t *testing.T) {
	server := redistest.New(t)
	server.Reply("LLEN jobs:emails", ":42\r\n")
	server.Reply("GET counter", redistest.Bulk("17"))
	server.Reply("GET missing", "$-1\r\n")
	server.Reply("HGETALL stats", "*4\r\n"+redistest.Bulk("sent")+redistest.Bulk("10")+redistest.Bulk("failed")+redistest.Bulk("2"))
	server.Reply("XINFO GROUPS events", "*1\r\n*4\r\n"+redistest.Bulk("name")+redistest.Bulk("workers")+redistest.Bulk("pending")+":5\r\n")
	server.Reply("ROLE", "*3\r\n"+redistest.Bulk("master")+":1240\r\n*1\r\n*3\r\n"+redistest.Bulk("10.0.0.2")+redistest.Bulk("6379")+redistest.Bulk("1234")+"")
	server.Reply("CLUSTER INFO", redistest.Bulk("cluster_state:ok\r\ncluster_slots_assigned:16384\r\n"))
	c := redisCollector(t, func(c *model.Collector) {
		c.Request.Commands = []model.RedisCommand{
			{Name: "queue", Command: []string{"llen", "jobs:{{param_queue}}"}},
			{Name: "counter", Command: []string{"GET", "counter"}},
			{Name: "missing", Command: []string{"GET", "missing"}},
			{Name: "stats", Command: []string{"HGETALL", "stats"}},
			{Name: "groups", Command: []string{"XINFO", "groups", "events"}},
			{Name: "role", Command: []string{"ROLE"}},
			{Name: "cluster", Command: []string{"CLUSTER", "INFO"}},
		}
	})
	body, err := redisScrape(t, c, server.Addr, map[string]string{"param_queue": "emails"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"commands":{"cluster":{"cluster_slots_assigned":16384,"cluster_state":"ok"},"counter":"17","groups":[{"name":"workers","pending":5}],"missing":null,"queue":42,"role":["master",1240,[["10.0.0.2","6379","1234"]]],"stats":{"failed":"2","sent":"10"}}}`
	if body != want {
		t.Errorf("document\n%s\nwant\n%s", body, want)
	}
	for _, command := range server.Commands() {
		if command[0] == "INFO" {
			t.Errorf("sent INFO without info_sections: %q", server.Commands())
		}
	}
	if method := RequestMethodFor(c, RequestOverrides{}); method != "COMMANDS" {
		t.Errorf("method %q", method)
	}
	if method := RequestMethodFor(redisCollector(t), RequestOverrides{}); method != "INFO" {
		t.Errorf("method %q", method)
	}
	both := redisCollector(t, func(c *model.Collector) {
		c.Request.InfoSections = []string{"memory"}
		c.Request.Commands = []model.RedisCommand{{Name: "role", Command: []string{"ROLE"}}}
	})
	if method := RequestMethodFor(both, RequestOverrides{}); method != "INFO+COMMANDS" {
		t.Errorf("the method of info_sections and commands together: %q", method)
	}
	if err := CheckPathParams(c, RequestOverrides{Params: map[string]string{"param_queue": "a", "param_other": "b"}}); err == nil || !strings.Contains(err.Error(), "param_other") || !strings.Contains(err.Error(), "request.commands") {
		t.Errorf("an unused parameter: %v", err)
	}
	server.Reply("LLEN jobs:emails", "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n")
	if _, err := redisScrape(t, c, server.Addr, map[string]string{"param_queue": "emails"}); err == nil || !strings.Contains(err.Error(), "LLEN jobs:emails: the server answered: WRONGTYPE") {
		t.Errorf("an error reply: %v", err)
	}
}

// A collector's keys are checked when it loads: only the read-only
// commands of the list, with the arguments they take and no placeholder in
// their name, each under a name of its own; a username needs a password
// file; a database is numbered from 0; a section of INFO is a word in
// lower case. A target is host, host:port or a redis:// or rediss:// URL
// of one, and redis:// contradicts a tls block.
func TestARedisCollectorsKeysAndTargetsAreChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*model.Collector)
		want string
	}{
		"a command that writes": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"SET", "a", "1"}}}
		}, "sends SET, which is not among the read-only commands"},
		"CONFIG GET": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"config", "get", "requirepass"}}}
		}, "sends CONFIG, which is not among"},
		"CLIENT LIST": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"CLIENT", "LIST"}}}
		}, "sends CLIENT, which is not among"},
		"KEYS": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"KEYS", "*"}}}
		}, "sends KEYS"},
		"XINFO STREAM FULL": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"XINFO", "STREAM", "s", "FULL"}}}
		}, "gives XINFO STREAM 2 arguments; it takes 1"},
		"too few arguments": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"LLEN"}}}
		}, "gives LLEN 0 arguments; it takes 1"},
		"a placeholder as command": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"{{param_c}}", "a"}}}
		}, "a placeholder in its command's name"},
		"a placeholder as subcommand": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"XINFO", "{{param_c}}", "a"}}}
		}, "a placeholder in its command's subcommand"},
		"a broken placeholder": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"LLEN", "{{param_"}}}
		}, "request.commands[0].command[1]"},
		"no command": {func(c *model.Collector) { c.Request.Commands = []model.RedisCommand{{Name: "x"}} }, "has no command"},
		"a name twice": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "x", Command: []string{"DBSIZE"}}, {Name: "x", Command: []string{"DBSIZE"}}}
		}, "names x twice"},
		"a name jq cannot reach": {func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "queue-len", Command: []string{"DBSIZE"}}}
		}, `request.commands[0].name "queue-len"`},
		"a username alone":          {func(c *model.Collector) { c.Request.Username = "exporter" }, "without request.password_file"},
		"a negative database":       {func(c *model.Collector) { c.Request.DB = -1 }, "request.db is -1"},
		"a section in capitals":     {func(c *model.Collector) { c.Request.InfoSections = []string{"Memory"} }, `request.info_sections[0] "Memory"`},
		"a section twice":           {func(c *model.Collector) { c.Request.InfoSections = []string{"memory", "memory"} }, "names memory twice"},
		"a key of another type":     {func(c *model.Collector) { c.Request.Statement = "SELECT 1" }, "sets request.statement, which does not apply"},
		"half a client certificate": {func(c *model.Collector) { c.Request.TLS.CertFile = "/c.pem" }, "only one of cert_file and key_file"},
	} {
		c := &model.Collector{Name: "cache", Request: model.RequestConfig{Type: RequestTypeRedis}}
		tc.edit(c)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	plain := redisCollector(t)
	withTLS := redisCollector(t, func(c *model.Collector) { c.Request.TLS.CAFile = "/ca.pem" })
	for target, want := range map[string]string{
		"cache.internal":            "",
		"cache.internal:6380":       "",
		"[2001:db8::1]:6379":        "",
		"redis://cache.internal":    "",
		"rediss://cache.internal/":  "",
		"http://cache.internal":     "has the scheme http://",
		"redis://user@cache":        "has a path, query or user",
		"cache.internal/2":          "has a path, query or user",
		"cache.internal:0":          `has the port "0"`,
		"2001:db8::1":               "",
		"cache.internal:redis":      `has the port "redis"`,
		"":                          "target is required",
		"redis://":                  "names no host",
		"cache internal":            "",
		"rediss://cache.internal:1": "",
	} {
		err := CheckTarget(plain, target, false)
		if target == "cache internal" {
			if err == nil {
				t.Errorf("%q: taken", target)
			}
			continue
		}
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", target, err, want)
		}
	}
	if err := CheckTarget(withTLS, "redis://cache.internal", false); err == nil || !strings.Contains(err.Error(), "asks for plaintext with redis://, but the collector sets request.tls") {
		t.Errorf("redis:// with a tls block: %v", err)
	}
	if label, err := RequestLabelFor("cache.internal", withTLS, RequestOverrides{}); err != nil || label != "rediss://cache.internal:6379/0" {
		t.Errorf("label %q, %v", label, err)
	}
}

// A scrape with a tls block, or of a rediss:// target, speaks TLS and
// verifies the server's certificate against ca_file; one that does not
// verify fails the scrape.
func TestARedisCollectorSpeaksTLS(t *testing.T) {
	server := redistest.NewTLS(t)
	server.SetInfo(redisInfo...)
	verified := redisCollector(t, func(c *model.Collector) { c.Request.TLS.CAFile = server.CAFile })
	for _, target := range []string{server.Addr, "rediss://" + server.Addr} {
		if _, err := redisScrape(t, verified, target, nil); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}
	if _, err := redisScrape(t, redisCollector(t), "rediss://"+server.Addr, nil); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+server.Addr) {
		t.Errorf("an unknown authority: %v", err)
	}
	if _, err := redisScrape(t, redisCollector(t), server.Addr, nil); err == nil {
		t.Error("plaintext to a TLS server was taken")
	}
}

// allowed_targets and denied_targets are kept to, and the cloud metadata
// service is refused without them: a refused target is never connected to.
func TestARedisCollectorKeepsToItsTargetLists(t *testing.T) {
	if _, err := redisScrape(t, redisCollector(t), "169.254.169.254:6379", nil); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("the metadata service: %v", err)
	}
	server := redistest.New(t)
	server.SetInfo(redisInfo...)
	for name, edit := range map[string]func(*model.Collector){
		"denied":      func(c *model.Collector) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} },
		"not allowed": func(c *model.Collector) { c.Request.AllowedTargets = []string{"10.0.0.0/8"} },
	} {
		if _, err := redisScrape(t, redisCollector(t, edit), server.Addr, nil); !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := server.Commands(); len(got) != 0 {
		t.Errorf("a refused target was sent %q", got)
	}
	if _, err := redisScrape(t, redisCollector(t, func(c *model.Collector) { c.Request.AllowedTargets = []string{"127.0.0.0/8"} }), server.Addr, nil); err != nil {
		t.Errorf("allowed: %v", err)
	}
}

// What a server answers is held to the response limit as it is read: the
// replies of a scrape together, a bulk string whose length is past what is
// left before a byte of it is read, an array whose elements could not fit,
// and arrays nested past the bound. A bulk string's length the server does
// not go on to send costs only the bytes that came.
func TestARedisServersRepliesAreHeldToTheResponseLimit(t *testing.T) {
	server := redistest.New(t)
	server.SetInfo(redisInfo...)
	small := redisCollector(t, func(c *model.Collector) { c.Request.MaxResponseBytes = 100 })
	if _, err := redisScrape(t, small, server.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("INFO past the limit: %v", err)
	}
	commands := func(limit model.ByteSize) *model.Collector {
		return redisCollector(t, func(c *model.Collector) {
			c.Request.MaxResponseBytes = limit
			c.Request.Commands = []model.RedisCommand{{Name: "v", Command: []string{"GET", "v"}}}
		})
	}
	for name, tc := range map[string]struct {
		reply string
		want  error
		text  string
	}{
		"a bulk string past the limit": {"$1000000000\r\n", model.ErrLimitExceeded, "larger than the response limit"},
		"an array past the limit":      {"*1000000000\r\n", model.ErrLimitExceeded, "larger than the response limit"},
		"arrays nested too deep":       {strings.Repeat("*1\r\n", 20) + ":1\r\n", errRedisProtocol, "nest more than 16 deep"},
		"a length that is no number":   {"$abc\r\n", errRedisProtocol, `the length "abc"`},
		"no RESP at all":               {"HTTP/1.1 400 Bad Request\r\n", errRedisProtocol, "is the target a Redis or Valkey server?"},
		"a line past its bound":        {"+" + strings.Repeat("x", redisMaxLine) + "\r\n", errRedisProtocol, "longer than"},
	} {
		server.Reply("GET v", tc.reply)
		_, err := redisScrape(t, commands(0), server.Addr, nil)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A limit large enough to take the length the server declares: what is
	// allocated is what came, not what was declared.
	server.Reply("GET v", "$1000000000\r\nonly this")
	// The server sends no more and keeps the connection, so the scrape
	// ends at its deadline.
	var err error
	allocated := alloctest.BytesAtMost(1, 32<<20, func() {
		_, err = FetchCollector(context.Background(), server.Addr, commands(2<<30), RequestOverrides{Timeout: 300 * time.Millisecond}, nil)
	})
	if err == nil {
		t.Error("a bulk string cut short was taken")
	}
	if allocated > 32<<20 {
		t.Errorf("a declared length of 1 GB allocated %d bytes", allocated)
	}
}

// A server that never answers fails the scrape at its deadline.
func TestARedisServerThatNeverAnswersFailsAtTheDeadline(t *testing.T) {
	server := redistest.New(t)
	release := make(chan struct{})
	defer close(release)
	server.Answer(func([]string) (string, bool) {
		<-release
		return "", false
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := FetchCollector(ctx, server.Addr, redisCollector(t), RequestOverrides{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "INFO") {
		t.Errorf("err %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s", elapsed)
	}
	// The probe's own timeout bounds it too.
	start = time.Now()
	_, err = FetchCollector(context.Background(), server.Addr, redisCollector(t), RequestOverrides{Timeout: 200 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Errorf("with a probe timeout: %v after %s", err, time.Since(start))
	}
}

// A debug probe's request section shows each command sent, AUTH with its
// password as ***, and how each was answered; --dry-run reads the password
// file as a scrape would.
func TestARedisScrapeIsTracedWithoutItsPassword(t *testing.T) {
	server := redistest.New(t)
	server.RequirePassword("exporter", "s3cret")
	server.Reply("LLEN jobs", ":42\r\n")
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := redisCollector(t, func(c *model.Collector) {
		c.Request.Username, c.Request.PasswordFile, c.Request.DB = "exporter", passwordFile, 1
		c.Request.Commands = []model.RedisCommand{{Name: "jobs", Command: []string{"LLEN", "jobs"}}}
	})
	ctx, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(ctx, server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range trace.Requests() {
		got = append(got, r.Method+" "+r.URL+" "+r.Header.Get("Redis-Command")+" -> "+r.Outcome)
	}
	label := "redis://" + server.Addr + "/1"
	want := []string{"AUTH " + label + " AUTH exporter *** -> the simple string OK", "SELECT " + label + " SELECT 1 -> the simple string OK", "LLEN " + label + " LLEN jobs -> the integer 42"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("traced\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}
	missing := redisCollector(t, func(c *model.Collector) { c.Request.PasswordFile = filepath.Join(t.TempDir(), "missing") })
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.password_file") {
		t.Errorf("warnings %v", warnings)
	}
}

// A connection refused fails the scrape and names the address.
func TestARedisConnectionRefusedFailsTheScrape(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	if _, err := redisScrape(t, redisCollector(t), addr, nil); err == nil || !strings.Contains(err.Error(), "connecting to "+addr) {
		t.Errorf("err %v", err)
	}
}

// optInRequestKeys lists the keys redis accepts that no type of a default
// build does: db, info_sections and commands (checkOptInRequestKeys).
func TestTheOptInRequestKeysAreRedissOwn(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeRedis)
}

// A reply holding bytes that are not UTF-8 fails the scrape naming the
// command, never put in the document with U+FFFD in their place: a bulk
// string, a name of HGETALL's pairs, CLUSTER INFO's text and INFO's.
func TestARedisReplyThatIsNotTextFailsTheScrape(t *testing.T) {
	server := redistest.New(t)
	server.SetInfo(redistest.Section{Name: "Server", Lines: []string{"redis_version:7.2.4", "os:\xff\xfe"}})
	server.Reply("GET v", redistest.Bulk("ok\xffok"))
	server.Reply("HGETALL h", "*2\r\n"+redistest.Bulk("\xc3")+redistest.Bulk("1"))
	server.Reply("CLUSTER INFO", redistest.Bulk("cluster_state:\xff\r\n"))
	for command, words := range map[string][]string{"GET v": {"GET", "v"}, "HGETALL h": {"HGETALL", "h"}, "CLUSTER INFO": {"CLUSTER", "INFO"}} {
		c := redisCollector(t, func(c *model.Collector) {
			c.Request.Commands = []model.RedisCommand{{Name: "v", Command: words}}
		})
		_, err := redisScrape(t, c, server.Addr, nil)
		if !errors.Is(err, errRedisNotText) || !strings.Contains(err.Error(), command) {
			t.Errorf("%s: %v", command, err)
		}
	}
	if _, err := redisScrape(t, redisCollector(t), server.Addr, nil); !errors.Is(err, errRedisNotText) || !strings.Contains(err.Error(), "INFO") {
		t.Errorf("INFO: %v", err)
	}
	server.Reply("GET v", redistest.Bulk("caf\u00e9"))
	c := redisCollector(t, func(c *model.Collector) {
		c.Request.Commands = []model.RedisCommand{{Name: "v", Command: []string{"GET", "v"}}}
	})
	if body, err := redisScrape(t, c, server.Addr, nil); err != nil || body != "{\"commands\":{\"v\":\"caf\u00e9\"}}" {
		t.Errorf("UTF-8 text: %s %v", body, err)
	}
}

// The document is held to the response limit as the replies are: replies
// within it whose control characters JSON writes six bytes each fail the
// scrape as a limit exceeded, and the same length of plain text is taken.
func TestARedisDocumentPastTheResponseLimitFailsTheScrape(t *testing.T) {
	server := redistest.New(t)
	c := redisCollector(t, func(c *model.Collector) {
		c.Request.MaxResponseBytes = 1000
		c.Request.Commands = []model.RedisCommand{{Name: "v", Command: []string{"GET", "v"}}}
	})
	server.Reply("GET v", redistest.Bulk(strings.Repeat("\x01", 400)))
	if _, err := redisScrape(t, c, server.Addr, nil); !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "as JSON") {
		t.Errorf("a document past the limit: %v", err)
	}
	server.Reply("GET v", redistest.Bulk(strings.Repeat("x", 400)))
	if _, err := redisScrape(t, c, server.Addr, nil); err != nil {
		t.Errorf("plain text within the limit: %v", err)
	}
}
