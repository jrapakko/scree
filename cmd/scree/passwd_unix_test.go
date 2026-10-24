//go:build unix && !darwin

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPasswdLineHome(t *testing.T) {
	for line, want := range map[string]string{
		"u:x:1000:1000:U:/home/u:/bin/sh":    "/home/u",
		"u:x:1000:1000::/home/u/:/bin/sh\r":  "/home/u",
		"u:x:1000:1000::home/u:/bin/sh":      "",
		"u:x:1000:1000::/home/u":             "",
		"u:x:01000:1000::/home/u:/bin/sh":    "",
		"u:x:1001:1000::/home/u:/bin/sh":     "",
		"+u:x:1000:1000::/home/u:/bin/sh":    "",
		"#u:x:1000:1000::/home/u:/bin/sh":    "",
		":x:1000:1000::/home/u:/bin/sh":      "",
		"u:x:1000:1000::/home/u:/bin/sh:xxx": "",
	} {
		got, ok := passwdLineHome(line, 1000)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", line, got, ok, want)
		}
	}
}

// fakePasswd points passwdHome at a passwd file holding content and a
// getent script printing getent ("" for none).
func fakePasswd(t *testing.T, content, getent string) {
	t.Helper()
	dir := tempDir(t)
	p := filepath.Join(dir, "passwd")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	g := filepath.Join(dir, "getent")
	if getent != "" {
		if err := os.WriteFile(g, []byte(getent), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	replace(t, &passwdFile, p)
	replace(t, &getentPath, g)
}

func TestPasswdHomeFromFile(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	t.Setenv("HOME", "/nonexistent")
	fakePasswd(t, "root:x:0:0::/root:/bin/sh\n\nother:x:12345678:1::/home/other:/bin/sh\nme:x:"+uid+":1::/home/me:/bin/sh\nme2:x:"+uid+":1::/home/me2:/bin/sh\n", "")
	if os.Getuid() == 0 {
		// root is the first line for uid 0.
		if h, err := passwdHome(); err != nil || h != "/root" {
			t.Errorf("home %q, %v", h, err)
		}
		return
	}
	if h, err := passwdHome(); err != nil || h != "/home/me" {
		t.Errorf("home %q, %v", h, err)
	}
}

// TestPasswdHomeGetent: a user missing from /etc/passwd (LDAP, sssd) is
// looked up with getent, which gets an empty environment; with neither,
// passwdHome fails rather than trusting $HOME.
func TestPasswdHomeGetent(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	uid := strconv.Itoa(os.Getuid())
	t.Setenv("HOME", "/from-env")
	t.Setenv("LEAK", "-leaked")
	fakePasswd(t, "other:x:12345678:1::/home/other:/bin/sh\n",
		"#!/bin/sh\n[ \"$1 $2\" = \"passwd "+uid+"\" ] || exit 2\necho \"me:x:"+uid+":1::/home/ldap${LEAK}:/bin/sh\"\n")
	if h, err := passwdHome(); err != nil || h != "/home/ldap" {
		t.Errorf("home %q, %v", h, err)
	}
	fakePasswd(t, "other:x:12345678:1::/home/other:/bin/sh\n", "")
	if h, err := passwdHome(); err == nil {
		t.Errorf("no entry: home %q", h)
	}
	fakePasswd(t, "other:x:12345678:1::/home/other:/bin/sh\n", "#!/bin/sh\necho 'x:x:12345678:1::/home/x:/bin/sh'\n")
	if h, err := passwdHome(); err == nil {
		t.Errorf("getent for another uid: home %q", h)
	}
}

// TestPasswdHomeReal: on this machine, the real lookup ignores $HOME.
func TestPasswdHomeReal(t *testing.T) {
	want, err := homeFromPasswd("/etc/passwd", os.Getuid())
	if err != nil {
		t.Skip("not in /etc/passwd:", err)
	}
	t.Setenv("HOME", t.TempDir())
	if h, err := passwdHome(); err != nil || h != want {
		t.Errorf("home %q, %v; want %q", h, err, want)
	}
}
