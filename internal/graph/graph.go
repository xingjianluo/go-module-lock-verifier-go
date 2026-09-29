// Package graph resolves the effective module set of a manifest against a
// read-only local module cache: it expands transitive requirements from cached
// go.mod files, applies replace and exclude rules, performs minimum-version
// selection, and surfaces missing dependencies and version conflicts as
// distinguishable errors together with introducer chains.
//
// Resolution is intentionally a simplification of the go command's MVS:
// versions are canonical semver (enforced upstream by the manifest parser),
// every selected module must be fully present in the local cache, and a
// selected version that is excluded is a hard conflict rather than an
// automatic upgrade.
package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/cache"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/semver"
)

// SelectedModule is one module that survives version selection.
type SelectedModule struct {
	Path         string
	Version      string // effective version after replacement
	IntroducedBy []string
	Direct       bool
	Replaced     bool
	ReplaceKey   string // "oldpath@oldversion" when replaced
	Local        bool
	LocalDir     string
	Excluded     bool // selected logical version is excluded
}

// ErrorCode enumerates distinguishable resolution failures.
type ErrorCode int

const (
	GraphErrNone ErrorCode = iota
	GraphErrMissingDependency
	GraphErrVersionConflict
	GraphErrBadDependencyManifest
	GraphErrReplaceTargetMissing
)

func (c ErrorCode) String() string {
	switch c {
	case GraphErrMissingDependency:
		return "dependency missing from local cache"
	case GraphErrVersionConflict:
		return "version conflict"
	case GraphErrBadDependencyManifest:
		return "dependency go.mod is malformed"
	case GraphErrReplaceTargetMissing:
		return "replace target missing from local cache"
	default:
		return "ok"
	}
}

// Error carries a distinguishable code plus module context.
type Error struct {
	Code     ErrorCode
	Module   cache.ModuleRef
	Detail   string
	Required []cache.ModuleRef // distinct requirements on this module
}

func (e *Error) Error() string {
	return fmt.Sprintf("graph: %s for %s: %s", e.Code, e.Module, e.Detail)
}

// Is matches graph errors by Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Errors is the aggregate of resolution errors, sorted by module path.
type Errors []*Error

func (errs Errors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d resolution error(s):\n", len(errs))
	for _, e := range errs {
		fmt.Fprintf(&b, "  - %s\n", e.Error())
	}
	return strings.TrimRight(b.String(), "\n")
}

// Is reports whether any aggregated error matches target via its own Is method,
// so errors.Is works on the aggregate returned by Resolve.
func (errs Errors) Is(target error) bool {
	for _, e := range errs {
		if errors.Is(e, target) {
			return true
		}
	}
	return false
}

// Unwrap exposes the aggregated errors to errors.As/errors.Is.
func (errs Errors) Unwrap() []error {
	out := make([]error, len(errs))
	for i, e := range errs {
		out[i] = e
	}
	return out
}

// Resolver performs offline graph resolution.
type Resolver struct {
	Cache cache.Cache
}

// Result is the stable, sorted resolution outcome.
type Result struct {
	Modules    []*SelectedModule
	MainModule string
}

type pending struct {
	logicalPath string
	version     string
	chain       []string // introducer chain including the requesting node
}

type requirement struct {
	version string
	chain   []string
}

type resolverState struct {
	r      *Resolver
	ctx    context.Context
	m      *manifest.Manifest
	mu     sync.Mutex
	reqs   map[string]map[string][]string // logical path -> version -> one chain
	direct map[string]bool
	errors map[string]*Error // keyed by code|path|version to stay distinct
}

