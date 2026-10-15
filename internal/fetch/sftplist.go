//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sftpLister lists a directory on the host as SFTP version 3
// (draft-ietf-secsh-filexfer-02) lists one, a batch of entries at a time,
// each asked for only when the one before has been handed out. pkg/sftp,
// which reads everything else, lists a directory only whole, every batch
// until the end before it answers, so a directory of a million entries
// would cost a million entries on every scrape before the bound on what a
// scrape lists (maxListedEntries) applied. This one asks for the next batch
// only when the directory read wants more, and stops when it stops.
//
// It speaks SFTP on a session of its own, opened on the connection for the
// listing and closed when the listing ends, however it ends: at the end of
// the directory, when the read stops listing or is abandoned, or on an
// error. pkg/sftp's session is not shared because what it sends and
// receives is its own. So a listing takes one SSH session beside the
// connection's, which the host's limit on sessions (OpenSSH's MaxSessions,
// 10 by default) must allow, one for each directory listed at once on the
// connection.
//
// What the host sends is held to bounds before anything is made of it: a
// packet past sftpMaxPacket is refused, as pkg/sftp refuses one, and so is
// a count of names or of extended attributes larger than the rest of the
// packet could hold, before anything is allocated for them, and a name that
// is empty or holds a / or a NUL.
//
// The conversation runs in a goroutine of its own (run), which ReadDir
// waits for, so that abandon, which sftpSource.abandon calls at the probe's
// deadline, ends a listing the host stopped answering at once: ReadDir
// returns, and the session is closed without waiting for the host.
type sftpLister struct {
	client *ssh.Client
	path   string

	// want asks run for the next batch, which it sends on batches, the
	// last with io.EOF or the error that ended the listing.
	want    chan struct{}
	batches chan sftpBatch
	// closed is closed by Close or abandon, and hurry by abandon: the
	// session is then closed without waiting for the host.
	closed    chan struct{}
	closeOnce sync.Once
	hurry     chan struct{}
	hurryOnce sync.Once
	// done is closed when run has ended and closed its session.
	done chan struct{}
	// hostKeepsSessions says a host did not close a listing's session on
	// this connection within sftpSessionCloseWait of the end of its input:
	// the listings after it on the connection do not wait for the host.
	// sftpSource shares one among the listings of a connection.
	hostKeepsSessions *atomic.Bool

	// mu guards session; busy, which says run waits for the host's answer
	// to a request; sessionClosed, which says Close closed the session of
	// a busy run; and started, which says ReadDir has started run.
	mu            sync.Mutex
	session       *ssh.Session
	busy          bool
	sessionClosed bool
	started       bool

	// pending are the entries of the last batch not handed out yet, and
	// err what ended the listing.
	pending []fs.DirEntry
	err     error
}

// sftpBatch is one answer to a READDIR request.
type sftpBatch struct {
	entries []fs.DirEntry
	err     error
}

// The SFTP version 3 packets, status codes and attribute flags the lister
// uses.
const (
	sftpPacketInit    = 1
	sftpPacketVersion = 2
	sftpPacketClose   = 4
	sftpPacketOpenDir = 11
	sftpPacketReadDir = 12
	sftpPacketStatus  = 101
	sftpPacketHandle  = 102
	sftpPacketName    = 104

	sftpStatusEOF              = 1
	sftpStatusNoSuchFile       = 2
	sftpStatusPermissionDenied = 3

	sftpAttrSize        = 0x00000001
	sftpAttrUIDGID      = 0x00000002
	sftpAttrPermissions = 0x00000004
	sftpAttrACModTime   = 0x00000008
	sftpAttrExtended    = 0x80000000
)

// sftpMaxPacket is the largest packet the lister takes from the host: what
// pkg/sftp's client takes, and more than any server sends for a batch of
// names.
const sftpMaxPacket = 256 << 10

