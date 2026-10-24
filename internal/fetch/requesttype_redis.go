//go:build request_type_redis

package fetch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The redis request type reads a Redis or Valkey server, which speak the
// same protocol: its INFO, and the replies of read-only commands the
// configuration lists, handed to the transforms as one JSON document.
//
//	request:
//	  type: redis
//	  username: exporter                       # optional: an ACL user
//	  password_file: /etc/exporter/redis-pass  # optional: AUTH
//	  db: 0                                    # optional: SELECT
//	  info_sections: [server, memory]          # optional: INFO's default sections when left out
//	  commands:                                # optional: read-only commands only
//	    - name: queue_len
//	      command: [LLEN, "jobs:{{param_queue}}"]
//
// The document is {"info": {section: {name: value}}, "commands": {name:
// reply}}: info when INFO was sent, which it is unless commands are listed
// and info_sections is not, commands when commands are listed. A value of
// INFO that is a number is a JSON number, written as the server wrote it,
// and one made of name=value pairs, as a keyspace line is, an object
// (redisresp.go).
//
// The probe's target is host, host:port, [v6]:port, or a redis:// or
// rediss:// URL of one, port 6379 when it is left out; rediss://, or a tls
// block, asks for TLS, and redis:// with a tls block is refused. The
// exporter dials the target through the target policy, as every type that
// reaches a server does, and makes one connection for each scrape, which is
// closed when the scrape is over: a scrape sends a few commands at most
// seconds apart, a connection costs a TCP handshake and AUTH, TLS's
// handshake besides, and a connection kept between scrapes would need its
// own keepalive, idle timeout and limit, as sshfile's and sql's do, for
// none of which there is a need here.
//
// Only the commands of redisAllowedCommands may be listed: commands that
// read, whose reply is bounded by the key they read, and that reveal no
// secret or other client of the server, which CONFIG GET and CLIENT LIST
// would. The password is read from password_file at every scrape, as
// every credential file is, and never shown: a debug report shows AUTH as
// AUTH user ***.
//
// The type is opt-in, built under its tag alone, and imports nothing
// outside the standard library: the client is the RESP2 of redisresp.go.

