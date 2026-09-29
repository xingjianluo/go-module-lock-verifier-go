// Package manifest parses go.mod files into a single normalized internal model
// covering direct requirements, indirect requirements, replace and exclude
// directives. Malformed entries are never silently skipped: every rejection is
// reported with a distinguishable reason; when any entry is rejected the whole
// parse fails and every rejection is returned together. Normalization orders
// entries deterministically so the same input always yields the same model.
package manifest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/semver"
)

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
	Line     int // 1-based line in the source file
	BlockPos int // 0 for single require; 1-based position inside a block
}

// Replacement describes the right-hand side of a replace directive.
type Replacement struct {
	// NewPath is the replacement module path; empty for same-path version pins.
	NewPath string
	// NewVersion is the replacement version; empty for filesystem replacements.
	NewVersion string
	// LocalDir is set for "=> ./path" style replacements; absolute after
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
	EntryErrBadPath
)

// EntryError describes a rejected entry and why.
type EntryError struct {
	Line     int
	BlockPos int    // 0 for single-line directives
	Kind     string // "require" | "replace" | "exclude"
	Code     EntryErrorCode
	Input    string
	Detail   string
}

func (e *EntryError) Error() string {
	return fmt.Sprintf("go.mod line %d: invalid %s directive %s: %s",
		e.Line, e.Kind, strings.TrimSpace(e.Input), e.message())
}

func (e *EntryError) message() string {
	switch e.Code {
	case EntryErrEmptyPath:
		return "module path is empty"
	case EntryErrEmptyVersion:
		return "version is empty"
	case EntryErrBadVersion:
		return "version is not a canonical semantic version: " + e.Detail
	case EntryErrMalformedDirective:
		return "directive is malformed: " + e.Detail
	case EntryErrDuplicateReplace:
		return "duplicate replace rule for the same path@version"
	case EntryErrDuplicateExclude:
		return "duplicate exclude rule for the same path@version"
	case EntryErrInvalidReplace:
		return "replacement must be a module@version or a local directory: " + e.Detail
	case EntryErrLocalDirMissing:
		return "local replacement directory does not exist: " + e.Detail
	case EntryErrBadPath:
		return "module path is invalid: " + e.Detail
	default:
		return e.Detail
	}
}

// Is matches EntryError values by Code.
func (e *EntryError) Is(target error) bool {
	t, ok := target.(*EntryError)
	return ok && t.Code == e.Code
}

// Errors is the aggregate of all entry errors found in one file.
type Errors []error

