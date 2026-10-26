//go:build request_type_sshexec

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"golang.org/x/crypto/ssh"
)

// The sshexec request type runs one fixed command on a host over SSH and
// hands what it wrote to stdout to the decoder and the transforms, as an
// http answer's body is handed to them, and as tcp hands its answer: the
// output of df, uptime, systemctl is-active, or a script of the host's
// own, read by regex, text, csv or prometheus.
//
//	request:
//	  type: sshexec
//	  command: [df, -P, /]                            # required: program and arguments
//	  username: metrics                               # required
//	  private_key_file: /etc/exporter/ssh/id_ed25519  # and/or password_file
//	  known_hosts_file: /etc/exporter/ssh/known_hosts # required
//	  success_exit_codes: [0, 1]                      # optional: [0] when left out
//
// The probe's target is the host, host:port or ssh://host:port, port 22
// when it is left out, as an sshfile collector's. The connection is
// sshfile's (sshconn.go, sshtarget.go): the host's key always verified
// against known_hosts_file, the private key, its passphrase and the
// password, keepalives, the idle timeout, max_open_connections and the
// target policy, each connection kept and shared by the scrapes reaching
// the host with the same settings. Each scrape opens one session on it and
// runs the command there.
//
// The command is a list, never a line a shell reads: the program, an
// absolute path or a bare name the account's PATH finds, and its
// arguments. sshd hands the command line to the account's shell, so the
// exporter writes every word in single quotes, a ' in it as '\'', and the
// shell makes each the one word it was in the list, whatever it holds:
// nothing of it is a variable, a glob, a separator or a redirection. An
// argument may hold {{param_name}} placeholders, filled from the probe,
// whose values may hold only the characters tcp's send takes (lineValue):
// a value adds no word, and nothing a forced command's wrapper could split
// on. The program takes none, and a static target changes nothing of the
// command, only the account and the credentials it runs with.
//
// stdout is read against the response limit as it comes, and the scrape
// fails as over the limit at the first byte past it. stderr is kept, its
// first sshExecStderrBytes, for the error and the debug report. The exit
// status decides: 0, or one of success_exit_codes, is a success, and
// anything else, a signal among it, fails the scrape with the status and
// the start of stderr. At the scrape's deadline the command is sent KILL
// and its session closed, which a server ends the command for; a server
// that does not answer within sshAbandonGrace has its connection dropped.
//
// No environment is sent: OpenSSH accepts only the variables its AcceptEnv
// names, which by default are none, so a request would fail or, worse, be
// ignored on some hosts and not others. The type is opt-in, built under its
// tag alone, and links x/crypto's SSH client but not the SFTP client.

// SSHExecConfig is an sshexec collector's own request keys: the command,
// its program first, and the exit statuses that are a success, 0 alone
// when empty.
type SSHExecConfig struct {
	Command          []string `yaml:"command,omitempty"`
	SuccessExitCodes []int    `yaml:"success_exit_codes,omitempty"`
}

// sshExecRequest is an sshexec collector's own request keys: the zero
// SSHExecConfig of a collector built in code whose request has not been
// validated, which sets them (ValidateRequest).
func sshExecRequest(c *model.Collector) SSHExecConfig {
	own, _ := c.Request.Own.(SSHExecConfig)
	return own
}

// sshExecConns are the connections of sshexec collectors, kept apart from
// sshfile's (sshconn.go); nothing is attached to them, each command having
// a session of its own.
var sshExecConns = newSSHConnCache(RequestTypeSSHExec, nil, "")

