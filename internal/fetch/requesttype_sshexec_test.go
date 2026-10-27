//go:build request_type_sshexec

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"golang.org/x/crypto/ssh"
)

func init() {
	ownPlaceholders[RequestTypeSSHExec] = func(c *model.Collector) {
		c.Request.Own = SSHExecConfig{Command: []string{"systemctl", "is-active", "{{param_unit:x}}"}}
	}
}

// execFixture is an SSH server that runs commands with exec, and the
// exporter's files to reach it with: a private key the server authorizes,
// a password file of the password it takes, and a known_hosts file of its
// host key.
type execFixture struct {
	server                                 *sshtest.Server
	dir, keyFile, passwordFile, knownHosts string
}

// execPassword is the password the fixture's server takes.
const execPassword = "exec-pass"

func newExecFixture(t *testing.T, exec func(ctx context.Context, command string, stdout, stderr io.Writer) int, hostKeys ...ssh.Signer) *execFixture {
	t.Helper()
	return newExecFixtureWith(t, sshtest.Options{HostKeys: hostKeys, Exec: exec})
}

// newExecFixtureWith is newExecFixture with a server of options, its
// password and authorized key set here.
func newExecFixtureWith(t *testing.T, options sshtest.Options) *execFixture {
	t.Helper()
	f := &execFixture{dir: t.TempDir()}
	var key ssh.PublicKey
	f.keyFile, key = sshtest.WriteClientKey(t, f.dir, "id_ed25519", "")
	f.passwordFile = sshtest.WriteFile(t, f.dir, "password", execPassword+"\n")
	options.Password, options.AuthorizedKeys = execPassword, []ssh.PublicKey{key}
	f.server = sshtest.Start(t, options)
	f.knownHosts = f.server.WriteKnownHosts(t, f.dir)
	t.Cleanup(sshExecConns.closeAll)
	return f
}

// collector is a validated sshexec collector of the fixture running
// command with its private key, changed by change first.
func (f *execFixture) collector(t *testing.T, name string, command []string, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := model.Collector{
		Name: name,
		Request: model.RequestConfig{
			Type: RequestTypeSSHExec, Username: "metrics", PrivateKeyFile: f.keyFile, KnownHostsFile: f.knownHosts,
			Own: SSHExecConfig{Command: command},
		},
		Transform:     model.TransformConfig{Type: "regex"},
		ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"},
	}
	for _, fn := range change {
		fn(&c.Request)
	}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func (f *execFixture) fetch(c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return FetchCollector(ctx, f.server.Addr, c, overrides, nil)
}

// withExitCodes sets a request's success_exit_codes.
func withExitCodes(codes ...int) func(*model.RequestConfig) {
	return func(r *model.RequestConfig) {
		own, _ := r.Own.(SSHExecConfig)
		own.SuccessExitCodes = codes
		r.Own = own
	}
}

// scripted is an Options.Exec that answers every command with out and err
// and exits with status.
func scripted(out, err string, status int) func(context.Context, string, io.Writer, io.Writer) int {
	return func(_ context.Context, _ string, stdout, stderr io.Writer) int {
		_, _ = io.WriteString(stdout, out)
		_, _ = io.WriteString(stderr, err)
		return status
	}
}

// A command's stdout is the answer, raw, as a tcp answer is: no status, no
// content type, nothing wrapped around it; the command line is the words
// of the list, each quoted.
func TestAnSSHExecCollectorAnswersTheCommandsOutput(t *testing.T) {
	f := newExecFixture(t, scripted("load 0.42\n", "", 0))
	resp, err := f.fetch(f.collector(t, "c", []string{"/usr/bin/uptime", "-p"}), RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "load 0.42\n" || !resp.NoStatus || len(resp.Headers) != 0 || resp.Target != f.server.Addr {
		t.Errorf("answer %q, NoStatus %v, headers %v, target %q", resp.Body, resp.NoStatus, resp.Headers, resp.Target)
	}
	if execs := f.server.Execs(); len(execs) != 1 || execs[0].Command != `'/usr/bin/uptime' '-p'` {
		t.Errorf("the server ran %+v", execs)
	}
}

// Exit status 0 is a success, and any other a failure saying the status,
// unless success_exit_codes lists it, which then replaces 0.
func TestAnSSHExecCollectorsExitStatusDecides(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		codes  []int
		want   string
	}{
		"0":                {0, nil, ""},
		"1":                {1, nil, "exited with 1, and wrote nothing to stderr"},
		"1 of [0, 1]":      {1, []int{0, 1}, ""},
		"2 of [0, 1]":      {2, []int{0, 1}, "exited with 2, which request.success_exit_codes [0 1] does not list"},
		"0 of [1]":         {0, []int{1}, "exited with 0, which request.success_exit_codes [1] does not list"},
		"255, ssh's own":   {255, nil, "exited with 255"},
		"1 of [1] matches": {1, []int{1}, ""},
	} {
		f := newExecFixture(t, scripted("x 1\n", "", tc.status))
		resp, err := f.fetch(f.collector(t, "c", []string{"grep", "-c", "x", "/var/log/app"}, withExitCodes(tc.codes...)), RequestOverrides{})
		switch {
		case tc.want == "" && (err != nil || string(resp.Body) != "x 1\n"):
			t.Errorf("%s: %v, want the output", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), "the command on "+f.server.Addr+" "+tc.want)):
			t.Errorf("%s: err=%v, want it to say %q", name, err, tc.want)
		}
	}
}

