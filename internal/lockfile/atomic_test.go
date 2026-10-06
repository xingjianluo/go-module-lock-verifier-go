package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriteAtomicBasic writes through the atomic path and reads back whole
// content with the requested permissions.
func TestWriteAtomicBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.lock")
	payload := []byte("# golock v1\npayload\n")
	if err := WriteAtomic(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: %q", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".golock-*.tmp"))
	if len(leftovers) > 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
	t.Logf("JUDGEMENT wrote %d bytes atomically, no leftovers", len(payload))
}

// TestWriteAtomicFailureLeavesNothing forces a failure (target directory
// does not exist) and checks that neither the target nor a temp file
// appears.
func TestWriteAtomicFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-dir", "go.lock")
	err := WriteAtomic(path, []byte("data"), 0o644)
	if err == nil {
		t.Fatal("write into a missing directory must fail")
	}
	t.Logf("JUDGEMENT failure surfaced: %v", err)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Fatalf("failed write left %s behind", e.Name())
	}
}

// TestWriteAtomicOverwriteFailurePreservesOld simulates a failed second
// phase by writing to an unwritable destination after a successful first
// write: the old content must survive untouched.
func TestWriteAtomicOverwriteFailurePreservesOld(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "go.lock")
	old := []byte("old-lock-content")
	if err := WriteAtomic(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the directory read-only so the rename cannot succeed.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	err := WriteAtomic(path, []byte("new-content"), 0o644)
	if err == nil {
		t.Fatal("write into read-only dir must fail")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(old) {
		t.Fatalf("failed overwrite must preserve old lock, got %q", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".golock-*.tmp"))
	if len(leftovers) > 0 {
		t.Fatalf("failed write left temp files: %v", leftovers)
	}
	t.Log("JUDGEMENT old lock preserved, temp cleaned")
}

// TestWriteAtomicConcurrent hammers one path from many goroutines with
// distinct payloads; every read during and after must see one whole
// payload, never a mixture.
func TestWriteAtomicConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.lock")
	payloads := []string{
		strings.Repeat("A", 4096),
		strings.Repeat("B", 2048),
		strings.Repeat("C", 1024),
	}
	valid := map[string]bool{}
	for _, p := range payloads {
		valid[p] = true
	}

	const n = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := WriteAtomic(path, []byte(payloads[i%len(payloads)]), 0o644); err != nil {
				t.Errorf("concurrent write: %v", err)
			}
		}(i)
	}
	close(start)
	// Readers observe the file while writes are in flight.
	for i := 0; i < 50; i++ {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !valid[string(data)] {
			t.Fatalf("observed partial/interleaved content of %d bytes", len(data))
		}
	}
	wg.Wait()
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !valid[string(final)] {
		t.Fatal("final content is not one whole payload")
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".golock-*.tmp"))
	if len(leftovers) > 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
	t.Logf("JUDGEMENT %d concurrent writers -> always whole payloads, no leftovers", n)
}
