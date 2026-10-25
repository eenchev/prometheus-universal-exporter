//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshFixture is an SSH server serving a temporary directory, root, and the
// exporter's files to reach it with: a private key the server authorizes, a
// password file holding the password it takes, and a known_hosts file of
// its host key.
type sshFixture struct {
	server                            *sshtest.Server
	root, dir                         string
	keyFile, passwordFile, knownHosts string
	// encryptedKey and passphraseFile are set by withEncryptedKey.
	encryptedKey, passphraseFile string
}

// sshPassword is the password the fixture's server takes.
const sshPassword = "s3cret-pass"

func newSSHFixture(t *testing.T, hostKeys ...ssh.Signer) *sshFixture {
	t.Helper()
	return newSSHFixtureWith(t, false, hostKeys...)
}

// newSSHFixtureWith is newSSHFixture with, when encrypted is set, a second
// private key, encrypted with the passphrase in passphraseFile. Encrypting
// and decrypting one takes a while, the more under the race detector, so
// only the tests of it make one.
func newSSHFixtureWith(t *testing.T, encrypted bool, hostKeys ...ssh.Signer) *sshFixture {
	t.Helper()
	f := &sshFixture{root: t.TempDir(), dir: t.TempDir()}
	keys := make([]ssh.PublicKey, 1, 2)
	f.keyFile, keys[0] = sshtest.WriteClientKey(t, f.dir, "id_ed25519", "")
	if encrypted {
		var key ssh.PublicKey
		f.encryptedKey, key = sshtest.WriteClientKey(t, f.dir, "id_encrypted", "open sesame")
		f.passphraseFile = sshtest.WriteFile(t, f.dir, "passphrase", "open sesame\n")
		keys = append(keys, key)
	}
	f.passwordFile = sshtest.WriteFile(t, f.dir, "password", sshPassword+"\n")
	f.server = sshtest.Start(t, sshtest.Options{Password: sshPassword, AuthorizedKeys: keys, HostKeys: hostKeys})
	f.knownHosts = f.server.WriteKnownHosts(t, f.dir)
	t.Cleanup(sshConns.closeAll)
	return f
}

// sshFileOwn edits an sshfile request's own keys.
func sshFileOwn(r *model.RequestConfig, edit func(*SSHFileConfig)) {
	own, _ := r.Own.(SSHFileConfig)
	edit(&own)
	r.Own = own
}

// editSSHFile edits an sshfile collector's own keys.
func editSSHFile(c *model.Collector, edit func(*SSHFileConfig)) {
	r := sshFileRequest(c)
	edit(&r)
	c.Request.Own = r
}

