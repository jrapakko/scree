//go:build !unix

package policy

import "io/fs"

// fileOwner returns -1: there is no ownership check outside unix.
func fileOwner(fs.FileInfo) int { return -1 }

func currentUID() int { return -1 }
