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

// diffLocks compares an existing lock against freshly resolved expectations.
// Every row identifies the module, the field, expected and actual values, and
// the introducer chain.
func diffLocks(existing, expected *lockfile.Lock) []DiffEntry {
	expByPath := map[string]lockfile.Entry{}
	for _, e := range expected.Entries {
		expByPath[e.Path] = e
	}
	gotByPath := map[string]lockfile.Entry{}
	for _, e := range existing.Entries {
		gotByPath[e.Path] = e
	}

	var diffs []DiffEntry
	compare := func(path string, got, exp lockfile.Entry, present bool) {
		chain := append([]string(nil), exp.IntroducedBy...)
		if !present {
			diffs = append(diffs, DiffEntry{Path: path, IntroducedBy: chain,
				Field: "present", Expected: "locked", Actual: "absent"})
			return
		}
		if got.Version != exp.Version {
			diffs = append(diffs, DiffEntry{Path: path, IntroducedBy: chain,
				Field: "version", Expected: exp.Version, Actual: got.Version})
		}
		if !digestEqual(got.GoModDigest, exp.GoModDigest) {
			diffs = append(diffs, DiffEntry{Path: path, IntroducedBy: chain,
				Field: "go_mod_digest", Expected: exp.GoModDigest, Actual: got.GoModDigest})
		}
		if !digestEqual(got.TreeDigest, exp.TreeDigest) {
			diffs = append(diffs, DiffEntry{Path: path, IntroducedBy: chain,
				Field: "tree_digest", Expected: exp.TreeDigest, Actual: got.TreeDigest})
		}
		if got.Direct != exp.Direct {
			diffs = append(diffs, DiffEntry{Path: path, IntroducedBy: chain,
				Field: "direct", Expected: fmt.Sprintf("%v", exp.Direct),
				Actual: fmt.Sprintf("%v", got.Direct)})
		}
	}

	for _, exp := range expected.Entries {
		got, ok := gotByPath[exp.Path]
		if !ok {
			diffs = append(diffs, DiffEntry{Path: exp.Path, IntroducedBy: exp.IntroducedBy,
				Field: "present", Expected: "locked", Actual: "absent"})
			continue
		}
		compare(exp.Path, got, exp, true)
	}
	for _, got := range existing.Entries {
		if _, ok := expByPath[got.Path]; !ok {
			diffs = append(diffs, DiffEntry{Path: got.Path, IntroducedBy: got.IntroducedBy,
				Field: "present", Expected: "absent", Actual: "locked"})
		}
	}
	sortDiffs(diffs)
	return diffs
}

func digestEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// verifyDigests recomputes digests from the local cache and compares them with
// the lock rows. A missing cache entry is reported on "present" as
// ErrMissingDependency by the caller's graph pass; here an absent module that
// the lock nonetheless records surfaces as a present=false diff, and digest
// differences are checksum mismatches.
func verifyDigests(ctx context.Context, cch cache.Cache, l *lockfile.Lock) ([]DiffEntry, error) {
	var diffs []DiffEntry
	for _, e := range l.Entries {
		var mod *cache.Module
		var err error
		if e.LocalDir != "" {
			mod, err = cache.LocalModule(cache.ModuleRef{Path: e.Path, Version: e.Version}, e.LocalDir)
		} else {
			effPath := e.Path
			if e.ReplacePath != "" {
				effPath = e.ReplacePath
			}
			mod, err = cch.Module(ctx, cache.ModuleRef{Path: effPath, Version: e.Version})
		}
		if err != nil {
			if ce, ok := cache.AsError(err); ok && ce.Code == cache.CacheErrMissingModule {
				diffs = append(diffs, DiffEntry{Path: e.Path, IntroducedBy: e.IntroducedBy,
					Field: "present", Expected: "cached", Actual: "missing"})
				continue
			}
			return nil, err
		}
		if !digestEqual(mod.GoModDigest, e.GoModDigest) {
			diffs = append(diffs, DiffEntry{Path: e.Path, IntroducedBy: e.IntroducedBy,
				Field: "go_mod_digest", Expected: e.GoModDigest, Actual: mod.GoModDigest})
		}
		if !digestEqual(mod.TreeDigest, e.TreeDigest) {
			diffs = append(diffs, DiffEntry{Path: e.Path, IntroducedBy: e.IntroducedBy,
				Field: "tree_digest", Expected: e.TreeDigest, Actual: mod.TreeDigest})
		}
	}
	sortDiffs(diffs)
	return diffs, nil
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
