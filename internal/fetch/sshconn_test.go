//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// heldReads is how many scrapes hold, or wait for, the connections kept to
// address.
func heldReads(address string) int {
	sshConns.mu.Lock()
	defer sshConns.mu.Unlock()
	held := 0
	for key, entry := range sshConns.entries {
		if key.address == address {
			held += entry.calls
		}
	}
	return held
}

// dialStage is how far the making of the connection kept to address has
// got, -1 when there is none.
func dialStage(address string) int32 {
	sshConns.mu.Lock()
	defer sshConns.mu.Unlock()
	for key, entry := range sshConns.entries {
		if key.address == address {
			return entry.stage.Load()
		}
	}
	return -1
}

// The exporter's shutdown forgets how many connections each collector made,
// with the connections: the count is of one exporter's life, so a test's
// exporter, or a second run of a test, starts it at 0.
func TestSSHFileShutdownForgetsTheConnectionsMade(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	if _, err := f.fetch(f.collector(t, "counted", "app.prom"), RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	if _, made := sshConns.collectorConnections("counted"); made != 1 {
		t.Fatalf("made=%d before the shutdown", made)
	}
	sshConns.closeAll()
	if open, made := sshConns.collectorConnections("counted"); open != 0 || made != 0 {
		t.Errorf("open=%d made=%d after the shutdown, want both 0", open, made)
	}
}

// A host whose key is certified by an authority known_hosts_file trusts with
// an @cert-authority line is taken: its certificate is asked for first, even
// beside its plain key and a line of the file for a plain key of that type,
// one it no longer has. A host certificate names the host without its port,
// and the authority's line names it with it.
func TestSSHFileTakesAHostCertifiedByAKnownAuthority(t *testing.T) {
	authority, host := sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t)
	cert := &ssh.Certificate{Key: host.PublicKey(), Serial: 1, CertType: ssh.HostCert, ValidPrincipals: []string{"127.0.0.1"}, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, authority); err != nil {
		t.Fatal(err)
	}
	certified, err := ssh.NewCertSigner(cert, host)
	if err != nil {
		t.Fatal(err)
	}
	f := newSSHFixture(t, host, certified)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	line := "@cert-authority " + knownhosts.Normalize(f.server.Addr) + " " + string(ssh.MarshalAuthorizedKey(authority.PublicKey()))
	file := sshtest.WriteFile(t, f.dir, "known_authority", f.server.KnownHostsLine(sshtest.NewEd25519Key(t))+line)
	if _, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = file }), RequestOverrides{}); err != nil {
		t.Errorf("a host certified by an @cert-authority of known_hosts_file: %v", err)
	}
}

// A host that no longer offers the key type known_hosts_file holds for it
// presents another, and is refused as presenting a key that is not the one
// the file holds, with its fingerprint, rather than with SSH's "no common
// algorithm".
func TestSSHFileRefusesAHostThatNoLongerOffersTheKnownKeyType(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	stale := sshtest.WriteFile(t, f.dir, "known_stale", f.server.KnownHostsLine(sshtest.NewECDSAKey(t)))
	_, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = stale }), RequestOverrides{})
	if err == nil || !strings.Contains(err.Error(), "which is not the one request.known_hosts_file "+stale+" holds for it") || !strings.Contains(err.Error(), ssh.FingerprintSHA256(f.server.HostKeys[0].PublicKey())) {
		t.Errorf("err=%v, want the presented key refused as not the known one, with its fingerprint", err)
	}
}

// knownHostKeyAlgorithmsAsItWas is knownHostKeyAlgorithms before the known
// types were a preference rather than the only ones asked for, and before
// @cert-authority lines asked for certificates: the oracle of the types it
// puts first.
func knownHostKeyAlgorithmsAsItWas(check ssh.HostKeyCallback, address string) []string {
	probe, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
	if err != nil {
		return nil
	}
	var refused *knownhosts.KeyError
	if !errors.As(check(address, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, probe), &refused) {
		return nil
	}
	var algorithms []string
	add := func(names ...string) {
		for _, name := range names {
			if !strings.Contains(strings.Join(algorithms, " ")+" ", name+" ") {
				algorithms = append(algorithms, name)
			}
		}
	}
	for _, known := range refused.Want {
		switch known.Key.Type() {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			add(known.Key.Type())
		}
	}
	return algorithms
}

// For a file without @cert-authority lines the algorithms asked for begin
// with exactly those asked for before, alone, followed by every other the
// client takes; a file that knows nothing of the address still asks for
// none, which is the client's own order.
func TestKnownHostKeyAlgorithmsKeepTheKnownTypesFirst(t *testing.T) {
	dir := t.TempDir()
	rsaKey := rsaPublicKey(t)
	ed, ecdsa := sshtest.NewEd25519Key(t).PublicKey(), sshtest.NewECDSAKey(t).PublicKey()
	address := "127.0.0.1:2222"
	name := knownhosts.Normalize(address)
	for label, lines := range map[string][]string{
		"ed25519":         {knownhosts.Line([]string{name}, ed)},
		"ecdsa":           {knownhosts.Line([]string{name}, ecdsa)},
		"rsa":             {knownhosts.Line([]string{name}, rsaKey)},
		"rsa then ed":     {knownhosts.Line([]string{name}, rsaKey), knownhosts.Line([]string{name}, ed)},
		"ecdsa, ed, ecda": {knownhosts.Line([]string{name}, ecdsa), knownhosts.Line([]string{name}, ed), knownhosts.Line([]string{name}, ecdsa)},
		"hashed":          {knownhosts.Line([]string{knownhosts.HashHostname(name)}, ecdsa)},
		"another host":    {knownhosts.Line([]string{"other.example"}, ed)},
		"another port":    {knownhosts.Line([]string{"127.0.0.1"}, ed)},
		"wildcard":        {knownhosts.Line([]string{"[127.0.0.*]:2222"}, ecdsa)},
	} {
		file := sshtest.WriteFile(t, dir, strings.ReplaceAll(label, " ", "_"), strings.Join(lines, "\n")+"\n")
		known, err := readKnownHosts(file)
		if err != nil {
			t.Fatal(err)
		}
		was, now := knownHostKeyAlgorithmsAsItWas(known.check, address), knownHostKeyAlgorithms(known, address)
		if len(was) == 0 {
			if now != nil {
				t.Errorf("%s: %v, want none, as before", label, now)
			}
			continue
		}
		if len(now) < len(was) || strings.Join(now[:len(was)], ",") != strings.Join(was, ",") {
			t.Errorf("%s: %v, want it to begin with %v", label, now, was)
		}
		for _, supported := range ssh.SupportedAlgorithms().HostKeys {
			if !strings.Contains(","+strings.Join(now, ",")+",", ","+supported+",") {
				t.Errorf("%s: %v lacks %s", label, now, supported)
			}
		}
	}
}

// rsaPublicKey is a new RSA public key, of the fewest bits Go makes one
// of, which is the quickest.
func rsaPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return public
}

