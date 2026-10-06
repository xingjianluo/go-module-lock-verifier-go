// Package model defines the tool's internal, toolchain-independent
// representation of module manifests, resolution graphs and lock files.
package model

// Requirement is a single module requirement (direct or indirect).
type Requirement struct {
	// Path is the module path being required.
	Path string
	// Version is the raw version selector as written in the manifest.
	Version string
	// Indirect records whether the requirement carried an "// indirect" comment.
	Indirect bool
	// Line is the 1-based source line this requirement appeared on.
	Line int
	// Source, when non-empty, names the submodule (module path) whose
	// manifest contributed this requirement. Single-module manifests leave
	// it empty and the resolver falls back to the main module path.
	Source string
}

// ReplaceRule is a replace directive. OldVersion may be empty to match every
// version of OldPath. NewVersion is empty when the target is a local path.
type ReplaceRule struct {
	OldPath    string
	OldVersion string
	NewPath    string
	NewVersion string
	// Local indicates the replacement target is a filesystem path rather than
	// another module version.
	Local bool
	Line  int
}

// ExcludeRule is an exclude directive pinning a single module version out.
type ExcludeRule struct {
	Path    string
	Version string
	Line    int
}

// Manifest is the normalized content of one go.mod file.
type Manifest struct {
	// ModulePath is the path declared by the module directive.
	ModulePath string
	// GoVersion is the version declared by the go directive.
	GoVersion string
	Requires  []Requirement
	Replaces  []ReplaceRule
	Excludes  []ExcludeRule
}

// LockedModule records the version that finally takes effect for one module.
type LockedModule struct {
	Path    string
	Version string
	// Replacement, when non-nil, records the effective replace rule.
	Replacement *ReplaceRule
	// Indirect is true when the module is not required directly by the main
	// module.
	Indirect bool
	// Hash is the content hash of the module ("sha256:<hex>"). Local
	// replacements carry an empty hash.
	Hash string
	// IntroducedBy lists module paths whose requirements caused this module
	// to be part of the build list (the main module is the manifest path).
	IntroducedBy []string
}

// LockFile is the on-disk lock representation.
type LockFile struct {
	// FormatVersion is the lock format version, currently "1".
	FormatVersion string
	// ModulePath mirrors the manifest's module path.
	ModulePath string
	// ManifestHash is the hash of the normalized manifest; a mismatch means
	// declarations changed without re-locking.
	ManifestHash string
	Modules      []LockedModule
}