func (errs Errors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d invalid declaration(s):\n", len(errs))
	for _, e := range errs {
		b.WriteString("  - ")
		b.WriteString(e.Error())
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// AsEntryErrors extracts the aggregate *EntryError slice.
func AsEntryErrors(err error) (Errors, bool) {
	var errs Errors
	if errors.As(err, &errs) {
		return errs, true
	}
	return nil, false
}

// Parser parses go.mod text. The zero value is usable.
type Parser struct {
	// ModuleDir is used to resolve relative local replace directories. When
	// empty they are kept relative.
	ModuleDir string
	// RequireLocalDirs, when true, rejects replace directives pointing at local
	// directories that do not exist. Defaults to true while parsing from disk.
	RequireLocalDirs bool
}

// Parse parses go.mod source and returns the normalized model. All errors are
// returned together; a non-nil error is always an Errors.
func (p *Parser) Parse(ctx context.Context, source []byte) (*Manifest, error) {
	return p.parse(source)
}

// ParseFile parses the go.mod file at path, resolving relative local replace
// directories against the file's own directory.
func (p *Parser) ParseFile(ctx context.Context, path string) (*Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pp := *p
	if pp.ModuleDir == "" {
		pp.ModuleDir = filepath.Dir(path)
	}
	pp.RequireLocalDirs = true
	return pp.parse(data)
}

type rawLine struct {
	no       int
	text     string // content with comments stripped
	indirect bool
}

func (p *Parser) parse(source []byte) (*Manifest, error) {
	var errs Errors
	m := &Manifest{}

	seenReplace := map[string]int{}
	seenExclude := map[string]int{}

	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var block string // active block directive or ""
	lineNo := 0
	blockPos := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		trimmed := strings.TrimSpace(raw)

		if block != "" {
			if trimmed == ")" {
				block = ""
				blockPos = 0
				continue
			}
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "(") {
				errs = append(errs, &EntryError{Line: lineNo, Kind: block, BlockPos: blockPos,
					Code: EntryErrMalformedDirective, Input: raw,
					Detail: "unexpected '(' inside block"})
				continue
			}
			blockPos++
			p.parseEntry(block, rawLine{no: lineNo, text: trimmed}, blockPos, m, &errs, seenReplace, seenExclude)
			continue
		}

		if trimmed == "" {
			continue
		}

		verb, _, ok := cutVerb(trimmed)
		if !ok {
			errs = append(errs, &EntryError{Line: lineNo, Kind: "syntax",
				Code: EntryErrMalformedDirective, Input: raw,
				Detail: "line is neither a directive nor a block terminator"})
			continue
		}
		// Arguments of single-line directives keep their trailing comments
		// (needed for "// indirect").
		_, argsRaw := splitFirstToken(trimmed)

		switch verb {
		case "module":
			path := strings.TrimSpace(stripComment(argsRaw))
			if !validModulePath(path) {
				errs = append(errs, &EntryError{Line: lineNo, Kind: "module",
					Code: EntryErrBadPath, Input: raw, Detail: path})
				continue
			}
			if m.ModulePath != "" {
				errs = append(errs, &EntryError{Line: lineNo, Kind: "module",
					Code: EntryErrMalformedDirective, Input: raw,
					Detail: "duplicate module directive"})
				continue
			}
			m.ModulePath = path
		case "go":
			m.GoVersion = strings.TrimSpace(stripComment(argsRaw))
		case "require", "replace", "exclude":
			if strings.TrimSpace(stripComment(argsRaw)) == "(" {
				block = verb
				blockPos = 0
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(stripComment(argsRaw)), "(") {
				errs = append(errs, &EntryError{Line: lineNo, Kind: verb,
					Code: EntryErrMalformedDirective, Input: raw,
					Detail: "inline block entries are not supported; use a multi-line block"})
				continue
			}
			p.parseEntry(verb, rawLine{no: lineNo, text: argsRaw}, 0, m, &errs, seenReplace, seenExclude)
		default:
			// Unknown directives (toolchain, retract, etc.) are out of scope
			// and are ignored, but syntactic junk already errored above.
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if block != "" {
		errs = append(errs, &EntryError{Line: lineNo, Kind: block,
			Code: EntryErrMalformedDirective, Input: "",
			Detail: "unterminated block, missing ')'"})
	}

	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool {
			return errorLess(errs[i], errs[j])
		})
		return nil, Errors(errs)
	}

	m.normalize()
	return m, nil
}

func errorLess(a, b error) bool {
	var ea, eb *EntryError
	if !errors.As(a, &ea) || !errors.As(b, &eb) {
		return a.Error() < b.Error()
	}
	if ea.Line != eb.Line {
		return ea.Line < eb.Line
	}
	if ea.BlockPos != 0 && eb.BlockPos != 0 && ea.BlockPos != eb.BlockPos {
		return ea.BlockPos < eb.BlockPos
	}
	return ea.Input < eb.Input
}

// stripComment removes a trailing // comment, remembering whether it carried
// the "indirect" marker. The marker is returned through the helper below.
func stripComment(line string) string {
	i := strings.Index(line, "//")
	if i < 0 {
		return line
	}
	return line[:i]
}

func isIndirect(line string) bool {
	i := strings.Index(line, "//")
	if i < 0 {
		return false
	}
	comment := strings.TrimSpace(line[i+2:])
	return comment == "indirect" || strings.HasPrefix(comment, "indirect ")
}

// splitFirstToken splits off the first whitespace-delimited token. Unlike
// cutVerb it does not strip comments from the remainder.
func splitFirstToken(line string) (first, rest string) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}

func cutVerb(line string) (verb, args string, ok bool) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, "", true
	}
	return line[:i], strings.TrimSpace(line[i+1:]), true
}

func (p *Parser) parseEntry(verb string, ln rawLine, blockPos int, m *Manifest,
	errs *Errors, seenReplace, seenExclude map[string]int) {
	raw := ln.text
	full := raw
	indirect := isIndirect(full)
	if i := strings.Index(full, "//"); i >= 0 {
		full = strings.TrimSpace(full[:i])
	}
	switch verb {
	case "require":
		p.parseRequire(ln.no, blockPos, full, indirect, m, errs)
	case "replace":
		p.parseReplace(ln.no, full, m, errs, seenReplace)
	case "exclude":
		p.parseExclude(ln.no, full, m, errs, seenExclude)
	}
}

