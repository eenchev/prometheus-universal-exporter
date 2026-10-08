//go:build unix

package fetch

import (
	"os"
	"syscall"
)

// The stat of a file says which it is here: its device and inode
// (FileIdentity).
func init() {
	statIdentity = func(st os.FileInfo) (dev, ino uint64, ok bool) {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			return uint64(sys.Dev), sys.Ino, true //nolint:gosec,unconvert // a device number is compared, never computed with; its type differs between systems.
		}
		return 0, 0, false
	}
}
