package verify

import (
	"context"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/cache"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/graph"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
)

// buildExpectedLock turns a resolution result into a fully digested lock.
func buildExpectedLock(ctx context.Context, cch cache.Cache, m *manifest.Manifest,
	res *graph.Result) (*lockfile.Lock, error) {
	l := &lockfile.Lock{
		FormatVersion: lockfile.FormatVersion,
		MainModule:    res.MainModule,
		ManifestHash:  m.CanonicalHash(),
		Entries:       make([]lockfile.Entry, 0, len(res.Modules)),
	}
	for _, sel := range res.Modules {
		entry := lockfile.Entry{
			Path:          sel.Path,
			Version:       sel.Version,
			Direct:        sel.Direct,
			Replaced:      sel.Replaced,
			ReplaceSource: sel.ReplaceKey,
			LocalDir:      sel.LocalDir,
			IntroducedBy:  append([]string(nil), sel.IntroducedBy...),
		}
		effPath, _, _ := replacementPath(m, sel.Path, originalVersion(m, sel))
		if sel.Replaced && effPath != sel.Path {
			entry.ReplacePath = effPath
		}
		mod, err := loadSelected(ctx, cch, m, sel)
		if err != nil {
			return nil, err
		}
		entry.GoModDigest = mod.GoModDigest
		entry.TreeDigest = mod.TreeDigest
		l.Entries = append(l.Entries, entry)
	}
	l.SortEntries()
	return l, nil
}

// originalVersion returns the requested (pre-replacement) version recorded by
// the replace rule, which identifies which rule applied for this module.
func originalVersion(m *manifest.Manifest, sel *graph.SelectedModule) string {
	if !sel.Replaced {
		return sel.Version
	}
	// ReplaceKey is "oldpath@oldversion".
	for i := len(sel.ReplaceKey) - 1; i >= 0; i-- {
		if sel.ReplaceKey[i] == '@' {
			return sel.ReplaceKey[i+1:]
		}
	}
	return sel.Version
}

func loadSelected(ctx context.Context, cch cache.Cache, m *manifest.Manifest, sel *graph.SelectedModule) (*cache.Module, error) {
	if sel.Local {
		return cache.LocalModule(cache.ModuleRef{Path: sel.Path, Version: sel.Version}, sel.LocalDir)
	}
	orig := originalVersion(m, sel)
	path, version, _ := replacementPath(m, sel.Path, orig)
	return cch.Module(ctx, cache.ModuleRef{Path: path, Version: version})
}

// resolveReplacementPath maps a replaced logical path to its effective module
// path, using manifest rules. Used when reading cache entries.
func replacementPath(m *manifest.Manifest, logicalPath, version string) (string, string, bool) {
	for _, r := range m.Replaces {
		if r.OldPath == logicalPath && (r.OldVersion == "" || r.OldVersion == version) {
			if r.New.LocalDir != "" {
				return logicalPath, "", true
			}
			return r.New.NewPath, r.New.NewVersion, false
		}
	}
	return logicalPath, version, false
}

func digestEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// EnsureLock builds the expected lock and writes it atomically when the on-disk
// contents differ. It returns the lock, the desired bytes and whether a write
// happened. Repeated calls with unchanged declarations perform no write.
func (v *Verifier) EnsureLock(ctx context.Context, opts Options) (*lockfile.Lock, []byte, bool, error) {
	goModPath := filepath.Join(opts.ModuleDir, "go.mod")
	m, err := (&manifest.Parser{}).ParseFile(ctx, goModPath)
	if err != nil {
		return nil, nil, false, &Failure{Code: ErrManifestMalformed, Detail: err.Error()}
	}
	cch, err := loadCache(opts.CacheRoot)
	if err != nil {
		return nil, nil, false, err
	}
	res, gerr := (&graph.Resolver{Cache: cch}).Resolve(ctx, m)
	if gerr != nil {
		return nil, nil, false, mapGraphError(gerr)
	}
	l, err := buildExpectedLock(ctx, cch, m, res)
	if err != nil {
		return nil, nil, false, err
	}
	desired, err := lockfile.Marshal(l)
	if err != nil {
		return nil, nil, false, err
	}

	path := opts.lockPath()
	existing, rerr := os.ReadFile(path)
	changed := rerr != nil || !bytesEqual(existing, desired)
	if changed {
		if err := atomicWrite(path, desired); err != nil {
			return nil, nil, false, err
		}
	}
	return l, desired, changed, nil
}

