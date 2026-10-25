//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The sshfile request type is localfile for another machine: it reads a
// file, or a directory of files, from a remote host over SFTP, and decodes,
// transforms and checks it exactly as localfile does a file of its own.
//
//	request:
//	  type: sshfile
//	  root: /var/lib/node_exporter/textfile_collector  # required, on the host
//	  path: backup.prom                                # or files: ["*.prom"]
//	  username: metrics                                # required
//	  private_key_file: /etc/exporter/ssh/id_ed25519   # and/or password_file
//	  known_hosts_file: /etc/exporter/ssh/known_hosts  # required
//
// The probe's target is the host, host:port or ssh://host:port, port 22 when
// it is left out. Everything the read shares with localfile — the path
// rules, regular files only, a file read again when it changes under the
// read, max_age, the response limit, the directory rules — is the same code
// (filereads.go, localfile_directory.go), over an SFTP session instead of
// an os.Root (sshsource.go).
//
//   - The host's key is always verified, against known_hosts_file: there is
//     no option to skip it.
//   - The connection to a host is kept and shared by every scrape that
//     reaches it with the same account and settings, with keepalives that
//     find a dead one, and closed when nothing has used it for idle_timeout
//     (sshconn.go).
//   - A read is confined to root twice: lexically, as localfile's names are,
//     and on the server, where every symbolic link on the way is followed by
//     the exporter and a path it leads outside root refused. The server
//     resolves the path again when the file is opened, so a link swapped in
//     between the check and the open is the server's to resolve: the account
//     the exporter reads as should be one that can read nothing it must not.
//
// This file and the sshconn.go and sshsource.go files are the only ones that
// import golang.org/x/crypto and github.com/pkg/sftp, all behind this type's
// build constraint, so a build without the type does not link them.

func init() {
	registerRequestType(&RequestType{
		Name: RequestTypeSSHFile,
		Fields: []string{
			"root", "path", "files", "max_files", "max_total_bytes", "max_age", "max_response_bytes",
			"username", "private_key_file", "passphrase_file", "password_file", "known_hosts_file",
			"keepalive_interval", "keepalive_count_max", "idle_timeout", "max_open_connections",
			"allowed_targets", "denied_targets",
		},
		Overrides: []string{"path", "timeout", PathParamPrefix},
		TargetFields: []string{
			"path", "timeout",
			"username", "private_key_file", "passphrase_file", "password_file",
		},
		Validate:       validateSSHFileRequest,
		FileExtensions: true,
		TargetHint:     func(*model.Collector) string { return "host or host:port" },
		Fetch:          fetchSSHFile,
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, fmt.Sprintf("its request.path (%q) or its label values", c.Request.Path))
		},
		SchemaRule:         sshFileSchemaRule,
		CheckTarget:        checkSSHTarget,
		CheckTargetRequest: checkSSHTargetRequest,
		Label:              sshFileLabel,
		Method:             func(*model.Collector, RequestOverrides) string { return localFileMethod },
		Display:            sshDisplay,
		Stage:              "ssh",
		CheckOverride: func(c *model.Collector, key string) error {
			// As for localfile: a param_ parameter can still fill a label
			// value of a collector reading a directory.
			if readsDirectory(c) && (key == "path" || strings.HasPrefix(key, PathParamPrefix) && c.LabelParams == nil) {
				return fmt.Errorf("collector %q reads every file of request.root that request.files matches, so there is no file for it to name", c.Name)
			}
			return nil
		},
		CheckPath:        checkSSHPath,
		CheckFiles:       checkSSHFiles,
		Follow:           sshConns.follow,
		CloseConnections: sshConns.closeAll,
		Connections:      sshConns.collectorConnections,
		ConnectionLimit:  sshConns.limitCounts,
		Families: []string{
			"http_exporter_sshfile_connections", "http_exporter_sshfile_connects_total",
			"http_exporter_sshfile_limit_waits_total", "http_exporter_sshfile_limit_failures_total",
		},
	})
}

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