// collector is a validated sshfile collector of the fixture reading path
// with its private key, changed by change first.
func (f *sshFixture) collector(t *testing.T, name, path string, change ...func(*model.RequestConfig)) *model.Collector {
	t.Helper()
	c := model.Collector{
		Name: name,
		Request: model.RequestConfig{
			Type: RequestTypeSSHFile, Root: f.root, Path: path,
			Username: "metrics", PrivateKeyFile: f.keyFile, Own: SSHFileConfig{KnownHostsFile: f.knownHosts},
		},
		Transform:     model.TransformConfig{Type: "prometheus"},
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

// fetch is a scrape of c on the fixture's server.
func (f *sshFixture) fetch(c *model.Collector, overrides RequestOverrides) (*HTTPResponse, error) {
	return FetchCollector(context.Background(), f.server.Addr, c, overrides, nil)
}

// sshEvents collects what happens to the connections of address
// (sshConnEvent) until the test ends.
func sshEvents(t *testing.T, address string) <-chan string {
	t.Helper()
	events := make(chan string, 256)
	hook := func(event, at string) {
		if at == address {
			select {
			case events <- event:
			default:
			}
		}
	}
	sshConnEvent.Store(&hook)
	t.Cleanup(func() { sshConnEvent.Store(nil) })
	return events
}

// waitEvent waits for event among events, failing the test when the
// connection is closed for another reason first.
func waitEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	deadline := time.After(time.Minute)
	for {
		select {
		case event := <-events:
			if event == want {
				return
			}
			if strings.HasPrefix(event, "closed: ") && strings.HasPrefix(want, "closed: ") {
				t.Fatalf("the connection was %s, want %s", event, want)
			}
		case <-deadline:
			t.Fatalf("no %q event in a minute", want)
		}
	}
}

const sshPromFile = "# TYPE app_jobs_total counter\napp_jobs_total 7\n"

// A file is read over SFTP with a private key, and presented as localfile
// presents one: no status, its content type from its extension, its length
// and its modification time.
func TestSSHFileReadsAFileWithAPrivateKey(t *testing.T) {
	f := newSSHFixture(t)
	file := testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	modified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(file, modified, modified); err != nil {
		t.Fatal(err)
	}
	resp, err := f.fetch(f.collector(t, "remote", "app.prom"), RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != sshPromFile || !resp.NoStatus || resp.Status() != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("resp=%+v", resp)
	}
	for header, want := range map[string]string{"Content-Type": "text/plain; version=0.0.4", "Content-Length": strconv.Itoa(len(sshPromFile)), "Last-Modified": "Fri, 02 Jan 2026 03:04:05 GMT"} {
		if got := resp.Headers.Get(header); got != want {
			t.Errorf("%s=%q, want %q", header, got, want)
		}
	}
}

// A password, a private key encrypted with a passphrase, and a private key
// the server refuses followed by a password it takes all authenticate; the
// key is tried first.
func TestSSHFileAuthenticatesWithAPasswordAndAnEncryptedKey(t *testing.T) {
	f := newSSHFixtureWith(t, true)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	stranger, _ := sshtest.WriteClientKey(t, f.dir, "stranger", "")
	for name, change := range map[string]func(*model.RequestConfig){
		"password": func(r *model.RequestConfig) { r.PrivateKeyFile, r.PasswordFile = "", f.passwordFile },
		"encrypted": func(r *model.RequestConfig) {
			r.PrivateKeyFile = f.encryptedKey
			sshFileOwn(r, func(o *SSHFileConfig) { o.PassphraseFile = f.passphraseFile })
		},
		"fallback": func(r *model.RequestConfig) { r.PrivateKeyFile, r.PasswordFile = stranger, f.passwordFile },
	} {
		if _, err := f.fetch(f.collector(t, name, "app.prom", change), RequestOverrides{}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Credentials the server refuses, an encrypted key without its passphrase,
// and a key file that is no key each fail the scrape saying so, naming the
// files and never their contents.
func TestSSHFileCredentialFailuresSaySo(t *testing.T) {
	f := newSSHFixtureWith(t, true)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	stranger, _ := sshtest.WriteClientKey(t, f.dir, "stranger", "")
	wrong := sshtest.WriteFile(t, f.dir, "wrong", "not-the-password")
	notKey := sshtest.WriteFile(t, f.dir, "notkey", "hello")
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"refused":   {func(r *model.RequestConfig) { r.PrivateKeyFile, r.PasswordFile = stranger, wrong }, `refused user "metrics": authentication failed with the private key in ` + stranger + " and the password in " + wrong},
		"user":      {func(r *model.RequestConfig) { r.Username = "root" }, `refused user "root"`},
		"encrypted": {func(r *model.RequestConfig) { r.PrivateKeyFile = f.encryptedKey }, "is encrypted; name the file of its passphrase in request.passphrase_file"},
		"passphrase": {func(r *model.RequestConfig) {
			r.PrivateKeyFile = f.encryptedKey
			sshFileOwn(r, func(o *SSHFileConfig) { o.PassphraseFile = wrong })
		}, "could not be decrypted with the passphrase in " + wrong},
		"not a key": {func(r *model.RequestConfig) { r.PrivateKeyFile = notKey }, "request.private_key_file " + notKey + " is not a private key"},
		"missing":   {func(r *model.RequestConfig) { r.PrivateKeyFile = filepath.Join(f.dir, "gone") }, "reading request.private_key_file"},
	} {
		_, err := f.fetch(f.collector(t, "c", "app.prom", tc.change), RequestOverrides{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want it to say %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "not-the-password") {
			t.Errorf("%s: the error shows the password: %v", name, err)
		}
	}
}

// The host key is always verified: a host known_hosts_file does not list,
// and one whose key is not the listed one, are refused saying what to do,
// with the key's fingerprint; an empty file, or one with a line that is no
// known_hosts line, is refused saying so.
func TestSSHFileVerifiesTheHostKey(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	presented := ssh.FingerprintSHA256(f.server.HostKeys[0].PublicKey())
	other := knownhosts.Line([]string{"[other.example]:22"}, f.server.HostKeys[0].PublicKey()) + "\n"
	for name, tc := range map[string]struct {
		content, want string
	}{
		"unknown":  {other, "is not in request.known_hosts_file"},
		"mismatch": {f.server.KnownHostsLine(sshtest.NewEd25519Key(t)), "which is not the one request.known_hosts_file"},
		"empty":    {"# nothing yet\n", "holds no host key"},
		"invalid":  {"this is no line\n", "cannot be read as known_hosts lines"},
	} {
		file := sshtest.WriteFile(t, f.dir, "known_"+strings.ReplaceAll(name, " ", "_"), tc.content)
		_, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.KnownHostsFile = file }) }), RequestOverrides{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want it to say %q", name, err, tc.want)
		}
		if (name == "unknown" || name == "mismatch") && (err == nil || !strings.Contains(err.Error(), presented)) {
			t.Errorf("%s: err=%v, want the fingerprint %s", name, err, presented)
		}
	}
	if _, err := f.fetch(f.collector(t, "c", "app.prom"), RequestOverrides{}); err != nil {
		t.Errorf("with its own line: %v", err)
	}
}

