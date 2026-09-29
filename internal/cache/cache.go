// Package cache provides a read-only view over a Go module cache laid out like
// GOMODCACHE (download cache plus extracted module trees). All operations are
// strictly local: a miss is a classified error rather than a network fetch.
//
// Two digests are produced for every module:
//
//	go_mod_digest = sha256 over the bytes of the module's go.mod
//	tree_digest   = sha256 over a stable, sorted listing of the extracted tree
//	                ("<relative posix path>\n<file sha256>\n" per regular file)
//
// These digests are intentionally independent of Go's own ziphash encoding but
// are sufficient to detect any change of locked content offline.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModuleRef identifies one resolved module version.
type ModuleRef struct {
	Path    string
	Version string
}

func (r ModuleRef) String() string { return r.Path + "@" + r.Version }

// Module represents a cached module version and exposes its go.mod and its
// on-disk content digests.
type Module struct {
	Ref                ModuleRef
	GoMod              []byte // contents of the cached go.mod
	ContentDir         string // extracted module directory on disk
	GoModDigest        string // "sha256:<hex>" of the go.mod bytes
	TreeDigest         string // "sha256:<hex>" over the sorted file listing
	IsLocalReplacement bool
}

// ErrorCode enumerates distinguishable cache failures.
type ErrorCode int

const (
	CacheErrNone ErrorCode = iota
	CacheErrMissingModule
	CacheErrMissingGoMod
	CacheErrUnreadable
	CacheErrBadPath
)

func (c ErrorCode) String() string {
	switch c {
	case CacheErrMissingModule:
		return "module not present in local cache"
	case CacheErrMissingGoMod:
		return "cached module is missing go.mod"
	case CacheErrUnreadable:
		return "cached module content is unreadable"
	case CacheErrBadPath:
		return "invalid module path or version"
	default:
		return "ok"
	}
}

// Error is a cache failure with a distinguishable code.
type Error struct {
	Code   ErrorCode
	Ref    ModuleRef
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("cache: %s (%s): %s", e.Code, e.Ref, e.Detail)
}

// Is matches cache errors by Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// AsError extracts a *Error.
func AsError(err error) (*Error, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// Cache is a read-only local module cache.
type Cache interface {
	// Module loads metadata for a cached module version without fetching.
	Module(ctx context.Context, ref ModuleRef) (*Module, error)
	// Exists reports whether the module version is fully available locally
	// (extracted tree and go.mod).
	Exists(ctx context.Context, ref ModuleRef) bool
	// Root returns the GOMODCACHE-style root this cache reads from.
	Root() string
}

type fileCache struct {
	root string
}

// New opens the cache at root (a GOMODCACHE layout).
func New(root string) (Cache, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, &Error{Code: CacheErrBadPath, Detail: "empty cache root"}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, &Error{Code: CacheErrBadPath, Detail: err.Error()}
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, &Error{Code: CacheErrBadPath, Detail: "cache root is not a directory: " + abs}
	}
	return &fileCache{root: abs}, nil
}

func (c *fileCache) Root() string { return c.root }

// downloadPath encodes a module path the way the go command stores download
// artifacts: each uppercase ASCII letter is replaced by "!" plus lowercase.
func downloadPath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *fileCache) modFile(ref ModuleRef) string {
	return filepath.Join(c.root, "cache", "download",
		filepath.FromSlash(downloadPath(ref.Path)), "@v", ref.Version+".mod")
}

func (c *fileCache) treeDir(ref ModuleRef) string {
	return filepath.Join(c.root,
		filepath.FromSlash(downloadPath(ref.Path))+"@"+ref.Version)
}

func (c *fileCache) Exists(ctx context.Context, ref ModuleRef) bool {
	if err := validateRef(ref); err != nil {
		return false
	}
	if _, err := os.Stat(c.treeDir(ref)); err != nil {
		return false
	}
	if _, err := os.Stat(c.modFile(ref)); err != nil {
		// Extracted tree can itself carry go.mod even without a .mod artifact.
		if _, err2 := os.Stat(filepath.Join(c.treeDir(ref), "go.mod")); err2 != nil {
			return false
		}
	}
	return true
}

func validateRef(ref ModuleRef) error {
	if ref.Path == "" || ref.Version == "" {
		return &Error{Code: CacheErrBadPath, Ref: ref, Detail: "path and version are required"}
	}
	if strings.ContainsAny(ref.Path, "@\x00") || filepath.IsAbs(ref.Path) {
		return &Error{Code: CacheErrBadPath, Ref: ref, Detail: "illegal module path"}
	}
	return nil
}

func (c *fileCache) Module(ctx context.Context, ref ModuleRef) (*Module, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	dir := c.treeDir(ref)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, &Error{Code: CacheErrMissingModule, Ref: ref,
			Detail: "extracted directory absent: " + dir}
	}

	// Prefer the go.mod inside the extracted tree; fall back to the download
	// cache .mod artifact (present when only the go.mod was ever fetched).
	goModBytes, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		goModBytes, err = os.ReadFile(c.modFile(ref))
		if err != nil {
			return nil, &Error{Code: CacheErrMissingGoMod, Ref: ref,
				Detail: "neither tree go.mod nor download .mod found"}
		}
	}

	treeDigest, err := DigestTree(ctx, dir)
	if err != nil {
		return nil, &Error{Code: CacheErrUnreadable, Ref: ref, Detail: err.Error()}
	}
	sum := sha256.Sum256(goModBytes)
	return &Module{
		Ref:         ref,
		GoMod:       goModBytes,
		ContentDir:  dir,
		GoModDigest: "sha256:" + hex.EncodeToString(sum[:]),
		TreeDigest:  treeDigest,
	}, nil
}

// DigestTree computes the stable tree digest of a directory: every regular
// file contributes its slash-separated relative path and its own SHA-256, in
// sorted path order. Directories and symlinks are listed for names but symlink
// targets are not followed.
func DigestTree(ctx context.Context, root string) (string, error) {
	type item struct {
		rel    string
		digest string
	}
	var items []item
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			items = append(items, item{rel: rel, digest: "symlink"})
			return nil
		}
		if !d.Type().IsRegular() {
			items = append(items, item{rel: rel, digest: "special"})
			return nil
		}
		h, herr := hashFile(path)
		if herr != nil {
			return herr
		}
		items = append(items, item{rel: rel, digest: h})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	h := sha256.New()
	for _, it := range items {
		io.WriteString(h, it.rel)
		h.Write([]byte{0})
		io.WriteString(h, it.digest)
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// DigestBytes returns the "sha256:<hex>" digest of data.
func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// LocalModule builds a Module view for a filesystem replacement directory.
// Its tree digest is computed exactly like a cached module tree.
func LocalModule(ref ModuleRef, dir string) (*Module, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, &Error{Code: CacheErrMissingModule, Ref: ref,
			Detail: "local replacement directory absent: " + dir}
	}
	goModBytes, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil, &Error{Code: CacheErrMissingGoMod, Ref: ref,
			Detail: "local replacement has no go.mod: " + dir}
	}
	treeDigest, err := DigestTree(context.Background(), dir)
	if err != nil {
		return nil, &Error{Code: CacheErrUnreadable, Ref: ref, Detail: err.Error()}
	}
	sum := sha256.Sum256(goModBytes)
	return &Module{
		Ref:                ref,
		GoMod:              goModBytes,
		ContentDir:         dir,
		GoModDigest:        "sha256:" + hex.EncodeToString(sum[:]),
		TreeDigest:         treeDigest,
		IsLocalReplacement: true,
	}, nil
}