// sftpDrainLimit is the most the lister reads of what the host still sends
// on a listing's session once it has ended the session's input, before it
// closes the session without waiting further: the answer to a request in
// flight, when the listing was closed while it waited for one, and the
// answer to the directory's CLOSE, each a packet of at most sftpMaxPacket.
// A host sending more than that is not answering as an SFTP server does.
const sftpDrainLimit = 2 * sftpMaxPacket

// sftpMaxEmptyBatches is how many answers in a row to a request for
// directory entries may hold no entry but . and .. (or none) before the
// listing fails: a server sending one entry a batch sends two such, and one
// that sends them without end would otherwise be asked again until the
// probe's deadline, none of its answers counting toward the listing's
// bound.
const sftpMaxEmptyBatches = 16

// sftpMinNameEntry is the fewest bytes one entry of a NAME packet takes: the
// lengths of its name and long name, and its attribute flags.
const sftpMinNameEntry = 12

// errListingAbandoned is what ReadDir answers once Close was called.
var errListingAbandoned = errors.New("the directory listing was abandoned")

func newSFTPLister(client *ssh.Client, path string) *sftpLister {
	return &sftpLister{client: client, path: path, want: make(chan struct{}), batches: make(chan sftpBatch), closed: make(chan struct{}), hurry: make(chan struct{}), done: make(chan struct{}), hostKeepsSessions: &atomic.Bool{}}
}

// ReadDir hands out the entries of the directory, at most n at a time, and
// io.EOF at its end. It asks the host for a batch only when the one before
// is handed out, and hands out no more than that batch, so a call may
// answer fewer than n before the end; n of 0 or less is the batch.
func (l *sftpLister) ReadDir(n int) ([]fs.DirEntry, error) {
	for len(l.pending) == 0 {
		if l.err != nil {
			return nil, l.err
		}
		// Close reads started from another goroutine.
		l.mu.Lock()
		start := !l.started
		l.started = true
		l.mu.Unlock()
		if start {
			go l.run()
		}
		select {
		case l.want <- struct{}{}:
		case <-l.closed:
			l.err = errListingAbandoned
			continue
		}
		select {
		case batch := <-l.batches:
			l.pending, l.err = batch.entries, batch.err
		case <-l.closed:
			l.err = errListingAbandoned
		}
	}
	if n <= 0 || n > len(l.pending) {
		n = len(l.pending)
	}
	out := l.pending[:n:n]
	l.pending = l.pending[n:]
	return out, nil
}

// sftpSessionCloseWait bounds how long a listing waits for the host to
// close its side of the listing's session before closing the session
// itself.
const sftpSessionCloseWait = 2 * time.Second

// Close ends the listing. A listing that ended, or that waits for nothing
// from the host, is waited for until run has ended, or until abandon: run
// waits for the host to close its side of the session, at most
// sftpSessionCloseWait (awaitHostClose), since the host counts the session
// against its limit (OpenSSH's MaxSessions) until then, and a listing of the
// next scrape would otherwise be refused a session the host has not freed
// yet; then it closes the session, the host having closed its side or not.
// Close has no timer of its own: one would end as run's does, and the
// listing after it could start before run had remembered a host that keeps
// sessions open. A listing waiting for the host's answer, to the session's
// opening or to a request, is not waited for: its session is closed at
// once, or, being opened, as soon as the host answers.
func (l *sftpLister) Close() error {
	if l.stop() {
		select {
		case <-l.done:
		case <-l.hurry:
		}
	}
	return nil
}

// abandon ends the listing as Close does but waits for nothing, and makes
// a Close waiting for the host stop waiting: the session is closed at once.
// It is called at the probe's deadline, with sftpSource's lock held.
func (l *sftpLister) abandon() {
	l.hurryOnce.Do(func() { close(l.hurry) })
	l.stop()
}

// stop tells run to end, the first time, and says whether the caller is to
// wait for it: not when it is not started, nor when it waits for the host,
// whose session it closes.
func (l *sftpLister) stop() (wait bool) {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.busy {
			// A session being opened is closed by open when the host
			// answers.
			if l.session != nil {
				l.sessionClosed = true
				session := l.session
				go func() { _ = session.Close() }()
			}
			return
		}
		wait = l.started
	})
	return wait
}

