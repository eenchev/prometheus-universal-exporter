//go:build !select_request_types || request_type_sshfile || request_type_sshexec

package fetch

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// What the sshfile and sshexec request types share of their configuration
// and their targets: the target, host or host:port; the account and its
// credential files, a static target's among them; the keys of the
// connection kept to a host (sshconn.go); and the check of the files for
// --dry-run. Built with either type, so that each builds without the other.

// The defaults of the connection settings.
const (
	defaultSSHKeepaliveInterval = 30 * time.Second
	defaultSSHKeepaliveCountMax = 3
	defaultSSHIdleTimeout       = 5 * time.Minute
)

// sshDefaultPort is the port of a target that names none.
const sshDefaultPort = "22"

// sshMinKeepaliveInterval is the shortest keepalive_interval but 0: each
// keepalive is given the interval to be answered in, and one shorter than a
// round trip would close every connection as soon as it is made.
const sshMinKeepaliveInterval = time.Second

// validateSSHConnection checks the keys of how an sshfile or sshexec
// collector connects: its account, credential files, known_hosts_file,
// keepalives, idle timeout and max_open_connections. account says what the
// account is to the type, as "the file is read as".
func validateSSHConnection(x *model.Collector, account string) error {
	r := &x.Request
	if strings.TrimSpace(r.Username) == "" {
		return fmt.Errorf("collector %q request.username is required for request.type %s: the account %s on the host", x.Name, x.Request.Type, account)
	}
	if strings.TrimSpace(r.KnownHostsFile) == "" {
		return fmt.Errorf("collector %q request.known_hosts_file is required for request.type %s: the host's key is always verified, against the lines of this file (ssh-keyscan writes them)", x.Name, x.Request.Type)
	}
	if err := checkSSHCredentialKeys(x.Request.Type, r.Username, r.PrivateKeyFile, r.PassphraseFile, r.PasswordFile, r.Order); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkSSHFileKey("request.known_hosts_file", r.KnownHostsFile); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	durations := map[string]*model.Duration{"keepalive_interval": r.KeepaliveInterval, "idle_timeout": r.IdleTimeout}
	for _, key := range model.InFileOrder([]string{"keepalive_interval", "idle_timeout"}, r.Order) {
		if value := durations[key]; value != nil && *value < 0 {
			return fmt.Errorf("collector %q request.%s must not be negative", x.Name, key)
		}
	}
	if r.KeepaliveInterval != nil && *r.KeepaliveInterval > 0 && time.Duration(*r.KeepaliveInterval) < sshMinKeepaliveInterval {
		return fmt.Errorf("collector %q request.keepalive_interval %s is under the least, %s; set 0 to send no keepalives", x.Name, time.Duration(*r.KeepaliveInterval), sshMinKeepaliveInterval)
	}
	if r.KeepaliveCountMax != nil && *r.KeepaliveCountMax < 1 {
		return fmt.Errorf("collector %q request.keepalive_count_max must be at least 1: how many keepalives in a row may go unanswered before the connection is closed", x.Name)
	}
	if r.MaxOpenConnections < 0 {
		return fmt.Errorf("collector %q request.max_open_connections must not be negative; 0, the default, sets no limit", x.Name)
	}
	return nil
}

// checkSSHCredentialKeys holds the account and credential keys to their
// rules: a private key, a password or both; a passphrase only for a private
// key; none written with blanks alone, which is neither a name nor the key
// left out, which "" is; and no placeholder in any of them, since none is a
// template. An empty username is the collector's to require; a static
// target's empty one keeps the collector's. requestType names the type in
// the refusal, and order is the request block's keys in the order the file
// has them, of which a refusal names the first.
func checkSSHCredentialKeys(requestType, username, privateKeyFile, passphraseFile, passwordFile string, order []string) error {
	if strings.Contains(username, "{{") {
		return fmt.Errorf("request.username %q has a placeholder; the account is named as it is, so it takes none", username)
	}
	if err := checkSSHBlank("request.username", username); err != nil {
		return err
	}
	files := map[string]string{"private_key_file": privateKeyFile, "passphrase_file": passphraseFile, "password_file": passwordFile}
	inOrder := model.InFileOrder([]string{"private_key_file", "passphrase_file", "password_file"}, order)
	for _, key := range inOrder {
		if err := checkSSHBlank("request."+key, files[key]); err != nil {
			return err
		}
	}
	if strings.TrimSpace(passphraseFile) != "" && strings.TrimSpace(privateKeyFile) == "" {
		return errors.New("sets request.passphrase_file without request.private_key_file; the passphrase is the private key's")
	}
	if strings.TrimSpace(privateKeyFile) == "" && strings.TrimSpace(passwordFile) == "" {
		return fmt.Errorf("sets neither request.private_key_file nor request.password_file; an %s request authenticates with a private key, a password, or both (the key is tried first)", requestType)
	}
	for _, key := range inOrder {
		if err := checkSSHFileKey("request."+key, files[key]); err != nil {
			return err
		}
	}
	return nil
}

