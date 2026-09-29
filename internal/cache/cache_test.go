package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// populateCache builds a minimal GOMODCACHE layout for one module version.
func populateCache(t *testing.T, root, path, version, goMod string, files map[string]string) {
	t.Helper()
	enc := filepath.Join(root, filepath.FromSlash(downloadPath(path))+"@"+version)
	if err := os.MkdirAll(enc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(enc, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(enc, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dl := filepath.Join(root, "cache", "download",
		filepath.FromSlash(downloadPath(path)), "@v")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dl, version+".mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModuleHitAndDigestsStable(t *testing.T) {
	root := t.TempDir()
	populateCache(t, root, "example.com/a", "v1.2.3",
		"module example.com/a\n\ngo 1.25\n",
		map[string]string{"foo.go": "package a\n", "sub/bar.go": "package sub\n"})

	c, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ref := ModuleRef{Path: "example.com/a", Version: "v1.2.3"}
	if !c.Exists(context.Background(), ref) {
		t.Fatal("expected module to exist")
	}
	m, err := c.Module(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if m.GoModDigest == "" || m.TreeDigest == "" {
		t.Fatal("digests missing")
	}
	m2, _ := c.Module(context.Background(), ref)
	if m.TreeDigest != m2.TreeDigest || m.GoModDigest != m2.GoModDigest {
		t.Fatal("digests not stable across reads")
	}
	t.Logf("go_mod_digest=%s tree_digest=%s dir=%s", m.GoModDigest, m.TreeDigest, m.ContentDir)

	// Changing file content changes tree digest.
	populateCache(t, root, "example.com/b", "v0.9.0", "module example.com/b\n",
		map[string]string{"x.go": "1"})
	b1, _ := c.Module(context.Background(), ModuleRef{"example.com/b", "v0.9.0"})
	if err := os.WriteFile(filepath.Join(b1.ContentDir, "x.go"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	b2, _ := c.Module(context.Background(), ModuleRef{"example.com/b", "v0.9.0"})
	if b1.TreeDigest == b2.TreeDigest {
		t.Fatal("tree digest must change when file content changes")
	}
	t.Logf("tampered content detected: %s -> %s", b1.TreeDigest, b2.TreeDigest)
}

func TestMissingClassifications(t *testing.T) {
	root := t.TempDir()
	c, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := c.Module(ctx, ModuleRef{"missing.com/m", "v1.0.0"}); err == nil {
		t.Fatal("expected missing module error")
	} else {
		ce, _ := AsError(err)
		if ce.Code != CacheErrMissingModule {
			t.Fatalf("want missing-module, got %v", ce.Code)
		}
		t.Logf("missing module classified: %v", err)
	}

	// Directory exists but has no go.mod at all -> missing go.mod.
	dir := filepath.Join(root, "missingmod.com", "m@v1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Module(ctx, ModuleRef{"missingmod.com/m", "v1.0.0"}); err == nil {
		t.Fatal("expected missing go.mod error")
	} else {
		ce, _ := AsError(err)
		if ce.Code != CacheErrMissingGoMod {
			t.Fatalf("want missing-go-mod, got %v (%v)", ce.Code, err)
		}
		if !errors.Is(err, &Error{Code: CacheErrMissingGoMod}) {
			t.Fatal("errors.Is classification failed")
		}
		t.Logf("missing go.mod classified: %v", err)
	}

	if _, err := New(""); err == nil {
		t.Fatal("expected bad root error")
	}
}

func TestLocalModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module local/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LocalModule(ModuleRef{"local/x", "v0.0.0"}, dir)
	if err != nil || !m.IsLocalReplacement {
		t.Fatalf("local module: %v %+v", err, m)
	}
	t.Logf("local module digests: %s %s", m.GoModDigest, m.TreeDigest)
}
