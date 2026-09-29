// Package semver parses and compares canonical semantic versions of the form
// vMAJOR.MINOR.PATCH with optional "-prerelease" and "+incompatible" suffixes,
// as used by Go modules for resolved module versions.
package semver

import (
	"errors"
	"strconv"
	"strings"
)

// Version is a parsed canonical semantic version.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string // prerelease identifiers without leading "-", e.g. "rc.1"
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

func (c ErrorCode) String() string {
	switch c {
	case ErrCodeEmpty:
		return "empty version"
	case ErrCodeMissingVPrefix:
		return "missing leading v"
	case ErrCodeBadNumber:
		return "malformed numeric component"
	case ErrCodeBadSuffix:
		return "malformed prerelease/build suffix"
	default:
		return "ok"
	}
}

// VersionError describes why a single version string was rejected.
type VersionError struct {
	Input string
	Code  ErrorCode
}

func (e *VersionError) Error() string {
	return "invalid module version " + strconv.Quote(e.Input) + ": " + e.Code.String()
}

// Is lets errors.Is match VersionError values by Code.
func (e *VersionError) Is(target error) bool {
	t, ok := target.(*VersionError)
	return ok && t.Code == e.Code
}

func bad(input string, code ErrorCode) error {
	return &VersionError{Input: input, Code: code}
}

// Parse parses a canonical semantic version. It returns a *VersionError whose
// Code distinguishes the rejection reason.
func Parse(s string) (*Version, error) {
	if s == "" {
		return nil, bad(s, ErrCodeEmpty)
	}
	if s[0] != 'v' {
		return nil, bad(s, ErrCodeMissingVPrefix)
	}
	rest := s[1:]
	incompat := false
	if strings.HasSuffix(rest, "+incompatible") {
		incompat = true
		rest = strings.TrimSuffix(rest, "+incompatible")
		// v1.2.3+incompatible is fine; anything else ending this way is not.
		if rest == "" {
			return nil, bad(s, ErrCodeBadSuffix)
		}
	} else if i := strings.IndexByte(rest, '+'); i >= 0 {
		// No other build metadata is accepted in canonical module versions.
		return nil, bad(s, ErrCodeBadSuffix)
	}

	var prerelease string
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		prerelease = rest[i+1:]
		rest = rest[:i]
		if !validPrerelease(prerelease) {
			return nil, bad(s, ErrCodeBadSuffix)
		}
	}

	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return nil, bad(s, ErrCodeBadNumber)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" {
			return nil, bad(s, ErrCodeBadNumber)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, bad(s, ErrCodeBadNumber)
		}
		// Canonical form forbids leading zeroes.
		if len(p) > 1 && p[0] == '0' {
			return nil, bad(s, ErrCodeBadNumber)
		}
		nums[i] = n
	}
	return &Version{
		Major:      nums[0],
		Minor:      nums[1],
		Patch:      nums[2],
		Prerelease: prerelease,
		Incompat:   incompat,
		Raw:        s,
	}, nil
}

func validPrerelease(p string) bool {
	if p == "" {
		return false
	}
	for _, ident := range strings.Split(p, ".") {
		if ident == "" {
			return false
		}
		numeric := true
		for _, r := range ident {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') || r == '-') {
				return false
			}
			if r < '0' || r > '9' {
				numeric = false
			}
		}
		// Numeric identifiers must not have leading zeroes.
		if numeric && len(ident) > 1 && ident[0] == '0' {
			return false
		}
	}
	return true
}

// IsValid reports whether s is a canonical semantic version.
func IsValid(s string) bool {
	_, err := Parse(s)
	return err == nil
}

// Compare returns -1, 0 or 1 comparing x against y by semantic precedence.
// The "+incompatible" flag does not participate in precedence.
func Compare(x, y *Version) int {
	if c := cmpInt(x.Major, y.Major); c != 0 {
		return c
	}
	if c := cmpInt(x.Minor, y.Minor); c != 0 {
		return c
	}
	if c := cmpInt(x.Patch, y.Patch); c != 0 {
		return c
	}
	return comparePrerelease(x.Prerelease, y.Prerelease)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePrerelease follows semver precedence: absent prerelease > present;
// otherwise identifiers compare numerically or lexicographically, shorter
// fields lose when they are a prefix.
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1 // release beats prerelease
	}
	if b == "" {
		return -1
	}
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		xNum, yNum := xerr == nil && allDigits(x), yerr == nil && allDigits(y)
		switch {
		case xNum && yNum:
			if c := cmpInt(xn, yn); c != 0 {
				return c
			}
		case xNum != yNum:
			if xNum {
				return -1 // numeric identifiers have lower precedence
			}
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(as), len(bs))
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// String renders the canonical version string.
func (v *Version) String() string {
	if v == nil {
		return ""
	}
	if v.Raw != "" {
		return v.Raw
	}
	var b strings.Builder
	b.WriteByte('v')
	b.WriteString(strconv.Itoa(v.Major))
	b.WriteByte('.')
	b.WriteString(strconv.Itoa(v.Minor))
	b.WriteByte('.')
	b.WriteString(strconv.Itoa(v.Patch))
	if v.Prerelease != "" {
		b.WriteByte('-')
		b.WriteString(v.Prerelease)
	}
	if v.Incompat {
		b.WriteString("+incompatible")
	}
	return b.String()
}

// AsVersionError extracts a *VersionError from err.
func AsVersionError(err error) (*VersionError, bool) {
	var ve *VersionError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