// A host that offers several host keys is asked for the one known_hosts_file
// has: an ECDSA key would be the client's first choice, and only the ed25519
// one is listed. A hashed line, as ssh-keyscan -H writes it, is read too.
func TestSSHFileAsksForTheHostKeyKnownHostsHas(t *testing.T) {
	ed := sshtest.NewEd25519Key(t)
	f := newSSHFixture(t, sshtest.NewECDSAKey(t), ed)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	plain := sshtest.WriteFile(t, f.dir, "known_ed", f.server.KnownHostsLine(ed))
	hashed := sshtest.WriteFile(t, f.dir, "known_hashed", knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(f.server.Addr))}, ed.PublicKey())+"\n")
	for _, file := range []string{plain, hashed} {
		if _, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.KnownHostsFile = file }) }), RequestOverrides{}); err != nil {
			t.Errorf("%s: %v", file, err)
		}
		sshConns.closeAll()
	}
}

// A read is confined to request.root: a symbolic link leading outside it is
// refused, one staying inside is followed, and a path parameter cannot name
// a directory above.
func TestSSHFileIsConfinedToRoot(t *testing.T) {
	f := newSSHFixture(t)
	outside := testutil.WriteIn(t, t.TempDir(), "secret.prom", "secret 1\n")
	testutil.WriteIn(t, f.root, "sub/real.prom", sshPromFile)
	for link, target := range map[string]string{"escape.prom": outside, "relative.prom": "../" + filepath.Base(filepath.Dir(outside)) + "/secret.prom", "inside.prom": "sub/real.prom", "dir": filepath.Dir(outside), "loop.prom": "loop.prom"} {
		if err := os.Symlink(target, filepath.Join(f.root, link)); err != nil {
			t.Fatal(err)
		}
	}
	// The relative link climbs from root to the outside directory's parent,
	// which is the temporary directories' own parent.
	if filepath.Dir(filepath.Dir(outside)) != filepath.Dir(f.root) {
		t.Skip("the temporary directories do not share a parent")
	}
	for _, path := range []string{"escape.prom", "relative.prom", "dir/secret.prom"} {
		_, err := f.fetch(f.collector(t, "c", path), RequestOverrides{})
		if err == nil || !strings.Contains(err.Error(), "leads outside request.root through a symbolic link") || strings.Contains(err.Error(), "secret 1") {
			t.Errorf("%s: err=%v, want it refused as leading outside root", path, err)
		}
	}
	if _, err := f.fetch(f.collector(t, "c", "loop.prom"), RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "leads through more than 40 symbolic links") {
		t.Errorf("a link to itself: %v", err)
	}
	resp, err := f.fetch(f.collector(t, "c", "inside.prom"), RequestOverrides{})
	if err != nil || string(resp.Body) != sshPromFile {
		t.Errorf("a link inside root: resp=%v err=%v", resp, err)
	}
	c := f.collector(t, "c", "{{param_name}}.prom")
	if _, err := f.fetch(c, RequestOverrides{Params: map[string]string{"param_name": "../x"}}); err == nil {
		t.Error("a path parameter climbing above root was taken")
	}
	if _, err := f.fetch(c, RequestOverrides{PathSet: true, Path: "../x.prom"}); err == nil || !strings.Contains(err.Error(), "leads outside request.root") {
		t.Errorf("a path probe parameter climbing above root: %v", err)
	}
}

