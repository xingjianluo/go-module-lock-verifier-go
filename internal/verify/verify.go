// Package verify ties manifests, offline resolution, the local module cache and
// lock files together: it regenerates the expected state from declarations,
// compares it against an existing lock, and verifies every dependency against
// the local cache.
//
// Failures are classified into six distinguishable codes:
//
//	ErrManifestMalformed  a go.mod entry is illegal / incomplete
//	ErrLockStale          declarations changed and the lock was not regenerated
//	ErrLockTampered       the lock body or recorded digests were modified
//	ErrMissingDependency  a selected dependency is absent from the local cache
//	ErrChecksumMismatch   cached content differs from what the lock records
//	ErrVersionConflict    selection cannot satisfy requirements / excludes
//
// A Verifier is stateless; every call builds independent local state, so
// concurrent verifications never share intermediate state or caches.
package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/cache"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/graph"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
)

// ErrorCode enumerates the top-level verification outcomes.
type ErrorCode int

const (
	ErrNone ErrorCode = iota
	ErrManifestMalformed
	ErrLockStale
	ErrLockTampered
	ErrMissingDependency
	ErrChecksumMismatch
	ErrVersionConflict
	ErrLockMalformed
)

func (c ErrorCode) String() string {
	switch c {
	case ErrManifestMalformed:
		return "manifest malformed"
	case ErrLockStale:
		return "lock stale"
	case ErrLockTampered:
		return "lock tampered"
	case ErrMissingDependency:
		return "missing dependency"
	case ErrChecksumMismatch:
		return "checksum mismatch"
	case ErrVersionConflict:
		return "version conflict"
	case ErrLockMalformed:
		return "lock malformed"
	default:
		return "ok"
	}
}

// DiffEntry is one expected-vs-actual row.
type DiffEntry struct {
	Path         string
	IntroducedBy []string
	Field        string // "present" | "version" | "go_mod_digest" | "tree_digest"
	Expected     string
	Actual       string
}

// Report is the full verification report.
type Report struct {
	MainModule   string
	ManifestHash string
	LockPresent  bool
	Diffs        []DiffEntry
	Checked      int
}

// Failure is a classified verification failure. Multiple failures are reported
// together in a FailureList.
type Failure struct {
	Code   ErrorCode
	Detail string
	Diffs  []DiffEntry
}

func (f *Failure) Error() string {
	return fmt.Sprintf("%s: %s", f.Code, f.Detail)
}

// Is matches failures by Code.
func (f *Failure) Is(target error) bool {
	t, ok := target.(*Failure)
	return ok && t.Code == f.Code
}

// FailureList is the aggregate of all verification failures.
type FailureList []*Failure

func (fl FailureList) Error() string {
	var b []byte
	b = append(b, fmt.Sprintf("%d verification failure(s):\n", len(fl))...)
	for _, f := range fl {
		b = append(b, "  - "+f.Error()+"\n"...)
		for _, d := range f.Diffs {
			b = append(b, fmt.Sprintf("      %s %s: expected %q, actual %q (introduced by %v)\n",
				d.Path, d.Field, d.Expected, d.Actual, d.IntroducedBy)...)
		}
	}
	return string(b)
}

// Is reports whether any failure matches target.
func (fl FailureList) Is(target error) bool {
	for _, f := range fl {
		if errors.Is(f, target) {
			return true
		}
	}
	return false
}

// Unwrap exposes failures to errors.As / errors.Is.
func (fl FailureList) Unwrap() []error {
	out := make([]error, len(fl))
	for i, f := range fl {
		out[i] = f
	}
	return out
}

// AsFailureList extracts the aggregate from err.
func AsFailureList(err error) (FailureList, bool) {
	var fl FailureList
	if errors.As(err, &fl) {
		return fl, true
	}
	return nil, false
}

// Options configures a verification run.
type Options struct {
	// ModuleDir contains go.mod (and go.lock when LockPath is empty).
	ModuleDir string
	// LockPath overrides the lock file location; empty means ModuleDir/go.lock.
	LockPath string
	// CacheRoot overrides GOMODCACHE; empty uses $GOMODCACHE or the default.
	CacheRoot string
}

func (o Options) lockPath() string {
	if o.LockPath != "" {
		return o.LockPath
	}
	return filepath.Join(o.ModuleDir, "go.lock")
}

// Verifier is stateless; every call uses independent local state.
type Verifier struct{}

// New creates a Verifier.
func New() *Verifier { return &Verifier{} }

// loadCache builds a fresh read-only cache handle for this call.
func loadCache(root string) (cache.Cache, error) {
	if root == "" {
		root = os.Getenv("GOMODCACHE")
	}
	if root == "" {
		// Default matching the go command: $GOPATH/pkg/mod.
		if gp := os.Getenv("GOPATH"); gp != "" {
			root = filepath.Join(gp, "pkg", "mod")
		} else if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, "go", "pkg", "mod")
		}
	}
	return cache.New(root)
}

// Verify loads declarations and lock, resolves offline and checks digests. It
// never mutates the lock file.
func (v *Verifier) Verify(ctx context.Context, opts Options) (*Report, error) {
	return v.run(ctx, opts, false)
}

