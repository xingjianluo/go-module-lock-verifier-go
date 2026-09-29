// Package graph resolves the effective module set of a manifest against a
// read-only local module cache: it expands transitive requirements from cached
// go.mod files, applies replace and exclude rules, performs simplified
// minimum-version selection, and surfaces version conflicts as distinguishable
// errors together with introducer chains.
package graph

import (
	"context"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/cache"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
)

// SelectedModule is one module that survives version selection.
type SelectedModule struct {
	Path       string
	Version    string
	IntroducedBy []string // chain from the main module; main module path first edge parent
	Direct     bool
	Replaced   bool
	ReplaceKey string // "path@version" of the replacement source when replaced
	Local      bool
	LocalDir   string
	Excluded   bool // true if a selected (non-replaced) version is excluded
}

// ErrorCode enumerates distinguishable resolution failures.
type ErrorCode int

const (
	GraphErrNone ErrorCode = iota
	GraphErrMissingDependency
	GraphErrVersionConflict
	GraphErrBadDependencyManifest
	GraphErrReplaceTargetMissing
)

// Error carries a distinguishable code plus module context.
type Error struct {
	Code   ErrorCode
	Module cache.ModuleRef
	Detail string
}

func (e *Error) Error() string { return "" }

// Resolver performs offline graph resolution.
type Resolver struct {
	Cache cache.Cache
}

// Result is the stable, sorted resolution outcome.
type Result struct {
	Modules []*SelectedModule
	MainModule string
}

// Resolve runs offline resolution for m. Errors carry distinguishable codes;
// the result is sorted by module path so repeated runs are identical.
func (r *Resolver) Resolve(ctx context.Context, m *manifest.Manifest) (*Result, error) {
	return nil, nil
}