// A host that takes passwords only through keyboard-interactive, as an
// OpenSSH that leaves them to PAM does, is answered with the password, and
// a question it echoes with the username; the wrong password fails naming
// the methods tried and what the questions were answered with.
func TestSSHFileAnswersKeyboardInteractiveWithThePassword(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	testutil.WriteIn(t, root, "app.prom", sshPromFile)
	server := sshtest.Start(t, sshtest.Options{Password: sshPassword, KeyboardInteractive: true})
	t.Cleanup(sshConns.closeAll)
	known := server.WriteKnownHosts(t, dir)
	right := sshtest.WriteFile(t, dir, "password", sshPassword+"\n")
	wrong := sshtest.WriteFile(t, dir, "wrong", "not-the-password\n")
	collector := func(password string) *model.Collector {
		c := model.Collector{Name: "pam", Request: model.RequestConfig{Type: RequestTypeSSHFile, Root: root, Path: "app.prom", Username: "metrics", PasswordFile: password, KnownHostsFile: known},
			Transform: model.TransformConfig{Type: "prometheus"}, ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"}}
		if err := ValidateRequest(&c); err != nil {
			t.Fatal(err)
		}
		return &c
	}
	if resp, err := FetchCollector(context.Background(), server.Addr, collector(right), RequestOverrides{}, nil); err != nil || string(resp.Body) != sshPromFile {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	_, err := FetchCollector(context.Background(), server.Addr, collector(wrong), RequestOverrides{}, nil)
	for _, want := range []string{
		`refused user "metrics": authentication failed with the password in ` + wrong + " (the SSH methods tried: keyboard-interactive)",
		"that the host's keyboard-interactive questions ask for the password alone",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want it to say %q", err, want)
		}
	}
	if err != nil && strings.Contains(err.Error(), "not-the-password") {
		t.Errorf("the error shows the password: %v", err)
	}
}

// A scrape that finds the connection being made for another waits for it
// within its own deadline: when the first scrape's deadline comes during the
// handshake, it fails saying so, and the connection is still made, for the
// second, which reads on it; the host took one connection.
func TestSSHFileWaitsForAConnectionPastTheDeadlineOfTheScrapeMakingIt(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "waits", "app.prom")
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	first, end := testutil.DeadlineEndedByHand()
	failed := make(chan error, 1)
	go func() {
		_, err := FetchCollector(first, f.server.Addr, c, RequestOverrides{}, nil)
		failed <- err
	}()
	testutil.WaitFor(t, "the first scrape's handshake", func() bool { return dialStage(f.server.Addr) == sshStageHandshake })
	second, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	read := make(chan error, 1)
	go func() {
		resp, err := FetchCollector(second, f.server.Addr, c, RequestOverrides{}, nil)
		if err == nil && string(resp.Body) != sshPromFile {
			err = errors.New("body " + string(resp.Body))
		}
		read <- err
	}()
	testutil.WaitFor(t, "the second scrape to wait", func() bool { return heldReads(f.server.Addr) == 2 })
	end()
	if err := <-failed; err == nil || !strings.Contains(err.Error(), "the SSH handshake with "+f.server.Addr+" did not finish: context deadline exceeded") {
		t.Errorf("the first scrape: %v", err)
	}
	f.server.Thaw()
	if err := <-read; err != nil {
		t.Errorf("the second scrape: %v", err)
	}
	if f.server.Accepted() != 1 {
		t.Errorf("the host took %d connections", f.server.Accepted())
	}
}

// A connection whose scrape gave up while it was made is made all the same,
// within its own bound, and kept for the next scrape, which reuses it.
func TestSSHFileKeepsAConnectionMadeForAScrapeThatGaveUp(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "gave_up", "app.prom")
	events := sshEvents(t, f.server.Addr)
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	ctx, end := testutil.DeadlineEndedByHand()
	failed := make(chan error, 1)
	go func() {
		_, err := FetchCollector(ctx, f.server.Addr, c, RequestOverrides{}, nil)
		failed <- err
	}()
	testutil.WaitFor(t, "the scrape to connect", func() bool { return f.server.Accepted() == 1 })
	end()
	if err := <-failed; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the scrape: %v", err)
	}
	f.server.Thaw()
	waitEvent(t, events, "made")
	traced, trace := WithRequestTrace(context.Background())
	if _, err := FetchCollector(traced, f.server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if requests := trace.Requests(); len(requests) != 1 || requests[0].Header.Get("Ssh-Connection") != "reused" || f.server.Accepted() != 1 {
		t.Errorf("requests=%+v accepted=%d, want the kept connection reused", requests, f.server.Accepted())
	}
}

// A backslash, which a Windows SFTP server takes for a separator, is refused
// in request.path at load, in a static target's request.path, in the path
// probe parameter before anything is sent, and in the target of a symbolic
// link the read would follow.
func TestSSHFileRefusesABackslashInAPath(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	escape := `..\..\outside.prom`
	c := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHFile, Root: f.root, Path: escape, Username: "metrics", PrivateKeyFile: f.keyFile, KnownHostsFile: f.knownHosts}}
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `request.path "..\\..\\outside.prom" has a backslash`) {
		t.Errorf("request.path: %v", err)
	}
	valid := f.collector(t, "c", "app.prom")
	target := model.StaticTarget{Name: "t", Collector: "c", Target: "h", Request: model.TargetRequestConfig{Path: escape, PathSet: true}}
	if err := CheckTargetRequest(&target, valid); err == nil || !strings.Contains(err.Error(), "has a backslash") {
		t.Errorf("a static target's request.path: %v", err)
	}
	if err := CheckOverrideParams(valid, url.Values{"path": {escape}}); err == nil || !strings.Contains(err.Error(), `probe parameter path "..\\..\\outside.prom" has a backslash`) {
		t.Errorf("the path probe parameter: %v", err)
	}
	if _, err := f.fetch(valid, RequestOverrides{PathSet: true, Path: escape}); err == nil || !strings.Contains(err.Error(), "has a backslash") {
		t.Errorf("a read of the path: %v", err)
	}
	if f.server.Accepted() != 0 {
		t.Errorf("the host took %d connections for a path refused", f.server.Accepted())
	}
	if err := os.Symlink(`sub\..\app.prom`, filepath.Join(f.root, "windows.prom")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.fetch(f.collector(t, "c", "windows.prom"), RequestOverrides{}); err == nil || !strings.Contains(err.Error(), `leads to "sub\\..\\app.prom", which has a backslash`) {
		t.Errorf("a link with a backslash: %v", err)
	}
}

// The account and credential keys are refused written with blanks alone,
// a collector's and a static target's: blanks are no name, and only "" is
// the key left out.
func TestSSHFileRefusesBlankCredentialKeys(t *testing.T) {
	valid := func() model.Collector {
		return model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHFile, Root: "/srv/m", Path: "a.prom", Username: "m", PrivateKeyFile: "/k", KnownHostsFile: "/kh"}}
	}
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"private key beside a password": {func(r *model.RequestConfig) { r.PrivateKeyFile, r.PasswordFile = " ", "/p" }, "request.private_key_file is written with blanks alone; name the file, or leave the key out"},
		"password beside a key":         {func(r *model.RequestConfig) { r.PasswordFile = "\t" }, "request.password_file is written with blanks alone"},
		"passphrase":                    {func(r *model.RequestConfig) { r.PassphraseFile = "  " }, "request.passphrase_file is written with blanks alone"},
		"username":                      {func(r *model.RequestConfig) { r.Username = "  " }, "request.username is required"},
		"known_hosts_file":              {func(r *model.RequestConfig) { r.KnownHostsFile = " " }, "request.known_hosts_file is required"},
	} {
		c := valid()
		tc.change(&c.Request)
		if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	c := valid()
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		request model.TargetRequestConfig
		want    string
	}{
		"username":    {model.TargetRequestConfig{Username: " "}, "request.username is written with blanks alone; name the account"},
		"private key": {model.TargetRequestConfig{PrivateKeyFile: " ", PasswordFile: "/p"}, "request.private_key_file is written with blanks alone"},
	} {
		target := model.StaticTarget{Name: "t", Collector: "c", Target: "h", Request: tc.request}
		if err := CheckTargetRequest(&target, &c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("target %s: err=%v, want %q", name, err, tc.want)
		}
	}
}

// An encrypted PKCS#8 key, which x/crypto cannot read, is named as such,
// with the command that converts it.
func TestSSHFileNamesAnEncryptedPKCS8Key(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	encrypted := sshtest.WriteFile(t, f.dir, "pkcs8", string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("sealed")})))
	passphrase := sshtest.WriteFile(t, f.dir, "passphrase", "pw\n")
	_, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.PrivateKeyFile, r.PassphraseFile = encrypted, passphrase }), RequestOverrides{})
	if want := "request.private_key_file " + encrypted + " is an encrypted PKCS#8 key, which cannot be read; convert it to OpenSSH's format with ssh-keygen -p -f " + encrypted; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err=%v, want %q", err, want)
	}
}

