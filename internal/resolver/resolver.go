// Package resolver computes the effective build list from a manifest using
// only a local module cache, and reports version conflicts distinctly.
//
// Resolution implements the observable behavior of minimal version selection
// needed offline:
//
//   - the highest required version of each module is selected;
//   - replace rules redirect a module (or module@version) to another version
//     or a local directory;
//   - exclude rules drop a single version from consideration;
//   - every module needed must have its .mod (and content) in the local
//     cache, or a distinct missing-module error is returned;
//   - requirements that cannot settle on one cached version produce a
//     distinct version-conflict error listing every requester.
package resolver

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// Options configures an offline resolution run.
type Options struct {
	// ModuleCache is the GOMODCACHE directory to read modules from.
	ModuleCache string
	// WorkDir is a private scratch directory for this run. Concurrent runs
	// must pass distinct WorkDir values so intermediate state is never
	// shared. The field is reserved and validated; resolution itself is
	// read-only against the cache.
	WorkDir string
}

type reqSource struct {
	version string
	by      string // module path that issued the requirement
}

type engine struct {
	mf    *model.Manifest
	cache *Cache
	opts  Options

	selected    map[string]string // module path -> highest seen required version
	excludedReq map[string][]reqSource
	sources     map[string][]reqSource
	excluded    map[string]map[string]bool
	replMap     map[string]map[string]*model.ReplaceRule // path -> version(or "") -> rule
	visited     map[string]bool                          // path@version whose .mod was loaded

	missing  []*model.VerifyError
	conflict []*model.VerifyError
}

// Resolve computes the effective module set entirely from the local cache.
// Version conflicts and missing modules are returned as *model.VerifyError;
// a hard error is returned only for unusable configuration.
func Resolve(m *model.Manifest, opts Options) ([]model.LockedModule, []error, error) {
	if opts.ModuleCache == "" {
		return nil, nil, fmt.Errorf("resolver: ModuleCache must be set")
	}
	if opts.WorkDir == "" {
		return nil, nil, fmt.Errorf("resolver: WorkDir must be set per run")
	}
	e := &engine{
		mf:          m,
		cache:       NewCache(opts.ModuleCache),
		opts:        opts,
		selected:    map[string]string{},
		excludedReq: map[string][]reqSource{},
		sources:     map[string][]reqSource{},
		excluded:    map[string]map[string]bool{},
		replMap:     map[string]map[string]*model.ReplaceRule{},
		visited:     map[string]bool{},
	}
	for _, ex := range m.Excludes {
		if e.excluded[ex.Path] == nil {
			e.excluded[ex.Path] = map[string]bool{}
		}
		e.excluded[ex.Path][ex.Version] = true
	}
	for i := range m.Replaces {
		r := m.Replaces[i]
		if e.replMap[r.OldPath] == nil {
			e.replMap[r.OldPath] = map[string]*model.ReplaceRule{}
		}
		e.replMap[r.OldPath][r.OldVersion] = &r
	}

	mainPath := m.ModulePath
	for _, req := range m.Requires {
		e.addRequirement(req.Path, req.Version, mainPath)
	}
	e.process()
	e.validateConflicts()

	modules := e.buildModules()

	var errs []error
	for _, v := range e.conflict {
		errs = append(errs, v)
	}
	for _, v := range e.missing {
		errs = append(errs, v)
	}
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return modules, errs, nil
}

func (e *engine) addRequirement(path, version, by string) {
	if e.excluded[path] != nil && e.excluded[path][version] {
		e.excludedReq[path] = append(e.excludedReq[path], reqSource{version: version, by: by})
		return
	}
	cur, ok := e.selected[path]
	if !ok || compareVersions(version, cur) > 0 {
		e.selected[path] = version
	}
	e.sources[path] = append(e.sources[path], reqSource{version: version, by: by})
}

// process drains newly selected modules, loads their cached go.mod files and
// folds in transitive requirements until a fixed point is reached.
func (e *engine) process() {
	for {
		var pending []string
		for path, ver := range e.selected {
			key := path + "@" + ver
			if !e.visited[key] {
				pending = append(pending, path)
			}
		}
		if len(pending) == 0 {
			return
		}
		sort.Strings(pending)
		for _, path := range pending {
			ver := e.selected[path]
			e.visited[path+"@"+ver] = true
			e.loadDeps(path, ver)
		}
	}
}

