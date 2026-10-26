//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// An sshfile read reads through an SFTP session what localfile reads through
// an os.Root (filereads.go): sftpSource is the fileSource of request.root on
// the host.
//
// The operating system confines an os.Root's names; an SFTP server confines
// nothing but to what the account may read. So every name is resolved here
// before it is used: request.root, once a read, and then each name under it,
// component by component, every symbolic link on the way followed as the
// server would follow it, with Lstat and ReadLink. A name that resolves
// outside root is refused (errLeavesRoot), and the server is handed the
// resolved path, which has no link left in it to follow. A link changed
// between that check and the open is followed by the server: what the
// exporter can then read is what the account can, which is why it should be
// one that can read nothing else it must not.

// sshMaxLinks is how many symbolic links one name may lead through, as
// Linux's own limit, before it is refused as a loop.
const sshMaxLinks = 40

// sftpSource is request.root on a host, through an SFTP session.
type sftpSource struct {
	client *sftp.Client
	// conn is the connection client's session is on, which a directory is
	// listed on, on a session of its own (sftpLister).
	conn *ssh.Client
	// hostKeepsSessions is shared by the listings of conn
	// (sftpLister.hostKeepsSessions); nil gives each listing its own.
	hostKeepsSessions *atomic.Bool
	// root is request.root as configured, and resolved where its links
	// lead, which run sets.
	root, resolved string
	// open are the files the read has open, and listing the directories it
	// lists, which abandon closes.
	mu        sync.Mutex
	open      map[*sftp.File]bool
	listing   map[*sftpLister]bool
	abandoned bool
}

// run resolves root and then reads.
func (s *sftpSource) run(read func() error) error {
	resolved, err := s.walk("/", splitRemote(s.root))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("request.root %s does not exist", s.root)
		}
		return fmt.Errorf("resolving request.root %s: %w", s.root, err)
	}
	s.resolved = resolved
	return read()
}

// abandon closes the files the read has open, each on its own: a close
// waits for the server's answer. A directory being listed, or whose
// listing is being closed, is abandoned too, which ends its listing at
// once, the host answering or not, and waits for nothing.
func (s *sftpSource) abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abandoned = true
	for f := range s.open {
		go func() { _ = f.Close() }()
	}
	for l := range s.listing {
		l.abandon()
	}
}

// splitRemote is the components of a path on the host.
func splitRemote(p string) []string {
	return strings.Split(strings.Trim(p, "/"), "/")
}