// keepalive_interval is 0, which sends none, or at least a second: each
// keepalive is given the interval to be answered in.
func TestSSHFileKeepaliveIntervalIsAtLeastASecond(t *testing.T) {
	for interval, want := range map[time.Duration]string{
		time.Microsecond:       "request.keepalive_interval 1µs is under the least, 1s; set 0 to send no keepalives",
		999 * time.Millisecond: "request.keepalive_interval 999ms is under the least, 1s",
		0:                      "",
		time.Second:            "",
	} {
		d := model.Duration(interval)
		c := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHFile, Root: "/srv/m", Path: "a.prom", Username: "m", PrivateKeyFile: "/k", KnownHostsFile: "/kh", KeepaliveInterval: &d}}
		err := ValidateRequest(&c)
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err=%v, want %q", interval, err, want)
		}
	}
}

// request.root may be / for sshfile, an account confined to its own tree,
// and the read is still confined: .. is refused. That localfile still
// refuses / is internal/config's requesttype_localfile_test.go's.
func TestSSHFileTakesTheRootOfTheHost(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	under := strings.TrimPrefix(filepath.Join(f.root, "app.prom"), "/")
	c := f.collector(t, "slash", under, func(r *model.RequestConfig) { r.Root = "/" })
	resp, err := f.fetch(c, RequestOverrides{})
	if err != nil || string(resp.Body) != sshPromFile {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if label, err := RequestLabelFor(f.server.Addr, c, RequestOverrides{}); err != nil || label != "sftp://"+f.server.Addr+"/"+under {
		t.Errorf("label %q %v", label, err)
	}
	dir := f.collector(t, "slash_dir", "", func(r *model.RequestConfig) { r.Root, r.Files = "/", []string{"*.prom"} })
	if label, err := RequestLabelFor(f.server.Addr, dir, RequestOverrides{}); err != nil || label != "sftp://"+f.server.Addr+"/" {
		t.Errorf("directory label %q %v", label, err)
	}
	if _, err := f.fetch(c, RequestOverrides{PathSet: true, Path: "../" + under}); err == nil || !strings.Contains(err.Error(), "leads outside request.root") {
		t.Errorf("a path above /: %v", err)
	}
}

// The error of a connection that names its host already is not followed by
// the host once more; that of a read on it is.
func TestSSHFileNamesTheHostOnce(t *testing.T) {
	f := newSSHFixture(t)
	wrong := sshtest.WriteFile(t, f.dir, "wrong", "nope")
	_, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.PrivateKeyFile, r.PasswordFile = "", wrong }), RequestOverrides{})
	if err == nil || strings.Count(err.Error(), f.server.Addr) != 1 {
		t.Errorf("err=%v, want the host named once", err)
	}
	_, err = f.fetch(f.collector(t, "c", "missing.prom"), RequestOverrides{})
	if err == nil || !strings.HasSuffix(err.Error(), "does not exist (on "+f.server.Addr+")") {
		t.Errorf("err=%v, want the host named after the read's error", err)
	}
}

// --dry-run's reading of the files: a key, a password and a known_hosts file
// that are fine give no warning; a missing key, a key that is none, an
// encrypted key without its passphrase, an empty password and a known_hosts
// file without a line each give one, naming the collector or the target;
// a target that sets no credential file of its own gives none.
func TestSSHFileWarningsOfItsFiles(t *testing.T) {
	f := newSSHFixtureWith(t, true)
	c := f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.PasswordFile = f.passwordFile })
	if warnings := FileWarnings(c, nil); len(warnings) != 0 {
		t.Errorf("warnings of good files: %v", warnings)
	}
	empty := sshtest.WriteFile(t, f.dir, "empty", "\n")
	noLine := sshtest.WriteFile(t, f.dir, "no_line", "# none\n")
	notKey := sshtest.WriteFile(t, f.dir, "notkey", "hello")
	for name, tc := range map[string]struct {
		change func(*model.RequestConfig)
		want   string
	}{
		"missing key":        {func(r *model.RequestConfig) { r.PrivateKeyFile = filepath.Join(f.dir, "gone") }, `collector "c": reading request.private_key_file: open ` + filepath.Join(f.dir, "gone")},
		"not a key":          {func(r *model.RequestConfig) { r.PrivateKeyFile = notKey }, "is not a private key"},
		"no passphrase":      {func(r *model.RequestConfig) { r.PrivateKeyFile = f.encryptedKey }, "is encrypted; name the file of its passphrase"},
		"missing passphrase": {func(r *model.RequestConfig) { r.PassphraseFile = filepath.Join(f.dir, "gone") }, "reading request.passphrase_file"},
		"empty password":     {func(r *model.RequestConfig) { r.PasswordFile = empty }, "request.password_file " + empty + " is empty"},
		"no known host":      {func(r *model.RequestConfig) { r.KnownHostsFile = noLine }, "holds no host key"},
	} {
		changed := *c
		tc.change(&changed.Request)
		warnings := FileWarnings(&changed, nil)
		if len(warnings) != 1 || !strings.Contains(warnings[0], tc.want) || !strings.Contains(warnings[0], "a scrape would fail on it now") {
			t.Errorf("%s: %v, want one warning saying %q", name, warnings, tc.want)
		}
	}
	own := model.StaticTarget{Name: "t", Collector: "c", Target: "h", Request: model.TargetRequestConfig{PasswordFile: empty}}
	if warnings := FileWarnings(c, &own); len(warnings) != 1 || !strings.HasPrefix(warnings[0], `target "t": request.password_file `+empty+" is empty") {
		t.Errorf("a target's own password: %v", warnings)
	}
	if warnings := FileWarnings(c, &model.StaticTarget{Name: "u", Collector: "c", Target: "h"}); len(warnings) != 0 {
		t.Errorf("a target with the collector's files: %v", warnings)
	}
}

// sshLimitHosts are two servers of one root and the files to reach both.
func sshLimitHosts(t *testing.T) (f *sshFixture, other *sshtest.Server, known string) {
	t.Helper()
	f = newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	key, err := os.ReadFile(f.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	other = sshtest.Start(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{signer.PublicKey()}, HostKeys: f.server.HostKeys})
	known = sshtest.WriteFile(t, f.dir, "known_both", f.server.KnownHostsLine()+other.KnownHostsLine())
	return f, other, known
}

// With max_open_connections 1, scrapes of two hosts in turn each close the
// other host's connection, no read holding it, before making their own: one
// connection is open at a time, and each scrape made one.
func TestSSHFileMaxOpenConnectionsClosesTheLeastRecentlyUsed(t *testing.T) {
	f, other, known := sshLimitHosts(t)
	c := f.collector(t, "limited", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = known, 1 })
	closed := sshEvents(t, f.server.Addr)
	for i, host := range []string{f.server.Addr, other.Addr, f.server.Addr} {
		if _, err := FetchCollector(context.Background(), host, c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
		if open, made := sshConns.collectorConnections("limited"); open != 1 || made != uint64(i+1) {
			t.Errorf("after the scrape of %s: open=%d made=%d", host, open, made)
		}
	}
	waitEvent(t, closed, "closed: max_open_connections")
	if f.server.Accepted() != 2 || other.Accepted() != 1 {
		t.Errorf("accepted %d and %d", f.server.Accepted(), other.Accepted())
	}
}