func mapGraphError(gerr error) error {
	var gerrs graph.Errors
	if !asGraphErrors(gerr, &gerrs) {
		return gerr
	}
	var fl FailureList
	for _, ge := range gerrs {
		f := &Failure{Detail: ge.Error()}
		switch ge.Code {
		case graph.GraphErrMissingDependency, graph.GraphErrReplaceTargetMissing:
			f.Code = ErrMissingDependency
		case graph.GraphErrVersionConflict:
			f.Code = ErrVersionConflict
		default:
			f.Code = ErrManifestMalformed
		}
		fl = append(fl, f)
	}
	return sortFailures(fl)
}

func asGraphErrors(err error, target *graph.Errors) bool {
	if gerrs, ok := err.(graph.Errors); ok {
		*target = gerrs
		return true
	}
	return false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".go.lock-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// sortDiffs orders diffs deterministically by path then field.
func sortDiffs(d []DiffEntry) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].Path != d[j].Path {
			return d[i].Path < d[j].Path
		}
		return d[i].Field < d[j].Field
	})
}

// threeWayDiff classifies differences between the lock, the freshly resolved
// declarations and the actual local cache, keeping failure causes distinct:
//
//	tamperDiffs    locked structural fields contradict the current declarations
//	               (version / direct / replacement / presence)
//	checksumDiffs  a recorded go.mod/tree digest differs from actual cache content
//	presentDiffs   a dependency cannot be found in the local cache
//
// Digests are anchored to the lock and checked purely against the cache,
// because they cannot be re-derived from go.mod declarations. Editing a lock
// without recomputing its self hash is rejected earlier as tampering.
func threeWayDiff(ctx context.Context, cch cache.Cache, lock, expected *lockfile.Lock) (
	tamperDiffs, checksumDiffs, presentDiffs []DiffEntry, err error) {

	type pair struct{ l, e *lockfile.Entry }
	byPath := map[string]pair{}
	for i := range expected.Entries {
		e := expected.Entries[i]
		byPath[e.Path] = pair{e: &e}
	}
	if lock != nil {
		for i := range lock.Entries {
			l := lock.Entries[i]
			r := byPath[l.Path]
			r.l = &l
			byPath[l.Path] = r
		}
	}

	paths := make([]string, 0, len(byPath))
	for pth := range byPath {
		paths = append(paths, pth)
	}
	sort.Strings(paths)

	for _, pth := range paths {
		r := byPath[pth]
		if lock == nil {
			// No lock: current-state content check only; there are no lock
			// rows to contradict the declarations.
			if r.e == nil {
				continue
			}
			e := r.e
			chain := append([]string(nil), e.IntroducedBy...)
			mod, merr := cch.Module(ctx, cache.ModuleRef{Path: pth, Version: e.Version})
			if merr != nil {
				if ce, ok := cache.AsError(merr); ok && ce.Code == cache.CacheErrMissingModule {
					presentDiffs = append(presentDiffs, DiffEntry{
						Path: pth, IntroducedBy: chain, Field: "present",
						Expected: "cached", Actual: "missing",
					})
					continue
				}
				return nil, nil, nil, merr
			}
			if !digestEqual(mod.GoModDigest, e.GoModDigest) {
				checksumDiffs = append(checksumDiffs, DiffEntry{
					Path: pth, IntroducedBy: chain, Field: "go_mod_digest",
					Expected: e.GoModDigest, Actual: mod.GoModDigest,
				})
			}
			if !digestEqual(mod.TreeDigest, e.TreeDigest) {
				checksumDiffs = append(checksumDiffs, DiffEntry{
					Path: pth, IntroducedBy: chain, Field: "tree_digest",
					Expected: e.TreeDigest, Actual: mod.TreeDigest,
				})
			}
			continue
		}
		if r.l == nil && r.e != nil {
			tamperDiffs = append(tamperDiffs, DiffEntry{
				Path: pth, IntroducedBy: r.e.IntroducedBy, Field: "present",
				Expected: "locked", Actual: "absent",
			})
			continue
		}
		if r.e == nil && r.l != nil {
			tamperDiffs = append(tamperDiffs, DiffEntry{
				Path: pth, IntroducedBy: r.l.IntroducedBy, Field: "present",
				Expected: "absent (not in declarations)", Actual: "locked",
			})
			continue
		}
		e := r.e
		l := r.l
		if l == nil {
			l = e // no lock present: pure current-state content check
		}

		if lock != nil {
			chain := append([]string(nil), e.IntroducedBy...)
			if l.Version != e.Version {
				tamperDiffs = append(tamperDiffs, DiffEntry{
					Path: pth, IntroducedBy: chain, Field: "version",
					Expected: e.Version, Actual: l.Version,
				})
			}
			if l.Direct != e.Direct {
				tamperDiffs = append(tamperDiffs, DiffEntry{
					Path: pth, IntroducedBy: chain, Field: "direct",
					Expected: fmt.Sprintf("%v", e.Direct), Actual: fmt.Sprintf("%v", l.Direct),
				})
			}
			if l.Replaced != e.Replaced || l.ReplaceSource != e.ReplaceSource ||
				l.ReplacePath != e.ReplacePath || l.LocalDir != e.LocalDir {
				tamperDiffs = append(tamperDiffs, DiffEntry{
					Path: pth, IntroducedBy: chain, Field: "replace",
					Expected: replaceSig(e), Actual: replaceSig(l),
				})
			}
		}

		var mod *cache.Module
		var cerr error
		if l.LocalDir != "" {
			mod, cerr = cache.LocalModule(cache.ModuleRef{Path: pth, Version: l.Version}, l.LocalDir)
		} else {
			effPath := pth
			if l.ReplacePath != "" {
				effPath = l.ReplacePath
			}
			mod, cerr = cch.Module(ctx, cache.ModuleRef{Path: effPath, Version: l.Version})
		}
		chain := append([]string(nil), l.IntroducedBy...)
		if cerr != nil {
			if ce, ok := cache.AsError(cerr); ok && ce.Code == cache.CacheErrMissingModule {
				presentDiffs = append(presentDiffs, DiffEntry{
					Path: pth, IntroducedBy: chain, Field: "present",
					Expected: "cached", Actual: "missing",
				})
				continue
			}
			return nil, nil, nil, cerr
		}

		for _, d := range []struct {
			field, recorded, actual string
		}{
			{"go_mod_digest", l.GoModDigest, mod.GoModDigest},
			{"tree_digest", l.TreeDigest, mod.TreeDigest},
		} {
			if !digestEqual(d.recorded, d.actual) {
				checksumDiffs = append(checksumDiffs, DiffEntry{
					Path: pth, IntroducedBy: chain, Field: d.field,
					Expected: d.recorded, Actual: d.actual,
				})
			}
		}
	}
	sortDiffs(tamperDiffs)
	sortDiffs(checksumDiffs)
	sortDiffs(presentDiffs)
	return tamperDiffs, checksumDiffs, presentDiffs, nil
}

func replaceSig(e *lockfile.Entry) string {
	if !e.Replaced {
		return "none"
	}
	if e.LocalDir != "" {
		return "local:" + e.LocalDir
	}
	return e.ReplacePath + "@" + e.Version + " (from " + e.ReplaceSource + ")"
}
