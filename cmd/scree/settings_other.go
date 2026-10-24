//go:build !unix

package main

import "io/fs"

// checkSettingsOwner has no owner to check outside Unix; loadSettings
// refuses there before it is reached.
func checkSettingsOwner(fs.FileInfo) error { return errLaunchUnsupported }