// Resolve runs offline resolution for m. The result is sorted by module path.
func (r *Resolver) Resolve(ctx context.Context, m *manifest.Manifest) (*Result, error) {
	if r.Cache == nil {
		return nil, &Error{Code: GraphErrBadDependencyManifest,
			Detail: "resolver has no cache configured"}
	}
	st := &resolverState{
		ctx:    ctx,
		r:      r,
		m:      m,
		reqs:   map[string]map[string][]string{},
		direct: map[string]bool{},
		errors: map[string]*Error{},
	}

	queue := make([]pending, 0, len(m.Requires))
	for _, req := range m.Requires {
		chain := []string{m.ModulePath}
		queue = append(queue, pending{req.Path, req.Version, chain})
		if req.Kind == manifest.KindDirect {
			st.direct[req.Path] = true
		}
	}

	expanded := map[string]bool{} // effective target keys already traversed
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		st.record(item)

		rule := st.findReplace(item.logicalPath, item.version)
		_, mod, isLocal, ok := st.loadTarget(item, rule)
		if !ok {
			continue
		}
		// One graph node per concrete logical requirement (path, version); the
		// replace rule makes its effective content unique. Deduping on the
		// effective target instead would merge distinct versions incorrectly.
		dedupeKey := item.logicalPath + "@" + item.version
		if expanded[dedupeKey] {
			continue
		}
		expanded[dedupeKey] = true
		if isLocal {
			// Transitive requirements of a local replacement are read from
			// its own go.mod; LocalModule loaded and parsed it already.
			sub := mod.goModRequirements
			for _, req := range sub {
				if req.Path == m.ModulePath {
					continue
				}
				queue = append(queue, pending{
					logicalPath: req.Path,
					version:     req.Version,
					chain:       appendChain(item.chain, mod.label),
				})
			}
			continue
		}

		for _, req := range mod.goModRequirements {
			if req.Path == m.ModulePath {
				continue
			}
			queue = append(queue, pending{
				logicalPath: req.Path,
				version:     req.Version,
				chain:       appendChain(item.chain, mod.label),
			})
		}
	}

	st.finishConflicts()
	if len(st.errors) > 0 {
		out := make([]*Error, 0, len(st.errors))
		for _, e := range st.errors {
			out = append(out, e)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Module.Path != out[j].Module.Path {
				return out[i].Module.Path < out[j].Module.Path
			}
			return out[i].Module.Version < out[j].Module.Version
		})
		return nil, Errors(out)
	}
	return st.buildResult(), nil
}

func appendChain(chain []string, node string) []string {
	out := make([]string, len(chain), len(chain)+1)
	copy(out, chain)
	return append(out, node)
}

type loadedTarget struct {
	label             string // "path@version" used in introducer chains
	goModRequirements []manifest.Requirement
}

func (st *resolverState) record(item pending) {
	st.mu.Lock()
	defer st.mu.Unlock()
	versions := st.reqs[item.logicalPath]
	if versions == nil {
		versions = map[string][]string{}
		st.reqs[item.logicalPath] = versions
	}
	if _, exists := versions[item.version]; !exists {
		chain := make([]string, len(item.chain))
		copy(chain, item.chain)
		versions[item.version] = chain
	}
}

func (st *resolverState) fail(code ErrorCode, ref cache.ModuleRef, detail string, required []cache.ModuleRef) {
	st.mu.Lock()
	defer st.mu.Unlock()
	key := fmt.Sprintf("%d|%s|%s", code, ref.Path, ref.Version)
	if _, exists := st.errors[key]; !exists {
		st.errors[key] = &Error{Code: code, Module: ref, Detail: detail, Required: required}
	}
}

func (st *resolverState) findReplace(path, version string) *manifest.ReplaceRule {
	for i := range st.m.Replaces {
		r := &st.m.Replaces[i]
		if r.OldPath == path && r.OldVersion == version {
			return r
		}
	}
	for i := range st.m.Replaces {
		r := &st.m.Replaces[i]
		if r.OldPath == path && r.OldVersion == "" {
			return r
		}
	}
	return nil
}

func (st *resolverState) loadTarget(item pending, rule *manifest.ReplaceRule) (string, *loadedTarget, bool, bool) {
	if rule != nil {
		if rule.New.LocalDir != "" {
			mod, err := cache.LocalModule(cache.ModuleRef{Path: item.logicalPath, Version: item.version}, rule.New.LocalDir)
			if err != nil {
				st.fail(GraphErrReplaceTargetMissing,
					cache.ModuleRef{Path: rule.OldPath, Version: rule.OldVersion},
					err.Error(), nil)
				return "", nil, false, false
			}
			sub, perr := (&manifest.Parser{}).Parse(st.ctx, mod.GoMod)
			if perr != nil {
				st.fail(GraphErrBadDependencyManifest,
					cache.ModuleRef{Path: item.logicalPath, Version: item.version},
					perr.Error(), nil)
				return "", nil, false, false
			}
			label := "local:" + rule.New.LocalDir
			return "local:" + rule.New.LocalDir,
				&loadedTarget{label: label, goModRequirements: sub.Requires}, true, true
		}
		ref := cache.ModuleRef{Path: rule.New.NewPath, Version: rule.New.NewVersion}
		mod, err := st.r.Cache.Module(st.ctx, ref)
		if err != nil {
			st.fail(GraphErrReplaceTargetMissing, ref, err.Error(), nil)
			return "", nil, false, false
		}
		return st.parseLoaded("replace:"+ref.String(), ref, mod)
	}

	ref := cache.ModuleRef{Path: item.logicalPath, Version: item.version}
	mod, err := st.r.Cache.Module(st.ctx, ref)
	if err != nil {
		st.fail(GraphErrMissingDependency, ref, err.Error(), nil)
		return "", nil, false, false
	}
	return st.parseLoaded("module:"+ref.String(), ref, mod)
}

