package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.lock")
	if err := WriteAtomic(path, []byte("payload-1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "payload-1" {
		t.Fatalf("read back %q, %v", data, err)
	}
	// Overwrite is atomic too.
	if err := WriteAtomic(path, []byte("payload-2-longer")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "payload-2-longer" {
		t.Fatalf("read back %q", data)
	}
	// No temp files left behind.
	leftover, _ := filepath.Glob(filepath.Join(dir, ".golock-*"))
	if len(leftover) != 0 {
		t.Fatalf("temp files left behind: %v", leftover)
	}
}

func TestWriteAtomicFailureLeavesNoPartialLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.lock")
	original := []byte("previous-good-lock")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the target path a directory so the rename fails after the temp
	// file was written and synced.
	blocker := filepath.Join(dir, "blocked", "go.lock")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	err := WriteAtomic(blocker, []byte("new-content"))
	if err == nil {
		t.Fatal("expected rename onto a directory to fail")
	}
	t.Logf("rename failure: %v", err)
	// The failed run must not leak temp files into the target directory.
	entries, _ := os.ReadDir(filepath.Dir(blocker))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".golock-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
	// And the unrelated pre-existing lock is untouched.
	data, _ := os.ReadFile(path)
	if string(data) != string(original) {
		t.Fatal("unrelated lock file was modified")
	}
}

func TestWriteAtomicIntoMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	err := WriteAtomic(filepath.Join(dir, "no-such-dir", "go.lock"), []byte("x"))
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
	t.Logf("missing dir: %v", err)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("directory should be empty, found %d entries", len(entries))
	}
}