func init() {
	registerRequestType(&RequestType{
		Name:    RequestTypeSSHExec,
		Article: "an",
		Fields: []string{
			"max_response_bytes",
			"username", "private_key_file", "password_file",
			"passphrase_file", "known_hosts_file", "keepalive_interval", "keepalive_count_max", "idle_timeout", "max_open_connections",
			"allowed_targets", "denied_targets",
		},
		Config:    SSHExecConfig{},
		KeyRules:  sshExecKeyRules,
		Overrides: []string{"timeout", PathParamPrefix},
		// A static target changes the account and the credentials alone:
		// the command is the collector's, whoever's host it runs on.
		TargetFields: []string{
			"timeout",
			"username", "private_key_file", "passphrase_file", "password_file",
		},
		Requester:  sshExecRequester{},
		TargetHint: sshTargetHint,
		Templates:  sshExecTemplates,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "the arguments of its request.command or its label values")
		},
		SchemaRule:         sshExecSchemaRule,
		CheckTargetRequest: checkSSHTargetAccount,
		Stage:              "ssh",
		CheckFiles:         checkSSHFiles,
		Follow:             sshExecConns.follow,
		CloseConnections:   sshExecConns.closeAll,
		Connections:        sshExecConns.collectorConnections,
		ConnectionLimit:    sshExecConns.limitCounts,
		Families: []string{
			"http_exporter_sshexec_connections", "http_exporter_sshexec_connects_total",
			"http_exporter_sshexec_limit_waits_total", "http_exporter_sshexec_limit_failures_total",
		},
	})
	templateEscapers[sshExecWordKind] = escapeSSHExecValue
}

// sshExecRequester is the sshexec request type's Requester.
type sshExecRequester struct{}

// Validate checks an sshexec collector's request and fills in its defaults.
func (sshExecRequester) Validate(c *model.Collector) error {
	return validateSSHExecRequest(c)
}

// Fetch performs one request of an sshexec collector.
func (sshExecRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	return fetchSSHExec(ctx, target, c, overrides)
}

// CheckTarget checks an sshexec collector's target.
func (sshExecRequester) CheckTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseSSHTarget(RequestTypeSSHExec, target)
	return err
}

// Label is the url label of an sshexec request: ssh://host:port, as
// sshfile's is sftp://host:port and the file.
func (sshExecRequester) Label(target string, _ *model.Collector, _ RequestOverrides) (string, error) {
	address, err := parseSSHTarget(RequestTypeSSHExec, target)
	if err != nil {
		return "", err
	}
	return sshExecURL(address), nil
}

// Method is the http_method label of an sshexec request.
func (sshExecRequester) Method(*model.Collector, RequestOverrides) string { return sshExecMethod }

// Display renders an sshexec target for logs and labels.
func (sshExecRequester) Display(target string) string {
	return sshDisplay(RequestTypeSSHExec, target)
}

// sshExecMethod is the http_method label of every sshexec scrape.
const sshExecMethod = "EXEC"

// sshExecWordKind is the kind of templated field an argument of the
// command is (templateEscapers).
const sshExecWordKind = "shell_word"

// sshExecStderrBytes is how much of what a command writes to stderr is
// kept, for its error and the debug report; the rest is read and dropped,
// so a command that writes much there is not held up.
const sshExecStderrBytes = 2 << 10

// sshExecStderrShown is how much of the stderr kept a failure's text shows,
// so that the status and the host still fit the bound on a failure's text
// (model.MaxFailureBytes).
const sshExecStderrShown = 1 << 10

// sshExecMaxCommand is the most bytes of the command as the configuration
// writes it, its words and their placeholders: a command is a program and a
// few arguments.
const sshExecMaxCommand = 64 << 10

// sshExecTimeout bounds a scrape with no deadline of its own.
const sshExecTimeout = 30 * time.Second

// sshExecValueRule is why a placeholder's value in an argument is held to
// lineValue.
const sshExecValueRule = "a value written into an argument of request.command may hold only letters, digits and . _ - : / @ + = , %, so that it stays the one argument it is written in, whatever splits the command line on the host"