// A failed command's error holds the start of its stderr, what reads as a
// credential in it masked, and no more of it than the bound; a command
// killed by a signal says so.
func TestAnSSHExecFailureShowsTheStartOfStderr(t *testing.T) {
	f := newExecFixture(t, scripted("", "df: /nope: No such file or directory (password=hunter2)\n", 1))
	_, err := f.fetch(f.collector(t, "c", []string{"df", "/nope"}), RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "exited with 1; its stderr began: df: /nope: No such file or directory") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err=%v, want stderr's start without the password", err)
	}
	long := newExecFixture(t, scripted("", strings.Repeat("e", 10<<10), 3))
	_, err = long.fetch(long.collector(t, "c", []string{"df"}), RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "exited with 3") || len(err.Error()) > 1500 || !strings.Contains(err.Error(), "(2048 bytes)") {
		t.Errorf("err=%v (%d bytes), want stderr cut to its kept start, shown to 1 KiB", err, len(fmt.Sprint(err)))
	}
	signalled := newExecFixture(t, scripted("", "Terminated\n", -15))
	_, err = signalled.fetch(signalled.collector(t, "c", []string{"df"}, withExitCodes(0, 1)), RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "the command on "+signalled.server.Addr+" was killed by signal TERM; its stderr began: Terminated") {
		t.Errorf("err=%v, want the signal", err)
	}
}

