// Package manifest parses go.mod files into a single normalized internal model
// covering direct requirements, indirect requirements, replace and exclude
// directives. Malformed entries are never silently skipped: every rejection is
// reported with a distinguishable reason and parsing a file with any rejected
// entry fails as a whole.
package manifest

import "context"

// RequirementKind distinguishes direct and indirect requirements.
type RequirementKind int

const (
	// KindDirect is a requirement without the "// indirect" comment.
	KindDirect RequirementKind = iota
	// KindIndirect is a requirement marked "// indirect".
	KindIndirect
)

// Requirement is one entry of a require block.
type Requirement struct {
	Path     string
	Version  string // raw version string as written
	Kind     RequirementKind
	Line     int    // 1-based line in the source file
	BlockPos int    // 0 for single require; 1-based position inside a block
}

// Replacement describes the right-hand side of a replace directive.
type Replacement struct {
	// NewPath is the replacement module path; empty for same-path version pins.
	NewPath string
	// NewVersion is the replacement version; empty for filesystem replacements.
	NewVersion string
	// LocalDir is set for "=> ./path" style replacements; always absolute after
	// resolution, otherwise empty.
	LocalDir string
}

// ReplaceRule maps an old module path (and optional old version) to a
// Replacement.
type ReplaceRule struct {
	OldPath    string
	OldVersion string // empty means the rule applies to all versions
	New        Replacement
	Line       int
}

// ExcludeRule removes one exact module version from version selection.
type ExcludeRule struct {
	Path    string
	Version string
	Line    int
}

// Manifest is the normalized model of one go.mod file.
type Manifest struct {
	ModulePath string
	GoVersion  string
	Requires   []Requirement
	Replaces   []ReplaceRule
	Excludes   []ExcludeRule
}

// EntryErrorCode enumerates distinguishable rejection reasons for one entry.
type EntryErrorCode int

const (
	EntryErrNone EntryErrorCode = iota
	EntryErrEmptyPath
	EntryErrEmptyVersion
	EntryErrBadVersion
	EntryErrMalformedDirective
	EntryErrDuplicateReplace
	EntryErrDuplicateExclude
	EntryErrInvalidReplace
	EntryErrLocalDirMissing
)

// EntryError describes a rejected entry and why.
type EntryError struct {
	Line  int
	Kind  string // "require" | "replace" | "exclude"
	Code  EntryErrorCode
	Input string
	Detail string
}

func (e *EntryError) Error() string { return "" }

// Errors is the aggregate of all entry errors found in one file.
type Errors []error

func (errs Errors) Error() string { return "" }

// Parser parses go.mod text. The zero value is usable.
type Parser struct {
	// ModuleDir is used to resolve relative local replace directories. When
	// empty they are kept as written.
	ModuleDir string
}

// Parse parses go.mod source and returns the normalized model. All errors are
// returned together; a non-nil error is always an Errors.
func (p *Parser) Parse(ctx context.Context, source []byte) (*Manifest, error) {
	return nil, nil
}

// ParseFile parses the go.mod file at path.
func (p *Parser) ParseFile(ctx context.Context, path string) (*Manifest, error) {
	return nil, nil
}

// CanonicalHash returns a stable hex SHA-256 over the normalized declarations,
// independent of source formatting and directive ordering.
func (m *Manifest) CanonicalHash() string { return "" }
