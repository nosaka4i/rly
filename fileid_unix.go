//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileID returns the device and inode of a file, used to notice when a log
// was replaced (deleted and recreated) rather than appended to.
func fileID(fi os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
