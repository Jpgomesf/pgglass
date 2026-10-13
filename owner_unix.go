//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// fileOwner returns the uid that owns the file info describes.
func fileOwner(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
