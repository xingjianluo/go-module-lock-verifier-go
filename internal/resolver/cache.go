package resolver

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Cache is read-only access to a GOMODCACHE-style directory. All hashing is
// local and deterministic; no network access is performed anywhere.
type Cache struct {
	root string
}

// NewCache opens a module cache rooted at root (typically GOMODCACHE).
func NewCache(root string) *Cache { return &Cache{root: root} }

// escapedPath encodes a module path the way the go command lays out the
// cache: each path element is upper-cased and prefixed with "!" when it
// contains uppercase letters.
func escapedPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.ContainsAny(p, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			var b strings.Builder
			for _, r := range p {
				if r >= 'A' && r <= 'Z' {
					b.WriteByte('!')
					b.WriteRune(r + ('a' - 'A'))
				} else {
					b.WriteRune(r)
				}
			}
			parts[i] = b.String()
		}
	}
	return strings.Join(parts, "/")
}

// ModFile returns the path of the cached go.mod for module@version.
func (c *Cache) ModFile(path, version string) string {
	return filepath.Join(c.root, "cache", "download",
		escapedPath(path), "@v", version+".mod")
}

// ZipFile returns the path of the cached module zip.
func (c *Cache) ZipFile(path, version string) string {
	return filepath.Join(c.root, "cache", "download",
		escapedPath(path), "@v", version+".zip")
}

// Dir returns the path of the extracted module directory.
func (c *Cache) Dir(path, version string) string {
	return filepath.Join(c.root, escapedPath(path)+"@"+version)
}

// HasMod reports whether the .mod file exists in the cache.
func (c *Cache) HasMod(path, version string) bool {
	_, err := os.Stat(c.ModFile(path, version))
	return err == nil
}

// ContentHash returns a deterministic "sha256:<hex>" of the module content.
//
// It prefers the canonical module zip; if only the extracted directory is
// present it hashes a stable ordering of relative paths, modes and contents.
// The hash therefore depends only on file content, never on mtimes, inode
// metadata or read order. A missing zip and missing directory is reported by
// ok=false.
func (c *Cache) ContentHash(path, version string) (hash string, ok bool) {
	if zp := c.ZipFile(path, version); fileExists(zp) {
		return hashZip(zp)
	}
	dir := c.Dir(path, version)
	if isDir(dir) {
		return hashDir(dir)
	}
	return "", false
}

func hashZip(path string) (string, bool) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", false
	}
	defer zr.Close()
	names := make([]string, 0, len(zr.File))
	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		f := byName[name]
		h.Write([]byte("file " + name + " mode " + f.Mode().String() + "\n"))
		if f.Mode().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", false
		}
		if _, err := io.Copy(h, rc); err != nil {
			rc.Close()
			return "", false
		}
		rc.Close()
		h.Write([]byte("\n"))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), true
}

func hashDir(root string) (string, bool) {
	type entry struct {
		rel, mode string
		size      int64
	}
	var entries []entry
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		entries = append(entries, entry{filepath.ToSlash(rel), info.Mode().String(), info.Size()})
		return nil
	})
	if err != nil {
		return "", false
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte("file " + e.rel + " mode " + e.mode + "\n"))
		full := filepath.Join(root, filepath.FromSlash(e.rel))
		if !isDir(full) {
			data, err := os.ReadFile(full)
			if err != nil {
				return "", false
			}
			h.Write(data)
			h.Write([]byte("\n"))
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), true
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