// walk follows the components from start, a path with no link in it, the
// way the server would, and returns the path they lead to, with no link left
// in it.
func (s *sftpSource) walk(start string, components []string) (string, error) {
	current := start
	links := 0
	for len(components) > 0 {
		part := components[0]
		components = components[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			current = path.Dir(current)
			continue
		}
		next := path.Join(current, part)
		info, err := s.client.Lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			current = next
			continue
		}
		if links++; links > sshMaxLinks {
			return "", fmt.Errorf("%s leads through more than %d symbolic links", next, sshMaxLinks)
		}
		target, err := s.client.ReadLink(next)
		if err != nil {
			return "", err
		}
		// A Windows SFTP server would take a backslash for a separator,
		// which this walk, splitting on / alone, would not follow.
		if strings.Contains(target, `\`) {
			return "", fmt.Errorf("the symbolic link %s leads to %q, which has a backslash; a link with one is not followed", next, target)
		}
		if path.IsAbs(target) {
			current = "/"
		}
		components = append(splitRemote(target), components...)
	}
	return current, nil
}

// resolve is the path on the host of name, relative to root, refused when
// it leads outside root.
func (s *sftpSource) resolve(name string) (string, error) {
	p, err := s.walk(s.resolved, splitRemote(name))
	if err != nil {
		return "", err
	}
	if s.resolved != "/" && p != s.resolved && !strings.HasPrefix(p, s.resolved+"/") {
		return "", errLeavesRoot
	}
	return p, nil
}

func (s *sftpSource) Stat(name string) (fs.FileInfo, error) {
	p, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	return s.client.Stat(p)
}

func (s *sftpSource) OpenFile(name string) (sourceFile, error) {
	p, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	f, err := s.client.OpenFile(p, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.abandoned {
		go func() { _ = f.Close() }()
		return nil, errors.New("the read was abandoned")
	}
	if s.open == nil {
		s.open = map[*sftp.File]bool{}
	}
	s.open[f] = true
	return &sftpFile{File: f, src: s}, nil
}

func (s *sftpSource) OpenDir(name string) (sourceDir, error) {
	p, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	info, err := s.client.Stat(p)
	if err != nil {
		return nil, err
	}
	l := newSFTPLister(s.conn, p)
	if s.hostKeepsSessions != nil {
		l.hostKeepsSessions = s.hostKeepsSessions
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.abandoned {
		return nil, errors.New("the read was abandoned")
	}
	if s.listing == nil {
		s.listing = map[*sftpLister]bool{}
	}
	s.listing[l] = true
	return &sftpDir{lister: l, src: s, info: info}, nil
}

// sftpFile is a file a read has open, until it closes it.
type sftpFile struct {
	*sftp.File
	src *sftpSource
}

func (f *sftpFile) Close() error {
	f.src.mu.Lock()
	delete(f.src.open, f.File)
	f.src.mu.Unlock()
	return f.File.Close()
}

// sftpDir lists a directory on the host, a batch at a time as the
// directory read asks for entries (sftpLister), so the bound on the entries
// a scrape lists is the bound on what is asked of the host and held in
// memory.
type sftpDir struct {
	lister *sftpLister
	src    *sftpSource
	info   fs.FileInfo
}

func (d *sftpDir) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *sftpDir) ReadDir(n int) ([]fs.DirEntry, error) { return d.lister.ReadDir(n) }

// Close closes the listing. It is forgotten only once closed, so that
// abandon still reaches a Close waiting for the host and ends that wait.
func (d *sftpDir) Close() error {
	err := d.lister.Close()
	d.src.mu.Lock()
	delete(d.src.listing, d.lister)
	d.src.mu.Unlock()
	return err
}

// sshConns are the connections of sshfile collectors, each with its SFTP
// session (sshconn.go).
var sshConns = newSSHConnCache(RequestTypeSSHFile, startSFTP, "starting the SFTP session")

// startSFTP starts the SFTP session of an sshfile connection, once its
// handshake is done.
func startSFTP(ctx context.Context, key sshConnKey, client *ssh.Client) (io.Closer, error) {
	session, err := sftp.NewClient(client)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("starting the SFTP session on %s: %w", key.address, ctx.Err())
		}
		return nil, fmt.Errorf("the host %s took the connection of user %q but did not start an SFTP session: %w; the account needs the sftp subsystem, which OpenSSH's Subsystem sftp line enables", key.address, key.username, err)
	}
	return &sftpConn{Client: session}, nil
}

// sftpConn is what an sshfile connection holds beside its SSH connection:
// its SFTP session, which it is closed with, and whether the host did not
// close a listing's session on it in time, which the connection's listings
// share (sftpLister.hostKeepsSessions).
type sftpConn struct {
	*sftp.Client
	listingSessionsKept atomic.Bool
}

// sftp is what an sshfile connection holds beside its SSH connection.
func (entry *sshConn) sftp() *sftpConn {
	return entry.attached.(*sftpConn)
}

// sftpClient is the SFTP session of an sshfile connection.
func (entry *sshConn) sftpClient() *sftp.Client {
	return entry.sftp().Client
}

// size reports how many connections a cache keeps, for tests.
func (c *sshConnCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// session runs read on a connection for key, with a fileSource of root on it,
// within ctx. A read that loses its connection is run once more, on a new
// one, while ctx lasts. shown is what a debug probe's request section shows
// of the read.
func (c *sshConnCache) session(ctx context.Context, key sshConnKey, collector string, limit int, root, shown string, read func(*sftpSource) error) error {
	for attempt := 0; ; attempt++ {
		entry, reused, err := c.get(ctx, key, collector, limit)
		traceSSH(ctx, localFileMethod, key, shown, nil, reused, err)
		if err != nil {
			return err
		}
		src := &sftpSource{client: entry.sftpClient(), conn: entry.client, hostKeepsSessions: &entry.sftp().listingSessionsKept, root: root}
		done := make(chan error, 1)
		go func() {
			if hook := beforeSSHRead.Load(); hook != nil {
				(*hook)()
			}
			done <- src.run(func() error { return read(src) })
		}()
		select {
		case err = <-done:
			c.release(entry)
		case <-ctx.Done():
			c.abandon(entry, src, done)
			err = fmt.Errorf("reading %s: %w", shown, ctx.Err())
			traceOutcome(ctx, "error: "+err.Error())
			return err
		}
		if err != nil && attempt == 0 && ctx.Err() == nil && entry.lost(err) {
			c.drop(entry, "lost")
			traceOutcome(ctx, "the connection was lost: "+err.Error())
			continue
		}
		if err != nil {
			traceOutcome(ctx, "error: "+err.Error())
		} else {
			traceOutcome(ctx, "read")
		}
		return err
	}
}

// directory reads a directory on a connection for key as session reads a
// file, and hands answer what was read: all of it, or, when ctx ended
// first, the files read by then (awaitDirectory).
func (c *sshConnCache) directory(ctx context.Context, key sshConnKey, collector string, limit int, root, shown string, read func(*sftpSource, *directoryProgress) error, answer func(*DirectoryRead)) error {
	for attempt := 0; ; attempt++ {
		entry, reused, err := c.get(ctx, key, collector, limit)
		traceSSH(ctx, localFileMethod, key, shown, nil, reused, err)
		if err != nil {
			return err
		}
		src := &sftpSource{client: entry.sftpClient(), conn: entry.client, hostKeepsSessions: &entry.sftp().listingSessionsKept, root: root}
		progress := &directoryProgress{}
		done := make(chan error, 1)
		go func() { done <- src.run(func() error { return read(src, progress) }) }()
		abandoned := false
		result, err := awaitDirectory(ctx, root, progress, done, func() {
			abandoned = true
			c.abandon(entry, src, done)
		})
		if !abandoned {
			c.release(entry)
			if err != nil && attempt == 0 && ctx.Err() == nil && entry.lost(err) {
				c.drop(entry, "lost")
				traceOutcome(ctx, "the connection was lost: "+err.Error())
				continue
			}
		}
		if err != nil {
			traceOutcome(ctx, "error: "+err.Error())
			return err
		}
		traceOutcome(ctx, fmt.Sprintf("read %d files", len(result.Files)))
		answer(result)
		return nil
	}
}

// abandon is a scrape giving up on its read: the files the read has open
// are closed, and the read is given sshAbandonGrace to return, after which
// the connection is dropped. The connection is released when the read
// returns.
func (c *sshConnCache) abandon(entry *sshConn, src *sftpSource, done <-chan error) {
	src.abandon()
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

// lost says a read's error is its connection ending under it.
func (entry *sshConn) lost(err error) bool {
	return errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || isClosedChannel(entry.ended)
}