// With max_open_connections 0, the default, a collector's connections to
// several hosts all stay open, and are shared with another collector of the
// same settings, as before the limit; a collector with a limit shares none.
func TestSSHFileWithoutMaxOpenConnectionsKeepsEveryConnection(t *testing.T) {
	f, other, known := sshLimitHosts(t)
	unlimited := f.collector(t, "unlimited", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = known })
	also := f.collector(t, "also", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = known })
	limited := f.collector(t, "limited", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = known, 2 })
	for _, c := range []*model.Collector{unlimited, also, limited} {
		for _, host := range []string{f.server.Addr, other.Addr} {
			if _, err := FetchCollector(context.Background(), host, c, RequestOverrides{}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, want := range map[string][2]int{"unlimited": {2, 2}, "also": {2, 0}, "limited": {2, 2}} {
		if open, made := sshConns.collectorConnections(name); open != want[0] || made != uint64(want[1]) {
			t.Errorf("%s: open=%d made=%d, want %v", name, open, made, want)
		}
	}
	if f.server.Accepted() != 2 || other.Accepted() != 2 {
		t.Errorf("accepted %d and %d, want the limited collector's connections its own", f.server.Accepted(), other.Accepted())
	}
}

// With max_open_connections 1 and its connection held by a read, a scrape of
// another host waits: it fails at its deadline naming the limit, or, when
// the read ends first, closes that connection and reads on a new one.
func TestSSHFileMaxOpenConnectionsWaitsForAConnectionToBeFree(t *testing.T) {
	f, other, known := sshLimitHosts(t)
	c := f.collector(t, "waiting", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = known, 1 })
	if _, err := FetchCollector(context.Background(), f.server.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	waiting := sshEvents(t, other.Addr)
	reading, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func() { once.Do(func() { close(reading); <-finish }) }
	beforeSSHRead.Store(&hook)
	t.Cleanup(func() { beforeSSHRead.Store(nil) })
	held := make(chan error, 1)
	go func() {
		_, err := FetchCollector(context.Background(), f.server.Addr, c, RequestOverrides{}, nil)
		held <- err
	}()
	<-reading
	ctx, end := testutil.DeadlineEndedByHand()
	failed := make(chan error, 1)
	go func() {
		_, err := FetchCollector(ctx, other.Addr, c, RequestOverrides{}, nil)
		failed <- err
	}()
	waitEvent(t, waiting, "waiting: max_open_connections")
	end()
	if err := <-failed; err == nil || !strings.Contains(err.Error(), `collector "waiting" has request.max_open_connections, 1, connections (1 open and held by a read), and none was free`) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the scrape at its deadline: %v", err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := FetchCollector(context.Background(), other.Addr, c, RequestOverrides{}, nil)
		read <- err
	}()
	waitEvent(t, waiting, "waiting: max_open_connections")
	close(finish)
	if err := <-held; err != nil {
		t.Errorf("the read holding the connection: %v", err)
	}
	if err := <-read; err != nil {
		t.Errorf("the scrape that waited: %v", err)
	}
	if open, _ := sshConns.collectorConnections("waiting"); open != 1 || other.Accepted() != 1 {
		t.Errorf("open=%d, the other host took %d", open, other.Accepted())
	}
}

// A configuration that lowers a collector's max_open_connections closes no
// connection at once, and the next connection the collector makes closes
// those no read holds down to the new limit, without waiting.
func TestSSHFileMaxOpenConnectionsFollowsAReload(t *testing.T) {
	f, other, _ := sshLimitHosts(t)
	third := sshtest.Start(t, sshtest.Options{Password: sshPassword, HostKeys: f.server.HostKeys})
	known := sshtest.WriteFile(t, f.dir, "known_three", strings.Join([]string{f.server.KnownHostsLine(), other.KnownHostsLine(), third.KnownHostsLine()}, ""))
	c := f.collector(t, "reloaded", "app.prom", func(r *model.RequestConfig) {
		r.KnownHostsFile, r.MaxOpenConnections, r.PasswordFile = known, 3, f.passwordFile
	})
	for _, host := range []string{f.server.Addr, other.Addr} {
		if _, err := FetchCollector(context.Background(), host, c, RequestOverrides{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	lowered := *c
	lowered.Request.MaxOpenConnections = 1
	sshConns.follow([]model.Collector{lowered}, nil)
	if open, _ := sshConns.collectorConnections("reloaded"); open != 2 {
		t.Errorf("open=%d after the reload, want both kept", open)
	}
	// A minute bounds a scrape that, wrongly, waited for a connection to be
	// free instead of closing an idle one.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := FetchCollector(ctx, third.Addr, &lowered, RequestOverrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if open, made := sshConns.collectorConnections("reloaded"); open != 1 || made != 3 {
		t.Errorf("open=%d made=%d, want the new limit held", open, made)
	}
}

// A connection whose making no scrape waits for any more, to a host that
// takes the TCP connection and never answers, does not hold the only room of
// a collector with max_open_connections 1 for the 30 seconds it is given:
// the scrape of a healthy host gives it up, its dial is cancelled, and the
// scrape reads once it has ended, counted as a scrape that waited.
func TestSSHFileMaxOpenConnectionsGivesUpAConnectionNoScrapeWaitsFor(t *testing.T) {
	f, other, known := sshLimitHosts(t)
	c := f.collector(t, "stalled", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = known, 1 })
	events := sshEvents(t, f.server.Addr)
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	ctx, end := testutil.DeadlineEndedByHand()
	failed := make(chan error, 1)
	go func() {
		_, err := FetchCollector(ctx, f.server.Addr, c, RequestOverrides{}, nil)
		failed <- err
	}()
	testutil.WaitFor(t, "the handshake with the frozen host", func() bool { return dialStage(f.server.Addr) == sshStageHandshake })
	end()
	if err := <-failed; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the scrape of the frozen host: %v", err)
	}
	// Ten seconds, a third of the time the making of the stalled connection
	// is given, bound a scrape that wrongly waited for it to fail.
	healthy, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := FetchCollector(healthy, other.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Errorf("the scrape of the healthy host: %v", err)
	}
	waitEvent(t, events, "given up: max_open_connections")
	if waited, failures := sshConns.limitCounts("stalled"); waited != 1 || failures != 0 {
		t.Errorf("waited=%d failed=%d, want 1 and 0", waited, failures)
	}
	if _, made := sshConns.collectorConnections("stalled"); made != 1 {
		t.Errorf("made=%d, want the healthy host's connection alone", made)
	}
}

// A connection being made that a scrape waits for is not given up: a scrape
// of another host at the limit fails at its deadline saying the collector's
// connection is being made for a scrape waiting for it, and counts as a
// scrape that waited and one that failed.
func TestSSHFileMaxOpenConnectionsSaysWhatHoldsItsConnections(t *testing.T) {
	f, other, known := sshLimitHosts(t)
	c := f.collector(t, "making", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = known, 1 })
	waiting := sshEvents(t, other.Addr)
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	first, endFirst := testutil.DeadlineEndedByHand()
	firstDone := make(chan error, 1)
	go func() {
		_, err := FetchCollector(first, f.server.Addr, c, RequestOverrides{}, nil)
		firstDone <- err
	}()
	testutil.WaitFor(t, "the handshake with the frozen host", func() bool { return dialStage(f.server.Addr) == sshStageHandshake })
	second, endSecond := testutil.DeadlineEndedByHand()
	secondDone := make(chan error, 1)
	go func() {
		_, err := FetchCollector(second, other.Addr, c, RequestOverrides{}, nil)
		secondDone <- err
	}()
	waitEvent(t, waiting, "waiting: max_open_connections")
	endSecond()
	if err := <-secondDone; err == nil || !strings.Contains(err.Error(), `collector "making" has request.max_open_connections, 1, connections (1 being made for a scrape waiting for it), and none was free before the scrape's deadline`) {
		t.Errorf("the scrape at the limit: %v", err)
	}
	endFirst()
	<-firstDone
	if waited, failures := sshConns.limitCounts("making"); waited != 1 || failures != 1 {
		t.Errorf("waited=%d failed=%d, want 1 and 1", waited, failures)
	}
	sshConns.closeAll()
	if waited, failures := sshConns.limitCounts("making"); waited != 0 || failures != 0 {
		t.Errorf("waited=%d failed=%d after the shutdown, want both 0", waited, failures)
	}
}