// A path placeholder takes its value from the probe, and a missing file, a
// file older than max_age and one larger than max_response_bytes each fail
// saying so; the last as a limit error.
func TestSSHFilePlaceholdersAndLimits(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "eu.prom", sshPromFile)
	c := f.collector(t, "c", "{{param_region}}.prom")
	if resp, err := f.fetch(c, RequestOverrides{Params: map[string]string{"param_region": "eu"}}); err != nil || string(resp.Body) != sshPromFile {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if _, err := f.fetch(c, RequestOverrides{Params: map[string]string{"param_region": "us"}}); err == nil || !strings.Contains(err.Error(), "file "+filepath.Join(f.root, "us.prom")+" does not exist") {
		t.Errorf("a missing file: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(f.root, "eu.prom"), old, old); err != nil {
		t.Fatal(err)
	}
	aged := f.collector(t, "aged", "eu.prom", func(r *model.RequestConfig) { r.MaxAge = model.Duration(time.Hour) })
	if _, err := f.fetch(aged, RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "longer than request.max_age 1h0m0s") {
		t.Errorf("an old file: %v", err)
	}
	small := f.collector(t, "small", "eu.prom", func(r *model.RequestConfig) { r.MaxResponseBytes = 10 })
	if _, err := f.fetch(small, RequestOverrides{}); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("a file past max_response_bytes: %v", err)
	}
}