func (p *Parser) parseRequire(line, pos int, text string, indirect bool,
	m *Manifest, errs *Errors) {
	fields := strings.Fields(text)
	kind := KindDirect
	if indirect {
		kind = KindIndirect
	}
	if len(fields) < 2 {
		code := EntryErrMalformedDirective
		detail := "expected 'path version'"
		if len(fields) == 1 && fields[0] != "" {
			if !validModulePath(fields[0]) {
				*errs = append(*errs, &EntryError{Line: line, Kind: "require",
					Code: EntryErrBadPath, Input: text, Detail: fields[0]})
				return
			}
			code = EntryErrEmptyVersion
			detail = ""
		}
		*errs = append(*errs, &EntryError{Line: line, Kind: "require",
			Code: code, Input: text, Detail: detail})
		return
	}
	if len(fields) > 2 {
		*errs = append(*errs, &EntryError{Line: line, Kind: "require",
			Code: EntryErrMalformedDirective, Input: text,
			Detail: fmt.Sprintf("unexpected extra token %q", fields[2])})
		return
	}
	path, version := fields[0], fields[1]
	if !validModulePath(path) {
		*errs = append(*errs, &EntryError{Line: line, Kind: "require",
			Code: EntryErrBadPath, Input: text, Detail: path})
		return
	}
	if _, err := semver.Parse(version); err != nil {
		*errs = append(*errs, &EntryError{Line: line, Kind: "require",
			Code: EntryErrBadVersion, Input: text, Detail: err.Error()})
		return
	}
	m.Requires = append(m.Requires, Requirement{
		Path: path, Version: version, Kind: kind, Line: line, BlockPos: pos,
	})
}

func splitModuleVersion(token string) (path, version string, hadAt bool) {
	i := strings.LastIndex(token, "@")
	if i < 0 {
		return token, "", false
	}
	return token[:i], token[i+1:], true
}

func (p *Parser) parseReplace(line int, text string, m *Manifest,
	errs *Errors, seen map[string]int) {
	left, right, ok := strings.Cut(text, "=>")
	if !ok {
		*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
			Code: EntryErrMalformedDirective, Input: text,
			Detail: "missing '=>'"})
		return
	}
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" {
		*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
			Code: EntryErrEmptyPath, Input: text, Detail: ""})
		return
	}
	lFields := strings.Fields(left)
	if len(lFields) > 2 {
		*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
			Code: EntryErrMalformedDirective, Input: text,
			Detail: "left side must be 'path' or 'path version'"})
		return
	}
	oldPath := lFields[0]
	oldVersion := ""
	if len(lFields) == 2 {
		oldVersion = lFields[1]
	}
	if !validModulePath(oldPath) {
		*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
			Code: EntryErrBadPath, Input: text, Detail: oldPath})
		return
	}
	if oldVersion != "" {
		if _, err := semver.Parse(oldVersion); err != nil {
			*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
				Code: EntryErrBadVersion, Input: text, Detail: err.Error()})
			return
		}
	}
	if right == "" {
		*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
			Code: EntryErrInvalidReplace, Input: text,
			Detail: "right side is empty"})
		return
	}

	r := Replacement{}
	if strings.HasPrefix(right, "./") || strings.HasPrefix(right, "../") || right == "." || right == ".." || filepath.IsAbs(right) {
		// Filesystem replacement: no version may be present.
		if strings.ContainsAny(right, " \t") {
			*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
				Code: EntryErrInvalidReplace, Input: text,
				Detail: "local directory replacement cannot carry a version"})
			return
		}
		dir := right
		if p.ModuleDir != "" && !filepath.IsAbs(dir) {
			dir = filepath.Join(p.ModuleDir, dir)
		}
		dir = filepath.Clean(dir)
		if p.RequireLocalDirs {
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
					Code: EntryErrLocalDirMissing, Input: text, Detail: dir})
				return
			}
		}
		r.LocalDir = dir
	} else {
		rFields := strings.Fields(right)
		if len(rFields) != 2 {
			*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
				Code: EntryErrInvalidReplace, Input: text,
				Detail: "module replacement must be 'path version'"})
			return
		}
		newPath, newVersion := rFields[0], rFields[1]
		if !validModulePath(newPath) {
			*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
				Code: EntryErrBadPath, Input: text, Detail: newPath})
			return
		}
		if _, err := semver.Parse(newVersion); err != nil {
			*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
				Code: EntryErrBadVersion, Input: text, Detail: err.Error()})
			return
		}
		r.NewPath = newPath
		r.NewVersion = newVersion
	}

	key := oldPath + "@" + oldVersion
	if prev, dup := seen[key]; dup {
		_ = prev
		*errs = append(*errs, &EntryError{Line: line, Kind: "replace",
			Code: EntryErrDuplicateReplace, Input: text,
			Detail: fmt.Sprintf("rule for %s already declared", key)})
		return
	}
	seen[key] = line
	m.Replaces = append(m.Replaces, ReplaceRule{
		OldPath: oldPath, OldVersion: oldVersion, New: r, Line: line,
	})
}