// A connection being made at the exporter's shutdown, or at a reload that
// leaves it to no collector, is given up when no scrape waits for it: it is
// not made after the shutdown nor counted into the next exporter life's
// connects, and a collector the reload removed gets no count back, even
// from a connection made for a scrape that waited for it.
func TestSSHFileGivesUpAConnectionBeingMadeAtShutdownAndReload(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "life", "app.prom")
	events := sshEvents(t, f.server.Addr)
	for _, end := range []struct {
		why  string
		stop func()
	}{{"shutdown", sshConns.closeAll}, {"reload", func() { sshConns.follow(nil, nil) }}} {
		f.server.Freeze()
		ctx, endScrape := testutil.DeadlineEndedByHand()
		failed := make(chan error, 1)
		go func() {
			_, err := FetchCollector(ctx, f.server.Addr, c, RequestOverrides{}, nil)
			failed <- err
		}()
		testutil.WaitFor(t, "the handshake with the frozen host", func() bool { return dialStage(f.server.Addr) == sshStageHandshake })
		endScrape()
		if err := <-failed; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: the scrape: %v", end.why, err)
		}
		end.stop()
		waitEvent(t, events, "given up: "+end.why)
		f.server.Thaw()
		testutil.WaitFor(t, "the connection given up to end", func() bool { return f.server.Open() == 0 })
		sshConns.mu.Lock()
		made, counted := sshConns.connects["life"]
		sshConns.mu.Unlock()
		if counted {
			t.Errorf("%s: the collector has made=%d, want no count", end.why, made)
		}
	}
	for drained := false; !drained; {
		select {
		case event := <-events:
			if event == "made" {
				t.Error("a connection given up was made")
			}
		default:
			drained = true
		}
	}
	// A reload while a scrape waits for the connection gives up nothing:
	// the scrape reads on it, it is closed when the read ends, and the
	// removed collector gets no count back.
	f.server.Freeze()
	read := make(chan error, 1)
	go func() {
		_, err := FetchCollector(context.Background(), f.server.Addr, c, RequestOverrides{Timeout: time.Minute}, nil)
		read <- err
	}()
	testutil.WaitFor(t, "the handshake with the frozen host", func() bool { return dialStage(f.server.Addr) == sshStageHandshake })
	sshConns.follow(nil, nil)
	f.server.Thaw()
	if err := <-read; err != nil {
		t.Errorf("the scrape waiting at the reload: %v", err)
	}
	waitEvent(t, events, "closed: unused")
	sshConns.mu.Lock()
	made, counted := sshConns.connects["life"]
	sshConns.mu.Unlock()
	if counted {
		t.Errorf("a connection made for a collector a reload removed counts again: made=%d", made)
	}
}

// A backslash is refused in request.root of an sshfile collector, as in its
// request.path.
func TestSSHFileRefusesABackslashInItsRoot(t *testing.T) {
	c := model.Collector{Name: "c", Request: model.RequestConfig{Type: RequestTypeSSHFile, Root: `/srv\metrics`, Path: "app.prom", Username: "metrics", PrivateKeyFile: "/k", KnownHostsFile: "/known"}}
	if err := ValidateRequest(&c); err == nil || !strings.Contains(err.Error(), `collector "c" request.root "/srv\\metrics" has a backslash`) {
		t.Errorf("err=%v", err)
	}
}

// passwordServer is a server whose configuration configure sets, a known_hosts
// file of it, a root holding the metrics file, and a collector of it that
// authenticates with password alone, and with the private key in keyFile
// first when it is set.
func passwordServer(t *testing.T, password, keyFile string, configure func(*ssh.ServerConfig)) (*sshtest.Server, *model.Collector) {
	t.Helper()
	dir, root := t.TempDir(), t.TempDir()
	testutil.WriteIn(t, root, "app.prom", sshPromFile)
	server := sshtest.Start(t, sshtest.Options{Configure: configure})
	t.Cleanup(sshConns.closeAll)
	c := model.Collector{Name: "pw", Request: model.RequestConfig{
		Type: RequestTypeSSHFile, Root: root, Path: "app.prom", Username: "metrics", PrivateKeyFile: keyFile,
		PasswordFile: sshtest.WriteFile(t, dir, "password", password+"\n"), KnownHostsFile: server.WriteKnownHosts(t, dir),
	}, Transform: model.TransformConfig{Type: "prometheus"}, ErrorHandling: model.ErrorHandling{OnFetchError: "fail", OnDecodeError: "fail", OnTransformError: "fail"}}
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return server, &c
}

// A host's keyboard-interactive questions are answered with the password in
// three rounds at most, rounds without a question not counted: a host that
// takes it at the third round, after rounds without a question, is read,
// and one that asks round after round has it three times and is refused,
// saying so.
func TestSSHFileAnswersKeyboardInteractiveInThreeRoundsAtMost(t *testing.T) {
	for _, takes := range []bool{true, false} {
		var sent atomic.Int64
		server, c := passwordServer(t, sshPassword, "", func(config *ssh.ServerConfig) {
			config.KeyboardInteractiveCallback = func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
				for range 20 {
					if _, err := challenge("", "Welcome", nil, nil); err != nil {
						return nil, err
					}
					answers, err := challenge("", "", []string{"Password: "}, []bool{false})
					if err != nil {
						return nil, err
					}
					if len(answers) == 1 && answers[0] == sshPassword {
						sent.Add(1)
					}
					if takes && sent.Load() == 3 {
						return &ssh.Permissions{}, nil
					}
				}
				return nil, errors.New("refused")
			}
		})
		_, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil)
		switch {
		case takes && err != nil:
			t.Errorf("a host taking the password at the third round: %v", err)
		case !takes && (err == nil || !strings.Contains(err.Error(), `refused user "metrics": it asked keyboard-interactive questions in more than 3 rounds`)):
			t.Errorf("a host asking round after round: %v", err)
		}
		if sent.Load() != 3 {
			t.Errorf("takes=%v: the password was sent %d times, want 3", takes, sent.Load())
		}
	}
}

// A host that takes both the password method and keyboard-interactive, as
// OpenSSH with PAM does by default, is given the password once, by the
// password method: a wrong one counts as one failed login, not two. A host
// that takes keyboard-interactive and not the password method is given it
// there, after the private key.
func TestSSHFileGivesAWrongPasswordOnce(t *testing.T) {
	var passwords, interactive atomic.Int64
	both := func(config *ssh.ServerConfig) {
		config.PasswordCallback = func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			passwords.Add(1)
			if string(password) == sshPassword {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("wrong")
		}
		config.KeyboardInteractiveCallback = func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			interactive.Add(1)
			answers, err := challenge("", "", []string{"Password: "}, []bool{false})
			if err == nil && len(answers) == 1 && answers[0] == sshPassword {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("wrong")
		}
	}
	server, c := passwordServer(t, "wrong-password", "", both)
	_, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "(the SSH methods tried: password)") {
		t.Errorf("a wrong password: %v", err)
	}
	if passwords.Load() != 1 || interactive.Load() != 0 {
		t.Errorf("a wrong password was given %d times by the password method and %d by keyboard-interactive, want once by the first", passwords.Load(), interactive.Load())
	}
	server, c = passwordServer(t, sshPassword, "", both)
	if _, err := FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil); err != nil || interactive.Load() != 0 {
		t.Errorf("the right password: err=%v, keyboard-interactive asked %d times", err, interactive.Load())
	}
	dir := t.TempDir()
	keyFile, _ := sshtest.WriteClientKey(t, dir, "id_ed25519", "")
	server, c = passwordServer(t, "wrong-password", keyFile, func(config *ssh.ServerConfig) {
		config.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, errors.New("not authorized")
		}
		both(config)
		config.PasswordCallback = nil
	})
	_, err = FetchCollector(context.Background(), server.Addr, c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "(the SSH methods tried: publickey, keyboard-interactive)") {
		t.Errorf("a host without the password method: %v", err)
	}
}

// sshHostCertificate is a host certificate of host for 127.0.0.1, signed by
// authority, presented with host's key.
func sshHostCertificate(t *testing.T, host, authority ssh.Signer) ssh.Signer {
	t.Helper()
	cert := &ssh.Certificate{Key: host.PublicKey(), Serial: 7, CertType: ssh.HostCert, ValidPrincipals: []string{"127.0.0.1"}, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, authority); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewCertSigner(cert, host)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// A host that presents a certificate of an authority known_hosts_file does
// not trust for it is taken by the line of its plain key, as OpenSSH takes
// it ("no matching CA found, retry with plain key"), beside an
// @cert-authority line of another authority; without that line, or with a
// line of another key, or when the file marks the certificate's authority
// @revoked, it is refused, saying the authority is not trusted, or revoked.
func TestSSHFileTakesAHostByItsPlainKeyWhenItsAuthorityIsNotKnown(t *testing.T) {
	known, other, host := sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t)
	f := newSSHFixture(t, host, sshHostCertificate(t, host, other))
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	_, port, _ := net.SplitHostPort(f.server.Addr)
	authorityLine := func(authority ssh.Signer) string {
		return "@cert-authority [*]:" + port + " " + string(ssh.MarshalAuthorizedKey(authority.PublicKey()))
	}
	authority := ssh.FingerprintSHA256(other.PublicKey())
	for label, test := range map[string]struct{ lines, want string }{
		"the plain key's line": {authorityLine(known) + f.server.KnownHostsLine(host), ""},
		"no plain line":        {authorityLine(known), "signed by the authority ssh-ed25519 " + authority + ", which no @cert-authority line of request.known_hosts_file"},
		"another key's line":   {authorityLine(known) + f.server.KnownHostsLine(sshtest.NewEd25519Key(t)), "which no @cert-authority line of request.known_hosts_file"},
		"a revoked authority":  {authorityLine(known) + f.server.KnownHostsLine(host) + "@revoked * " + string(ssh.MarshalAuthorizedKey(other.PublicKey())), "signed by the authority ssh-ed25519 " + authority + ", which request.known_hosts_file"},
	} {
		file := sshtest.WriteFile(t, f.dir, "known_"+strings.ReplaceAll(label, " ", "_"), test.lines)
		_, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = file }), RequestOverrides{})
		switch {
		case test.want == "" && err != nil:
			t.Errorf("%s: %v", label, err)
		case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
			t.Errorf("%s: %v, want it to say %q", label, err, test.want)
		}
	}
}

