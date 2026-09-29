package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
)

// fixture builds a GOMODCACHE with a->b and a main module under test.
func fixture(t *testing.T) (cacheRoot, moduleDir string, v *Verifier) {
	t.Helper()
	cacheRoot = t.TempDir()
	putModule(t, cacheRoot, "example.com/a", "v1.0.0",
		"module example.com/a\nrequire example.com/b v1.0.0 // indirect\n",
		map[string]string{"a.go": "package a\n"})
	putModule(t, cacheRoot, "example.com/b", "v1.0.0",
		"module example.com/b\n",
		map[string]string{"b.go": "package b\n"})

	moduleDir = t.TempDir()
	writeMain(t, moduleDir,
		"module example.com/main\nrequire example.com/a v1.0.0\n")
	return cacheRoot, moduleDir, New()
}

func putModule(t *testing.T, root, path, version, gomod string, files map[string]string) {
	t.Helper()
	enc := encodePath(path)
	dir := filepath.Join(root, filepath.FromSlash(enc)+"@"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dl := filepath.Join(root, "cache", "download",
		filepath.FromSlash(enc), "@v")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dl, version+".mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
}

func encodePath(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'A' && c <= 'Z' {
			out = append(out, '!', c+32)
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

func writeMain(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func opts(cacheRoot, moduleDir string) Options {
	return Options{ModuleDir: moduleDir, CacheRoot: cacheRoot}
}

func hasCode(err error, code ErrorCode) bool {
	return errors.Is(err, &Failure{Code: code})
}

func TestLockThenVerifyHappyPath(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	ctx := context.Background()
	o := opts(cacheRoot, moduleDir)

	_, desired, changed, err := v.EnsureLock(ctx, o)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if !changed {
		t.Fatal("first lock must write the file")
	}
	rep, err := v.Verify(ctx, o)
	if err != nil {
		t.Fatalf("verify after lock: %v", err)
	}
	if rep.Checked != 2 {
		t.Fatalf("checked %d, want 2", rep.Checked)
	}
	t.Logf("verified %d modules; manifest=%s", rep.Checked, rep.ManifestHash)

	// Repeated locking is idempotent and byte-identical.
	before := desired
	_, desired2, changed2, err := v.EnsureLock(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if changed2 {
		t.Fatal("second lock must not rewrite an up-to-date lock")
	}
	if string(before) != string(desired2) {
		t.Fatal("repeated lock output differs")
	}
	t.Log("repeated locking idempotent and byte-identical")
}

func TestStaleLockDistinguishedFromTampered(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	ctx := context.Background()
	o := opts(cacheRoot, moduleDir)
	if _, _, _, err := v.EnsureLock(ctx, o); err != nil {
		t.Fatal(err)
	}

	// Declaration changes without regenerating the lock -> stale.
	writeMain(t, moduleDir,
		"module example.com/main\nrequire example.com/a v1.1.0\n")
	_, err := v.Verify(ctx, o)
	if err == nil {
		t.Fatal("expected stale lock error")
	}
	if !hasCode(err, ErrLockStale) {
		t.Fatalf("want stale, got %v", err)
	}
	if hasCode(err, ErrLockTampered) {
		t.Fatal("stale must not be reported as tampered")
	}
	t.Logf("stale classified:\n%v", err)

	// Restore declarations, tamper with a locked digest but keep a valid self
	// hash -> tampered, not stale.
	writeMain(t, moduleDir,
		"module example.com/main\nrequire example.com/a v1.0.0\n")
	lockPath := filepath.Join(moduleDir, "go.lock")
	l, lerr := lockfile.Unmarshal(mustRead(t, lockPath))
	if lerr != nil {
		t.Fatal(lerr)
	}
	l.Entries[0].TreeDigest = "sha256:" + repeatf('f', 64)
	l.LockHash = l.SelfHash() // internally consistent body -> survives integrity
	tampered, merr := lockfile.Marshal(l)
	if merr != nil {
		t.Fatal(merr)
	}
	if err := os.WriteFile(lockPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(ctx, o)
	if err == nil {
		t.Fatal("expected checksum mismatch for edited-and-rehashed digest")
	}
	if !hasCode(err, ErrChecksumMismatch) {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if hasCode(err, ErrLockTampered) {
		t.Fatal("a self-consistent lock with a bad digest must not claim self-hash tampering")
	}
	t.Logf("edited-and-rehashed digest classified as checksum mismatch:\n%v", err)
}

func TestTamperedStructuralFieldAfterRehash(t *testing.T) {
	// Editing a declaration-derived field (version) while keeping both hashes
	// self-consistent is structural tampering and must be distinguished from a
	// pure cache digest drift.
	cacheRoot, moduleDir, v := fixture(t)
	ctx := context.Background()
	o := opts(cacheRoot, moduleDir)
	if _, _, _, err := v.EnsureLock(ctx, o); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(moduleDir, "go.lock")
	l, lerr := lockfile.Unmarshal(mustRead(t, lockPath))
	if lerr != nil {
		t.Fatal(lerr)
	}
	l.Entries[0].Version = "v9.9.9"
	l.LockHash = l.SelfHash()
	data, merr := lockfile.Marshal(l)
	if merr != nil {
		t.Fatal(merr)
	}
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := v.Verify(ctx, o)
	if !hasCode(err, ErrLockTampered) {
		t.Fatalf("want structural tamper, got %v", err)
	}
	t.Logf("structural tamper classified:\n%v", err)
}

func TestTamperedLockHashDirectly(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	ctx := context.Background()
	o := opts(cacheRoot, moduleDir)
	if _, _, _, err := v.EnsureLock(ctx, o); err != nil {
		t.Fatal(err)
	}
	// Corrupt a digest hex character without rehashing: document stays valid
	// JSON but its self hash no longer matches.
	lockPath := filepath.Join(moduleDir, "go.lock")
	data := []byte(mustRead(t, lockPath))
	idx := indexAll(data, []byte("tree_digest"))
	if len(idx) == 0 {
		t.Fatal("tree_digest key not found in lock")
	}
	pos := idx[0] + len("tree_digest\": \"sha256:")
	if data[pos] == 'a' {
		data[pos] = 'b'
	} else {
		data[pos] = 'a'
	}
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := v.Verify(ctx, o)
	if !hasCode(err, ErrLockTampered) {
		t.Fatalf("want tampered (bad self hash), got %v", err)
	}
	t.Logf("broken self-hash classified: %v", err)
}

func TestChecksumMismatchFromCacheChange(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	ctx := context.Background()
	o := opts(cacheRoot, moduleDir)
	if _, _, _, err := v.EnsureLock(ctx, o); err != nil {
		t.Fatal(err)
	}
	// Mutate cached file content of b without touching the lock.
	bDir := filepath.Join(cacheRoot, filepath.FromSlash(encodePath("example.com/b"))+"@v1.0.0")
	if err := os.WriteFile(filepath.Join(bDir, "b.go"), []byte("package b // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := v.Verify(ctx, o)
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
	if !hasCode(err, ErrChecksumMismatch) {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	var fl FailureList
	if errors.As(err, &fl) {
		for _, f := range fl {
			for _, d := range f.Diffs {
				t.Logf("diff path=%s field=%s expected=%s actual=%s by=%v",
					d.Path, d.Field, d.Expected, d.Actual, d.IntroducedBy)
			}
		}
	}
	if rep == nil || len(rep.Diffs) == 0 {
		t.Fatal("report must carry expected-vs-actual diffs")
	}
}

func TestMissingDependencyOffline(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	ctx := context.Background()
	o := opts(cacheRoot, moduleDir)
	// Remove module b entirely from the cache.
	if err := os.RemoveAll(filepath.Join(cacheRoot, filepath.FromSlash(encodePath("example.com/b"))+"@v1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(cacheRoot, "cache", "download",
		filepath.FromSlash(encodePath("example.com/b")))); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := v.EnsureLock(ctx, o)
	if !hasCode(err, ErrMissingDependency) {
		t.Fatalf("want missing dependency, got %v", err)
	}
	t.Logf("missing dependency classified: %v", err)
}

func TestDeterministicAcrossRepeatedAndConcurrent(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	ctx := context.Background()
	o := opts(cacheRoot, moduleDir)

	// Baseline lock.
	_, base, _, err := v.EnsureLock(ctx, o)
	if err != nil {
		t.Fatal(err)
	}

	// Each concurrent run gets its own module copy and cache root copy, so no
	// run can observe another's intermediate state or writes.
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cr := copyDir(t, cacheRoot)
			md := copyDir(t, moduleDir)
			rep, verr := v.Verify(ctx, Options{ModuleDir: md, CacheRoot: cr})
			if verr != nil {
				errs <- fmt.Errorf("worker %d verify: %w", i, verr)
				return
			}
			if rep.Checked != 2 {
				errs <- fmt.Errorf("worker %d checked %d", i, rep.Checked)
			}
			// Second verify in the same worker must be identical.
			rep2, verr2 := v.Verify(ctx, Options{ModuleDir: md, CacheRoot: cr})
			if verr2 != nil {
				errs <- fmt.Errorf("worker %d repeat: %w", i, verr2)
				return
			}
			if rep2.ManifestHash != rep.ManifestHash || rep2.Checked != rep.Checked {
				errs <- fmt.Errorf("worker %d nondeterministic repeat", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// After concurrent reads nothing in the source module dir changed.
	if data := mustRead(t, filepath.Join(moduleDir, "go.lock")); string(data) != string(base) {
		t.Fatal("concurrent verification mutated the source lock")
	}
	t.Log("concurrent verification runs isolated and mutually consistent")
}

func indexAll(haystack, needle []byte) []int {
	var out []int
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			out = append(out, i)
		}
	}
	return out
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func repeatf(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestManifestMalformedClassifiedByVerify(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	writeMain(t, moduleDir,
		"module example.com/main\nrequire example.com/a v1.0\n") // bad version
	_, err := v.Verify(context.Background(), opts(cacheRoot, moduleDir))
	if err == nil {
		t.Fatal("expected manifest malformed")
	}
	if !hasCode(err, ErrManifestMalformed) {
		t.Fatalf("want manifest malformed, got %v", err)
	}
	t.Logf("malformed manifest classified: %v", err)
}

func TestVersionConflictClassifiedByVerify(t *testing.T) {
	cacheRoot := t.TempDir()
	putModule(t, cacheRoot, "example.com/a", "v1.0.0",
		"module example.com/a\nrequire example.com/b v1.2.0 // indirect\n",
		map[string]string{"a.go": "package a\n"})
	putModule(t, cacheRoot, "example.com/b", "v1.2.0",
		"module example.com/b\n", map[string]string{"b.go": "package b\n"})
	moduleDir := t.TempDir()
	writeMain(t, moduleDir,
		"module example.com/main\nrequire example.com/a v1.0.0\nexclude example.com/b v1.2.0\n")
	_, _, _, err := New().EnsureLock(context.Background(), opts(cacheRoot, moduleDir))
	if !hasCode(err, ErrVersionConflict) {
		t.Fatalf("want version conflict, got %v", err)
	}
	t.Logf("version conflict classified: %v", err)
}

func TestVerifyWithoutLockSucceedsWhenCacheComplete(t *testing.T) {
	cacheRoot, moduleDir, v := fixture(t)
	// No go.lock present: current-state verification against cache.
	rep, err := v.Verify(context.Background(), opts(cacheRoot, moduleDir))
	if err != nil {
		t.Fatalf("verify without lock should succeed on complete cache: %v", err)
	}
	if rep.LockPresent || rep.Checked != 2 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	t.Log("verify without lock OK (current-state, nothing to drift)")
}