func (st *resolverState) parseLoaded(key string, ref cache.ModuleRef, mod *cache.Module) (string, *loadedTarget, bool, bool) {
	sub, err := (&manifest.Parser{}).Parse(st.ctx, mod.GoMod)
	if err != nil {
		st.fail(GraphErrBadDependencyManifest, ref, err.Error(), nil)
		return "", nil, false, false
	}
	return key, &loadedTarget{label: ref.String(), goModRequirements: sub.Requires}, false, true
}

func (st *resolverState) isExcluded(path, version string) bool {
	for _, ex := range st.m.Excludes {
		if ex.Path == path && ex.Version == version {
			return true
		}
	}
	return false
}

// finishConflicts selects the maximum required version per logical path
// (minimum-version selection) and rejects a selected-but-excluded version.
func (st *resolverState) finishConflicts() {
	st.mu.Lock()
	defer st.mu.Unlock()

	paths := make([]string, 0, len(st.reqs))
	for p := range st.reqs {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		versions := st.reqs[p]
		vs := make([]string, 0, len(versions))
		for v := range versions {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool {
			a, _ := semver.Parse(vs[i])
			b, _ := semver.Parse(vs[j])
			return semver.Compare(a, b) < 0
		})
		best := vs[len(vs)-1]

		if rule := st.findReplaceLocked(p, best); rule != nil {
			// Replaced modules are pinned by the declaration; exclude rules do
			// not apply to the replacement target itself.
			continue
		}
		if st.isExcluded(p, best) {
			reqs := make([]cache.ModuleRef, 0, len(vs))
			for _, v := range vs {
				reqs = append(reqs, cache.ModuleRef{Path: p, Version: v})
			}
			st.errors[fmt.Sprintf("%d|%s|%s", GraphErrVersionConflict, p, best)] = &Error{
				Code:   GraphErrVersionConflict,
				Module: cache.ModuleRef{Path: p, Version: best},
				Detail: fmt.Sprintf(
					"minimum-version selection picks %s, but that version is excluded",
					best),
				Required: reqs,
			}
		}
	}
}

func (st *resolverState) findReplaceLocked(path, version string) *manifest.ReplaceRule {
	for i := range st.m.Replaces {
		r := &st.m.Replaces[i]
		if r.OldPath == path && (r.OldVersion == version || r.OldVersion == "") {
			return r
		}
	}
	return nil
}

func (st *resolverState) buildResult() *Result {
	paths := make([]string, 0, len(st.reqs))
	for p := range st.reqs {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	res := &Result{MainModule: st.m.ModulePath}
	for _, p := range paths {
		versionsMap := st.reqs[p]
		vs := make([]string, 0, len(versionsMap))
		for v := range versionsMap {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool {
			a, _ := semver.Parse(vs[i])
			b, _ := semver.Parse(vs[j])
			return semver.Compare(a, b) < 0
		})
		best := vs[len(vs)-1]
		chain := versionsMap[best]
		chainCopy := make([]string, len(chain))
		copy(chainCopy, chain)

		sel := &SelectedModule{
			Path:         p,
			Version:      best,
			IntroducedBy: chainCopy,
			Direct:       st.direct[p],
			Excluded:     st.isExcluded(p, best),
		}
		if rule := st.findReplaceLocked(p, best); rule != nil {
			sel.Replaced = true
			sel.ReplaceKey = rule.OldPath + "@" + rule.OldVersion
			if rule.New.LocalDir != "" {
				sel.Local = true
				sel.LocalDir = rule.New.LocalDir
			} else {
				sel.Version = rule.New.NewVersion
			}
		}
		res.Modules = append(res.Modules, sel)
	}
	return res
}