// run is the conversation with the host: it opens the session and the
// directory, then answers each want with a batch until the end of the
// directory, an error or Close, and closes the directory and the session.
func (l *sftpLister) run() {
	var c sftpConversation
	defer close(l.done)
	defer func() {
		l.mu.Lock()
		session, closed := l.session, l.sessionClosed
		l.mu.Unlock()
		if session == nil {
			return
		}
		// The directory's CLOSE and the end of the input tell the host's
		// SFTP server the session is done; its output ends once the host
		// has closed its side, which is when it no longer counts the
		// session. That is waited for, bounded, before the session is
		// closed. (Closing the session first would end the output at
		// once, and Session.Wait answers at once for a session that
		// started a subsystem rather than a command.)
		if c.w != nil && c.r != nil && !closed {
			l.awaitHostClose(&c)
		}
		_ = session.Close()
	}()
	if !l.await() {
		return
	}
	if err := l.open(&c); err != nil {
		l.deliver(sftpBatch{err: l.sessionOnly(err)})
		return
	}
	empty := 0
	for {
		if !l.setBusy(true) {
			return
		}
		entries, err := c.readDir()
		l.setBusy(false)
		if err == nil && len(entries) == 0 {
			// A batch of . and .. alone: the next one is asked for at
			// once, as pkg/sftp does, a few times in a row at most.
			if empty++; empty < sftpMaxEmptyBatches {
				continue
			}
			// The conversation is not broken: the directory is closed.
			err = fmt.Errorf("the host's SFTP server answered %d requests for directory entries in a row with no entry but . and .., rather than with entries or the end of the directory; the listing was stopped", sftpMaxEmptyBatches)
		}
		empty = 0
		if !l.deliver(sftpBatch{entries, l.sessionOnly(err)}) || err != nil {
			return
		}
		if !l.await() {
			return
		}
	}
}

// awaitHostClose closes the directory and ends the input of the listing's
// session, and reads what the host still sends on it until the host closes
// its side, at most sftpDrainLimit bytes; all of it within
// sftpSessionCloseWait, a host that does not read its input holding the
// writes as one that does not close holds the read, and not past abandon.
// A host that did not close it in time is remembered for the connection
// before this returns, so the listings after it there, which may start once
// Close has returned, wait for nothing: the scrape pays the wait once, and
// the session is closed at once. The goroutine goes on after the session is
// closed until the host confirms the close, which an SSH server does at
// once, or the connection ends.
func (l *sftpLister) awaitHostClose(c *sftpConversation) {
	if l.hostKeepsSessions.Load() {
		return
	}
	if hook := sftpHostCloseWait.Load(); hook != nil {
		(*hook)()
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		if c.handle != "" && c.err == nil {
			_ = c.send(sftpPacketClose, c.nextID(), sshString(c.handle))
		}
		if closer, ok := c.w.(io.Closer); ok {
			_ = closer.Close()
		}
		_, _ = io.CopyN(io.Discard, c.r, sftpDrainLimit)
	}()
	timer := time.NewTimer(sftpSessionCloseWait)
	defer timer.Stop()
	select {
	case <-drained:
	case <-l.hurry:
	case <-timer.C:
		l.hostKeepsSessions.Store(true)
	}
}