func (p *Parser) parseExclude(line int, text string, m *Manifest,
	errs *Errors, seen map[string]int) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		*errs = append(*errs, &EntryError{Line: line, Kind: "exclude",
			Code: EntryErrMalformedDirective, Input: text,
			Detail: "expected 'path version'"})
		return
	}
	if len(fields) > 2 {
		*errs = append(*errs, &EntryError{Line: line, Kind: "exclude",
			Code: EntryErrMalformedDirective, Input: text,
			Detail: fmt.Sprintf("unexpected extra token %q", fields[2])})
		return
	}
	path, version := fields[0], fields[1]
	if !validModulePath(path) {
		*errs = append(*errs, &EntryError{Line: line, Kind: "exclude",
			Code: EntryErrBadPath, Input: text, Detail: path})
		return
	}
	if _, err := semver.Parse(version); err != nil {
		*errs = append(*errs, &EntryError{Line: line, Kind: "exclude",
			Code: EntryErrBadVersion, Input: text, Detail: err.Error()})
		return
	}
	key := path + "@" + version
	if _, dup := seen[key]; dup {
		*errs = append(*errs, &EntryError{Line: line, Kind: "exclude",
			Code: EntryErrDuplicateExclude, Input: text,
			Detail: fmt.Sprintf("rule for %s already declared", key)})
		return
	}
	seen[key] = line
	m.Excludes = append(m.Excludes, ExcludeRule{Path: path, Version: version, Line: line})
}

// validModulePath applies the conservative path rules relevant here:
// non-empty, no whitespace or control characters, valid dot-separated elements.
func validModulePath(p string) bool {
	if p == "" {
		return false
	}
	if strings.ContainsAny(p, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return false
	}
	if p == "." || p == ".." || strings.Contains(p, "//") || strings.Contains(p, "/./") || strings.Contains(p, "/../") {
		return false
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == "" || elem == "." || elem == ".." {
			return false
		}
		// A leading domain-style element must contain a dot, but we do not
		// enforce DNS rules; reject control characters and quotes only.
		for _, r := range elem {
			if r < 0x20 || r == 0x7f || r == '"' || r == '\'' {
				return false
			}
		}
	}
	return true
}

// normalize orders every collection deterministically. The same declarations
// always produce the same normalized manifest regardless of source ordering.
func (m *Manifest) normalize() {
	sort.SliceStable(m.Requires, func(i, j int) bool {
		a, b := m.Requires[i], m.Requires[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Version < b.Version
	})
	sort.SliceStable(m.Replaces, func(i, j int) bool {
		a, b := m.Replaces[i], m.Replaces[j]
		if a.OldPath != b.OldPath {
			return a.OldPath < b.OldPath
		}
		return a.OldVersion < b.OldVersion
	})
	sort.SliceStable(m.Excludes, func(i, j int) bool {
		a, b := m.Excludes[i], m.Excludes[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Version < b.Version
	})
	if m.Requires == nil {
		m.Requires = []Requirement{}
	}
	if m.Replaces == nil {
		m.Replaces = []ReplaceRule{}
	}
	if m.Excludes == nil {
		m.Excludes = []ExcludeRule{}
	}
}

// CanonicalHash returns a stable hex SHA-256 over the normalized declarations,
// independent of source formatting, comments and directive ordering.
func (m *Manifest) CanonicalHash() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "module %s\n", m.ModulePath)
	fmt.Fprintf(&b, "go %s\n", m.GoVersion)
	for _, r := range m.Requires {
		kind := "direct"
		if r.Kind == KindIndirect {
			kind = "indirect"
		}
		fmt.Fprintf(&b, "require %s %s %s\n", r.Path, r.Version, kind)
	}
	for _, r := range m.Replaces {
		switch {
		case r.New.LocalDir != "":
			fmt.Fprintf(&b, "replace %s@%s => local %s\n", r.OldPath, nz(r.OldVersion), r.New.LocalDir)
		default:
			fmt.Fprintf(&b, "replace %s@%s => %s@%s\n",
				r.OldPath, nz(r.OldVersion), r.New.NewPath, r.New.NewVersion)
		}
	}
	for _, e := range m.Excludes {
		fmt.Fprintf(&b, "exclude %s %s\n", e.Path, e.Version)
	}
	sum := sha256.Sum256(b.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:])
}

func nz(s string) string {
	if s == "" {
		return "*"
	}
	return s
}