// Every word of the command reaches the host's shell as the one word it is
// in the list, whatever it holds: blanks, quotes, $, a backquote, a glob, a
// separator and a filled placeholder each arrive unchanged.
func TestAnSSHExecCommandsWordsReachTheShellUnchanged(t *testing.T) {
	f := newExecFixture(t, sshtest.Shell)
	words := []string{"a b", `it's "quoted"`, "$HOME", "`id`", "*", "; rm -rf /", "x\ny", "", "--unit={{param_unit}}"}
	command := append([]string{"printf", `[%s]\n`}, words...)
	resp, err := f.fetch(f.collector(t, "c", command), RequestOverrides{Params: map[string]string{"param_unit": "nginx.service"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "[a b]\n[it's \"quoted\"]\n[$HOME]\n[`id`]\n[*]\n[; rm -rf /]\n[x\ny]\n[]\n[--unit=nginx.service]\n"
	if string(resp.Body) != want {
		t.Errorf("the shell made of the words\n%s\nwant\n%s", resp.Body, want)
	}
}

// A placeholder's value, a probe's or a default, may hold only the
// characters of a name: a separator, a blank or a quote is refused before
// anything is sent, and so is a placeholder in the program.
func TestAnSSHExecPlaceholderValueIsHeldToOneWord(t *testing.T) {
	f := newExecFixture(t, scripted("ok\n", "", 0))
	c := f.collector(t, "c", []string{"systemctl", "is-active", "{{param_unit}}"})
	for _, value := range []string{"a;reboot", "a b", `a"b`, "a'b", "$(id)", "a\nb"} {
		_, err := f.fetch(c, RequestOverrides{Params: map[string]string{"param_unit": value}})
		if err == nil || !strings.Contains(err.Error(), "request.command[2]: the value of param_unit holds") {
			t.Errorf("%q: err=%v, want it refused", value, err)
		}
	}
	if f.server.Accepted() != 0 {
		t.Errorf("the server took %d connections for refused values", f.server.Accepted())
	}
	if _, err := f.fetch(c, RequestOverrides{Params: map[string]string{"param_unit": "nginx.service"}}); err != nil {
		t.Errorf("a value of a name: %v", err)
	}
}

// A placeholder's value may not begin its argument with a -, which the
// program would read as an option, as wc reads --files0-from=/etc/passwd,
// nor have .. as a path component; a - after what the configuration wrote
// is part of the value, and a .. within a name is no component. Each
// refusal names the parameter and why, and nothing is sent.
func TestAnSSHExecPlaceholderValueIsNoOptionAndNoClimb(t *testing.T) {
	f := newExecFixture(t, scripted("1\n", "", 0))
	c := f.collector(t, "c", []string{"wc", "-l", "{{param_file}}", "--unit={{param_unit:x}}", "/var/log/{{param_name:app}}", "{{param_a:}}{{param_b:x}}"})
	refused := map[string]struct{ param, value, want string }{
		"option":            {"param_file", "--files0-from=/etc/passwd", "request.command[2]: the value of param_file begins with -, and begins the argument"},
		"dash":              {"param_file", "-", "the value of param_file begins with -"},
		"after empty":       {"param_b", "-x", "request.command[5]: the value of param_b begins with -"},
		"dot dot":           {"param_name", "..", "request.command[4]: the value of param_name has .. as a path component"},
		"leading dot dot":   {"param_name", "../../etc/shadow", "has .. as a path component"},
		"trailing dot dot":  {"param_name", "app/..", "has .. as a path component"},
		"inner dot dot":     {"param_name", "app/../../etc", "has .. as a path component"},
		"dot dot in a file": {"param_file", "../x", "the value of param_file has .. as a path component"},
	}
	for name, tc := range refused {
		_, err := f.fetch(c, RequestOverrides{Params: map[string]string{"param_file": "/etc/hostname", tc.param: tc.value}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
		if _, err := CheckRequestParams(c, RequestOverrides{Params: map[string]string{"param_file": "/etc/hostname", tc.param: tc.value}}); err == nil {
			t.Errorf("%s: the probe's parameters pass the check a probe is answered 400 by", name)
		}
	}
	if f.server.Accepted() != 0 {
		t.Errorf("the server took %d connections for refused values", f.server.Accepted())
	}
	for name, params := range map[string]map[string]string{
		"option after a prefix": {"param_file": "/etc/hostname", "param_unit": "-x"},
		"dots in names":         {"param_file": "a..b", "param_name": "..app/x.../...", "param_b": "x-"},
		"a dash later":          {"param_file": "a-b", "param_a": "x", "param_b": "-y"},
	} {
		if _, err := f.fetch(c, RequestOverrides{Params: params}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		word, want string
	}{
		"option default":  {"{{param_p:-x}}", "request.command[1]: the default of param_p begins with -"},
		"dot dot default": {"/srv/{{param_p:../etc}}", "request.command[1]: the default of param_p has .. as a path component"},
	} {
		x := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHExec, Username: "m", PrivateKeyFile: "/k", KnownHostsFile: "/kh", Own: SSHExecConfig{Command: []string{"df", tc.word}}}}
		if err := ValidateRequest(&x); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	x := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHExec, Username: "m", PrivateKeyFile: "/k", KnownHostsFile: "/kh", Own: SSHExecConfig{Command: []string{"df", "--since={{param_p:-1d}}"}}}}
	if err := ValidateRequest(&x); err != nil {
		t.Errorf("a default after a prefix: %v", err)
	}
}

// A failed command's stderr is the operator's: the error says it, and what
// a probe's caller is answered (model.CallerText) says the status and
// where stderr is, not what it holds.
func TestAnSSHExecFailuresStderrIsNotForTheCaller(t *testing.T) {
	f := newExecFixture(t, scripted("", "wc: cannot open 'root:x:0:0:root:/root:/bin/bash'\n", 1))
	_, err := f.fetch(f.collector(t, "c", []string{"wc", "-l", "/etc/hostname"}), RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "root:x:0:0") {
		t.Fatalf("err=%v, want stderr in the error", err)
	}
	caller := model.CallerText(fmt.Errorf("wrapped: %w", err))
	want := "wrapped: the command on " + f.server.Addr + " exited with 1; what it wrote to stderr is in the exporter's log"
	if caller != want {
		t.Errorf("the caller is answered %q, want %q", caller, want)
	}
}

// stdout is held to the response limit as it comes: the scrape fails as
// over the limit, and the command is ended rather than read to its end:
// it is sent KILL and its session closed. Which of the two ends the
// stand-in's command first is the server's to say — the close fails the
// write the command is blocked in before the signal is read, as often as
// not — so the test waits for the command to have stopped, by either, and
// for the KILL to have been sent; it waited for the signal alone (Ended),
// which failed when the close came first.
func TestAnSSHExecOutputIsHeldToTheResponseLimit(t *testing.T) {
	var stopped atomic.Bool
	f := newExecFixture(t, func(ctx context.Context, _ string, stdout, _ io.Writer) int {
		defer stopped.Store(true)
		chunk := strings.Repeat("x", 1024)
		for ctx.Err() == nil {
			if _, err := io.WriteString(stdout, chunk); err != nil {
				return 1
			}
		}
		return 0
	})
	c := f.collector(t, "c", []string{"yes"}, func(r *model.RequestConfig) { r.MaxResponseBytes = 4096 })
	_, err := f.fetch(c, RequestOverrides{})
	if !errors.Is(err, model.ErrLimitExceeded) || !strings.Contains(err.Error(), "larger than the response limit of 4096 bytes") {
		t.Errorf("err=%v, want the response limit", err)
	}
	testutil.WaitFor(t, "the command stopped", stopped.Load)
	testutil.WaitFor(t, "the command sent KILL", func() bool {
		execs := f.server.Execs()
		return len(execs) == 1 && slices.Contains(execs[0].Signals, "KILL")
	})
}

// At the scrape's deadline the command is sent KILL and its session closed,
// which ends it on the host, and the scrape returns.
func TestAnSSHExecCommandIsKilledAtTheDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newExecFixture(t, func(ctx context.Context, _ string, _, _ io.Writer) int {
		started <- struct{}{}
		<-ctx.Done()
		return 137
	})
	c := f.collector(t, "c", []string{"sleep", "3600"})
	ctx, end := context.WithCancel(context.Background())
	deadline, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := FetchCollector(deadline, f.server.Addr, c, RequestOverrides{}, nil)
		done <- err
	}()
	<-started
	end()
	if err := <-done; !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "running the command on "+f.server.Addr) {
		t.Errorf("err=%v, want the scrape ended", err)
	}
	testutil.WaitFor(t, "the command ended", func() bool { return f.server.Ended() == 1 })
	if execs := f.server.Execs(); len(execs) != 1 || len(execs[0].Signals) != 1 || execs[0].Signals[0] != "KILL" {
		t.Errorf("the server saw %+v, want one command sent KILL", execs)
	}
}