// validateConflicts marks the distinct case where every version ever
// requested for a module was excluded: the module has a real requester set but
// no selectable version. Missing artifacts are reported separately as missing
// modules during loading.
func (e *engine) validateConflicts() {
	paths := make([]string, 0, len(e.excludedReq))
	for p, reqs := range e.excludedReq {
		if _, ok := e.selected[p]; !ok && len(reqs) > 0 {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		var wantVals, by []string
		seen := map[string]struct{}{}
		for _, rq := range e.excludedReq[path] {
			wantVals = append(wantVals, rq.by+"@"+rq.version)
			if _, ok := seen[rq.by]; !ok {
				seen[rq.by] = struct{}{}
				by = append(by, rq.by)
			}
		}
		e.conflict = append(e.conflict, &model.VerifyError{
			Kind:         model.VerifyVersionConflict,
			Module:       path,
			Expected:     strings.Join(sortedStrings(wantVals), ", "),
			Actual:       "all requested versions excluded",
			IntroducedBy: sortedStrings(by),
			Detail:       "every requested version is removed by exclude directives",
		})
	}
}

func (e *engine) resolveTarget(path, ver string) (realPath, realVer string, rule *model.ReplaceRule, localPath string) {
	if m := e.replMap[path]; m != nil {
		if r := m[ver]; r != nil {
			rule = r
		} else if r := m[""]; r != nil {
			rule = r
		}
	}
	if rule == nil {
		return path, ver, nil, ""
	}
	if rule.Local {
		return rule.NewPath, "", rule, rule.NewPath
	}
	if rule.NewVersion != "" {
		return rule.NewPath, rule.NewVersion, rule, ""
	}
	return rule.NewPath, ver, rule, ""
}

func (e *engine) loadDeps(path, ver string) {
	realPath, realVer, _, localPath := e.resolveTarget(path, ver)
	if localPath != "" {
		// Local replacements are taken as-is; their requirements are read
		// from the replacement directory's go.mod when present.
		e.loadLocalDeps(localPath, path)
		return
	}
	modPath := e.cache.ModFile(realPath, realVer)
	dm, _, err := manifest.ParseFile(modPath)
	if err != nil {
		e.missing = append(e.missing, &model.VerifyError{
			Kind:         model.VerifyMissingModule,
			Module:       realPath,
			Expected:     realVer,
			IntroducedBy: e.requesters(path),
			Detail:       fmt.Sprintf("cached go.mod not found at %s", modPath),
		})
		return
	}
	for _, req := range dm.Requires {
		e.addRequirement(req.Path, req.Version, path)
	}
}

func (e *engine) loadLocalDeps(dir, replacedPath string) {
	dm, _, err := manifest.ParseFile(dir + "/go.mod")
	if err != nil {
		// A local replacement without a readable go.mod contributes no
		// transitive requirements; the replacement itself still locks.
		return
	}
	for _, req := range dm.Requires {
		e.addRequirement(req.Path, req.Version, replacedPath)
	}
}

func (e *engine) requesters(path string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range e.sources[path] {
		if _, ok := seen[s.by]; !ok {
			seen[s.by] = struct{}{}
			out = append(out, s.by)
		}
	}
	sort.Strings(out)
	return out
}

func (e *engine) buildModules() []model.LockedModule {
	paths := make([]string, 0, len(e.selected))
	for p := range e.selected {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	direct := map[string]bool{}
	for _, r := range e.mf.Requires {
		if !r.Indirect {
			direct[r.Path] = true
		}
	}

	out := make([]model.LockedModule, 0, len(paths))
	for _, path := range paths {
		ver := e.selected[path]
		_, _, rule, _ := e.resolveTarget(path, ver)

		realPath, realVer, _, localPath := e.resolveTarget(path, ver)
		lm := model.LockedModule{
			Path:         path,
			Version:      ver,
			Replacement:  copyRule(rule),
			Indirect:     !direct[path],
			IntroducedBy: e.requesters(path),
		}
		if localPath != "" {
			lm.Hash = "" // local content is machine-specific; not hashed
		} else if h, ok := e.cache.ContentHash(realPath, realVer); ok {
			lm.Hash = h
		}
		out = append(out, lm)
	}
	return out
}

func copyRule(r *model.ReplaceRule) *model.ReplaceRule {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// compareVersions compares canonical module versions following semver
// precedence (prereleases sort lower than the release; pseudo-versions are
// treated as prereleases). Returns -1/0/1.
func compareVersions(a, b string) int {
	pa, pb := parseSemver(a), parseSemver(b)
	return pa.compare(pb)
}

type semver struct {
	major, minor, patch int
	pre                 []string
	incompatible        bool
}

func parseSemver(v string) semver {
	var s semver
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimSuffix(v, "+incompatible")
	if strings.Contains(v, "+") {
		// Drop build metadata; keep the prerelease portion.
		if i := strings.Index(v, "+"); i >= 0 {
			v = v[:i]
		}
	}
	if i := strings.Index(v, "-"); i >= 0 {
		s.pre = strings.Split(v[i+1:], ".")
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	s.major, _ = strconv.Atoi(parts[0])
	if len(parts) > 1 {
		s.minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		s.patch, _ = strconv.Atoi(parts[2])
	}
	return s
}

func (s semver) compare(t semver) int {
	for _, pair := range [][2]int{
		{s.major, t.major}, {s.minor, t.minor}, {s.patch, t.patch},
	} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return comparePre(s.pre, t.pre)
}

// comparePre implements semver prerelease precedence; an empty prerelease
// (release) has higher precedence than a non-empty one.
func comparePre(a, b []string) int {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1
	}
	if len(b) == 0 {
		return -1
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		x, y := a[i], b[i]
		xi, xerr := strconv.Atoi(x)
		yi, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xi != yi {
				if xi < yi {
					return -1
				}
				return 1
			}
		case xerr == nil && yerr != nil:
			return -1 // numeric identifiers sort before alphanumeric
		case xerr != nil && yerr == nil:
			return 1
		case x != y:
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}
