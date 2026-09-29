// Package lockfile defines the on-disk lock model, its stable serialization
// and its integrity protection. A lock records the effective version and
// verification digests of every dependency, traces how each dependency was
// introduced, carries a hash of the normalized declarations (to detect stale
// locks) and a self-hash covering the whole document (to detect tampering).
//
// The serialized form is UTF-8 JSON with two-space indentation. Integrity hashes
// are computed over a separate canonical text rendering, so the hash is
// independent of JSON whitespace and field ordering.
package lockfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FormatVersion is the lock format emitted by this implementation.
const FormatVersion = "gmlv1"

// Entry is one locked dependency.
type Entry struct {
	Path          string `json:"path"`
	Version       string `json:"version"`
	Direct        bool   `json:"direct"`
	Replaced      bool   `json:"replaced"`
	ReplaceSource string `json:"replace_source,omitempty"`
	// ReplacePath is the effective module path served from the cache when the
	// replacement redirects to another module (path-only version pins keep the
	// same path so this stays empty).
	ReplacePath  string   `json:"replace_path,omitempty"`
	LocalDir     string   `json:"local_dir,omitempty"`
	GoModDigest  string   `json:"go_mod_digest"`
	TreeDigest   string   `json:"tree_digest"`
	IntroducedBy []string `json:"introduced_by"`
}

// Lock is the on-disk lock document. Entries are always stored sorted by path.
type Lock struct {
	FormatVersion string  `json:"format_version"`
	MainModule    string  `json:"main_module"`
	ManifestHash  string  `json:"manifest_hash"` // normalized declarations hash
	Entries       []Entry `json:"entries"`
	LockHash      string  `json:"lock_hash"` // integrity hash over all fields above
}

// ErrorCode enumerates distinguishable lock failures.
type ErrorCode int

const (
	LockErrNone ErrorCode = iota
	// LockErrStale: declarations changed and the lock was not regenerated.
	LockErrStale
	// LockErrTampered: lock body changed but ManifestHash still matches.
	LockErrTampered
	// LockErrMalformed: structurally invalid document.
	LockErrMalformed
	// LockErrDigestMismatch: a recorded digest is malformed or does not match.
	LockErrDigestMismatch
	// LockErrDuplicateEntry: two entries lock the same path.
	LockErrDuplicateEntry
)

func (c ErrorCode) String() string {
	switch c {
	case LockErrStale:
		return "lock is stale: declarations changed"
	case LockErrTampered:
		return "lock is tampered: integrity hash mismatch"
	case LockErrMalformed:
		return "lock file is malformed"
	case LockErrDigestMismatch:
		return "locked digest mismatch"
	case LockErrDuplicateEntry:
		return "duplicate lock entry"
	default:
		return "ok"
	}
}

// Error carries a distinguishable code and a human-readable Detail.
type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("lock: %s: %s", e.Code, e.Detail) }

// Is matches lock errors by Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// AsError extracts a *Error.
func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var le *Error
	if errors.As(err, &le) {
		return le, true
	}
	return nil, false
}

func malformed(format string, args ...any) error {
	return &Error{Code: LockErrMalformed, Detail: fmt.Sprintf(format, args...)}
}

// canonicalRender produces the stable text covered by LockHash. Every string
// is length-prefixed on its own line to avoid ambiguity; entries are sorted.
func canonicalRender(l *Lock) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "format_version\t%d\t%s\n", len(l.FormatVersion), l.FormatVersion)
	fmt.Fprintf(&b, "main_module\t%d\t%s\n", len(l.MainModule), l.MainModule)
	fmt.Fprintf(&b, "manifest_hash\t%d\t%s\n", len(l.ManifestHash), l.ManifestHash)
	entries := append([]Entry(nil), l.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for _, e := range entries {
		fmt.Fprintf(&b, "entry\t%d\t%s\n", len(e.Path), e.Path)
		fmt.Fprintf(&b, "  version\t%d\t%s\n", len(e.Version), e.Version)
		fmt.Fprintf(&b, "  direct\t%t\n", e.Direct)
		fmt.Fprintf(&b, "  replaced\t%t\n", e.Replaced)
		fmt.Fprintf(&b, "  replace_source\t%d\t%s\n", len(e.ReplaceSource), e.ReplaceSource)
		fmt.Fprintf(&b, "  replace_path\t%d\t%s\n", len(e.ReplacePath), e.ReplacePath)
		fmt.Fprintf(&b, "  local_dir\t%d\t%s\n", len(e.LocalDir), e.LocalDir)
		fmt.Fprintf(&b, "  go_mod_digest\t%d\t%s\n", len(e.GoModDigest), e.GoModDigest)
		fmt.Fprintf(&b, "  tree_digest\t%d\t%s\n", len(e.TreeDigest), e.TreeDigest)
		intro := append([]string(nil), e.IntroducedBy...)
		fmt.Fprintf(&b, "  introduced_by_count\t%d\n", len(intro))
		for _, s := range intro {
			fmt.Fprintf(&b, "    chain\t%d\t%s\n", len(s), s)
		}
	}
	fmt.Fprintf(&b, "entry_count\t%d\n", len(entries))
	return b.Bytes()
}