// A command the host will not end at the deadline, as OpenSSH will not
// end a forced command — the signal refused, the close unanswered while
// the command lives — fails its scrape at the deadline and retires its
// connection rather than closing it: a scrape whose command runs on it
// meanwhile completes, a later scrape makes a new connection, and the
// retired one is closed once that command has ended.
func TestAnSSHExecStalledSessionRetiresItsConnection(t *testing.T) {
	stuck, slow := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	f := newExecFixtureWith(t, sshtest.Options{RefuseSignals: true, Exec: func(_ context.Context, command string, stdout, _ io.Writer) int {
		switch command {
		case "'sleep' 'forever'":
			<-stuck
		case "'slow'":
			<-slow
		}
		_, _ = io.WriteString(stdout, "done\n")
		return 0
	}})
	sshExecCloseUnanswered.Store(true)
	t.Cleanup(func() { sshExecCloseUnanswered.Store(false) })
	saved := sshAbandonGrace
	sshAbandonGrace = 50 * time.Millisecond
	t.Cleanup(func() { sshAbandonGrace = saved })

	slowDone := make(chan error, 1)
	go func() {
		_, err := f.fetch(f.collector(t, "slow", []string{"slow"}), RequestOverrides{})
		slowDone <- err
	}()
	testutil.WaitFor(t, "the slow command started", func() bool { return len(f.server.Execs()) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := FetchCollector(ctx, f.server.Addr, f.collector(t, "stuck", []string{"sleep", "forever"}), RequestOverrides{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want the scrape to fail at its deadline", err)
	}
	if f.server.Accepted() != 1 {
		t.Fatalf("the server took %d connections, want the two scrapes on one", f.server.Accepted())
	}
	testutil.WaitFor(t, "the stuck command sent KILL", func() bool {
		execs := f.server.Execs()
		return len(execs) == 2 && slices.Equal(execs[1].Signals, []string{"KILL"})
	})
	time.Sleep(4 * sshAbandonGrace)
	if resp, err := f.fetch(f.collector(t, "later", []string{"uptime"}), RequestOverrides{}); err != nil || string(resp.Body) != "done\n" {
		t.Fatalf("a later scrape: %v", err)
	}
	if f.server.Accepted() != 2 || f.server.Open() != 2 {
		t.Errorf("the server took %d connections, %d open, want a new one beside the retired one", f.server.Accepted(), f.server.Open())
	}
	close(slow)
	if err := <-slowDone; err != nil {
		t.Errorf("the scrape whose command ran on the retired connection: %v", err)
	}
	testutil.WaitFor(t, "the retired connection closed", func() bool { return f.server.Open() == 1 })
}

// A connection lost once the exec request was written is not retried: the
// host may have taken the command, which must not run twice. The scrape
// fails, and the host saw the command once.
func TestAnSSHExecCommandIsNotSentTwice(t *testing.T) {
	f := newExecFixtureWith(t, sshtest.Options{DropAtExec: true, Exec: scripted("x\n", "", 0)})
	_, err := f.fetch(f.collector(t, "c", []string{"/usr/local/bin/rotate"}), RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "the connection was lost once the command was sent, so it is not sent again") {
		t.Errorf("err=%v, want the scrape to fail", err)
	}
	if execs := f.server.Execs(); len(execs) != 1 {
		t.Errorf("the host took the command %d times, want once", len(execs))
	}
}

// A command reads nothing: the client sends the end of stdin at once, so a
// command that reads it, as cat does, sees its end and does not wait.
func TestAnSSHExecCommandReadsAnEmptyStdin(t *testing.T) {
	f := newExecFixtureWith(t, sshtest.Options{ExecInput: func(_ context.Context, _ string, stdin io.Reader, stdout, _ io.Writer) int {
		if _, err := io.Copy(stdout, stdin); err != nil {
			return 1
		}
		return 0
	}})
	start := time.Now()
	resp, err := f.fetch(f.collector(t, "c", []string{"cat"}), RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Body) != 0 {
		t.Fatalf("body %q, want an empty output", resp.Body)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cat took %v, want it to end at once", elapsed)
	}
}

// The host key is verified as sshfile verifies it: a host presenting a key
// known_hosts_file does not hold for it is refused, and nothing is run.
func TestAnSSHExecHostWithAnotherKeyIsRefused(t *testing.T) {
	f := newExecFixture(t, scripted("ok\n", "", 0))
	other := f.server.WriteKnownHosts(t, t.TempDir(), sshtest.NewEd25519Key(t))
	_, err := f.fetch(f.collector(t, "c", []string{"uptime"}, func(r *model.RequestConfig) { r.KnownHostsFile = other }), RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "which is not the one request.known_hosts_file") {
		t.Errorf("err=%v, want the host key refused", err)
	}
	if len(f.server.Execs()) != 0 {
		t.Errorf("the server ran %+v", f.server.Execs())
	}
}

// The collector's target lists refuse a host before anything is sent, and
// an address allowed_targets does not list when the connection is made.
func TestAnSSHExecCollectorHoldsTheTargetPolicy(t *testing.T) {
	f := newExecFixture(t, scripted("ok\n", "", 0))
	for name, change := range map[string]func(*model.RequestConfig){
		"denied":      func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} },
		"not allowed": func(r *model.RequestConfig) { r.AllowedTargets = []string{"10.0.0.0/8"} },
	} {
		_, err := f.fetch(f.collector(t, "policy", []string{"uptime"}, change), RequestOverrides{})
		if !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: err=%v, want the target refused", name, err)
		}
	}
	if f.server.Accepted() != 0 {
		t.Errorf("the server accepted %d connections", f.server.Accepted())
	}
}

