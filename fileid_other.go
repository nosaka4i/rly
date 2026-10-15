//go:build !unix

package main

import "os"

// fileID is unavailable here; replacement is detected by size shrink only.
func fileID(fi os.FileInfo) (dev, ino uint64, ok bool) { return 0, 0, false }