// sessionOnly is err, the error that ended the listing, unless it is the
// listing's session ending under it (sftpSessionEnded) on a connection that
// is still up: then it is errListingSessionEnded, which does not say the
// connection was lost, so the connection is kept rather than dropped and
// made again. A host's SFTP server may end its session on a request it
// does not take, the host closing that session alone; and a session ends
// too when the connection does, its channels closed with it, which is what
// the mapping to a lost connection is for. Both read as the end of the
// session's output, so the connection is asked: a keepalive@openssh.com
// request, which an SSH server answers, refusing it or not, while the
// connection is up, and which fails at once once it has ended, since
// x/crypto/ssh ends the connection's answers to requests right after its
// channels when it ends. A connection that does not answer within
// sftpConnectionCheckWait, or before the listing is abandoned, is taken for
// lost, as before. The listing is not tried again on a new session: what it
// handed out already would be listed again, and a server that ended one
// session on the listing's requests ends the next too.
func (l *sftpLister) sessionOnly(err error) error {
	if err == nil || !errors.Is(err, sftp.ErrSSHFxConnectionLost) {
		return err
	}
	answered := make(chan error, 1)
	go func() {
		_, _, err := l.client.SendRequest("keepalive@openssh.com", true, nil)
		answered <- err
	}()
	timer := time.NewTimer(sftpConnectionCheckWait)
	defer timer.Stop()
	select {
	case failed := <-answered:
		if failed == nil {
			return errListingSessionEnded
		}
	case <-timer.C:
	case <-l.closed:
	}
	return err
}

// sftpConnectionCheckWait bounds how long a listing whose session ended
// waits for the connection to answer whether it is up (sessionOnly).
const sftpConnectionCheckWait = 2 * time.Second

// errListingSessionEnded is the error of a listing whose session the host
// ended while the connection stayed up.
var errListingSessionEnded = errors.New("the host's SFTP server ended the session the directory was listed on before the listing was done, while the SSH connection stayed up, which is kept; the host's log of its SFTP server says why it ended the session")

// sftpHostCloseWait, set by tests, runs when a listing starts waiting for
// the host to close its session.
var sftpHostCloseWait atomic.Pointer[func()]

// await waits for ReadDir to want a batch, false when Close came first.
func (l *sftpLister) await() bool {
	select {
	case <-l.want:
		return true
	case <-l.closed:
		return false
	}
}

// deliver hands ReadDir a batch, false when Close came first.
func (l *sftpLister) deliver(batch sftpBatch) bool {
	select {
	case l.batches <- batch:
		return true
	case <-l.closed:
		return false
	}
}

// setBusy marks run waiting for the host, or not. It is false, and run is
// not marked, once Close was called: Close closes the session of a run
// marked busy, so one that is not must not start waiting.
func (l *sftpLister) setBusy(busy bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if busy {
		select {
		case <-l.closed:
			return false
		default:
		}
	}
	l.busy = busy
	return true
}

// open opens the session, starts SFTP on it and opens the directory.
func (l *sftpLister) open(c *sftpConversation) error {
	if !l.setBusy(true) {
		return errListingAbandoned
	}
	defer l.setBusy(false)
	session, err := l.client.NewSession()
	if err != nil {
		var refused *ssh.OpenChannelError
		if errors.As(err, &refused) && l.hostKeepsSessions.Load() {
			return fmt.Errorf("the host refused an SSH session to list the directory in (%w); it did not close the session of an earlier listing on this connection within %v of the end of its input, and may still count it against its limit on sessions, MaxSessions in OpenSSH: the host's SFTP server must end when its input ends", err, sftpSessionCloseWait)
		}
		if errors.As(err, &refused) {
			return fmt.Errorf("the host refused an SSH session to list the directory in (%w); a directory is listed on a session of its own beside the connection's SFTP session, so the host's limit on sessions, MaxSessions in OpenSSH (10 by default), must allow one more for each directory listed at once on the connection", err)
		}
		return sftpSessionEnded(err)
	}
	l.mu.Lock()
	select {
	case <-l.closed:
		l.mu.Unlock()
		_ = session.Close()
		return errListingAbandoned
	default:
	}
	l.session = session
	l.mu.Unlock()
	if c.w, err = session.StdinPipe(); err != nil {
		return err
	}
	if c.r, err = session.StdoutPipe(); err != nil {
		return err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return err
	}
	if err := session.RequestSubsystem("sftp"); err != nil {
		return fmt.Errorf("the host did not start an SFTP session to list the directory in: %w", sftpSessionEnded(err))
	}
	// What the server writes to its standard error is read and dropped, as
	// pkg/sftp does, so that it does not hold the session's window.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	return c.start(l.path)
}

