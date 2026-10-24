//go:build osusergo

package main

// With osusergo, os/user on macOS reads /etc/passwd, which lacks normal
// users, and then trusts $HOME, which a repo's .mcp.json sets. Refuse to
// build rather than take the settings file's location from the repo.
var _ int = "scree must not be built with the osusergo tag on macOS: os/user would take the home directory from $HOME"
