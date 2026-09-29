// Package testutil builds self-contained GOMODCACHE layouts for tests: no
// network access is needed because every module is fabricated locally.
package testutil

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Cache is a fabricated module cache root.
type Cache struct {
	Root string
}

// NewCache creates an empty cache under t.TempDir().
func NewCache(t *testing.T) *Cache {
	t.Helper()
	return &Cache{Root: filepath.Join(t.TempDir(), "gomodcache")}
}

func escapePath(path string) string {
	var b strings.Builder
	for _, seg := range strings.Split(path, "/") {
		if b.Len() > 0 {
			b.WriteByte('/')
		}
		hasUpper := false
		for _, r := range seg {
			if r >= 'A' && r <= 'Z' {
				hasUpper = true
				break
			}
		}
		if !hasUpper {
			b.WriteString(seg)
			continue
		}
		for _, r := range seg {
			if r >= 'A' && r <= 'Z' {
				b.WriteByte('!')
				b.WriteRune(r + ('a' - 'A'))
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// Module describes one fabricated module.
type Module struct {
	Path    string
	Version string
	// ModContent is the module's go.mod content.
	ModContent string
	// Files maps zip-relative file names (without the module prefix) to
	// content; nil publishes only the .mod.
	Files map[string]string
}

// Add publishes a module: its .mod in cache/download and, when Files is set,
// both an extracted directory and a canonical zip.
func (c *Cache) Add(t *testing.T, m Module) {
	t.Helper()
	dl := filepath.Join(c.Root, "cache", "download", escapePath(m.Path), "@v")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatalf("mkdir download: %v", err)
	}
	modFile := filepath.Join(dl, m.Version+".mod")
	if err := os.WriteFile(modFile, []byte(m.ModContent), 0o644); err != nil {
		t.Fatalf("write mod: %v", err)
	}

	dir := filepath.Join(c.Root, escapePath(m.Path)+"@"+m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir module dir: %v", err)
	}
	for name, content := range m.Files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir file dir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(m.ModContent), 0o644); err != nil {
		t.Fatalf("write dir go.mod: %v", err)
	}

	if m.Files != nil {
		zipPath := filepath.Join(dl, m.Version+".zip")
		writeZip(t, zipPath, m)
	}
}

func writeZip(t *testing.T, path string, m Module) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	prefix := m.Path + "@" + m.Version + "/"
	write := func(name, content string) {
		w, err := zw.Create(prefix + name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	write("go.mod", m.ModContent)
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		write(n, m.Files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// WriteFile writes an arbitrary file inside a work directory.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
