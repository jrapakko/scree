package lock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTryLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	l, pid, err := TryLock(path)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("TryLock = %d, %v", pid, err)
	}
	defer l.Release()
	if _, pid, err := TryLock(path); !errors.Is(err, ErrHeld) || pid != os.Getpid() {
		t.Fatalf("second TryLock = %d, %v; want ErrHeld and pid %d", pid, err, os.Getpid())
	}
	if got := HolderPID(path); got != os.Getpid() {
		t.Errorf("HolderPID = %d", got)
	}
}

func TestReleaseAllowsRelock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	l, _, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, _, err := TryLock(path)
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	l2.Release()
	if err := l2.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

func TestLockFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	path := filepath.Join(t.TempDir(), "writer.lock")
	if err := os.WriteFile(path, []byte("123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, _, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

func TestHolderPIDGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	for _, s := range []string{"", "x\n", "-3\n"} {
		os.WriteFile(path, []byte(s), 0o600)
		if got := HolderPID(path); got != 0 {
			t.Errorf("HolderPID(%q) = %d", s, got)
		}
	}
	if HolderPID(filepath.Join(t.TempDir(), "missing")) != 0 {
		t.Error("missing file")
	}
}

func TestProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	if held, _, err := Probe(path); held || err != nil {
		t.Fatalf("missing file: %v, %v", held, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Probe created the file")
	}
	l, _, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if held, pid, err := Probe(path); !held || pid != os.Getpid() || err != nil {
		t.Fatalf("held: %v, %d, %v", held, pid, err)
	}
	l.Release()
	if held, _, err := Probe(path); held || err != nil {
		t.Fatalf("released: %v, %v", held, err)
	}
	// Probing never takes the lock away from a later TryLock.
	l, _, err = TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}
