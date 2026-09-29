// Package cache provides a read-only view over a Go module cache laid out like
// GOMODCACHE (download cache and extracted module trees). All operations are
// strictly local: a miss is an error rather than a network fetch.
package cache

import "context"

// ModuleRef identifies one resolved module version.
type ModuleRef struct {
	Path    string
	Version string
}

// Module represents a cached module version and exposes its go.mod and its
// on-disk content digest.
type Module struct {
	Ref          ModuleRef
	GoMod        []byte // contents of the cached go.mod
	ContentDir   string // extracted module directory on disk
	GoModDigest  string // "sha256:<hex>" of the go.mod bytes
	TreeDigest   string // "sha256:<hex>" over the sorted file list and content
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

// Error is a cache failure with a distinguishable code.
type Error struct {
	Code  ErrorCode
	Ref   ModuleRef
	Detail string
}

func (e *Error) Error() string { return "" }

// Cache is a read-only local module cache.
type Cache interface {
	// Module loads metadata for a cached module version without fetching.
	Module(ctx context.Context, ref ModuleRef) (*Module, error)
	// Exists reports whether the module version is fully available locally.
	Exists(ctx context.Context, ref ModuleRef) bool
	// Root returns the GOMODCACHE-style root this cache reads from.
	Root() string
}

// New opens the cache at root (a GOMODCACHE layout).
func New(root string) (Cache, error) {
	return nil, nil
}