// sftpConversation is the SFTP packets of one listing.
type sftpConversation struct {
	r      io.Reader
	w      io.Writer
	id     uint32
	handle string
	// err is set once the conversation is broken: the directory is then
	// not closed, since nothing more is sent.
	err error
}

func (c *sftpConversation) nextID() uint32 {
	c.id++
	return c.id
}

// start sends INIT for version 3 and takes the host's VERSION, and opens
// the directory.
func (c *sftpConversation) start(path string) error {
	var version [4]byte
	binary.BigEndian.PutUint32(version[:], 3)
	if err := c.sendRaw(sftpPacketInit, version[:]); err != nil {
		return c.fail(err)
	}
	typ, data, err := c.receive()
	if err != nil {
		return c.fail(err)
	}
	if typ != sftpPacketVersion {
		return c.fail(fmt.Errorf("the host's SFTP server answered the start of the listing's session with a packet of type %d rather than its version", typ))
	}
	if v, _, ok := sshUint32(data); !ok || v != 3 {
		return c.fail(errors.New("the host's SFTP server does not speak SFTP version 3, which the listing asks for"))
	}
	id := c.nextID()
	if err := c.send(sftpPacketOpenDir, id, sshString(path)); err != nil {
		return c.fail(err)
	}
	typ, data, err = c.answer(id)
	if err != nil {
		return c.fail(err)
	}
	switch typ {
	case sftpPacketHandle:
		handle, _, ok := sshStringAt(data)
		if !ok {
			return c.fail(errors.New("the host's SFTP server sent a malformed handle for the directory"))
		}
		c.handle = handle
		return nil
	case sftpPacketStatus:
		return sftpStatusError(data)
	}
	return c.fail(fmt.Errorf("the host's SFTP server answered the opening of the directory with a packet of type %d", typ))
}

// readDir asks for the next batch of entries: none with io.EOF at the end
// of the directory.
func (c *sftpConversation) readDir() ([]fs.DirEntry, error) {
	id := c.nextID()
	if err := c.send(sftpPacketReadDir, id, sshString(c.handle)); err != nil {
		return nil, c.fail(err)
	}
	typ, data, err := c.answer(id)
	if err != nil {
		return nil, c.fail(err)
	}
	switch typ {
	case sftpPacketName:
		entries, err := parseSFTPNames(data)
		if err != nil {
			return nil, c.fail(err)
		}
		return entries, nil
	case sftpPacketStatus:
		return nil, sftpStatusError(data)
	}
	return nil, c.fail(fmt.Errorf("the host's SFTP server answered a request for directory entries with a packet of type %d", typ))
}

func (c *sftpConversation) fail(err error) error {
	c.err = err
	return err
}

// sftpSessionEnded is the error of a session that ended under the listing,
// as pkg/sftp reports one: not io.EOF, which is the end of the directory,
// and lost, so the read is tried once more on a new connection, as when
// the connection's own session ends, unless the connection is still up
// (sessionOnly).
func sftpSessionEnded(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("the SSH session listing the directory ended: %w", sftp.ErrSSHFxConnectionLost)
	}
	return err
}

// send sends a request: its type, id and body.
func (c *sftpConversation) send(typ byte, id uint32, body []byte) error {
	b := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(b, id)
	return c.sendRaw(typ, append(b, body...))
}

func (c *sftpConversation) sendRaw(typ byte, payload []byte) error {
	b := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(b, uint32(1+len(payload))) //nolint:gosec // G115: a request of a path and a handle, far under 4 GiB
	b[4] = typ
	_, err := c.w.Write(append(b, payload...))
	return sftpSessionEnded(err)
}