// checkSSHBlank refuses a key written with blanks alone: "" is the key left
// out, and blanks are no name of an account or a file.
func checkSSHBlank(key, value string) error {
	if value == "" || strings.TrimSpace(value) != "" {
		return nil
	}
	if key == "request.username" {
		return fmt.Errorf("%s is written with blanks alone; name the account, or leave the key out", key)
	}
	return fmt.Errorf("%s is written with blanks alone; name the file, or leave the key out", key)
}

// checkSSHFileKey refuses a placeholder in a key that names a file: like
// every credential file's, the path is read as it is written.
func checkSSHFileKey(key, value string) error {
	if strings.Contains(value, "{{") {
		return fmt.Errorf("%s %q has a placeholder; a credential file is named as it is, so it takes none", key, value)
	}
	return nil
}

// checkSSHTargetAccount checks the account and credential keys a static
// target's request block sets for an sshfile or sshexec collector, c. The
// credential files go together: a target that sets any of
// private_key_file, passphrase_file and password_file replaces all three of
// the collector's with its own, so it must name a key or a password of its
// own.
func checkSSHTargetAccount(c *model.Collector, t *model.StaticTarget) error {
	r := &t.Request
	if r.PrivateKeyFile == "" && r.PassphraseFile == "" && r.PasswordFile == "" {
		if strings.Contains(r.Username, "{{") {
			return fmt.Errorf("request.username %q has a placeholder; the account is named as it is, so it takes none", r.Username)
		}
		return checkSSHBlank("request.username", r.Username)
	}
	if err := checkSSHCredentialKeys(c.Request.Type, r.Username, r.PrivateKeyFile, r.PassphraseFile, r.PasswordFile, r.Order); err != nil {
		return fmt.Errorf("%w; a target that sets a credential file replaces the collector's credential files with its own", err)
	}
	return nil
}

// sshAddress is a target as an sshfile request reaches it: host is the
// name or address as the policy and known_hosts_file match it, and
// hostPort what is dialed.
type sshAddress struct {
	host     string
	hostPort string
}

// parseSSHTarget reads a target: host, host:port, [v6]:port, or the same
// after ssh://. Nothing else may come with it: the account is
// request.username, and the file request.root and request.path or the
// command request.command. requestType names the type in the refusal.
func parseSSHTarget(requestType, raw string) (sshAddress, error) {
	raw = strings.TrimSpace(raw)
	rest := raw
	if scheme, after, found := strings.Cut(rest, "://"); found {
		if scheme != "ssh" {
			return sshAddress{}, fmt.Errorf("target %q has the scheme %s://; an %s target is host, host:port or ssh://host:port", raw, scheme, requestType)
		}
		rest = after
	}
	rest = strings.TrimSuffix(rest, "/")
	if strings.ContainsAny(rest, "/?#@") {
		return sshAddress{}, fmt.Errorf("target %q has a path, query or user in it; an %s target is only the host and its port: the account is request.username, and %s", raw, requestType, sshTargetElsewhere(requestType))
	}
	host, port := rest, sshDefaultPort
	if h, p, err := net.SplitHostPort(rest); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
		host = rest[1 : len(rest)-1]
	} else if strings.Count(rest, ":") == 1 {
		return sshAddress{}, fmt.Errorf("target %q is not host or host:port; write an IPv6 address in brackets, as [2001:db8::1]:22", raw)
	}
	if host == "" {
		return sshAddress{}, fmt.Errorf("target %q names no host; an %s target is host, host:port or ssh://host:port", raw, requestType)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return sshAddress{}, fmt.Errorf("target %q has the port %q; want a number from 1 to 65535", raw, port)
	}
	canonical, err := canonicalHost(host)
	if err != nil {
		return sshAddress{}, err
	}
	return sshAddress{host: canonical, hostPort: net.JoinHostPort(canonical, port)}, nil
}