// A host certificate whose authority known_hosts_file marks @revoked is
// refused saying so, naming the authority, and one whose own key it marks
// @revoked naming that key.
func TestSSHFileRefusesAHostCertificateOfARevokedAuthority(t *testing.T) {
	authority, host := sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t)
	f := newSSHFixture(t, sshHostCertificate(t, host, authority))
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	_, port, _ := net.SplitHostPort(f.server.Addr)
	trusted := "@cert-authority [*]:" + port + " " + string(ssh.MarshalAuthorizedKey(authority.PublicKey()))
	for label, test := range map[string]struct {
		revoked ssh.Signer
		want    string
	}{
		"authority": {authority, "presented a host certificate, ssh-ed25519-cert-v01@openssh.com SHA256:"},
		"host key":  {host, "presented a host certificate of the key ssh-ed25519 " + ssh.FingerprintSHA256(host.PublicKey()) + ", which request.known_hosts_file"},
	} {
		file := sshtest.WriteFile(t, f.dir, "known_revoked_"+strings.ReplaceAll(label, " ", "_"), trusted+"@revoked * "+string(ssh.MarshalAuthorizedKey(test.revoked.PublicKey())))
		_, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = file }), RequestOverrides{})
		if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "marks @revoked") {
			t.Errorf("%s revoked: %v, want it to say %q and @revoked", label, err, test.want)
		}
		if label == "authority" && (err == nil || !strings.Contains(err.Error(), "signed by the authority ssh-ed25519 "+ssh.FingerprintSHA256(authority.PublicKey()))) {
			t.Errorf("the revoked authority is not named: %v", err)
		}
	}
}

// An @cert-authority line naming every host with a bare * trusts its
// authority for a host on any port, as OpenSSH's does; a line naming the
// host without its port still names port 22 alone.
func TestSSHFileTakesABareWildcardAuthorityOnAnyPort(t *testing.T) {
	authority, host := sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t)
	f := newSSHFixture(t, sshHostCertificate(t, host, authority))
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	key := string(ssh.MarshalAuthorizedKey(authority.PublicKey()))
	bare := sshtest.WriteFile(t, f.dir, "known_bare", "@cert-authority * "+key)
	if _, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = bare }), RequestOverrides{}); err != nil {
		t.Errorf("a bare * line, the host on a port other than 22: %v", err)
	}
	portless := sshtest.WriteFile(t, f.dir, "known_portless", "@cert-authority 127.0.0.1 "+key)
	if _, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = portless }), RequestOverrides{}); err == nil || !strings.Contains(err.Error(), "which no @cert-authority line") {
		t.Errorf("a line of the host without its port, the host on another port: %v", err)
	}
}

// knownHostKeyAlgorithmsBeforeAnyHost is knownHostKeyAlgorithms before an
// @cert-authority line naming every host with a bare * asked for
// certificates on every port: the oracle of what every other file asks for.
func knownHostKeyAlgorithmsBeforeAnyHost(check ssh.HostKeyCallback, address string, authorities map[string]bool) []string {
	probe, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
	if err != nil {
		return nil
	}
	var refused *knownhosts.KeyError
	if !errors.As(check(address, &net.TCPAddr{IP: net.IPv4zero, Port: 22}, probe), &refused) || len(refused.Want) == 0 {
		return nil
	}
	var algorithms []string
	add := func(names ...string) {
		for _, name := range names {
			if !slices.Contains(algorithms, name) {
				algorithms = append(algorithms, name)
			}
		}
	}
	supported := ssh.SupportedAlgorithms().HostKeys
	for _, known := range refused.Want {
		if authorities[string(known.Key.Marshal())] {
			add(slices.DeleteFunc(slices.Clone(supported), func(name string) bool { return !strings.Contains(name, "-cert-v01@openssh.com") })...)
		}
	}
	for _, known := range refused.Want {
		if authorities[string(known.Key.Marshal())] {
			continue
		}
		switch known.Key.Type() {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			add(known.Key.Type())
		}
	}
	add(supported...)
	return algorithms
}

// sshHostKeyErrorAsItWas is sshHostKeyError before the refusal of a host
// certificate no line trusts said why its key was not taken either: the
// oracle of every other refusal's message.
func sshHostKeyErrorAsItWas(key sshConnKey, hostKey ssh.PublicKey, err error, known *knownHosts) error {
	presented := sshKeyName(hostKey)
	var refused *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	cert, isCert := hostKey.(*ssh.Certificate)
	switch {
	case errors.As(err, &revoked):
		return fmt.Errorf("the host %s presented the key %s, which request.known_hosts_file %s marks @revoked", key.address, presented, key.knownHostsFile)
	case isCert && (revokedCertificate(err) || isNoAuthority(err)) && known.revoked[string(cert.SignatureKey.Marshal())]:
		return fmt.Errorf("the host %s presented a host certificate, %s, signed by the authority %s, which request.known_hosts_file %s marks @revoked", key.address, presented, sshKeyName(cert.SignatureKey), key.knownHostsFile)
	case isCert && revokedCertificate(err):
		return fmt.Errorf("the host %s presented a host certificate of the key %s, which request.known_hosts_file %s marks @revoked", key.address, sshKeyName(cert.Key), key.knownHostsFile)
	case isCert && isNoAuthority(err):
		return fmt.Errorf("the host %s presented a host certificate, %s, signed by the authority %s, which no @cert-authority line of request.known_hosts_file %s trusts for the host, and no line of the file holds the key it certifies; add an @cert-authority line of the authority that signs the host's certificates, naming the host as it is scraped, [host]:port or a pattern such as [*]:port for a port other than 22, or the host's own line", key.address, presented, sshKeyName(cert.SignatureKey), key.knownHostsFile)
	case errors.As(err, &refused) && len(refused.Want) == 0:
		host, port, _ := net.SplitHostPort(key.address)
		return fmt.Errorf("the host %s is not in request.known_hosts_file %s; add its line, which `ssh-keyscan -p %s %s` prints, after checking that the key's fingerprint, %s, is the host's own (ssh-keygen -lf on the host's /etc/ssh/ssh_host_*_key.pub)", key.address, key.knownHostsFile, port, host, presented)
	case errors.As(err, &refused):
		return fmt.Errorf("the host %s presented the key %s, which is not the one request.known_hosts_file %s holds for it; either the host's key changed and its line must be replaced, or something stands between the exporter and the host", key.address, presented, key.knownHostsFile)
	}
	return fmt.Errorf("the key of the host %s could not be checked against request.known_hosts_file %s: %w", key.address, key.knownHostsFile, err)
}