// answer receives the answer to request id: its type and what follows the
// id.
func (c *sftpConversation) answer(id uint32) (byte, []byte, error) {
	typ, data, err := c.receive()
	if err != nil {
		return 0, nil, err
	}
	got, data, ok := sshUint32(data)
	if !ok {
		return 0, nil, errors.New("the host's SFTP server sent a packet too short to hold a request id")
	}
	if got != id {
		return 0, nil, fmt.Errorf("the host's SFTP server answered request %d when request %d was asked", got, id)
	}
	return typ, data, nil
}

// receive reads one packet: its type and the rest.
func (c *sftpConversation) receive() (byte, []byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, nil, sftpSessionEnded(err)
	}
	length := binary.BigEndian.Uint32(head[:])
	if length > sftpMaxPacket {
		return 0, nil, model.Errorf("the host's SFTP server sent a packet of %d bytes, more than the %d an SFTP client takes", model.Size(length), sftpMaxPacket)
	}
	if length == 0 {
		return 0, nil, errors.New("the host's SFTP server sent an empty packet")
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(c.r, b); err != nil {
		return 0, nil, sftpSessionEnded(err)
	}
	return b[0], b[1:], nil
}

// sftpStatusError is the error a STATUS packet, what follows its id, says,
// as pkg/sftp makes it: io.EOF at the end of the directory, fs.ErrNotExist
// and fs.ErrPermission for those two, and pkg/sftp's text for another.
func sftpStatusError(data []byte) error {
	code, data, ok := sshUint32(data)
	if !ok {
		return errors.New("the host's SFTP server sent a status too short to hold its code")
	}
	switch code {
	case sftpStatusEOF, 0:
		// pkg/sftp takes OK for the end of the directory too.
		return io.EOF
	case sftpStatusNoSuchFile:
		return fs.ErrNotExist
	case sftpStatusPermissionDenied:
		return fs.ErrPermission
	}
	msg, _, _ := sshStringAt(data)
	return &sftpStatus{code: code, msg: msg}
}

// sftpStatus is a status other than those sftpStatusError names, with
// the text pkg/sftp gives it.
type sftpStatus struct {
	code uint32
	msg  string
}

func (s *sftpStatus) Error() string {
	// pkg/sftp names a code by its lowest byte.
	names := []string{"SSH_FX_OK", "SSH_FX_EOF", "SSH_FX_NO_SUCH_FILE", "SSH_FX_PERMISSION_DENIED", "SSH_FX_FAILURE", "SSH_FX_BAD_MESSAGE", "SSH_FX_NO_CONNECTION", "SSH_FX_CONNECTION_LOST", "SSH_FX_OP_UNSUPPORTED"}
	name := "unknown"
	if low := int(s.code & 0xff); low < len(names) {
		name = names[low]
	}
	return fmt.Sprintf("sftp: %q (%s)", s.msg, name)
}

// parseSFTPNames reads the entries of a NAME packet, what follows its id,
// leaving out . and .., as pkg/sftp does.
func parseSFTPNames(data []byte) ([]fs.DirEntry, error) {
	count, data, ok := sshUint32(data)
	if !ok {
		return nil, errors.New("the host's SFTP server sent directory entries without their count")
	}
	if uint64(count) > uint64(len(data)/sftpMinNameEntry) {
		return nil, model.Errorf("the host's SFTP server sent a count of %d directory entries, more than the %d bytes after it can hold", model.Size(count), model.Size(len(data)))
	}
	entries := make([]fs.DirEntry, 0, count)
	for i := range count {
		var name string
		if name, data, ok = sshStringAt(data); !ok {
			return nil, model.Errorf("the host's SFTP server sent directory entry %d cut short in its name", model.Position(i+1))
		}
		if _, data, ok = sshStringAt(data); !ok {
			return nil, model.Errorf("the host's SFTP server sent directory entry %d cut short in its long name", model.Position(i+1))
		}
		var stat *sftp.FileStat
		var err error
		if stat, data, err = parseSFTPAttrs(data); err != nil {
			return nil, model.Errorf("the host's SFTP server sent directory entry %d cut short in its attributes: %w", model.Position(i+1), err)
		}
		if name == "." || name == ".." {
			continue
		}
		if name == "" || strings.ContainsAny(name, "/\x00") {
			return nil, fmt.Errorf("the host's SFTP server sent the directory entry %q, which is not a name in a directory", name)
		}
		entries = append(entries, fs.FileInfoToDirEntry(&sftpEntryInfo{name: name, stat: stat}))
	}
	return entries, nil
}