// A static target may run the command as another account with credentials
// of its own, which replace the collector's together, and set its timeout;
// nothing of the command.
func TestAnSSHExecStaticTargetChangesTheAccountAlone(t *testing.T) {
	f := newExecFixture(t, scripted("ok\n", "", 0))
	c := f.collector(t, "c", []string{"uptime"}, func(r *model.RequestConfig) { r.PrivateKeyFile = f.dir + "/no-such-key" })
	if _, err := f.fetch(c, RequestOverrides{}); err == nil {
		t.Fatal("the collector's own, missing, key connected")
	}
	target := model.StaticTarget{Name: "t", Collector: "c", Target: f.server.Addr, Request: model.TargetRequestConfig{PasswordFile: f.passwordFile}}
	if err := CheckTargetRequest(&target, c); err != nil {
		t.Fatal(err)
	}
	if resp, err := f.fetch(c, TargetOverrides(&target)); err != nil || string(resp.Body) != "ok\n" {
		t.Errorf("with the target's password: %v", err)
	}
	for name, tc := range map[string]struct {
		request model.TargetRequestConfig
		want    string
	}{
		"path":            {model.TargetRequestConfig{Path: "x", PathSet: true}, "sets request.path, which does not apply"},
		"lone passphrase": {model.TargetRequestConfig{PassphraseFile: "/pp"}, "passphrase_file without request.private_key_file"},
		"placeholder":     {model.TargetRequestConfig{PasswordFile: "/{{param_x}}"}, "has a placeholder"},
		"no credential":   {model.TargetRequestConfig{PassphraseFile: "/pp", PrivateKeyFile: " "}, "written with blanks alone"},
	} {
		target := model.StaticTarget{Name: "t", Collector: "c", Target: "h", Request: tc.request}
		if err := CheckTargetRequest(&target, c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
}

// A debug probe's request section shows the host as ssh://host:port, EXEC,
// the account, the command line as it was sent, whether the connection was
// made for the scrape or kept, and how the command ended: its status, the
// bytes read and the start of stderr.
func TestAnSSHExecScrapeIsTraced(t *testing.T) {
	f := newExecFixture(t, scripted("up 3 days\n", "warning: token=abc123\n", 0))
	c := f.collector(t, "traced", []string{"uptime", "-p", "a b"})
	var shown []string
	for range 2 {
		ctx, trace := WithRequestTrace(context.Background())
		if _, err := FetchCollector(ctx, f.server.Addr, c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		for _, r := range trace.Requests() {
			shown = append(shown, fmt.Sprintf("%s %s %s %s %s -> %s", r.Method, r.URL, r.Header.Get("Ssh-User"), r.Header.Get("Ssh-Connection"), r.Header.Get("Ssh-Command"), r.Outcome))
		}
	}
	url := "ssh://" + f.server.Addr
	outcome := "exited with 0, read 10 bytes; stderr: warning: token=" + Redacted + "\n"
	want := []string{
		"EXEC " + url + " metrics new 'uptime' '-p' 'a b' -> " + outcome,
		"EXEC " + url + " metrics reused 'uptime' '-p' 'a b' -> " + outcome,
	}
	if strings.Join(shown, "\n") != strings.Join(want, "\n") {
		t.Errorf("traced\n%s\nwant\n%s", strings.Join(shown, "\n"), strings.Join(want, "\n"))
	}
}

// The labels of a scrape: ssh://host:port and EXEC.
func TestAnSSHExecScrapesLabels(t *testing.T) {
	c := &model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHExec}}
	rt := RequestTypes[RequestTypeSSHExec]
	if url, err := rt.Label("Host.Example", c, RequestOverrides{}); err != nil || url != "ssh://host.example:22" {
		t.Errorf("label %q %v", url, err)
	}
	if method := rt.Method(c, RequestOverrides{}); method != "EXEC" {
		t.Errorf("method %q", method)
	}
	if _, err := parseSSHTarget(RequestTypeSSHExec, "ssh://host/x"); err == nil || !strings.Contains(err.Error(), "an sshexec target is only the host and its port: the account is request.username, and the command request.command") {
		t.Errorf("a target with a path: %v", err)
	}
}

// The configuration's rules: a command, its program an absolute path or a
// bare name without a placeholder, its arguments' placeholder defaults
// held to a name's characters, exit statuses from 0 to 255, and the
// connection's keys as sshfile's.
func TestSSHExecConfigurationRules(t *testing.T) {
	base := func() model.Collector {
		return model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHExec, Username: "m", PrivateKeyFile: "/k", KnownHostsFile: "/kh", Own: SSHExecConfig{Command: []string{"df", "-P"}}}}
	}
	command := func(words ...string) func(*model.RequestConfig) {
		return func(r *model.RequestConfig) {
			own, _ := r.Own.(SSHExecConfig)
			own.Command = words
			r.Own = own
		}
	}
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"no command":          {command(), "request.command is required for request.type sshexec"},
		"blank program":       {command(" ", "x"), "the program, is empty or blanks alone"},
		"relative program":    {command("bin/check"), `request.command[0] "bin/check" is a relative path`},
		"dot program":         {command("./check"), "is a relative path"},
		"program placeholder": {command("{{param_p}}"), "has a placeholder; the program is run as it is written"},
		"control in program":  {command("df\n"), "holds a control character"},
		"NUL argument":        {command("df", "a\x00b"), "request.command[1] holds a NUL byte"},
		"bad default":         {command("df", "{{param_p:a;b}}"), `request.command[1]: the default of param_p holds ';'`},
		"blank default":       {command("df", "{{param_p:a b}}"), "the default of param_p holds a blank (U+0020)"},
		"long":                {command("df", strings.Repeat("x", 65<<10)), "more than the 65536 an sshexec collector may run"},
		"exit 256":            {withExitCodes(0, 256), "request.success_exit_codes[1] is 256; an exit status is a number from 0 to 255"},
		"exit -1":             {withExitCodes(-1), "request.success_exit_codes[0] is -1"},
		"no username":         {func(r *model.RequestConfig) { r.Username = "" }, "request.username is required for request.type sshexec: the account the command runs as on the host"},
		"no known_hosts":      {func(r *model.RequestConfig) { r.KnownHostsFile = "" }, "request.known_hosts_file is required for request.type sshexec"},
		"no credential":       {func(r *model.RequestConfig) { r.PrivateKeyFile = "" }, "an sshexec request authenticates with a private key, a password, or both"},
		"count 0":             {func(r *model.RequestConfig) { zero := 0; r.KeepaliveCountMax = &zero }, "request.keepalive_count_max must be at least 1"},
		"root":                {func(r *model.RequestConfig) { r.Root = "/srv" }, "request.root"},
	} {
		c := base()
		tc.change(&c.Request)
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	for name, change := range map[string]func(*model.RequestConfig){
		"absolute program":  command("/usr/local/bin/check-backup", "--since", "{{param_since:1d}}"),
		"bare program":      command("systemctl", "is-active", "nginx"),
		"quotes in an arg":  command("printf", `it's "%s"`),
		"exit codes":        withExitCodes(0, 1),
		"password and key":  func(r *model.RequestConfig) { r.PasswordFile = "/p" },
		"connection limits": func(r *model.RequestConfig) { r.MaxOpenConnections = 2 },
	} {
		c := base()
		change(&c.Request)
		if err := ValidateRequest(&c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The keys the opt-in sshexec type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheSSHExecTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeSSHExec)
}
