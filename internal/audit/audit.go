// Package audit answers, strictly read-only, "what changed between the
// existing lock and the current declarations/cache". It classifies every
// finding into exactly one category:
//
//   - declarations changed, lock must be regenerated  (*model.LockError, lock_stale)
//   - the lock file itself was edited after generation (*model.LockError, lock_tampered)
//   - local cache content disagrees with the lock      (*model.VerifyError)
//   - workspace-level problems (conflicting replaces…) (*model.WorkspaceError)
//   - manifest entries that cannot be accepted         (*model.RejectError)
//
// An audit never writes: not the lock, not the manifest, not the cache.
// All scratch state lives in the caller-provided per-run WorkDir.
package audit

import (
	"fmt"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/verify"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/workspace"
)

// Options configures one audit run. Exactly one of ManifestPath (single
// module) or WorkspacePath (multi-module workspace) must be set.
type Options struct {
	ManifestPath  string
	WorkspacePath string
	LockPath      string
	Resolver      resolver.Options
}

// Result is the outcome of an audit run.
type Result struct {
	// Subject is the module path or workspace name audited.
	Subject string
	// Modules lists workspace submodule module paths (deterministic order);
	// nil for single-module audits.
	Modules []string
	// Report is the module-by-module comparison, when the lock parsed.
	Report *model.DiffReport
	// Lock is the parsed lock as found on disk; the audit never mutates it.
	Lock *model.LockFile
}

// Run performs the audit. Classified findings come back as []error; a hard
// error is returned only for I/O failures before classification.
func Run(opts Options) (*Result, []error, error) {
	if opts.LockPath == "" {
		return nil, nil, fmt.Errorf("audit: LockPath must be set")
	}
	mf, subject, modules, declErrs, err := declarations(opts)
	if err != nil || len(declErrs) > 0 {
		return &Result{Subject: subject, Modules: modules}, declErrs, err
	}

	report, lf, errs, err := verify.Check(mf, opts.LockPath, opts.Resolver)
	if err != nil {
		return nil, nil, err
	}
	return &Result{
		Subject: subject, Modules: modules,
		Report: report, Lock: lf,
	}, errs, nil
}

// declarations loads the effective declaration set: either a single go.mod
// or the merged view of a workspace. Workspace conflicts abort the audit
// before resolution because picking either side silently is not allowed.
func declarations(opts Options) (mf *model.Manifest, subject string, modules []string, errs []error, err error) {
	if opts.WorkspacePath != "" {
		ws, loadErrs, lerr := workspace.Load(opts.WorkspacePath)
		if lerr != nil {
			return nil, "", nil, nil, fmt.Errorf("load workspace: %w", lerr)
		}
		subject = ws.Name
		for _, m := range ws.Modules {
			if m.Manifest != nil {
				modules = append(modules, m.Manifest.ModulePath)
			}
		}
		if len(loadErrs) > 0 {
			return nil, subject, modules, loadErrs, nil
		}
		merged, mergeErrs := workspace.Merge(ws)
		if len(mergeErrs) > 0 {
			return nil, subject, modules, mergeErrs, nil
		}
		return merged.Manifest, subject, modules, nil, nil
	}
	if opts.ManifestPath == "" {
		return nil, "", nil, nil, fmt.Errorf("audit: ManifestPath or WorkspacePath must be set")
	}
	mf, rejects, perr := manifest.ParseFile(opts.ManifestPath)
	if perr != nil {
		return nil, "", nil, nil, fmt.Errorf("read manifest: %w", perr)
	}
	if len(rejects) > 0 {
		return nil, mf.ModulePath, nil, rejects, nil
	}
	return mf, mf.ModulePath, nil, nil, nil
}