// For every known_hosts file without an @cert-authority line naming every
// host with a bare *, the algorithms asked for are exactly those asked for
// before, and every key is checked as before but a host certificate no line
// trusts for the host, every refusal saying what it said but that of such a
// certificate whose key no line takes; with a bare * line besides the host's
// own, the host is asked for its known types first, as before, and a
// certificate of the bare * authority is still taken.
func TestKnownHostsChangeOnlyForCertificatesTheyDidNotTrust(t *testing.T) {
	dir := t.TempDir()
	ed, ecdsa, rsaKey := sshtest.NewEd25519Key(t), sshtest.NewECDSAKey(t), rsaPublicKey(t)
	authority, stranger := sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t)
	address := "127.0.0.1:2222"
	name := knownhosts.Normalize(address)
	ca := func(pattern string, key ssh.Signer) string {
		return "@cert-authority " + pattern + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key.PublicKey())))
	}
	revoked := "@revoked * " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ecdsa.PublicKey())))
	files := map[string][]string{
		"ed25519":            {knownhosts.Line([]string{name}, ed.PublicKey())},
		"rsa then ecdsa":     {knownhosts.Line([]string{name}, rsaKey), knownhosts.Line([]string{name}, ecdsa.PublicKey())},
		"hashed":             {knownhosts.Line([]string{knownhosts.HashHostname(name)}, ecdsa.PublicKey())},
		"another port":       {knownhosts.Line([]string{"127.0.0.1"}, ed.PublicKey())},
		"authority":          {ca(name, authority)},
		"authority and key":  {ca(name, authority), knownhosts.Line([]string{name}, ed.PublicKey())},
		"key and authority":  {knownhosts.Line([]string{name}, ecdsa.PublicKey()), ca(name, authority)},
		"wildcard authority": {ca("[127.0.0.*]:2222", authority), knownhosts.Line([]string{name}, ed.PublicKey())},
		"port 22 authority":  {ca("127.0.0.1", authority)},
		"revoked":            {knownhosts.Line([]string{name}, ed.PublicKey()), revoked, ca(name, authority)},
	}
	edCertificate := sshHostCertificate(t, ed, authority).PublicKey()
	certificates := []ssh.PublicKey{edCertificate, sshHostCertificate(t, ecdsa, authority).PublicKey(), sshHostCertificate(t, ed, stranger).PublicKey()}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}
	for label, lines := range files {
		known, err := readKnownHosts(sshtest.WriteFile(t, dir, strings.ReplaceAll(label, " ", "_"), strings.Join(lines, "\n")+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if was, now := knownHostKeyAlgorithmsBeforeAnyHost(known.check, address, known.authorities), knownHostKeyAlgorithms(known, address); strings.Join(was, ",") != strings.Join(now, ",") {
			t.Errorf("%s: %v, want %v, as before", label, now, was)
		}
		for _, key := range append([]ssh.PublicKey{ed.PublicKey(), ecdsa.PublicKey(), rsaKey, authority.PublicKey()}, certificates...) {
			before, now := known.check(address, remote, key), known.verify(address, remote, key)
			cert, isCert := key.(*ssh.Certificate)
			untakenKey := isCert && now != nil && isNoAuthority(now) && !known.revoked[string(cert.SignatureKey.Marshal())] && known.check(address, remote, cert.Key) != nil
			if now != nil && !untakenKey {
				// Every refusal but that of a certificate no line trusts,
				// whose key no line takes, says what it said.
				connKey := sshConnKey{address: address}
				if was, msg := sshHostKeyErrorAsItWas(connKey, key, now, known), sshHostKeyError(connKey, key, now, known); was.Error() != msg.Error() {
					t.Errorf("%s: %s refused saying %q, want %q, as before", label, key.Type(), msg, was)
				}
			}
			if isCert && before != nil && isNoAuthority(before) {
				// Taken by the plain key's line alone.
				if plain := known.check(address, remote, cert.Key); (now == nil) != (plain == nil && !known.revoked[string(cert.SignatureKey.Marshal())]) {
					t.Errorf("%s: a certificate no line trusts: %v, its plain key: %v", label, now, plain)
				}
				continue
			}
			if fmt.Sprint(before) != fmt.Sprint(now) {
				t.Errorf("%s: %s checked %v, want %v, as before", label, key.Type(), now, before)
			}
		}
	}
	known, err := readKnownHosts(sshtest.WriteFile(t, dir, "bare", ca("*", authority)+"\n"+knownhosts.Line([]string{name}, ecdsa.PublicKey())+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if was, now := knownHostKeyAlgorithmsBeforeAnyHost(known.check, address, known.authorities), knownHostKeyAlgorithms(known, address); len(now) == 0 || now[0] != ecdsa.PublicKey().Type() || strings.Join(was, ",") != strings.Join(now, ",") {
		t.Errorf("a bare * authority line and the host's own: %v, want %v, the known type first, as before", now, was)
	}
	if err := known.verify(address, remote, edCertificate); err != nil {
		t.Errorf("a certificate of the bare * authority: %v", err)
	}
	// Without a line of the host, a bare * line asks for the certificate
	// algorithms first, as before.
	known, err = readKnownHosts(sshtest.WriteFile(t, dir, "bare_alone", ca("*", authority)+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if now := knownHostKeyAlgorithms(known, address); len(now) == 0 || !strings.Contains(now[0], "-cert-v01@openssh.com") || len(now) != len(ssh.SupportedAlgorithms().HostKeys) {
		t.Errorf("a bare * authority line alone: %v, want the certificate algorithms first", now)
	}
}

// keptConn is the connection kept to address, nil when there is none.
func keptConn(address string) *sshConn {
	sshConns.mu.Lock()
	defer sshConns.mu.Unlock()
	for key, entry := range sshConns.entries {
		if key.address == address {
			return entry
		}
	}
	return nil
}

// changeKnownHosts changes the known_hosts file of c on disk, so the next
// scrape of a host replaces the connection kept to it.
func changeKnownHosts(t *testing.T, c *model.Collector) {
	t.Helper()
	data, err := os.ReadFile(c.Request.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Request.KnownHostsFile, append(data, "# changed\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stallAScrapeOf starts a scrape of c on the frozen host at address, waits
// until its connection is in the SSH handshake, and returns the end of the
// scrape's deadline and its result.
func stallAScrapeOf(t *testing.T, address string, c *model.Collector) (end func(), result <-chan error) {
	t.Helper()
	before := keptConn(address)
	ctx, end := testutil.DeadlineEndedByHand()
	failed := make(chan error, 1)
	go func() {
		_, err := FetchCollector(ctx, address, c, RequestOverrides{}, nil)
		failed <- err
	}()
	testutil.WaitFor(t, "the handshake with the frozen host", func() bool {
		entry := keptConn(address)
		return entry != nil && entry != before && entry.stage.Load() == sshStageHandshake
	})
	return end, failed
}

// A connection being made that no scrape waits for any more, replaced at the
// next scrape because a file of it changed, is given up, its dial cancelled,
// rather than left to its 30 seconds: at max_open_connections 1, neither
// the scrape that replaced it nor a scrape of a healthy host after that
// waits for it, and none fails at the limit.
func TestSSHFileGivesUpAReplacedConnectionNoScrapeWaitsFor(t *testing.T) {
	f, other, known := sshLimitHosts(t)
	c := f.collector(t, "replaced", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = known, 1 })
	events := sshEvents(t, f.server.Addr)
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	end, failed := stallAScrapeOf(t, f.server.Addr, c)
	end()
	if err := <-failed; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the scrape of the frozen host: %v", err)
	}
	changeKnownHosts(t, c)
	end, failed = stallAScrapeOf(t, f.server.Addr, c)
	waitEvent(t, events, "given up: replaced")
	end()
	if err := <-failed; !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "max_open_connections") {
		t.Errorf("the scrape that replaced the connection: %v, want its deadline in the handshake", err)
	}
	// Ten seconds, a third of the time the making of a connection is given,
	// bound a scrape that wrongly waited for it.
	healthy, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := FetchCollector(healthy, other.Addr, c, RequestOverrides{}, nil); err != nil {
		t.Errorf("the scrape of the healthy host: %v", err)
	}
	if _, failures := sshConns.limitCounts("replaced"); failures != 0 {
		t.Errorf("failed=%d at the limit, want 0", failures)
	}
}

// A connection being made that a reload leaves to no collector while a
// scrape waits for it is kept for that scrape, a scrape at
// max_open_connections 1 failing meanwhile saying so, and given up when the
// scrape stops waiting: the reloaded collector's scrape of a healthy host
// then does not wait for it.
func TestSSHFileGivesUpAConnectionAReloadLeftWhenItsScrapeStopsWaiting(t *testing.T) {
	f, other, known := sshLimitHosts(t)
	c := f.collector(t, "reloaded", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = known, 1 })
	events := sshEvents(t, f.server.Addr)
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	end, failed := stallAScrapeOf(t, f.server.Addr, c)
	data, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	moved := sshtest.WriteFile(t, f.dir, "known_moved", string(data))
	reloaded := f.collector(t, "reloaded", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile, r.MaxOpenConnections = moved, 1 })
	sshConns.follow([]model.Collector{*reloaded}, nil)
	ctx, endWaiting := testutil.DeadlineEndedByHand()
	limited := make(chan error, 1)
	go func() {
		_, err := FetchCollector(ctx, other.Addr, reloaded, RequestOverrides{}, nil)
		limited <- err
	}()
	testutil.WaitFor(t, "the scrape waiting at the limit", func() bool { waited, _ := sshConns.limitCounts("reloaded"); return waited == 1 })
	endWaiting()
	if err := <-limited; err == nil || !strings.Contains(err.Error(), "connections (1 being made for a scrape waiting for it)") {
		t.Errorf("the scrape at the limit while the scrape waits: %v", err)
	}
	end()
	if err := <-failed; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the scrape of the frozen host: %v", err)
	}
	healthy, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := FetchCollector(healthy, other.Addr, reloaded, RequestOverrides{}, nil); err != nil {
		t.Errorf("the reloaded collector's scrape of the healthy host: %v", err)
	}
	waitEvent(t, events, "given up: unused")
	if _, failures := sshConns.limitCounts("reloaded"); failures != 1 {
		t.Errorf("failed=%d at the limit, want the one scrape that failed while the scrape waited", failures)
	}
}

// replaceWhileAScrapeWaits starts a scrape of c on the frozen host that
// waits for its connection, then replaces that connection by a scrape that
// stops waiting for its own, a file of c having changed; it returns the
// waiting scrape's result.
func replaceWhileAScrapeWaits(t *testing.T, f *sshFixture, c *model.Collector) <-chan error {
	t.Helper()
	f.server.Freeze()
	t.Cleanup(f.server.Thaw)
	read := make(chan error, 1)
	go func() {
		_, err := FetchCollector(context.Background(), f.server.Addr, c, RequestOverrides{Timeout: time.Minute}, nil)
		read <- err
	}()
	testutil.WaitFor(t, "the handshake with the frozen host", func() bool { return dialStage(f.server.Addr) == sshStageHandshake })
	changeKnownHosts(t, c)
	end, failed := stallAScrapeOf(t, f.server.Addr, c)
	end()
	if err := <-failed; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the scrape that replaced the connection: %v", err)
	}
	return read
}

// A connection replaced in the cache while a scrape waits for it is made
// for that scrape, which reads on it, and counted in connects_total, as is
// the connection that replaced it.
func TestSSHFileCountsAReplacedConnectionAScrapeReadOn(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "counted", "app.prom")
	events := sshEvents(t, f.server.Addr)
	read := replaceWhileAScrapeWaits(t, f, c)
	f.server.Thaw()
	if err := <-read; err != nil {
		t.Fatalf("the waiting scrape: %v", err)
	}
	waitEvent(t, events, "made")
	waitEvent(t, events, "made")
	if _, made := sshConns.collectorConnections("counted"); made != 2 {
		t.Errorf("made=%d, want both connections", made)
	}
}