// A directory is read as localfile reads one: the matching files in name
// order, at most max_files, each refused alone past max_total_bytes.
func TestSSHFileReadsADirectory(t *testing.T) {
	f := newSSHFixture(t)
	for _, name := range []string{"a.prom", "b.prom", "c.prom", "d.prom"} {
		testutil.WriteIn(t, f.root, name, sshPromFile)
	}
	testutil.WriteIn(t, f.root, "big.prom", sshPromFile+strings.Repeat("# padding\n", 10))
	testutil.WriteIn(t, f.root, "notes.txt", "not matched")
	testutil.WriteIn(t, f.root, ".hidden.prom", sshPromFile)
	c := f.collector(t, "dir", "", func(r *model.RequestConfig) {
		r.Files = []string{"*.prom"}
		r.MaxFiles = 4
		r.MaxTotalBytes = model.ByteSize(3 * len(sshPromFile))
	})
	resp, err := f.fetch(c, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	read := resp.Directory
	if read == nil || read.Path != f.root || read.Matched != 5 || len(read.Skipped) != 1 || read.Skipped[0] != "d.prom" {
		t.Fatalf("read=%+v", read)
	}
	var got []string
	for _, file := range read.Files {
		state := "ok"
		if file.Err != nil {
			state = "refused"
			if !errors.Is(file.Err, model.ErrLimitExceeded) {
				t.Errorf("%s: %v", file.Name, file.Err)
			}
		}
		got = append(got, file.Name+" "+state)
	}
	if want := "a.prom ok,b.prom ok,big.prom refused,c.prom ok"; strings.Join(got, ",") != want {
		t.Errorf("files %v, want %s", got, want)
	}
	if _, err := f.fetch(c, RequestOverrides{PathSet: true, Path: "a.prom"}); err == nil {
		t.Log("the path parameter of a directory reader is refused by CheckOverrideParams before a fetch")
	}
}

// One connection serves every scrape of a host: twenty at once, before
// there is any, wait for the one the first makes, and three in a row after
// them use it too; the server accepted one connection.
func TestSSHFileKeepsOneConnectionForAHost(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "shared", "app.prom")
	scrapes := alloctest.UnlessRaced(20, 8)
	var wg sync.WaitGroup
	errs := make(chan error, scrapes)
	for range scrapes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := f.fetch(c, RequestOverrides{})
			if err == nil && string(resp.Body) != sshPromFile {
				err = fmt.Errorf("body %q", resp.Body)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	for range 3 {
		if _, err := f.fetch(c, RequestOverrides{}); err != nil {
			t.Fatal(err)
		}
	}
	open, made := sshConns.collectorConnections("shared")
	if open != 1 || made != 1 || f.server.Accepted() != 1 {
		t.Errorf("open=%d made=%d accepted=%d, want one connection", open, made, f.server.Accepted())
	}
}

// A connection that ended is noticed by the next scrape, which connects
// again within its own deadline.
func TestSSHFileConnectsAgainAfterTheConnectionEnded(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "again", "app.prom")
	events := sshEvents(t, f.server.Addr)
	if _, err := f.fetch(c, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	waitEvent(t, events, "closed: ended")
	restarted := sshtest.Start(t, sshtest.Options{Password: sshPassword, HostKeys: f.server.HostKeys})
	c.Request.PrivateKeyFile, c.Request.PasswordFile = "", f.passwordFile
	editSSHFile(c, func(r *SSHFileConfig) { r.KnownHostsFile = restarted.WriteKnownHosts(t, t.TempDir()) })
	if _, err := FetchCollector(context.Background(), restarted.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, made := sshConns.collectorConnections("again"); made != 2 {
		t.Errorf("made=%d, want 2", made)
	}
}

// A read that loses its connection connects again, once, and reads; and a
// credential file that changed on disk gets a new connection at the next
// scrape, made with the file as it is now.
func TestSSHFileReadsAgainOnANewConnection(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "lost", "app.prom", func(r *model.RequestConfig) { r.PrivateKeyFile, r.PasswordFile = "", f.passwordFile })
	if _, err := f.fetch(c, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	hook := func() { once.Do(f.server.DropConnections) }
	beforeSSHRead.Store(&hook)
	t.Cleanup(func() { beforeSSHRead.Store(nil) })
	if resp, err := f.fetch(c, RequestOverrides{}); err != nil || string(resp.Body) != sshPromFile {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if _, made := sshConns.collectorConnections("lost"); made != 2 {
		t.Errorf("made=%d, want a second connection for the read that lost the first", made)
	}
	sshtest.WriteFile(t, f.dir, "password", sshPassword+"\n\n")
	if _, err := f.fetch(c, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	if _, made := sshConns.collectorConnections("lost"); made != 3 {
		t.Errorf("made=%d, want a new connection for the changed password file", made)
	}
}

// A host that stops answering is found by the keepalives: after
// keepalive_count_max unanswered, the connection is closed, and the next
// scrape makes a new one.
func TestSSHFileKeepalivesCloseADeadConnection(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	interval := model.Duration(20 * time.Millisecond)
	most := 2
	c := f.collector(t, "alive", "app.prom", func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.KeepaliveCountMax = &most }) })
	// Past the check of the configuration, which refuses an interval under
	// a second, so the test waits milliseconds for what takes seconds.
	editSSHFile(c, func(r *SSHFileConfig) { r.KeepaliveInterval = &interval })
	events := sshEvents(t, f.server.Addr)
	if _, err := f.fetch(c, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "keepalive")
	f.server.Freeze()
	waitEvent(t, events, "closed: keepalive")
	f.server.Thaw()
	if _, err := f.fetch(c, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	if _, made := sshConns.collectorConnections("alive"); made != 2 {
		t.Errorf("made=%d, want a new connection after the dead one", made)
	}
}

// A connection nothing uses for idle_timeout is closed; with idle_timeout 0
// it is kept, keepalive after keepalive.
func TestSSHFileIdleTimeout(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	short := model.Duration(30 * time.Millisecond)
	events := sshEvents(t, f.server.Addr)
	if _, err := f.fetch(f.collector(t, "idle", "app.prom", func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.IdleTimeout = &short }) }), RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "closed: idle")
	never, interval := model.Duration(0), model.Duration(10*time.Millisecond)
	kept := f.collector(t, "kept", "app.prom", func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.IdleTimeout = &never }) })
	// Past the check of the configuration, which refuses an interval under
	// a second, so the test waits milliseconds for what takes seconds.
	editSSHFile(kept, func(r *SSHFileConfig) { r.KeepaliveInterval = &interval })
	if _, err := f.fetch(kept, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		waitEvent(t, events, "keepalive")
	}
	if open, _ := sshConns.collectorConnections("kept"); open != 1 {
		t.Errorf("open=%d after five keepalives, want the connection kept", open)
	}
}

