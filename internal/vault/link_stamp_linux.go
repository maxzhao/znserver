//go:build linux

package vault

import (
	"io/fs"
	"syscall"
)

// ctime catches in-place writes followed by restoring mtime and size. Inode
// identity separately catches atomic replacement with preserved timestamps.
func linkChangeTime(info fs.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ctim.Sec*1e9 + stat.Ctim.Nsec
	}
	return -1
}