// SelfHash recomputes the integrity hash of all fields excluding LockHash.
func (l *Lock) SelfHash() string {
	sum := sha256.Sum256(canonicalRender(l))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Validate structurally validates a lock without touching the filesystem.
func (l *Lock) Validate() error {
	if err := l.validateExceptSelf(); err != nil {
		return err
	}
	if !looksLikeHash(l.LockHash) {
		return malformed("lock_hash %q is not a sha256 digest", l.LockHash)
	}
	return nil
}

// validateExceptSelf checks everything except the format of LockHash itself;
// used by Marshal right before the self hash is computed.
func (l *Lock) validateExceptSelf() error {
	if l.FormatVersion != FormatVersion {
		return malformed("unsupported format_version %q (want %q)", l.FormatVersion, FormatVersion)
	}
	if l.MainModule == "" {
		return malformed("main_module is empty")
	}
	if !looksLikeHash(l.ManifestHash) {
		return malformed("manifest_hash %q is not a sha256 digest", l.ManifestHash)
	}
	seen := map[string]int{}
	for i, e := range l.Entries {
		if e.Path == "" {
			return malformed("entries[%d]: empty path", i)
		}
		if e.Version == "" {
			return malformed("entries[%s]: empty version", e.Path)
		}
		if !looksLikeHash(e.GoModDigest) {
			return malformed("entries[%s]: malformed go_mod_digest", e.Path)
		}
		if !looksLikeHash(e.TreeDigest) {
			return malformed("entries[%s]: malformed tree_digest", e.Path)
		}
		if prev, ok := seen[e.Path]; ok {
			return &Error{Code: LockErrDuplicateEntry,
				Detail: fmt.Sprintf("%s appears at positions %d and %d", e.Path, prev, i)}
		}
		seen[e.Path] = i
	}
	return nil
}

func looksLikeHash(s string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	hexPart := s[len(prefix):]
	if len(hexPart) != 64 {
		return false
	}
	for _, r := range hexPart {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// VerifyIntegrity validates structure and checks the stored LockHash. It
// reports LockErrTampered when the body was changed without recomputing hash.
func (l *Lock) VerifyIntegrity() error {
	if err := l.Validate(); err != nil {
		return err
	}
	if got := l.SelfHash(); got != l.LockHash {
		return &Error{Code: LockErrTampered,
			Detail: fmt.Sprintf("stored %s but body hashes to %s", l.LockHash, got)}
	}
	return nil
}

// SortEntries orders entries deterministically by path. Introducer chains keep
// their meaningful order and are not sorted.
func (l *Lock) SortEntries() {
	sort.Slice(l.Entries, func(i, j int) bool { return l.Entries[i].Path < l.Entries[j].Path })
}

// Marshal serializes a lock deterministically: it sorts entries, rejects
// duplicates, fills FormatVersion and recomputes LockHash.
func Marshal(l *Lock) ([]byte, error) {
	if l == nil {
		return nil, malformed("nil lock")
	}
	if l.FormatVersion == "" {
		l.FormatVersion = FormatVersion
	}
	l.SortEntries()
	// Structural validation except the self-hash, which we compute next.
	if err := l.validateExceptSelf(); err != nil {
		return nil, err
	}
	l.LockHash = l.SelfHash()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(l); err != nil {
		return nil, malformed("json encoding failed: %s", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal parses and structurally validates a lock document. Integrity is
// *not* checked here; call VerifyIntegrity explicitly.
func Unmarshal(data []byte) (*Lock, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, malformed("empty document")
	}
	var l Lock
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&l); err != nil {
		return nil, malformed("invalid JSON: %s", err)
	}
	if dec.More() {
		return nil, malformed("unexpected trailing content")
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return &l, nil
}