// parseSFTPAttrs reads the attributes of an entry: its size, owner, mode
// and times, each when its flag says it is there, and the extended
// attributes, which are skipped.
func parseSFTPAttrs(data []byte) (*sftp.FileStat, []byte, error) {
	flags, data, ok := sshUint32(data)
	if !ok {
		return nil, nil, errors.New("no flags")
	}
	stat := &sftp.FileStat{}
	if flags&sftpAttrSize != 0 {
		if len(data) < 8 {
			return nil, nil, errors.New("no size")
		}
		stat.Size, data = binary.BigEndian.Uint64(data), data[8:]
	}
	fields := []struct {
		flag uint32
		to   *uint32
		what string
	}{
		{sftpAttrUIDGID, &stat.UID, "no owner"}, {sftpAttrUIDGID, &stat.GID, "no group"},
		{sftpAttrPermissions, &stat.Mode, "no mode"},
		{sftpAttrACModTime, &stat.Atime, "no access time"}, {sftpAttrACModTime, &stat.Mtime, "no modification time"},
	}
	for _, f := range fields {
		if flags&f.flag == 0 {
			continue
		}
		if *f.to, data, ok = sshUint32(data); !ok {
			return nil, nil, errors.New(f.what)
		}
	}
	if flags&sftpAttrExtended != 0 {
		var count uint32
		if count, data, ok = sshUint32(data); !ok {
			return nil, nil, errors.New("no count of extended attributes")
		}
		// Each takes two strings, eight bytes at least.
		if uint64(count) > uint64(len(data)/8) {
			return nil, nil, errors.New("more extended attributes than the packet holds")
		}
		for range 2 * count {
			if _, data, ok = sshStringAt(data); !ok {
				return nil, nil, errors.New("an extended attribute cut short")
			}
		}
	}
	return stat, data, nil
}

// sftpEntryInfo is an entry's fs.FileInfo, as pkg/sftp's own: its mode and
// modification time are those pkg/sftp makes of the attributes.
type sftpEntryInfo struct {
	name string
	stat *sftp.FileStat
}

func (fi *sftpEntryInfo) Name() string       { return fi.name }
func (fi *sftpEntryInfo) Size() int64        { return int64(fi.stat.Size) } //nolint:gosec // G115: as pkg/sftp's own
func (fi *sftpEntryInfo) Mode() fs.FileMode  { return fi.stat.FileMode() }
func (fi *sftpEntryInfo) ModTime() time.Time { return fi.stat.ModTime() }
func (fi *sftpEntryInfo) IsDir() bool        { return fi.Mode().IsDir() }
func (fi *sftpEntryInfo) Sys() any           { return fi.stat }

// sshUint32 reads a uint32, false when data is too short.
func sshUint32(data []byte) (uint32, []byte, bool) {
	if len(data) < 4 {
		return 0, data, false
	}
	return binary.BigEndian.Uint32(data), data[4:], true
}

// sshStringAt reads a string, false when its length is past the end of
// data.
func sshStringAt(data []byte) (string, []byte, bool) {
	n, rest, ok := sshUint32(data)
	if !ok || uint64(n) > uint64(len(rest)) {
		return "", data, false
	}
	return string(rest[:n]), rest[n:], true
}

// sshString is s as SFTP writes a string: its length, then its bytes.
func sshString(s string) []byte {
	b := make([]byte, 4, 4+len(s))
	binary.BigEndian.PutUint32(b, uint32(len(s))) //nolint:gosec // G115: a path or a handle, far under 4 GiB
	return append(b, s...)
}
