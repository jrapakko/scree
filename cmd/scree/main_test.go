package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMain points the settings file at an empty home, so no test reads the
// real ~/.config/scree/env; tests that need one replace settingsHome. exec
// is an error unless a test replaces execProgram, so no test replaces the
// test binary.
//
// With SCREE_TEST_MAIN_HOME set, the test binary is scree instead: main
// runs SCREE_TEST_MAIN_ARGS (split on spaces) with that home.
func TestMain(m *testing.M) {
	if h := os.Getenv("SCREE_TEST_MAIN_HOME"); h != "" {
		settingsHome = func() (string, error) { return h, nil }
		os.Args = append([]string{"scree"}, strings.Fields(os.Getenv("SCREE_TEST_MAIN_ARGS"))...)
		main()
		os.Exit(0)
	}
	// A caller's SCREE_KEY_FROM (an agent run sets it to none) would make
	// commands under test act as re-executed children.
	os.Unsetenv(keyFromVar)
	home, err := os.MkdirTemp("", "scree-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	settingsHome = func() (string, error) { return home, nil }
	execProgram = func(string, []string, []string) error { return errors.New("exec in a test") }
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