func init() {
	registerRequestType(&RequestType{
		Name:               RequestTypeRedis,
		Fields:             []string{"username", "password_file", "db", "info_sections", "commands", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Overrides:          []string{"timeout", PathParamPrefix},
		TargetFields:       []string{"timeout", "username", "password_file", "tls"},
		Validate:           validateRedisRequest,
		Fetch:              fetchRedis,
		CheckTarget:        checkRedisTarget,
		CheckTargetRequest: checkRedisTargetRequest,
		Label:              redisLabel,
		Method:             redisMethod,
		Display:            redisDisplay,
		Stage:              "redis",
		CheckFiles:         checkRedisFiles,
	})
}

// redisDefaultPort is the port of a target that names none.
const redisDefaultPort = "6379"

// redisTimeout bounds a scrape with no deadline of its own.
const redisTimeout = 30 * time.Second

// The http_method labels of a scrape: INFO when it sends INFO alone,
// COMMANDS when it sends the configuration's commands alone, and
// INFO+COMMANDS when it sends info_sections' INFOs and the commands.
const (
	redisMethodInfo     = "INFO"
	redisMethodCommands = "COMMANDS"
	redisMethodBoth     = "INFO+COMMANDS"
)

// redisCommandRule is what a command of request.commands may be sent with:
// the fewest and the most arguments after its name, and its subcommand when
// it has one, -1 for no most, and how its reply becomes JSON.
type redisCommandRule struct {
	least, most int
	shape       redisReplyShape
}

// redisAllowedCommands are the commands request.commands may list, by name,
// or by name and subcommand: commands that only read, about the server or
// one key, whose replies are as large as what they read and no larger.
// Commands that list keys (KEYS, SCAN), dump a whole collection without a
// bound (LRANGE, SMEMBERS, ZRANGE), reveal settings with secrets among them
// (CONFIG GET) or the server's other clients (CLIENT LIST), or block, are
// not among them, nor is any command that writes.
var redisAllowedCommands = map[string]redisCommandRule{
	"DBSIZE":          {0, 0, redisPlain},
	"LASTSAVE":        {0, 0, redisPlain},
	"ROLE":            {0, 0, redisPlain},
	"EXISTS":          {1, -1, redisPlain},
	"GET":             {1, 1, redisPlain},
	"STRLEN":          {1, 1, redisPlain},
	"TYPE":            {1, 1, redisPlain},
	"TTL":             {1, 1, redisPlain},
	"PTTL":            {1, 1, redisPlain},
	"LLEN":            {1, 1, redisPlain},
	"LINDEX":          {2, 2, redisPlain},
	"SCARD":           {1, 1, redisPlain},
	"SISMEMBER":       {2, 2, redisPlain},
	"ZCARD":           {1, 1, redisPlain},
	"ZCOUNT":          {3, 3, redisPlain},
	"ZLEXCOUNT":       {3, 3, redisPlain},
	"ZSCORE":          {2, 2, redisPlain},
	"HLEN":            {1, 1, redisPlain},
	"HGET":            {2, 2, redisPlain},
	"HMGET":           {2, -1, redisPlain},
	"HEXISTS":         {2, 2, redisPlain},
	"HGETALL":         {1, 1, redisPairs},
	"PFCOUNT":         {1, -1, redisPlain},
	"XLEN":            {1, 1, redisPlain},
	"XPENDING":        {2, 2, redisPlain},
	"XINFO STREAM":    {1, 1, redisPairs},
	"XINFO GROUPS":    {1, 1, redisPairsList},
	"XINFO CONSUMERS": {2, 2, redisPairsList},
	"CLUSTER INFO":    {0, 0, redisInfoText},
	"MEMORY USAGE":    {1, 1, redisPlain},
	"SLOWLOG LEN":     {0, 0, redisPlain},
	"OBJECT ENCODING": {1, 1, redisPlain},
	"OBJECT FREQ":     {1, 1, redisPlain},
	"OBJECT IDLETIME": {1, 1, redisPlain},
	"OBJECT REFCOUNT": {1, 1, redisPlain},
}

// redisSubcommands are the commands of redisAllowedCommands named with a
// subcommand.
var redisSubcommands = []string{"CLUSTER", "MEMORY", "OBJECT", "SLOWLOG", "XINFO"}

// redisAllowedNames lists redisAllowedCommands for a refusal, sorted.
func redisAllowedNames() string {
	names := make([]string, 0, len(redisAllowedCommands))
	for name := range redisAllowedCommands {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

var (
	redisInfoSection = regexp.MustCompile(model.RedisInfoSectionPattern)
	redisCommandName = regexp.MustCompile(model.RedisCommandNamePattern)
)

// redisCommandKey is the name, or name and subcommand, a command is allowed
// by, and the arguments after it.
func redisCommandKey(command []string) (string, []string) {
	name := strings.ToUpper(command[0])
	if slices.Contains(redisSubcommands, name) && len(command) > 1 {
		return name + " " + strings.ToUpper(command[1]), command[2:]
	}
	return name, command[1:]
}

func validateRedisRequest(x *model.Collector) error {
	r := &x.Request
	t := r.TLS
	if (t.CertFile == "") != (t.KeyFile == "") {
		return fmt.Errorf("collector %q request.tls sets only one of cert_file and key_file; a client certificate needs both", x.Name)
	}
	if t.ServerName != "" {
		if _, err := canonicalHost(t.ServerName); err != nil {
			return fmt.Errorf("collector %q request.tls.server_name %q: %w", x.Name, t.ServerName, err)
		}
	}
	if r.Username != "" {
		if err := checkRedisUsername(r.Username); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
		if HasPathParams(r.Username) {
			return fmt.Errorf("collector %q request.username %q has a placeholder; the account is used as it is written", x.Name, r.Username)
		}
		if r.PasswordFile == "" {
			return fmt.Errorf("collector %q sets request.username without request.password_file; an ACL user logs in with AUTH username password, so name the file of its password", x.Name)
		}
	}
	if r.PasswordFile != "" && strings.TrimSpace(r.PasswordFile) == "" {
		return fmt.Errorf("collector %q request.password_file is blanks alone; name the file of the password, or take the key out", x.Name)
	}
	if r.DB < 0 {
		return fmt.Errorf("collector %q request.db is %d; a logical database is numbered from 0", x.Name, r.DB)
	}
	seen := map[string]bool{}
	for i, section := range r.InfoSections {
		if !redisInfoSection.MatchString(section) {
			return fmt.Errorf("collector %q request.info_sections[%d] %q is not a section of INFO, which are named in lower case, such as server, memory or keyspace", x.Name, i, section)
		}
		if seen[section] {
			return fmt.Errorf("collector %q request.info_sections names %s twice", x.Name, section)
		}
		seen[section] = true
	}
	names := map[string]bool{}
	for i := range r.Commands {
		if err := checkRedisCommand(&r.Commands[i], i, names); err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
	}
	return nil
}

// checkRedisUsername refuses a request.username that is no ACL user's name.
func checkRedisUsername(name string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsFunc(name, func(c rune) bool { return c <= ' ' || c == 0x7f }) {
		return fmt.Errorf("request.username %q is not an ACL user's name, which holds no blank or control character", name)
	}
	return nil
}

// checkRedisTargetRequest checks a static target's own account and tls
// block, which replace its collector's for that target
// (withTargetAccount), as the collector's are checked: a username comes
// with the password_file of its password, here the target's own, since the
// collector's password is not another user's, and a password_file alone is
// the password of the collector's user. No value of them takes a
// placeholder: a target's own values are used as written. A redis://
// target, which asks for plaintext, with a tls block of its own contradicts
// itself, as with the collector's.
func checkRedisTargetRequest(_ *model.Collector, t *model.StaticTarget) error {
	r := &t.Request
	for _, key := range []struct{ name, value string }{{"request.username", r.Username}, {"request.password_file", r.PasswordFile}} {
		if strings.Contains(key.value, "{{") {
			return fmt.Errorf("%s %q has a placeholder; a static target's account and credential files are used as they are written, with no probe to fill one, so write the value out", key.name, key.value)
		}
	}
	if r.Username != "" {
		if err := checkRedisUsername(r.Username); err != nil {
			return err
		}
		if r.PasswordFile == "" {
			return errors.New("sets request.username without request.password_file; a target's own user logs in with its own password, so name the file of the password beside it")
		}
	}
	if r.PasswordFile != "" && strings.TrimSpace(r.PasswordFile) == "" {
		return errors.New("request.password_file is blanks alone; name the file of the password, or take the key out")
	}
	if err := checkTargetTLS(t); err != nil {
		return err
	}
	if a, err := parseRedisTarget(t.Target); err == nil && a.plain && r.TLS != nil {
		return errors.New("target asks for plaintext with redis://, but sets request.tls of its own; use rediss:// or a bare host:port")
	}
	return nil
}

// checkRedisCommand checks an entry of request.commands, and writes its
// name, and subcommand, in upper case, as the debug report and the refusal
// show them.
func checkRedisCommand(cmd *model.RedisCommand, i int, names map[string]bool) error {
	where := fmt.Sprintf("request.commands[%d]", i)
	if !redisCommandName.MatchString(cmd.Name) {
		return fmt.Errorf("%s.name %q is not a name the document can hold the reply under; write letters, digits and _, starting with a letter or _, as .commands.%s reaches it", where, cmd.Name, "queue_len")
	}
	if names[cmd.Name] {
		return fmt.Errorf("request.commands names %s twice; each command's reply is put under its own name", cmd.Name)
	}
	names[cmd.Name] = true
	if len(cmd.Command) == 0 {
		return fmt.Errorf("%s (%s) has no command; write it as a list, such as [LLEN, jobs]", where, cmd.Name)
	}
	if HasPathParams(cmd.Command[0]) {
		return fmt.Errorf("%s (%s) has a placeholder in its command's name; a placeholder may stand in an argument, such as a key", where, cmd.Name)
	}
	key, args := redisCommandKey(cmd.Command)
	if strings.Contains(key, " ") && HasPathParams(cmd.Command[1]) {
		return fmt.Errorf("%s (%s) has a placeholder in its command's subcommand; a placeholder may stand in an argument, such as a key", where, cmd.Name)
	}
	rule, ok := redisAllowedCommands[key]
	if !ok {
		return fmt.Errorf("%s (%s) sends %s, which is not among the read-only commands a redis collector may send: %s", where, cmd.Name, key, redisAllowedNames())
	}
	switch {
	case len(args) < rule.least, rule.most >= 0 && len(args) > rule.most:
		return fmt.Errorf("%s (%s) gives %s %d arguments; it takes %s", where, cmd.Name, key, len(args), redisArity(rule))
	}
	cmd.Command[0] = strings.ToUpper(cmd.Command[0])
	if strings.Contains(key, " ") {
		cmd.Command[1] = strings.ToUpper(cmd.Command[1])
	}
	for j, arg := range cmd.Command[1:] {
		if _, err := (templateField{fmt.Sprintf("%s.command[%d]", where, j+1), arg, "arg"}).parse(); err != nil {
			return err
		}
	}
	return nil
}

// redisArity says how many arguments a command takes.
func redisArity(rule redisCommandRule) string {
	switch {
	case rule.most < 0:
		return fmt.Sprintf("%d or more", rule.least)
	case rule.least == rule.most:
		return strconv.Itoa(rule.least)
	}
	return fmt.Sprintf("%d to %d", rule.least, rule.most)
}

// redisAddress is a target as a scrape reaches it: host as the policy
// matches it and TLS verifies it, hostPort what is dialed, and whether
// rediss:// asked for TLS or redis:// for plaintext.
type redisAddress struct {
	host, hostPort string
	tls, plain     bool
}

// parseRedisTarget reads a target: host, host:port, [v6]:port, or a redis://
// or rediss:// URL of one, port 6379 when it names none. Nothing else may
// come with it: the database is request.db, the account the collector's.
func parseRedisTarget(raw string) (redisAddress, error) {
	var a redisAddress
	target := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(target, "rediss://"):
		a.tls, target = true, strings.TrimPrefix(target, "rediss://")
	case strings.HasPrefix(target, "redis://"):
		a.plain, target = true, strings.TrimPrefix(target, "redis://")
	case strings.Contains(target, "://"):
		scheme, _, _ := strings.Cut(target, "://")
		return a, fmt.Errorf("target %q has the scheme %s://; a redis target is host, host:port, redis://host:port or rediss://host:port", raw, scheme)
	}
	target = strings.TrimSuffix(target, "/")
	if strings.ContainsAny(target, "/?#@") {
		return a, fmt.Errorf("target %q has a path, query or user in it; a redis target is only the host and its port, the database is request.db and the account request.username", raw)
	}
	host, port := target, redisDefaultPort
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
	} else if strings.Count(target, ":") == 1 {
		return a, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:6379", raw)
	}
	if host == "" {
		return a, fmt.Errorf("target %q names no host; a redis target is host or host:port", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return a, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return a, err
	}
	a.host, a.hostPort = canonical, net.JoinHostPort(canonical, port)
	return a, nil
}

// useTLS says whether a scrape of a goes over TLS: rediss://, or a tls
// block, even an empty one (model.TLSConfig.Set), on a target that did not
// ask for plaintext with redis://, which the two together contradict.
func (a redisAddress) useTLS(c *model.Collector) (bool, error) {
	configured := c.Request.TLS != model.TLSConfig{}
	if a.plain && configured {
		return false, errors.New("target asks for plaintext with redis://, but the collector sets request.tls; use rediss:// or a bare host:port")
	}
	return a.tls || configured, nil
}

func checkRedisTarget(c *model.Collector, target string, _ bool) error {
	a, err := parseRedisTarget(target)
	if err != nil {
		return err
	}
	_, err = a.useTLS(c)
	return err
}

// redisLabel is the url label of a scrape: redis://host:port/db, or
// rediss:// over TLS.
func redisLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	c = withTargetAccount(c, overrides)
	a, err := parseRedisTarget(target)
	if err != nil {
		return "", err
	}
	secure, err := a.useTLS(c)
	if err != nil {
		return "", err
	}
	return redisURL(a, secure, c.Request.DB), nil
}

func redisURL(a redisAddress, secure bool, db int) string {
	scheme := "redis"
	if secure {
		scheme = "rediss"
	}
	return scheme + "://" + a.hostPort + "/" + strconv.Itoa(db)
}

func redisMethod(c *model.Collector, _ RequestOverrides) string {
	switch {
	case len(c.Request.Commands) == 0:
		return redisMethodInfo
	case len(c.Request.InfoSections) > 0:
		return redisMethodBoth
	}
	return redisMethodCommands
}

// redisDisplay renders a target for logs and labels: as written, since it
// holds no credentials, when it is one at all.
func redisDisplay(target string) string {
	if _, err := parseRedisTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// redisCall is a command a scrape sends for the document: INFO, or INFO
// of one section, or an entry of request.commands, its placeholders
// filled.
type redisCall struct {
	args []string
	// name is the command's entry of the document; "" for INFO.
	name    string
	section string
	shape   redisReplyShape
}

// redisCalls is what a scrape of c sends after it has logged in and
// selected its database.
func redisCalls(c *model.Collector, params map[string]string) ([]redisCall, error) {
	r := &c.Request
	var calls []redisCall
	switch {
	case len(r.InfoSections) > 0:
		// One INFO for each section: before Redis 7 INFO takes one.
		for _, section := range r.InfoSections {
			calls = append(calls, redisCall{args: []string{"INFO", section}, section: section})
		}
	case len(r.Commands) == 0:
		calls = append(calls, redisCall{args: []string{"INFO"}})
	}
	for i, cmd := range r.Commands {
		args := make([]string, len(cmd.Command))
		args[0] = cmd.Command[0]
		for j, arg := range cmd.Command[1:] {
			value, err := templateField{fmt.Sprintf("request.commands[%d].command[%d]", i, j+1), arg, "arg"}.render(params)
			if err != nil {
				return nil, err
			}
			args[j+1] = value
		}
		key, _ := redisCommandKey(cmd.Command)
		calls = append(calls, redisCall{args: args, name: cmd.Name, shape: redisAllowedCommands[key].shape})
	}
	return calls, nil
}

// redisDocument is a scrape as the transforms read it.
type redisDocument struct {
	Info     map[string]map[string]any `json:"info,omitempty"`
	Commands map[string]any            `json:"commands,omitempty"`
}

// redisShownCommand is a command as a debug report shows it: its words,
// quoted where they hold a blank or anything not printable, and AUTH's
// password as ***.
func redisShownCommand(args []string) string {
	shown := make([]string, len(args))
	for i, arg := range args {
		switch {
		case args[0] == "AUTH" && i == len(args)-1 && i > 0:
			shown[i] = "***"
		case arg == "" || strings.ContainsFunc(arg, func(r rune) bool { return r == ' ' || r == '"' || !unicode.IsPrint(r) }):
			shown[i] = strconv.Quote(arg)
		default:
			shown[i] = arg
		}
	}
	return redisShown(strings.Join(shown, " "))
}

// fetchRedis makes a scrape's connection, logs in, selects the database and
// answers the document of INFO and the commands, with a static target's own
// account and tls block in place of the collector's.
func fetchRedis(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	c = withTargetAccount(c, overrides)
	address, err := parseRedisTarget(target)
	if err != nil {
		return nil, err
	}
	secure, err := address.useTLS(c)
	if err != nil {
		return nil, err
	}
	calls, err := redisCalls(c, overrides.Params)
	if err != nil {
		return nil, err
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, redisTimeout)
		defer cancel()
	}
	var password string
	if c.Request.PasswordFile != "" {
		if password, err = ReadCredentialFile(c.Request.PasswordFile); err != nil {
			return nil, fmt.Errorf("reading request.password_file: %w", err)
		}
		if password == "" {
			return nil, fmt.Errorf("request.password_file %s is empty", c.Request.PasswordFile)
		}
	}
	var cfg *tls.Config
	if secure {
		if cfg, err = tlsConfig(c.Request.TLS); err != nil {
			return nil, fmt.Errorf("request.tls: %w", err)
		}
		if cfg.ServerName == "" {
			cfg.ServerName = address.host
		}
	}
	// The host's name now; the address the connection is made to when it
	// is made, as an http request's connections are checked.
	if policy := policyOf(c); policy != nil {
		guard := &policyGuard{policy: policy, hosts: map[string]bool{}}
		if err := guard.check(ctx, &url.URL{Scheme: "redis", Host: address.hostPort}); err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, policyGuardKey{}, guard)
	}
	label := redisURL(address, secure, c.Request.DB)
	dialer := &net.Dialer{}
	conn, err := policyDialer(dialer.DialContext)(ctx, "tcp", address.hostPort)
	if err != nil {
		if !errors.Is(err, ErrTargetRefused) {
			err = redisFailure(ctx, fmt.Errorf("connecting to %s: %w", address.hostPort, err))
		}
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	// Every read and write ends at the scrape's deadline, or as soon as it
	// is cancelled.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if secure {
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, redisFailure(ctx, fmt.Errorf("TLS handshake with %s: %w", address.hostPort, err))
		}
		conn = tlsConn
	}
	session := newRedisConn(conn, responseLimit(c))
	if password != "" {
		args := []string{"AUTH", password}
		if c.Request.Username != "" {
			args = []string{"AUTH", c.Request.Username, password}
		}
		if _, err := redisSend(ctx, session, label, args); err != nil {
			return nil, err
		}
	}
	if c.Request.DB != 0 {
		if _, err := redisSend(ctx, session, label, []string{"SELECT", strconv.Itoa(c.Request.DB)}); err != nil {
			return nil, err
		}
	}
	var doc redisDocument
	for _, call := range calls {
		reply, err := redisSend(ctx, session, label, call.args)
		if err != nil {
			return nil, err
		}
		if call.name == "" {
			raw, ok := reply.([]byte)
			if !ok {
				return nil, fmt.Errorf("%s: the reply is %s, not INFO's text", redisShownCommand(call.args), redisKind(reply))
			}
			text, err := redisText(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", redisShownCommand(call.args), err)
			}
			if doc.Info == nil {
				doc.Info = map[string]map[string]any{}
			}
			if parseRedisInfo(text, doc.Info) == 0 && call.section != "" {
				return nil, fmt.Errorf("INFO %s: the server answered no section; it has none of that name (INFO everything lists those it has)", call.section)
			}
			continue
		}
		value, err := redisJSON(reply, call.shape)
		if err != nil {
			return nil, fmt.Errorf("%s (request.commands %s): %w", redisShownCommand(call.args), call.name, err)
		}
		if doc.Commands == nil {
			doc.Commands = map[string]any{}
		}
		doc.Commands[call.name] = value
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// The document is held to the limit the replies were: JSON writes a
	// control character as six bytes, \u0001, and a quote or backslash as
	// two, so replies within the limit can make a document past it.
	if limit := responseLimit(c); int64(len(body)) > limit {
		return nil, model.MarkError(model.Errorf("the document of the replies is %d bytes as JSON, larger than the response limit of %d bytes, the replies' control characters, quotes and backslashes taking more as JSON escapes them; raise request.max_response_bytes or limits.max_response_bytes, or ask for less", len(body), model.Size(limit)), model.ErrLimitExceeded)
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

// redisSend sends one command and reads its reply, recording both in a
// debug report's trace. An error reply fails the scrape, with the server's
// words.
func redisSend(ctx context.Context, session *redisConn, label string, args []string) (any, error) {
	shown := redisShownCommand(args)
	traceRequest(ctx, args[0], label, http.Header{"Redis-Command": {shown}}, "", false)
	reply, err := session.do(args)
	if err != nil {
		err = redisFailure(ctx, fmt.Errorf("%s: %w", shown, err))
		traceOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	if failed, ok := reply.(*redisError); ok {
		err := fmt.Errorf("%s: %w", shown, failed)
		traceOutcome(ctx, "error: "+err.Error())
		return nil, err
	}
	traceOutcome(ctx, redisKind(reply))
	return reply, nil
}

// redisFailure is the failure of a read or a write, or a handshake: the
// scrape's deadline or cancellation when that is what ended it, since the
// connection's deadline is set to make it end. The connection's deadline is
// the scrape's, and its timer can fire before the context's own, so a
// connection that timed out is the deadline's failure whether or not the
// context says so yet.
func redisFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w (%w)", ctxErr, err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (%w)", context.DeadlineExceeded, err)
	}
	return err
}

// checkRedisFiles reads the files a scrape reads, for --dry-run: the
// password file, and the tls block's when the collector sets one, or those
// a static target names of its own. Loading the configuration reads none of
// them, since a file may be mounted after the exporter starts.
func checkRedisFiles(c *model.Collector, t *model.StaticTarget) []string {
	return accountFileWarnings(c, t)
}
