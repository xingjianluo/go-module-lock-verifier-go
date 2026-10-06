// Package verify runs offline, concurrency-safe verification of a lock file
// against the manifest and the local module cache.
//
// A run never writes to the module cache or the lock file. All intermediate
// state lives in memory or in the run's private Options.Resolver.WorkDir, so
// concurrent runs are isolated; repeated runs on unchanged inputs produce
// identical conclusions.
package verify

import (
	"fmt"
	"os"
	"sort"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
)

// Options configures a verification run.
type Options struct {
	ManifestPath string
	LockPath     string
	Resolver     resolver.Options
}

// Result is the outcome of a verification run.
type Result struct {
	Report *model.DiffReport
	Lock   *model.LockFile
}

// Run parses the manifest, validates the lock, resolves offline and compares
// the effective selection with the locked one.
//
// Error classification:
//
//   - manifest entry problems: *model.RejectError
//   - stale declarations / tampered lock: *model.LockError
//   - missing cache content / content hash mismatch: *model.VerifyError
//
// A hard error is returned only for I/O failures before classification.
func Run(opts Options) (*Result, []error, error) {
	mf, rejects, err := manifest.ParseFile(opts.ManifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	var errs []error
	for _, r := range rejects {
		errs = append(errs, r)
	}
	if len(errs) > 0 {
		return nil, errs, nil
	}
	return RunManifest(mf, opts.LockPath, opts.Resolver)
}

// RunManifest is the manifest-agnostic core of Run: callers that already
// hold a normalized manifest (for example a merged multi-module workspace)
// audit it against the lock and cache directly. It is strictly read-only:
// neither the lock file nor the module cache is ever written.
func RunManifest(mf *model.Manifest, lockPath string, resOpts resolver.Options) (*Result, []error, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read lock: %w", err)
	}
	lf, err := lockfile.Unmarshal(data)
	if err != nil {
		return nil, []error{err}, nil
	}
	// A stale lock is recorded but does not short-circuit: still resolve and
	// build the diff report so the caller can see what drifted.
	var errs2 []error
	if err := lockfile.CheckManifest(lf, mf); err != nil {
		errs2 = append(errs2, err)
	}

	modules, resErrs, err := resolver.Resolve(mf, resOpts)
	if err != nil {
		return nil, nil, err
	}
	errs2 = append(errs2, resErrs...)

	report := &model.DiffReport{ModulePath: mf.ModulePath}
	compareLocked(report, &errs2, lf, modules, resOpts.ModuleCache)

	sort.SliceStable(errs2, func(i, j int) bool { return errs2[i].Error() < errs2[j].Error() })
	return &Result{Report: report, Lock: lf}, errs2, nil
}

func compareLocked(report *model.DiffReport, errs *[]error, lf *model.LockFile, modules []model.LockedModule, _ string) {
	byPath := map[string]model.LockedModule{}
	for _, m := range modules {
		byPath[m.Path] = m
	}
	locked := map[string]model.LockedModule{}
	for _, m := range lf.Modules {
		locked[m.Path] = m
	}

	paths := map[string]struct{}{}
	for p := range byPath {
		paths[p] = struct{}{}
	}
	for p := range locked {
		paths[p] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)

	for _, p := range ordered {
		got, haveGot := byPath[p]
		want, haveWant := locked[p]
		entry := model.DiffEntry{Path: p}
		if haveWant {
			entry.LockedVer, entry.LockedHash = want.Version, want.Hash
			entry.IntroducedBy = append([]string(nil), want.IntroducedBy...)
		}
		if haveGot {
			entry.ActualVer = got.Version
		}

		switch {
		case !haveWant:
			entry.Status = "unlocked"
			*errs = append(*errs, &model.LockError{
				Kind: model.LockStale, Module: p, Actual: got.Version,
				Detail: "resolved module is absent from the lock",
			})
		case !haveGot:
			entry.Status = "extra-locked"
			*errs = append(*errs, &model.LockError{
				Kind: model.LockStale, Module: p, Expected: want.Version,
				Detail: "locked module is no longer part of the resolution",
			})
		case want.Version != got.Version:
			entry.Status = "version-mismatch"
			*errs = append(*errs, &model.LockError{
				Kind: model.LockStale, Module: p,
				Expected: want.Version, Actual: got.Version,
				Detail: "locked version differs from resolved version",
			})
		default:
			entry.ActualHash = got.Hash
			switch {
			case want.Replacement != nil && want.Replacement.Local:
				entry.Status = "ok-local-replace"
			case got.Hash == "":
				entry.Status = "missing"
				*errs = append(*errs, &model.VerifyError{
					Kind: model.VerifyMissingModule, Module: p,
					Expected: want.Hash, Actual: "",
					IntroducedBy: want.IntroducedBy,
					Detail:       "module content is not present in the local cache",
				})
			case want.Hash == "":
				entry.Status = "hash-mismatch"
				*errs = append(*errs, &model.VerifyError{
					Kind: model.VerifyHashMismatch, Module: p,
					Expected: want.Hash, Actual: got.Hash,
					IntroducedBy: want.IntroducedBy,
					Detail:       "lock records no content hash but cache content is present",
				})
			case want.Hash != got.Hash:
				entry.Status = "hash-mismatch"
				*errs = append(*errs, &model.VerifyError{
					Kind: model.VerifyHashMismatch, Module: p,
					Expected: want.Hash, Actual: got.Hash,
					IntroducedBy: want.IntroducedBy,
					Detail:       "cached module content hash differs from the lock",
				})
			default:
				entry.Status = "ok"
			}
		}
		report.Entries = append(report.Entries, entry)
	}
}
