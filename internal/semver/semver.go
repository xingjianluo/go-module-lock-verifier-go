// Package semver parses and compares canonical semantic versions of the form
// vMAJOR.MINOR.PATCH with an optional "+incompatible" suffix, as used by Go
// modules for resolved module versions.
package semver

// Version is a parsed canonical semantic version.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string // includes leading "-" when non-empty
	Incompat   bool   // "+incompatible" suffix present
	Raw        string // original canonical string
}

// ErrorCode enumerates the distinguishable reasons a version string is rejected.
type ErrorCode int

const (
	// ErrCodeNone is the zero value; no error.
	ErrCodeNone ErrorCode = iota
	// ErrCodeEmpty means the version string is empty.
	ErrCodeEmpty
	// ErrCodeMissingVPrefix means the version does not start with "v".
	ErrCodeMissingVPrefix
	// ErrCodeBadNumber means a numeric component is missing or non-numeric.
	ErrCodeBadNumber
	// ErrCodeBadSuffix means a prerelease / build suffix is malformed.
	ErrCodeBadSuffix
)

// VersionError describes why a single version string was rejected.
type VersionError struct {
	Input string
	Code  ErrorCode
}

func (e *VersionError) Error() string { return "" }

// Parse parses a canonical semantic version. It returns a *VersionError whose
// Code distinguishes the rejection reason.
func Parse(s string) (*Version, error) {
	return nil, nil
}

// Compare returns -1, 0 or 1 comparing x against y by semantic precedence.
func Compare(x, y *Version) int { return 0 }

// IsValid reports whether s is a canonical semantic version.
func IsValid(s string) bool { return false }

// String renders the canonical version string.
func (v *Version) String() string { return "" }