// A configuration that changes a collector's connection settings, or drops
// the collector, closes the connections only it used; one another collector
// still uses stays.
func TestSSHFileFollowingAConfigurationClosesWhatNoCollectorUses(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	events := sshEvents(t, f.server.Addr)
	a, b := f.collector(t, "a", "app.prom"), f.collector(t, "b", "app.prom")
	password := f.collector(t, "p", "app.prom", func(r *model.RequestConfig) { r.PrivateKeyFile, r.PasswordFile = "", f.passwordFile })
	for _, c := range []*model.Collector{a, b, password} {
		if _, err := f.fetch(c, RequestOverrides{}); err != nil {
			t.Fatal(err)
		}
	}
	if sshConns.size() != 2 {
		t.Fatalf("%d connections, want a and b's and p's", sshConns.size())
	}
	// a goes, and p reads with the key now: the password connection is p's
	// alone, and closed; the key connection is still b's.
	changed := *password
	changed.Request.PrivateKeyFile, changed.Request.PasswordFile = f.keyFile, ""
	sshConns.follow([]model.Collector{*b, changed}, nil)
	waitEvent(t, events, "closed: reload")
	if sshConns.size() != 1 {
		t.Errorf("%d connections, want b's", sshConns.size())
	}
	if open, _ := sshConns.collectorConnections("b"); open != 1 {
		t.Errorf("b has %d connections open", open)
	}
}

// A host that stops answering during a read does not hold the scrape past
// its deadline; the connection the read is left on is dropped.
func TestSSHFileReturnsAtTheDeadlineWhenTheHostStalls(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "stall", "app.prom")
	events := sshEvents(t, f.server.Addr)
	if _, err := f.fetch(c, RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	saved := sshAbandonGrace
	sshAbandonGrace = 0
	t.Cleanup(func() { sshAbandonGrace = saved })
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	started := make(chan struct{}, 1)
	hook := func() { started <- struct{}{} }
	beforeSSHRead.Store(&hook)
	t.Cleanup(func() { beforeSSHRead.Store(nil) })
	ctx, end := testutil.DeadlineEndedByHand()
	done := make(chan error, 1)
	go func() {
		_, err := FetchCollector(ctx, f.server.Addr, c, RequestOverrides{}, nil)
		done <- err
	}()
	<-started
	end()
	if err := <-done; err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err=%v, want the deadline", err)
	}
	waitEvent(t, events, "closed: stalled")
}

// The collector's denied_targets refuses the host before anything is sent,
// and allowed_targets that does not list it refuses its address when the
// connection is made.
func TestSSHFileHoldsTheTargetPolicy(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	for name, change := range map[string]func(*model.RequestConfig){
		"denied":      func(r *model.RequestConfig) { r.DeniedTargets = []string{"127.0.0.0/8"} },
		"not allowed": func(r *model.RequestConfig) { r.AllowedTargets = []string{"10.0.0.0/8"} },
	} {
		_, err := f.fetch(f.collector(t, "policy", "app.prom", change), RequestOverrides{})
		if !errors.Is(err, ErrTargetRefused) {
			t.Errorf("%s: err=%v, want the target refused", name, err)
		}
	}
	if f.server.Accepted() != 0 {
		t.Errorf("the server accepted %d connections", f.server.Accepted())
	}
}

