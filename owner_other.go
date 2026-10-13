//go:build !unix

package main

import "io/fs"

// fileOwner cannot tell who owns a file on this system, so spillDir never
// trusts a temp directory with spill files.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