func validateSSHExecRequest(x *model.Collector) error {
	own := sshExecRequest(x)
	if len(own.Command) == 0 {
		return fmt.Errorf("collector %q request.command is required for request.type sshexec: the program to run on the host and its arguments, as a list, such as [df, -P, /]", x.Name)
	}
	if err := checkSSHExecProgram(own.Command[0]); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	size := 0
	for i, word := range own.Command {
		size += len(word)
		if strings.ContainsRune(word, 0) {
			return fmt.Errorf("collector %q request.command[%d] holds a NUL byte, which no argument of a program can", x.Name, i)
		}
		if i == 0 {
			continue
		}
		placeholders, err := (templateField{fmt.Sprintf("request.command[%d]", i), word, sshExecWordKind}).parse()
		if err != nil {
			return fmt.Errorf("collector %q %w", x.Name, err)
		}
		// A default is held to what a probe's value is, here rather than at
		// every scrape that leaves the parameter out.
		for _, p := range placeholders {
			if character, refused := lineValueRefused(p.Default); p.HasDefault && refused {
				return fmt.Errorf("collector %q request.command[%d]: the default of %s holds %s; %s", x.Name, i, p.Name, character, sshExecValueRule)
			}
		}
	}
	if size > sshExecMaxCommand {
		return fmt.Errorf("collector %q request.command is %d bytes, more than the %d an sshexec collector may run; it is a program and its arguments", x.Name, size, sshExecMaxCommand)
	}
	for i, code := range own.SuccessExitCodes {
		if code < 0 || code > 255 {
			return fmt.Errorf("collector %q request.success_exit_codes[%d] is %d; an exit status is a number from 0 to 255", x.Name, i, code)
		}
	}
	return validateSSHConnection(x, "the command runs as")
}

// checkSSHExecProgram holds the program to its rules: written, with no
// placeholder, no control character, and either an absolute path or a
// bare name, which the account's PATH finds. A relative path with a / in
// it would be found from the account's home directory, which the exporter
// cannot see, so it is refused.
func checkSSHExecProgram(program string) error {
	switch {
	case strings.TrimSpace(program) == "":
		return errors.New("request.command[0], the program, is empty or blanks alone; name the program, as an absolute path or a bare name the account's PATH finds")
	case strings.Contains(program, "{{"):
		return fmt.Errorf("request.command[0] %q has a placeholder; the program is run as it is written, and only its arguments take placeholders", program)
	case strings.IndexFunc(program, unicode.IsControl) >= 0:
		return fmt.Errorf("request.command[0] %q holds a control character, which no program's name does", program)
	case !strings.HasPrefix(program, "/") && strings.Contains(program, "/"):
		return fmt.Errorf("request.command[0] %q is a relative path; write the program as an absolute path, such as /usr/local/bin/check, or as a bare name the account's PATH finds, such as df", program)
	}
	return nil
}

// sshExecTemplates are the arguments of a collector's command that hold a
// placeholder.
func sshExecTemplates(c *model.Collector, _ RequestOverrides) []templateField {
	var out []templateField
	for i, word := range sshExecRequest(c).Command {
		if i > 0 && HasPathParams(word) {
			out = append(out, templateField{fmt.Sprintf("request.command[%d]", i), word, sshExecWordKind})
		}
	}
	return out
}

// escapeSSHExecValue refuses a value lineValue does not take. The value is
// not quoted here: the whole argument is, once it is filled in
// (sshExecLine).
func escapeSSHExecValue(where, name, value string) (string, error) {
	if character, refused := lineValueRefused(value); refused {
		return "", fmt.Errorf("%s: the value of %s holds %s; %s", where, name, character, sshExecValueRule)
	}
	return value, nil
}

// sshExecWords are the words of a scrape's command, the placeholders of
// its arguments filled in from params.
func sshExecWords(c *model.Collector, params map[string]string) ([]string, error) {
	command := sshExecRequest(c).Command
	words := make([]string, len(command))
	for i, word := range command {
		if i == 0 || !HasPathParams(word) {
			words[i] = word
			continue
		}
		filled, err := templateField{fmt.Sprintf("request.command[%d]", i), word, sshExecWordKind}.render(params)
		if err != nil {
			return nil, err
		}
		words[i] = filled
	}
	return words, nil
}

