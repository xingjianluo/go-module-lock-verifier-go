// Package lockfile defines the on-disk lock model, its stable serialization
// and its integrity protection. A lock records the effective version and
// verification digests of every dependency, traces how each dependency was
// introduced, a hash of the normalized declarations (to detect stale locks) and
// a self-hash covering the whole document (to detect tampering).
package lockfile

// FormatVersion is the lock format emitted by this implementation.
const FormatVersion = "gmlv1"

// Entry is one locked dependency.
type Entry struct {
	Path          string   `json:"path"`
	Version       string   `json:"version"`
	Direct        bool     `json:"direct"`
	Replaced      bool     `json:"replaced"`
	ReplaceSource string   `json:"replace_source,omitempty"`
	LocalDir      string   `json:"local_dir,omitempty"`
	GoModDigest   string   `json:"go_mod_digest"`
	TreeDigest    string   `json:"tree_digest"`
	IntroducedBy  []string `json:"introduced_by"`
}

// Lock is the on-disk lock document. Entries are always stored sorted by path.
type Lock struct {
	FormatVersion string  `json:"format_version"`
	MainModule    string  `json:"main_module"`
	ManifestHash  string  `json:"manifest_hash"` // normalized declarations hash
	Entries       []Entry `json:"entries"`
	LockHash      string  `json:"lock_hash"`     // integrity hash over all fields above
}

// ErrorCode enumerates distinguishable lock failures.
type ErrorCode int

const (
	LockErrNone ErrorCode = iota
	LockErrStale
	LockErrTampered
	LockErrMalformed
	LockErrDigestMismatch
	LockErrDuplicateEntry
)

// Error carries a distinguishable code and a human-readable Detail.
type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string { return "" }

// Marshal serializes a lock deterministically: it sorts entries, rejects
// duplicate paths and recomputes ManifestHash/LockHash.
func Marshal(l *Lock) ([]byte, error) { return nil, nil }

// Unmarshal parses and structurally validates a lock document without checking
// it against anything.
func Unmarshal(data []byte) (*Lock, error) { return nil, nil }

// SelfHash recomputes the integrity hash of all fields excluding LockHash.
func (l *Lock) SelfHash() string { return "" }

// VerifyIntegrity checks the stored LockHash. It reports LockErrTampered on
// mismatch.
func (l *Lock) VerifyIntegrity() error { return nil }