func (v *Verifier) run(ctx context.Context, opts Options, locking bool) (*Report, error) {
	_ = locking
	var failures FailureList

	// 1. Parse declarations.
	goModPath := filepath.Join(opts.ModuleDir, "go.mod")
	m, err := (&manifest.Parser{}).ParseFile(ctx, goModPath)
	if err != nil {
		var entryErrs manifest.Errors
		if errors.As(err, &entryErrs) {
			return nil, FailureList{&Failure{Code: ErrManifestMalformed, Detail: err.Error()}}
		}
		return nil, err
	}
	currentManifestHash := m.CanonicalHash()

	// 2. Load and structurally validate the lock.
	lockBytes, lockErr := os.ReadFile(opts.lockPath())
	var existing *lockfile.Lock
	lockPresent := lockErr == nil
	if lockPresent {
		l, perr := lockfile.Unmarshal(lockBytes)
		if perr != nil {
			return nil, FailureList{&Failure{Code: ErrLockMalformed, Detail: perr.Error()}}
		}
		if ierr := l.VerifyIntegrity(); ierr != nil {
			return nil, FailureList{&Failure{Code: ErrLockTampered, Detail: ierr.Error()}}
		}
		existing = l

		// 3. Stale-ness takes precedence: when declarations changed the lock
		//    must be regenerated, and we deliberately do not resolve against
		//    the new (possibly not yet cached) declarations.
		if l.ManifestHash != currentManifestHash {
			return nil, FailureList{&Failure{
				Code: ErrLockStale,
				Detail: fmt.Sprintf(
					"declarations hash is %s but lock records %s; regenerate the lock",
					currentManifestHash, l.ManifestHash),
				Diffs: []DiffEntry{{
					Path: m.ModulePath, Field: "manifest_hash",
					Expected: currentManifestHash, Actual: l.ManifestHash,
				}},
			}}
		}
	}

	// 4. Resolve offline with an independent, read-only cache handle.
	cch, err := loadCache(opts.CacheRoot)
	if err != nil {
		return nil, err
	}
	res, gerr := (&graph.Resolver{Cache: cch}).Resolve(ctx, m)
	if gerr != nil {
		var gerrs graph.Errors
		if errors.As(gerr, &gerrs) {
			for _, ge := range gerrs {
				f := &Failure{Detail: ge.Error()}
				switch ge.Code {
				case graph.GraphErrMissingDependency, graph.GraphErrReplaceTargetMissing:
					f.Code = ErrMissingDependency
				case graph.GraphErrVersionConflict:
					f.Code = ErrVersionConflict
					for _, req := range ge.Required {
						f.Diffs = append(f.Diffs, DiffEntry{
							Path:     ge.Module.Path,
							Field:    "version",
							Expected: ge.Module.Version + " (selected but excluded)",
							Actual:   req.Version + " required",
						})
					}
				default:
					f.Code = ErrManifestMalformed
				}
				failures = append(failures, f)
			}
			return nil, sortFailures(failures)
		}
		return nil, gerr
	}

	// 5. Build the expected lock and, when a lock exists, compare rows.
	expected, err := buildExpectedLock(ctx, cch, m, res)
	if err != nil {
		return nil, err
	}
	report := &Report{
		MainModule:   m.ModulePath,
		ManifestHash: currentManifestHash,
		LockPresent:  lockPresent,
		Checked:      len(expected.Entries),
	}

	if existing != nil {
		if diffs := diffLocks(existing, expected); len(diffs) > 0 {
			// Declarations hash matched and the self hash verified, yet rows
			// differ from resolution: the recorded content was edited, i.e.
			// tampering with a recomputed self hash.
			failures = append(failures, &Failure{
				Code:   ErrLockTampered,
				Detail: fmt.Sprintf("%d locked row(s) do not match declarations", len(diffs)),
				Diffs:  diffs,
			})
		}
	}

	// 6. Independently verify digests against the local cache.
	cdiffs, cerr := verifyDigests(ctx, cch, existingOrExpected(existing, expected))
	if cerr != nil {
		return nil, cerr
	}
	report.Diffs = cdiffs
	if len(cdiffs) > 0 {
		code := ErrChecksumMismatch
		for _, d := range cdiffs {
			if d.Field == "present" {
				code = ErrMissingDependency
				break
			}
		}
		failures = append(failures, &Failure{
			Code:   code,
			Detail: fmt.Sprintf("%d dependency difference(s) versus the local cache", len(cdiffs)),
			Diffs:  cdiffs,
		})
	}

	if len(failures) > 0 {
		return report, sortFailures(failures)
	}
	return report, nil
}

func existingOrExpected(existing, expected *lockfile.Lock) *lockfile.Lock {
	if existing != nil {
		return existing
	}
	return expected
}

func sortFailures(fl FailureList) FailureList {
	sort.Slice(fl, func(i, j int) bool {
		if fl[i].Code != fl[j].Code {
			return fl[i].Code < fl[j].Code
		}
		return fl[i].Detail < fl[j].Detail
	})
	return fl
}