func validateSSHFileRequest(x *model.Collector) error {
	if err := validateFileRequest(x, RequestTypeSSHFile, false); err != nil {
		return err
	}
	if err := validateLocalDirectory(x); err != nil {
		return err
	}
	r := &x.Request
	if err := checkSSHPath("request.root", r.Root); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkSSHPath("request.path", r.Path); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if strings.TrimSpace(r.Username) == "" {
		return fmt.Errorf("collector %q request.username is required for request.type sshfile: the account the file is read as on the host", x.Name)
	}
	if strings.TrimSpace(r.KnownHostsFile) == "" {
		return fmt.Errorf("collector %q request.known_hosts_file is required for request.type sshfile: the host's key is always verified, against the lines of this file (ssh-keyscan writes them)", x.Name)
	}
	if err := checkSSHCredentialKeys(r.Username, r.PrivateKeyFile, r.PassphraseFile, r.PasswordFile); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	if err := checkSSHFileKey("request.known_hosts_file", r.KnownHostsFile); err != nil {
		return fmt.Errorf("collector %q %w", x.Name, err)
	}
	for key, value := range map[string]*model.Duration{"keepalive_interval": r.KeepaliveInterval, "idle_timeout": r.IdleTimeout} {
		if value != nil && *value < 0 {
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

// checkSSHPath refuses a backslash in request.root, or in a path under it:
// the lexical check of a path is the exporter's, for which \ is a character
// of a name, and a Windows SFTP server would take it as a separator, so that
// ..\.. would leave root. what names the key or the probe parameter.
func checkSSHPath(what, name string) error {
	if strings.Contains(name, `\`) {
		return fmt.Errorf("%s %q has a backslash; a path on the host is written with / alone, and a backslash, which a Windows SFTP server reads as a separator, is refused", what, name)
	}
	return nil
}

// checkSSHCredentialKeys holds the account and credential keys to their
// rules: a private key, a password or both; a passphrase only for a private
// key; none written with blanks alone, which is neither a name nor the key
// left out, which "" is; and no placeholder in any of them, since none is a
// template. An empty username is the collector's to require; a static
// target's empty one keeps the collector's.
func checkSSHCredentialKeys(username, privateKeyFile, passphraseFile, passwordFile string) error {
	if strings.Contains(username, "{{") {
		return fmt.Errorf("request.username %q has a placeholder; the account is named as it is, so it takes none", username)
	}
	if err := checkSSHBlank("request.username", username); err != nil {
		return err
	}
	for _, key := range []struct{ name, value string }{{"request.private_key_file", privateKeyFile}, {"request.passphrase_file", passphraseFile}, {"request.password_file", passwordFile}} {
		if err := checkSSHBlank(key.name, key.value); err != nil {
			return err
		}
	}
	if strings.TrimSpace(passphraseFile) != "" && strings.TrimSpace(privateKeyFile) == "" {
		return errors.New("sets request.passphrase_file without request.private_key_file; the passphrase is the private key's")
	}
	if strings.TrimSpace(privateKeyFile) == "" && strings.TrimSpace(passwordFile) == "" {
		return errors.New("sets neither request.private_key_file nor request.password_file; an sshfile request authenticates with a private key, a password, or both (the key is tried first)")
	}
	for key, value := range map[string]string{"request.private_key_file": privateKeyFile, "request.passphrase_file": passphraseFile, "request.password_file": passwordFile} {
		if err := checkSSHFileKey(key, value); err != nil {
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

// checkSSHTargetRequest checks what a static target's request block sets for
// an sshfile collector: its path, as a probe's path parameter is checked;
// and its account and credential keys. The credential files go together:
// a target that sets any of private_key_file, passphrase_file and
// password_file replaces all three of the collector's with its own, so it
// must name a key or a password of its own.
func checkSSHTargetRequest(_ *model.Collector, t *model.StaticTarget) error {
	r := &t.Request
	if r.PathSet {
		if err := checkLocalFileName("request.path", r.Path); err != nil {
			return err
		}
		if err := checkSSHPath("request.path", r.Path); err != nil {
			return err
		}
	}
	if r.PrivateKeyFile == "" && r.PassphraseFile == "" && r.PasswordFile == "" {
		if strings.Contains(r.Username, "{{") {
			return fmt.Errorf("request.username %q has a placeholder; the account is named as it is, so it takes none", r.Username)
		}
		return checkSSHBlank("request.username", r.Username)
	}
	if err := checkSSHCredentialKeys(r.Username, r.PrivateKeyFile, r.PassphraseFile, r.PasswordFile); err != nil {
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
// request.username and the file request.root and request.path.
func parseSSHTarget(raw string) (sshAddress, error) {
	raw = strings.TrimSpace(raw)
	rest := raw
	if scheme, after, found := strings.Cut(rest, "://"); found {
		if scheme != "ssh" {
			return sshAddress{}, fmt.Errorf("target %q has the scheme %s://; an sshfile target is host, host:port or ssh://host:port", raw, scheme)
		}
		rest = after
	}
	rest = strings.TrimSuffix(rest, "/")
	if strings.ContainsAny(rest, "/?#@") {
		return sshAddress{}, fmt.Errorf("target %q has a path, query or user in it; an sshfile target is only the host and its port: the account is request.username, and the file request.root and request.path", raw)
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
		return sshAddress{}, fmt.Errorf("target %q names no host; an sshfile target is host, host:port or ssh://host:port", raw)
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

// checkSSHTarget is a probe's and a static target's check of the target.
func checkSSHTarget(_ *model.Collector, target string, _ bool) error {
	_, err := parseSSHTarget(target)
	return err
}

// sshDisplay renders a target for logs and labels: as written, since an
// sshfile target holds no credentials, when it is one at all.
func sshDisplay(target string) string {
	if _, err := parseSSHTarget(target); err != nil {
		return "<invalid target>"
	}
	return strings.TrimSpace(target)
}

// sshFileLabel is the url label of a read: sftp://host:port/ and the file's
// path on the host, with path parameters as their placeholders, or the
// directory's, ending in /, for a collector reading one.
func sshFileLabel(target string, c *model.Collector, overrides RequestOverrides) (string, error) {
	address, err := parseSSHTarget(target)
	if err != nil {
		return "", err
	}
	if readsDirectory(c) {
		return "sftp://" + address.hostPort + sshDirectory(c.Request.Root), nil
	}
	file, err := fileUnderRoot("", c, overrides, false, sshNoFile)
	if err != nil {
		return "", err
	}
	return "sftp://" + address.hostPort + path.Join(c.Request.Root, file), nil
}

// sshNoFile is the error of a read that names no file.
const sshNoFile = "no file to read: set request.path, or give the probe a path parameter naming a file under request.root"

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

// fetchSSHFile reads a scrape's file, or directory, from the target host.
func fetchSSHFile(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseSSHTarget(target)
	if err != nil {
		return nil, err
	}
	file := ""
	if !readsDirectory(c) {
		if file, err = fileUnderRoot("", c, overrides, true, sshNoFile); err != nil {
			return nil, err
		}
		if err := checkSSHPath("file", file); err != nil {
			return nil, err
		}
	}
	if overrides.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overrides.Timeout)
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
	if readsDirectory(c) {
		read, err := readSSHDirectory(ctx, key, c, target)
		if err != nil {
			return nil, err
		}
		return &HTTPResponse{StatusCode: http.StatusOK, NoStatus: true, Target: target, Collector: c.Name, Duration: time.Since(start), Directory: read}, nil
	}
	full := path.Join(c.Request.Root, file)
	var read struct {
		body []byte
		info fs.FileInfo
	}
	err = sshConns.session(ctx, key, c.Name, c.Request.MaxOpenConnections, c.Request.Root, "sftp://"+address.hostPort+full, func(src *sftpSource) error {
		var err error
		read.body, read.info, err = readSourceFile(src, c.Request.Root, file, responseLimit(c))
		return err
	})
	if err != nil {
		return nil, onSSHHost(err, address.hostPort)
	}
	if err := checkMaxAge(c, full, read.info.ModTime()); err != nil {
		return nil, onSSHHost(err, address.hostPort)
	}
	resp := localFileResponse(file, read.body, read.info, c)
	resp.Target, resp.Duration = target, time.Since(start)
	return resp, nil
}

// readSSHDirectory reads the matching files of request.root on the host, as
// a localfile collector reads a directory: what was read when the deadline
// came is answered, and the files not read fail alone.
func readSSHDirectory(ctx context.Context, key sshConnKey, c *model.Collector, target string) (*DirectoryRead, error) {
	var read *DirectoryRead
	err := sshConns.directory(ctx, key, c.Name, c.Request.MaxOpenConnections, c.Request.Root, "sftp://"+key.address+sshDirectory(c.Request.Root), func(src *sftpSource, progress *directoryProgress) error {
		return readSourceDirectory(src, c, "", progress)
	}, func(r *DirectoryRead) { read = r })
	if err != nil {
		return nil, onSSHHost(err, key.address)
	}
	for i := range read.Files {
		if f := read.Files[i].Response; f != nil {
			f.Target = target
		}
	}
	return read, nil
}

// sshDirectory is root as a directory's URL path, ending in /: root / is
// written once.
func sshDirectory(root string) string {
	return strings.TrimSuffix(root, "/") + "/"
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

// sshFileSchemaRule is what an sshfile collector's request requires, as the
// exporter does (validateSSHFileRequest): root, username and
// known_hosts_file, each written with something other than ""; a private key
// or a password, or both; and a passphrase only beside a private key.
func sshFileSchemaRule() map[string]any {
	keys := []string{"root", "username", "known_hosts_file"}
	properties := map[string]any{}
	for _, key := range keys {
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