// A target is a host, with a port or ssh:// or both, and nothing else; one
// is required.
func TestSSHFileTargets(t *testing.T) {
	c := &model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHFile}}
	for target, want := range map[string]string{
		"host.example":            "host.example:22",
		"Host.Example.:2222":      "host.example:2222",
		"ssh://host.example":      "host.example:22",
		"ssh://host.example:2222": "host.example:2222",
		"[2001:db8::1]:2222":      "[2001:db8::1]:2222",
		"[2001:db8::1]":           "[2001:db8::1]:22",
		"2001:db8::1":             "[2001:db8::1]:22",
	} {
		a, err := parseSSHTarget(target)
		if err != nil || a.hostPort != want {
			t.Errorf("%q: %+v %v, want %s", target, a, err, want)
		}
	}
	for _, target := range []string{"http://host", "user@host", "ssh://host/path", "host:0", "host:x", ":22", "hö.example"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("%q was taken", target)
		}
	}
	if err := CheckTarget(c, "", false); !errors.Is(err, ErrMissingTarget) {
		t.Errorf("no target: %v", err)
	}
}

// The configuration's rules: root, username and known_hosts_file required,
// a private key or a password, a passphrase only with a key, no placeholder
// in a file key or the username, and the connection settings in range.
func TestSSHFileConfigurationRules(t *testing.T) {
	valid := func() model.Collector {
		return model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHFile, Root: "/srv/m", Path: "a.prom", Username: "m", PrivateKeyFile: "/k", Own: SSHFileConfig{KnownHostsFile: "/kh"}}}
	}
	negative, minusOne, zero := model.Duration(-time.Second), -1, 0
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"no root":        {func(r *model.RequestConfig) { r.Root = "" }, "request.root is required for request.type sshfile"},
		"relative root":  {func(r *model.RequestConfig) { r.Root = "srv" }, "must be an absolute path"},
		"no username":    {func(r *model.RequestConfig) { r.Username = " " }, "request.username is required"},
		"no known_hosts": {func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.KnownHostsFile = "" }) }, "request.known_hosts_file is required"},
		"no credential":  {func(r *model.RequestConfig) { r.PrivateKeyFile = "" }, "sets neither request.private_key_file nor request.password_file"},
		"lone passphrase": {func(r *model.RequestConfig) {
			r.PrivateKeyFile, r.PasswordFile = "", "/p"
			sshFileOwn(r, func(o *SSHFileConfig) { o.PassphraseFile = "/pp" })
		}, "passphrase_file without request.private_key_file"},
		"placeholder key": {func(r *model.RequestConfig) { r.PrivateKeyFile = "/k/{{param_x}}" }, "request.private_key_file \"/k/{{param_x}}\" has a placeholder"},
		"placeholder hosts": {func(r *model.RequestConfig) {
			sshFileOwn(r, func(o *SSHFileConfig) { o.KnownHostsFile = "/{{param_x}}" })
		}, "request.known_hosts_file \"/{{param_x}}\" has a placeholder"},
		"placeholder user": {func(r *model.RequestConfig) { r.Username = "{{param_x}}" }, "request.username \"{{param_x}}\" has a placeholder"},
		"negative keepalive": {func(r *model.RequestConfig) {
			sshFileOwn(r, func(o *SSHFileConfig) { o.KeepaliveInterval = &negative })
		}, "request.keepalive_interval must not be negative"},
		"negative idle": {func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.IdleTimeout = &negative }) }, "request.idle_timeout must not be negative"},
		"count 0":       {func(r *model.RequestConfig) { sshFileOwn(r, func(o *SSHFileConfig) { o.KeepaliveCountMax = &zero }) }, "request.keepalive_count_max must be at least 1"},
		"count -1": {func(r *model.RequestConfig) {
			sshFileOwn(r, func(o *SSHFileConfig) { o.KeepaliveCountMax = &minusOne })
		}, "request.keepalive_count_max must be at least 1"},
		"escaping path":      {func(r *model.RequestConfig) { r.Path = "../a.prom" }, "leads outside request.root"},
		"absolute path":      {func(r *model.RequestConfig) { r.Path = "/a.prom" }, "must be relative to request.root"},
		"path and files":     {func(r *model.RequestConfig) { r.Files = []string{"*.prom"} }, "sets both request.path and request.files"},
		"max_files alone":    {func(r *model.RequestConfig) { r.MaxFiles = 3 }, "applies only to a collector reading a directory"},
		"another type's key": {func(r *model.RequestConfig) { r.BearerToken = "x" }, `sets request.bearer_token, which does not apply to request.type "sshfile"`},
	} {
		c := valid()
		tc.change(&c.Request)
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	c := valid()
	c.Request.PasswordFile = "/p"
	editSSHFile(&c, func(r *SSHFileConfig) { r.PassphraseFile = "/pp" })
	if err := ValidateRequest(&c); err != nil {
		t.Errorf("a key with its passphrase and a password: %v", err)
	}
	settings := sshSettingsOf(&c, RequestOverrides{})
	if settings.keepaliveInterval != 30*time.Second || settings.keepaliveCountMax != 3 || settings.idleTimeout != 5*time.Minute {
		t.Errorf("defaults %+v", settings)
	}
	editSSHFile(&c, func(r *SSHFileConfig) { r.KeepaliveInterval, r.IdleTimeout = new(model.Duration), new(model.Duration) })
	if settings := sshSettingsOf(&c, RequestOverrides{}); settings.keepaliveInterval != 0 || settings.idleTimeout != 0 {
		t.Errorf("0 must switch keepalives and the idle timeout off: %+v", settings)
	}
}

