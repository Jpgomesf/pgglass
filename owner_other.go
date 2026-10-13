//go:build !unix

package main

import "io/fs"

func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