// The exporter's shutdown gives up a connection being made that the cache
// has forgotten, replaced while a scrape waits for it, as well as the one
// it keeps: the waiting scrape fails at once rather than at its deadline.
func TestSSHFileShutdownGivesUpAReplacedConnectionBeingMade(t *testing.T) {
	f := newSSHFixture(t)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	c := f.collector(t, "shut", "app.prom")
	events := sshEvents(t, f.server.Addr)
	read := replaceWhileAScrapeWaits(t, f, c)
	sshConns.closeAll()
	waitEvent(t, events, "given up: shutdown")
	waitEvent(t, events, "given up: shutdown")
	if err := <-read; err == nil {
		t.Error("the waiting scrape read on a connection given up")
	}
}

// A host on a port other than 22 that known_hosts_file pins by its plain
// ECDSA line is asked for that type first, as OpenSSH asks, though the file
// also trusts an unrelated authority with a bare * line and the host also
// presents an ed25519 certificate of another authority: it is taken by its
// line. When it presents that certificate alone, it is refused saying the
// key certified is not the one the file holds for it.
func TestSSHFileAsksAHostPinnedByItsLineForItsTypeBesideABareWildcardAuthority(t *testing.T) {
	edHost, ecHost, stranger, authority := sshtest.NewEd25519Key(t), sshtest.NewECDSAKey(t), sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t)
	certificate := sshHostCertificate(t, edHost, stranger)
	f := newSSHFixture(t, certificate, ecHost)
	testutil.WriteIn(t, f.root, "app.prom", sshPromFile)
	known := sshtest.WriteFile(t, f.dir, "known_pinned", f.server.KnownHostsLine(ecHost)+"@cert-authority * "+string(ssh.MarshalAuthorizedKey(authority.PublicKey())))
	if _, err := f.fetch(f.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = known }), RequestOverrides{}); err != nil {
		t.Errorf("the host's line and an unrelated bare * authority: %v", err)
	}
	alone := newSSHFixture(t, certificate)
	testutil.WriteIn(t, alone.root, "app.prom", sshPromFile)
	known = sshtest.WriteFile(t, alone.dir, "known_pinned", alone.server.KnownHostsLine(ecHost)+"@cert-authority * "+string(ssh.MarshalAuthorizedKey(authority.PublicKey())))
	_, err := alone.fetch(alone.collector(t, "c", "app.prom", func(r *model.RequestConfig) { r.KnownHostsFile = known }), RequestOverrides{})
	if want := "of the key ssh-ed25519 " + ssh.FingerprintSHA256(edHost.PublicKey()) + ", which is not the one the file holds for the host"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("the certificate alone: %v, want it to say %q", err, want)
	}
}

// A host certificate of an authority no line trusts for the host is refused
// saying why its plain key was not taken either: no line holds it, the
// host's line holds another key, or the file marks it @revoked.
func TestSSHFileSaysWhyTheKeyOfAnUntrustedCertificateIsNotTaken(t *testing.T) {
	dir := t.TempDir()
	host, other, stranger := sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t), sshtest.NewEd25519Key(t)
	address := "127.0.0.1:2222"
	name := knownhosts.Normalize(address)
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}
	certificate := sshHostCertificate(t, host, stranger).PublicKey()
	certified := "of the key ssh-ed25519 " + ssh.FingerprintSHA256(host.PublicKey())
	for label, test := range map[string]struct{ lines, want string }{
		"no line":            {knownhosts.Line([]string{"[10.0.0.9]:2222"}, other.PublicKey()), "and no line of the file holds the key it certifies"},
		"another key's line": {knownhosts.Line([]string{name}, other.PublicKey()), certified + ", which is not the one the file holds for the host"},
		"a revoked key":      {knownhosts.Line([]string{name}, host.PublicKey()) + "\n@revoked * " + string(ssh.MarshalAuthorizedKey(host.PublicKey())), certified + ", which the file marks @revoked"},
	} {
		file := sshtest.WriteFile(t, dir, strings.NewReplacer(" ", "_", "'", "").Replace(label), test.lines+"\n")
		known, err := readKnownHosts(file)
		if err != nil {
			t.Fatal(err)
		}
		refused := known.verify(address, remote, certificate)
		if refused == nil {
			t.Fatalf("%s: the certificate was taken", label)
		}
		key := sshConnKey{address: address}
		key.knownHostsFile = file
		if msg := sshHostKeyError(key, certificate, refused, known).Error(); !strings.Contains(msg, test.want) || !strings.Contains(msg, "which no @cert-authority line of request.known_hosts_file") {
			t.Errorf("%s: %s, want it to say %q", label, msg, test.want)
		}
	}
}
