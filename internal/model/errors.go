package model

import "fmt"

// RejectKind classifies why a manifest entry was rejected.
// Each constant is a mutually distinguishable rejection reason.
type RejectKind string

const (
	// RejectMalformedVersion means the version string is not a valid semantic
	// version or canonical version query.
	RejectMalformedVersion RejectKind = "malformed_version"
	// RejectMissingPath means a required module path is empty.
	RejectMissingPath RejectKind = "missing_path"
	// RejectMissingVersion means a requirement is missing its version
	// argument (outside of a replace-to-local-path position).
	RejectMissingVersion RejectKind = "missing_version"
	// RejectIncompleteEntry means the directive has too few arguments to be
	// interpreted at all.
	RejectIncompleteEntry RejectKind = "incomplete_entry"
	// RejectBadDirective means the directive keyword itself is unknown or the
	// line cannot be tokenized.
	RejectBadDirective RejectKind = "bad_directive"
	// RejectDuplicateReplace means two replace rules target the same
	// old@oldversion pair.
	RejectDuplicateReplace RejectKind = "duplicate_replace"
	// RejectReplaceConflict means a rule both looks like a local path and a
	// versioned target, or the new path is missing.
	RejectReplaceConflict RejectKind = "replace_conflict"
	// RejectExcludeNoVersion means an exclude directive omits the version.
	RejectExcludeNoVersion RejectKind = "exclude_without_version"
)

// RejectError describes one manifest entry that could not be accepted.
type RejectError struct {
	Kind      RejectKind
	Line      int
	Directive string
	Detail    string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("line %d: %s: %s", e.Line, e.Kind, e.Detail)
}

// LockErrorKind classifies mismatches detected while comparing a lock file
// against the current declarations or its own integrity data.
type LockErrorKind string

const (
	// LockStale means the manifest changed since the lock was generated: the
	// lock must be regenerated. This is distinct from tampering.
	LockStale LockErrorKind = "lock_stale"
	// LockTampered means the lock's own integrity hash does not match its
	// serialized content, so the file was edited after generation.
	LockTampered LockErrorKind = "lock_tampered"
	// LockHashMismatch means a per-module recorded hash disagrees with the
	// value derived from the same declared inputs.
	LockHashMismatch LockErrorKind = "lock_hash_mismatch"
	// LockMalformed means the lock file cannot be parsed at all.
	LockMalformed LockErrorKind = "lock_malformed"
	// LockVersionUnsupported means FormatVersion is unknown.
	LockVersionUnsupported LockErrorKind = "lock_version_unsupported"
)

// LockError carries the classified mismatch and, where useful, the offending
// module path and expected/actual values.
type LockError struct {
	Kind     LockErrorKind
	Module   string
	Expected string
	Actual   string
	Detail   string
}

func (e *LockError) Error() string {
	s := string(e.Kind)
	if e.Module != "" {
		s += ": " + e.Module
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// VerifyErrorKind classifies failures of an offline verification run.
type VerifyErrorKind string

const (
	// VerifyMissingModule means a needed module is not present in the local
	// module cache.
	VerifyMissingModule VerifyErrorKind = "missing_module"
	// VerifyHashMismatch means the cached module content hash differs from
	// the locked hash.
	VerifyHashMismatch VerifyErrorKind = "hash_mismatch"
	// VerifyVersionConflict means two requirements cannot be satisfied by one
	// selected version.
	VerifyVersionConflict VerifyErrorKind = "version_conflict"
)

// VerifyError is one verification finding.
type VerifyError struct {
	Kind     VerifyErrorKind
	Module   string
	Expected string
	Actual   string
	// IntroducedBy lists the module paths that asked for this module/version.
	IntroducedBy []string
	Detail       string
}

func (e *VerifyError) Error() string {
	s := string(e.Kind)
	if e.Module != "" {
		s += ": " + e.Module
	}
	if e.Expected != "" || e.Actual != "" {
		s += fmt.Sprintf(" (expected %s, actual %s)", e.Expected, e.Actual)
	}
	return s
}

// WorkspaceErrorKind classifies multi-module workspace failures.
type WorkspaceErrorKind string

const (
	// WorkspaceMalformed means the workspace file itself cannot be parsed.
	WorkspaceMalformed WorkspaceErrorKind = "workspace_malformed"
	// WorkspaceMissingModule means a "use" directory has no readable go.mod.
	WorkspaceMissingModule WorkspaceErrorKind = "workspace_missing_module"
	// WorkspaceDuplicateModule means two "use" directories declare the same
	// module path.
	WorkspaceDuplicateModule WorkspaceErrorKind = "workspace_duplicate_module"
	// WorkspaceReplaceConflict means two submodules map the same
	// replace source (path[@version]) to different targets.
	WorkspaceReplaceConflict WorkspaceErrorKind = "workspace_replace_conflict"
)

// WorkspaceConflictEntry records one submodule's contribution to a conflict.
type WorkspaceConflictEntry struct {
	// Module is the submodule's declared module path.
	Module string
	// Dir is the submodule directory (as written in the workspace file).
	Dir string
	// Rule is the conflicting declaration rendered as text.
	Rule string
	// Line is the 1-based line of the declaration inside the submodule's
	// go.mod (0 when not applicable).
	Line int
}

// WorkspaceError is one workspace-level failure. For conflicts, Entries
// lists every contributing submodule so nothing is silently picked.
type WorkspaceError struct {
	Kind    WorkspaceErrorKind
	Module  string // submodule module path, when the error is module-scoped
	Dir     string
	Subject string // e.g. the replace source "old@version" in conflict
	Entries []WorkspaceConflictEntry
	Detail  string
}

func (e *WorkspaceError) Error() string {
	s := string(e.Kind)
	if e.Subject != "" {
		s += ": " + e.Subject
	}
	if e.Module != "" {
		s += ": " + e.Module
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// DiffReport is the human/machine-readable report produced by verification:
// who introduced each module, and expected vs. actual effective values.
type DiffReport struct {
	ModulePath string
	Entries    []DiffEntry
}

// DiffEntry compares the locked view of one module with what verification
// observed in the local cache.
type DiffEntry struct {
	Path         string
	IntroducedBy []string
	LockedVer    string
	ActualVer    string
	LockedHash   string
	ActualHash   string
	Status       string
}
