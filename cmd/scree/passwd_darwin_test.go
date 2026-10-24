package main

import (
	"path/filepath"
	"testing"
)

// TestPasswdHomeIgnoresHOME: on macOS the lookup goes through libSystem,
// not $HOME.
func TestPasswdHomeIgnoresHOME(t *testing.T) {
	fake := t.TempDir()
	t.Setenv("HOME", fake)
	h, err := passwdHome()
	if err != nil {
		t.Skip("no user database entry:", err)
	}
	if h == fake || !filepath.IsAbs(h) {
		t.Errorf("home %q", h)
	}
}