// sshExecLine is the command line sent for words: each word in single
// quotes, a single quote in it written as the quotes closed, an escaped
// quote and the quotes opened again, so that a POSIX shell makes each the
// one word it is, whatever it holds.
func sshExecLine(words []string) string {
	var b strings.Builder
	for i, word := range words {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(word, `'`, `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}

// sshExecURL is the url label and the trace's URL of a scrape.
func sshExecURL(address sshAddress) string {
	return "ssh://" + address.hostPort
}

// sshExecSuccess says whether a command's exit status is a success: 0, or
// one of success_exit_codes when they are set.
func sshExecSuccess(c *model.Collector, status int) bool {
	codes := sshExecRequest(c).SuccessExitCodes
	if len(codes) == 0 {
		return status == 0
	}
	return slices.Contains(codes, status)
}

// fetchSSHExec runs a scrape's command on the target host and answers what
// it wrote to stdout.
func fetchSSHExec(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseSSHTarget(RequestTypeSSHExec, target)
	if err != nil {
		return nil, err
	}
	words, err := sshExecWords(c, overrides.Params)
	if err != nil {
		return nil, err
	}
	line := sshExecLine(words)
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
		defer cancel()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sshExecTimeout)
		defer cancel()
	}
	settings := sshSettingsOf(c, overrides)
	// The host's name now; the address each connection is made to when it
	// is made, as an http request's connections are checked.
	if settings.policy != nil {
		if _, err := settings.policy.check(ctx, address.host, false); err != nil {
			return nil, err
		}
	}
	key := sshConnKey{address: address.hostPort, host: address.host, sshSettings: settings}
	run, err := sshExecConns.run(ctx, key, c, line, sshExecURL(address))
	if err != nil {
		return nil, onSSHHost(err, address.hostPort)
	}
	if !sshExecSuccess(c, run.status) {
		return nil, sshExecFailure(c, address.hostPort, run)
	}
	return &HTTPResponse{
		StatusCode: http.StatusOK,
		NoStatus:   true,
		// No Content-Type: the decoder is decoder.type, or what the body
		// reads as, as for a file whose extension says nothing.
		Headers:   http.Header{},
		Body:      run.stdout,
		Target:    target,
		Collector: c.Name,
		Duration:  time.Since(start),
	}, nil
}

// sshExecRun is what a command did: what it wrote to stdout, the start of
// what it wrote to stderr, and its exit status, or the signal that ended
// it, with status -1.
type sshExecRun struct {
	stdout []byte
	stderr string
	status int
	signal string
}

// outcome is how a debug report's request section ends for a run.
func (r *sshExecRun) outcome() string {
	how := "exited with " + strconv.Itoa(r.status)
	if r.signal != "" {
		how = "was killed by signal " + r.signal
	}
	outcome := fmt.Sprintf("%s, read %d bytes", how, len(r.stdout))
	if r.stderr != "" {
		outcome += "; stderr: " + RedactText(r.stderr)
	}
	return outcome
}

// sshExecFailure is the error of a command whose exit status is not a
// success: the status, or the signal, and the start of stderr, with what
// reads as a credential in it masked, as in every text the exporter did not
// write.
func sshExecFailure(c *model.Collector, hostPort string, r *sshExecRun) error {
	how := "exited with " + strconv.Itoa(r.status)
	if r.signal != "" {
		how = "was killed by signal " + r.signal
	}
	if codes := sshExecRequest(c).SuccessExitCodes; len(codes) > 0 && r.signal == "" {
		how += fmt.Sprintf(", which request.success_exit_codes %v does not list", codes)
	}
	if r.stderr == "" {
		return fmt.Errorf("the command on %s %s, and wrote nothing to stderr", hostPort, how)
	}
	return model.Errorf("the command on %s %s; its stderr began: %s", hostPort, how, model.Shown(RedactText(r.stderr), sshExecStderrShown))
}

// run runs line on a connection for key, in a session of its own, within
// ctx. A session that cannot be opened because the connection was lost is
// opened once more, on a new connection, while ctx lasts; a command that
// started is never run again. shown is what a debug probe's request section
// shows of the host.
func (c *sshConnCache) run(ctx context.Context, key sshConnKey, x *model.Collector, line, shown string) (*sshExecRun, error) {
	limit := responseLimit(x)
	for attempt := 0; ; attempt++ {
		entry, reused, err := c.get(ctx, key, x.Name, x.Request.MaxOpenConnections)
		traceSSH(ctx, sshExecMethod, key, shown, http.Header{"Ssh-Command": {model.CutTo(line, model.MaxFailureBytes, false)}}, reused, err)
		if err != nil {
			return nil, err
		}
		es := &sshExecSession{}
		done := make(chan sshExecDone, 1)
		go func() {
			if hook := beforeSSHRead.Load(); hook != nil {
				(*hook)()
			}
			run, started, err := es.run(entry.client, line, limit)
			done <- sshExecDone{run, started, err}
		}()
		var result sshExecDone
		select {
		case result = <-done:
			c.release(entry)
		case <-ctx.Done():
			c.abandonExec(entry, es, done)
			err = fmt.Errorf("running the command on %s: %w", key.address, ctx.Err())
			traceOutcome(ctx, "error: "+err.Error())
			return nil, err
		}
		if result.err != nil && !result.started && attempt == 0 && ctx.Err() == nil && entry.lostExec(result.err) {
			c.drop(entry, "lost")
			traceOutcome(ctx, "the connection was lost: "+result.err.Error())
			continue
		}
		if result.err != nil {
			traceOutcome(ctx, "error: "+result.err.Error())
			return nil, result.err
		}
		traceOutcome(ctx, result.run.outcome())
		return result.run, nil
	}
}

// sshExecDone is what a session's run returned.
type sshExecDone struct {
	run     *sshExecRun
	started bool
	err     error
}

// abandonExec is a scrape giving up on its command at its deadline: the
// command is sent KILL and its session closed, which ends it on a server
// that answers, and the run is given sshAbandonGrace to return, after which
// the connection is dropped. The connection is released when the run
// returns.
func (c *sshConnCache) abandonExec(entry *sshConn, es *sshExecSession, done <-chan sshExecDone) {
	es.abandon()
	go func() {
		grace := time.NewTimer(sshAbandonGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
			c.drop(entry, "stalled")
			<-done
		}
		c.release(entry)
	}()
}

// lostExec says a session's error is its connection ending under it.
func (entry *sshConn) lostExec(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || isClosedChannel(entry.ended)
}

// sshExecSession is the session a command runs in, which abandon ends from
// another goroutine, before the session is opened or after.
type sshExecSession struct {
	mu        sync.Mutex
	session   *ssh.Session
	abandoned bool
}

// opened notes the session once it is opened, and closes it at once when
// the scrape has been abandoned meanwhile.
func (e *sshExecSession) opened(session *ssh.Session) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.abandoned {
		go func() { _ = session.Close() }()
		return false
	}
	e.session = session
	return true
}

// abandon sends the command KILL and closes its session: closing the
// session's channel is what a server ends a command for whatever its
// signals support, OpenSSH's sshd among them, which ignores signals before
// 8.1. Each is sent apart, since a request waits for the connection.
func (e *sshExecSession) abandon() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.abandoned = true
	if session := e.session; session != nil {
		go func() {
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		}()
	}
}

// run opens a session on client, runs line in it, and reads stdout,
// held to limit, and stderr, of which its first sshExecStderrBytes are
// kept, until the command exits. started says the command was sent.
func (e *sshExecSession) run(client *ssh.Client, line string, limit int64) (*sshExecRun, bool, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, false, fmt.Errorf("opening a session: %w", err)
	}
	defer func() { _ = session.Close() }()
	if !e.opened(session) {
		return nil, false, errors.New("the scrape was abandoned")
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return nil, false, err
	}
	if err := session.Start(line); err != nil {
		return nil, false, fmt.Errorf("the host refused to run the command: %w", err)
	}
	kept := &boundedBuffer{room: sshExecStderrBytes}
	stderrRead := make(chan struct{})
	go func() {
		defer close(stderrRead)
		_, _ = io.Copy(kept, stderr)
	}()
	body, err := readSSHExecStdout(stdout, limit)
	if err != nil {
		// The command is ended rather than waited for: what it writes past
		// the limit is of no use.
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return nil, true, err
	}
	waitErr := session.Wait()
	<-stderrRead
	run := &sshExecRun{stdout: body, stderr: kept.String()}
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exit) && exit.Signal() != "":
		run.status, run.signal = -1, exit.Signal()
	case errors.As(waitErr, &exit):
		run.status = exit.ExitStatus()
	case errors.As(waitErr, &missing):
		return nil, true, errors.New("the session ended without the command's exit status; the host closed it, or the command was ended by something the host does not report")
	default:
		return nil, true, waitErr
	}
	return run, true, nil
}

// readSSHExecStdout reads a command's stdout to its end, failing at the
// first byte past limit.
func readSSHExecStdout(stdout io.Reader, limit int64) ([]byte, error) {
	var body []byte
	chunk := make([]byte, 32<<10)
	for {
		n, err := stdout.Read(chunk)
		if n > 0 {
			if int64(len(body))+int64(n) > limit {
				return nil, model.MarkError(model.Errorf("the command's output is larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes", model.Size(limit)), model.ErrLimitExceeded)
			}
			body = append(body, chunk[:n]...)
		}
		switch {
		case errors.Is(err, io.EOF):
			return body, nil
		case err != nil:
			return nil, fmt.Errorf("reading the command's output: %w", err)
		}
	}
}

// boundedBuffer keeps the first room bytes written to it and drops the
// rest, taking every write whole.
type boundedBuffer struct {
	b    []byte
	room int
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if left := w.room - len(w.b); left > 0 {
		w.b = append(w.b, p[:min(left, len(p))]...)
	}
	return len(p), nil
}

// String is what was kept, cut back to the last whole character.
func (w *boundedBuffer) String() string {
	return string(w.b[:model.HeadOf(string(w.b), len(w.b))])
}

// sshExecSchemaRule is what an sshexec collector's request requires, as the
// exporter does (validateSSHExecRequest): command, username and
// known_hosts_file, the two written with something other than ""; a private
// key or a password, or both; and a passphrase only beside a private key.
func sshExecSchemaRule() map[string]any {
	keys := []string{"command", "username", "known_hosts_file"}
	properties := map[string]any{}
	for _, key := range keys[1:] {
		properties[key] = SchemaWrittenKey(key)["properties"].(map[string]any)[key]
	}
	return map[string]any{
		"required":   keys,
		"properties": properties,
		"anyOf":      []any{SchemaWrittenKey("private_key_file"), SchemaWrittenKey("password_file")},
		"if":         SchemaWrittenKey("passphrase_file"),
		"then":       SchemaWrittenKey("private_key_file"),
	}
}

// sshExecKeyRules is what the configuration schema says of an sshexec
// collector's own keys.
func sshExecKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"command":            {"minItems": 1, "description": "sshexec, required: the command run on the host, a list of its program, an absolute path or a bare name the account's PATH finds, and its arguments, such as [df, -P, /]; never a line a shell reads. Every word is sent in single quotes, so the host's shell makes each one word whatever it holds. An argument may contain {{param_name}} placeholders, whose values, and defaults, may hold only letters, digits and . _ - : / @ + = , %; the program takes none. sshexec is in a build with OPT_IN_REQUEST_TYPES naming it alone. See docs/SSHEXEC.md."},
		"success_exit_codes": {"minItems": 1, "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 255}, "description": "sshexec: the exit statuses that are a success, such as [0, 1] for grep, whose 1 says it found nothing; any other fails the scrape. Defaults to [0]."},
	}
}
