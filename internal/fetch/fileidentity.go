package fetch

import "os"

// FileIdentity tells which file st, the stat of a path following its links,
// is: its device and inode, which another file never has while this one
// exists, where the system puts them in the stat (fileidentity_unix.go), and
// ok false where it does not. It looks at nothing beyond the stat, so the
// check of a grpc collector's descriptor files at each call and the
// configuration's watch, which both stamp a file with it, tell the same
// change of file at no cost beyond the stat they make anyway: a file renamed
// over by another of the same time, size and permissions is another file to
// both.
//
// The device and inode are what the filesystem reports. One that does not
// keep a file's inode number while the file stays as it is — a FUSE
// filesystem mounted without use_ino, once the kernel has dropped the file
// from its cache, or the same files mounted again — gives the file another
// identity, and both take it for another file: the next call compiles the
// files again and the next tick of the watch reloads, once for each time the
// number changes.
func FileIdentity(st os.FileInfo) (dev, ino uint64, ok bool) {
	return statIdentity(st)
}

// statIdentity is FileIdentity, set where the system gives the identity, and
// by a test.
var statIdentity = func(os.FileInfo) (dev, ino uint64, ok bool) { return 0, 0, false }