// A static target may set its account and credential files, which replace
// the collector's together, and its path and timeout; nothing else of the
// type's, and no file key with a placeholder.
func TestSSHFileStaticTargetRequest(t *testing.T) {
	c := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHFile, Root: "/srv/m", Path: "a.prom", Username: "m", PrivateKeyFile: "/k", Own: SSHFileConfig{KnownHostsFile: "/kh"}}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	ok := model.StaticTarget{Name: "t", Collector: "c", Target: "h", Request: model.TargetRequestConfig{Username: "other", PasswordFile: "/pw", Timeout: model.Duration(time.Second)}}
	if err := CheckTargetRequest(&ok, &c); err != nil {
		t.Errorf("%v", err)
	}
	settings := sshSettingsOf(&c, TargetOverrides(&ok))
	if settings.username != "other" || settings.privateKeyFile != "" || settings.passwordFile != "/pw" {
		t.Errorf("the target's credentials %+v, want them to replace the collector's together", settings)
	}
	for name, tc := range map[string]struct {
		request model.TargetRequestConfig
		want    string
	}{
		"lone passphrase": {model.TargetRequestConfig{PassphraseFile: "/pp"}, "passphrase_file without request.private_key_file"},
		"placeholder":     {model.TargetRequestConfig{PasswordFile: "/{{param_x}}"}, "has a placeholder"},
		"known_hosts":     {model.TargetRequestConfig{Headers: map[string]string{"a": "b"}}, "sets request.headers, which does not apply"},
		"escaping path":   {model.TargetRequestConfig{Path: "../x", PathSet: true}, "leads outside request.root"},
	} {
		target := model.StaticTarget{Name: "t", Collector: "c", Target: "h", Request: tc.request}
		if err := CheckTargetRequest(&target, &c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
}

// A debug probe's request section shows the host, the account, the file and
// whether the connection was made for the scrape or kept from an earlier one.
func TestSSHFileTracesItsReads(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "traced", "app.prom")
	var shown []string
	for range 2 {
		ctx, trace := WithRequestTrace(context.Background())
		if _, err := FetchCollector(ctx, f.server.Addr, c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		for _, r := range trace.Requests() {
			shown = append(shown, fmt.Sprintf("%s %s %s %s %s", r.Method, r.URL, r.Header.Get("Ssh-User"), r.Header.Get("Ssh-Connection"), r.Outcome))
		}
	}
	url := "sftp://" + f.server.Addr + filepath.Join(f.root, "app.prom")
	if want := []string{"READ " + url + " metrics new read", "READ " + url + " metrics reused read"}; strings.Join(shown, "\n") != strings.Join(want, "\n") {
		t.Errorf("traced\n%s\nwant\n%s", strings.Join(shown, "\n"), strings.Join(want, "\n"))
	}
}
