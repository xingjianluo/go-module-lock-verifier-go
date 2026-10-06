package workspace

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// Merge combines all submodule manifests into one effective manifest.
// Conflicts are returned as *model.WorkspaceError; when any conflict exists
// the returned manifest is nil. The merge is read-only against the
// submodules and deterministic: the same workspace always merges to the
// same declarations in the same order.
func (w *Workspace) Merge() (*model.Manifest, []error) {
	merged := &model.Manifest{
		ModulePath: w.Name,
		GoVersion:  w.GoVersion,
	}
	if merged.ModulePath == "" {
		merged.ModulePath = "workspace"
	}

	var errs []error
	errs = append(errs, w.mergeRequires(merged)...)
	errs = append(errs, w.mergeReplaces(merged)...)
	w.mergeExcludes(merged)

	if merged.GoVersion == "" {
		merged.GoVersion = maxGoVersion(w.Modules)
	}
	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		return nil, errs
	}
	return merged, nil
}

// mergeRequires folds every submodule requirement into the merged manifest.
// Identical requirements deduplicate; every submodule stating a requirement
// keeps its own entry so the resolver can attribute introducers. A module
// required at two different versions is a requirement_conflict.
func (w *Workspace) mergeRequires(merged *model.Manifest) []error {
	type reqKey struct {
		path, version, source string
	}
	byPath := map[string]map[string][]string{} // path -> version -> sources
	seen := map[reqKey]bool{}
	var reqs []model.Requirement

	for _, mf := range w.Modules {
		for _, r := range mf.Requires {
			src := mf.ModulePath
			if byPath[r.Path] == nil {
				byPath[r.Path] = map[string][]string{}
			}
			byPath[r.Path][r.Version] = appendUnique(byPath[r.Path][r.Version], src)
			key := reqKey{r.Path, r.Version, src}
			if seen[key] {
				continue
			}
			seen[key] = true
			reqs = append(reqs, model.Requirement{
				Path:     r.Path,
				Version:  r.Version,
				Indirect: r.Indirect,
				Source:   src,
			})
		}
	}

	var errs []error
	paths := sortedKeys2(byPath)
	for _, path := range paths {
		versions := byPath[path]
		if len(versions) <= 1 {
			continue
		}
		var sources []string
		var vals []string
		for _, ver := range sortedKeys(versions) {
			vals = append(vals, ver)
			for _, src := range versions[ver] {
				sources = append(sources, fmt.Sprintf("%s: require %s %s", src, path, ver))
			}
		}
		errs = append(errs, &model.WorkspaceError{
			Kind:     model.WorkspaceRequirementConflict,
			Module:   path,
			Expected: vals[0],
			Actual:   strings.Join(vals[1:], ", "),
			Sources:  sources,
			Detail:   "submodules require different versions of the same module",
		})
	}
	if len(errs) > 0 {
		return errs
	}

	// A requirement is direct for the workspace when any submodule states it
	// without the indirect marker.
	direct := map[string]bool{}
	for _, mf := range w.Modules {
		for _, r := range mf.Requires {
			if !r.Indirect {
				direct[r.Path] = true
			}
		}
	}
	for i := range reqs {
		reqs[i].Indirect = !direct[reqs[i].Path]
	}
	sort.SliceStable(reqs, func(i, j int) bool {
		if reqs[i].Path != reqs[j].Path {
			return reqs[i].Path < reqs[j].Path
		}
		if reqs[i].Version != reqs[j].Version {
			return reqs[i].Version < reqs[j].Version
		}
		return reqs[i].Source < reqs[j].Source
	})
	merged.Requires = reqs
	return nil
}

// mergeReplaces unions replace rules. Rules for the same old path@version
// must agree on the target exactly; disagreement is a replace_conflict
// naming every submodule involved.
func (w *Workspace) mergeReplaces(merged *model.Manifest) []error {
	type ruleKey struct {
		oldPath, oldVersion string
	}
	byKey := map[ruleKey][]struct {
		rule model.ReplaceRule
		src  string
	}{}
	var order []ruleKey

	for _, mf := range w.Modules {
		for _, r := range mf.Replaces {
			k := ruleKey{r.OldPath, r.OldVersion}
			if _, ok := byKey[k]; !ok {
				order = append(order, k)
			}
			byKey[k] = append(byKey[k], struct {
				rule model.ReplaceRule
				src  string
			}{r, mf.ModulePath})
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].oldPath != order[j].oldPath {
			return order[i].oldPath < order[j].oldPath
		}
		return order[i].oldVersion < order[j].oldVersion
	})

	var errs []error
	for _, k := range order {
		entries := byKey[k]
		first := entries[0].rule
		conflict := false
		var sources []string
		for _, e := range entries {
			r := e.rule
			sources = append(sources, fmt.Sprintf("%s: replace %s => %s",
				e.src, ruleLeft(k), ruleRight(r)))
			if r.NewPath != first.NewPath || r.NewVersion != first.NewVersion ||
				r.Local != first.Local {
				conflict = true
			}
		}
		if conflict {
			errs = append(errs, &model.WorkspaceError{
				Kind:     model.WorkspaceReplaceConflict,
				Module:   k.oldPath,
				Expected: ruleRight(first),
				Actual:   "conflicting targets",
				Sources:  sources,
				Detail:   "submodules replace the same module with different targets",
			})
			continue
		}
		merged.Replaces = append(merged.Replaces, first)
	}
	return errs
}

// mergeExcludes unions exclude rules, deduplicating identical entries.
func (w *Workspace) mergeExcludes(merged *model.Manifest) {
	type exKey struct {
		path, version string
	}
	seen := map[exKey]bool{}
	for _, mf := range w.Modules {
		for _, ex := range mf.Excludes {
			k := exKey{ex.Path, ex.Version}
			if seen[k] {
				continue
			}
			seen[k] = true
			merged.Excludes = append(merged.Excludes, ex)
		}
	}
	sort.SliceStable(merged.Excludes, func(i, j int) bool {
		if merged.Excludes[i].Path != merged.Excludes[j].Path {
			return merged.Excludes[i].Path < merged.Excludes[j].Path
		}
		return merged.Excludes[i].Version < merged.Excludes[j].Version
	})
}

func ruleLeft(k struct{ oldPath, oldVersion string }) string {
	if k.oldVersion == "" {
		return k.oldPath
	}
	return k.oldPath + "@" + k.oldVersion
}

func ruleRight(r model.ReplaceRule) string {
	if r.Local {
		return r.NewPath + " (local)"
	}
	return r.NewPath + "@" + r.NewVersion
}

// maxGoVersion picks the highest go directive among submodule manifests
// using numeric dotted comparison, so "1.23" wins over "1.9".
func maxGoVersion(mods []*model.Manifest) string {
	best := ""
	for _, mf := range mods {
		if compareDotted(mf.GoVersion, best) > 0 {
			best = mf.GoVersion
		}
	}
	return best
}

func compareDotted(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			fmt.Sscanf(as[i], "%d", &x)
		}
		if i < len(bs) {
			fmt.Sscanf(bs[i], "%d", &y)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func appendUnique(in []string, s string) []string {
	for _, v := range in {
		if v == s {
			return in
		}
	}
	return append(in, s)
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys2(m map[string]map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