// sshTargetElsewhere says where what a target of the type does not hold
// is written instead.
func sshTargetElsewhere(requestType string) string {
	if requestType == RequestTypeSSHExec {
		return "the command request.command"
	}
	return "the file request.root and request.path"
}

// sshTargetHint is what the collectors page and the debug page say of a
// target: the account is request.username, never part of it.
func sshTargetHint(*model.Collector) (string, string) {
	return "SSH host", "host or host:" + sshDefaultPort
}

// sshDisplay renders a target for logs and labels: as written, since an
// SSH target holds no credentials, when it is one at all.
func sshDisplay(requestType, target string) string {
	if _, err := parseSSHTarget(requestType, target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// sshSettingsOf is how a scrape of c with overrides connects: the
// collector's account, credentials and connection settings, with a static
// target's account and credential files over them, and its policy.
func sshSettingsOf(c *model.Collector, overrides RequestOverrides) sshSettings {
	r := &c.Request
	s := sshSettings{
		username:          strings.TrimSpace(r.Username),
		privateKeyFile:    r.PrivateKeyFile,
		passphraseFile:    r.PassphraseFile,
		passwordFile:      r.PasswordFile,
		knownHostsFile:    r.KnownHostsFile,
		keepaliveInterval: defaultSSHKeepaliveInterval,
		keepaliveCountMax: defaultSSHKeepaliveCountMax,
		idleTimeout:       defaultSSHIdleTimeout,
		policy:            policyOf(c),
	}
	if r.MaxOpenConnections > 0 {
		s.owner = c.Name
	}
	if r.KeepaliveInterval != nil {
		s.keepaliveInterval = time.Duration(*r.KeepaliveInterval)
	}
	if r.KeepaliveCountMax != nil {
		s.keepaliveCountMax = *r.KeepaliveCountMax
	}
	if r.IdleTimeout != nil {
		s.idleTimeout = time.Duration(*r.IdleTimeout)
	}
	if name := strings.TrimSpace(overrides.Username); name != "" {
		s.username = name
	}
	if overrides.PrivateKeyFile != "" || overrides.PassphraseFile != "" || overrides.PasswordFile != "" {
		s.privateKeyFile, s.passphraseFile, s.passwordFile = overrides.PrivateKeyFile, overrides.PassphraseFile, overrides.PasswordFile
	}
	return s
}

// onSSHHost names the host in a failed read's error, unless the error
// names it already, as every error of making the connection does.
func onSSHHost(err error, hostPort string) error {
	if strings.Contains(err.Error(), hostPort) {
		return err
	}
	return fmt.Errorf("%w (on %s)", err, hostPort)
}

// checkSSHFiles reads the credential files and known_hosts_file of a
// collector, or the credential files of a static target that sets its own,
// as making a connection reads them, for --dry-run: a file missing or
// unreadable, a key that is none or is encrypted without its passphrase,
// a passphrase that does not decrypt it, an empty password, a known_hosts
// file without a line or with one that is none. Loading the configuration
// reads none of them, since a file may be mounted after the exporter starts.
func checkSSHFiles(c *model.Collector, t *model.StaticTarget) []string {
	who := fmt.Sprintf("collector %q", c.Name)
	overrides := RequestOverrides{}
	if t != nil {
		r := &t.Request
		if r.PrivateKeyFile == "" && r.PassphraseFile == "" && r.PasswordFile == "" {
			return nil
		}
		who, overrides = fmt.Sprintf("target %q", t.Name), TargetOverrides(t)
	}
	s := sshSettingsOf(c, overrides)
	var problems []error
	if s.privateKeyFile != "" {
		if _, err := sshSigner(s.privateKeyFile, s.passphraseFile); err != nil {
			problems = append(problems, err)
		} else if s.passphraseFile != "" {
			if _, err := ReadCredentialFile(s.passphraseFile); err != nil {
				problems = append(problems, fmt.Errorf("reading request.passphrase_file: %w", err))
			}
		}
	}
	if s.passwordFile != "" {
		if _, err := readSSHPassword(s.passwordFile); err != nil {
			problems = append(problems, err)
		}
	}
	if t == nil {
		if _, err := readKnownHosts(s.knownHostsFile); err != nil {
			problems = append(problems, err)
		}
	}
	warnings := make([]string, 0, len(problems))
	for _, err := range problems {
		warnings = append(warnings, fmt.Sprintf("%s: %v; a scrape would fail on it now (the file is read when a connection is made, so one put in place later is used then)", who, err))
	}
	return warnings
}
