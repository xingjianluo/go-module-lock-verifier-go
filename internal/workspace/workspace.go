// Package workspace loads a multi-module workspace file and merges the
// declarations of its submodules into one unified, deterministic view.
//
// Workspace file format (line oriented, UTF-8; "#" starts a comment):
//
//	name example.com/my-workspace   (optional; defaults to "workspace")
//	use ./services/api
//	use ./services/worker
//
// Each "use" directory must contain a go.mod. Merging dedupes requirements
// across submodules (the highest requested version wins, per the same
// version precedence the resolver uses), unions exclude rules, and unifies
// replace rules. Replace rules that map the same source (path[@version]) to
// different targets are a hard conflict reported with every contributing
// submodule; nothing is silently picked.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// Module is one submodule referenced by the workspace.
type Module struct {
	// Dir is the submodule directory as written in the workspace file
	// (relative to the workspace root).
	Dir string
	// Manifest is the parsed go.mod of the submodule.
	Manifest *model.Manifest
}

// Workspace is a loaded workspace file plus its parsed submodules.
type Workspace struct {
	// Name is the workspace identity used as the merged module path.
	Name string
	// Root is the absolute directory containing the workspace file.
	Root string
	// Modules lists submodules in the deterministic processing order
	// (sorted by Dir).
	Modules []Module
}

// Merged is the unified declaration set of a workspace, with provenance.
type Merged struct {
	// Manifest is the merged manifest fed to the resolver. Its ModulePath
	// is the workspace name.
	Manifest *model.Manifest
	// RequireSources maps "path@version" to the submodule module paths that
	// declared the requirement, sorted.
	RequireSources map[string][]string
	// ReplaceSources maps a rule key to the submodule module paths that
	// declared the rule, sorted.
	ReplaceSources map[string][]string
	// ExcludeSources maps "path@version" to declaring submodule paths.
	ExcludeSources map[string][]string
}

// Load reads and parses the workspace file at path and parses every
// referenced submodule go.mod. Manifest rejections inside submodules are
// returned as []error (annotated with the submodule); structural problems
// with the workspace file itself come back as *model.WorkspaceError.
func Load(path string) (*Workspace, []error, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read workspace file: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	ws := &Workspace{Name: "workspace", Root: filepath.Dir(abs)}

	seenDir := map[string]int{}
	lineNo := 0
	for _, raw := range strings.Split(string(data), "\n") {
		lineNo++
		line := raw
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "name":
			if len(fields) != 2 {
				return nil, nil, &model.WorkspaceError{
					Kind:   model.WorkspaceMalformed,
					Detail: fmt.Sprintf("line %d: name expects exactly one value", lineNo),
				}
			}
			ws.Name = fields[1]
		case "use":
			if len(fields) != 2 {
				return nil, nil, &model.WorkspaceError{
					Kind:   model.WorkspaceMalformed,
					Detail: fmt.Sprintf("line %d: use expects exactly one directory", lineNo),
				}
			}
			dir := fields[1]
			if prev, dup := seenDir[dir]; dup {
				return nil, nil, &model.WorkspaceError{
					Kind: model.WorkspaceMalformed, Dir: dir,
					Detail: fmt.Sprintf("line %d: duplicate use of %q (first on line %d)", lineNo, dir, prev),
				}
			}
			seenDir[dir] = lineNo
			ws.Modules = append(ws.Modules, Module{Dir: dir})
		default:
			return nil, nil, &model.WorkspaceError{
				Kind:   model.WorkspaceMalformed,
				Detail: fmt.Sprintf("line %d: unknown directive %q", lineNo, fields[0]),
			}
		}
	}
	if len(ws.Modules) == 0 {
		return nil, nil, &model.WorkspaceError{
			Kind:   model.WorkspaceMalformed,
			Detail: "workspace declares no submodules (no use lines)",
		}
	}
	// Deterministic processing order independent of file ordering.
	sort.SliceStable(ws.Modules, func(i, j int) bool { return ws.Modules[i].Dir < ws.Modules[j].Dir })

	var errs []error
	seenModule := map[string]string{}
	for i := range ws.Modules {
		mod := &ws.Modules[i]
		gomod := filepath.Join(ws.Root, filepath.FromSlash(mod.Dir), "go.mod")
		mf, rejects, perr := manifest.ParseFile(gomod)
		if perr != nil {
			errs = append(errs, &model.WorkspaceError{
				Kind: model.WorkspaceMissingModule, Dir: mod.Dir,
				Detail: fmt.Sprintf("cannot read %s: %v", gomod, perr),
			})
			continue
		}
		mod.Manifest = mf
		if prev, dup := seenModule[mf.ModulePath]; dup {
			errs = append(errs, &model.WorkspaceError{
				Kind: model.WorkspaceDuplicateModule, Module: mf.ModulePath, Dir: mod.Dir,
				Detail: fmt.Sprintf("module path also declared by %s", prev),
			})
			continue
		}
		seenModule[mf.ModulePath] = mod.Dir
		for _, r := range rejects {
			if re, ok := r.(*model.RejectError); ok {
				cp := *re
				cp.Detail = fmt.Sprintf("%s (%s): %s", mf.ModulePath, mod.Dir, re.Detail)
				errs = append(errs, &cp)
			} else {
				errs = append(errs, r)
			}
		}
	}
	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		return ws, errs, nil
	}
	return ws, nil, nil
}

// Merge unifies the declarations of all submodules. Conflicting replace
// rules are reported as *model.WorkspaceError of kind
// WorkspaceReplaceConflict; the merged result is still returned for
// inspection but must not be locked when conflicts exist.
func Merge(ws *Workspace) (*Merged, []error) {
	m := &Merged{
		Manifest:       &model.Manifest{ModulePath: ws.Name},
		RequireSources: map[string][]string{},
		ReplaceSources: map[string][]string{},
		ExcludeSources: map[string][]string{},
	}

	type reqKey struct{ path, version string }
	reqSeen := map[reqKey]bool{}
	reqDirect := map[reqKey]bool{}

	type ruleBody struct {
		newPath, newVersion string
		local               bool
	}
	replaceByKey := map[string]model.ReplaceRule{}
	replaceBody := map[string]ruleBody{}
	replaceSrc := map[string][]model.WorkspaceConflictEntry{}
	replaceConflict := map[string]bool{}

	type exKey struct{ path, version string }
	exSeen := map[exKey]bool{}

	goVersion := ""
	for _, mod := range ws.Modules {
		mf := mod.Manifest
		if mf == nil {
			continue
		}
		if compareGoVersion(mf.GoVersion, goVersion) > 0 {
			goVersion = mf.GoVersion
		}
		for _, req := range mf.Requires {
			k := reqKey{req.Path, req.Version}
			m.RequireSources[req.Path+"@"+req.Version] = appendSortedUnique(
				m.RequireSources[req.Path+"@"+req.Version], mf.ModulePath)
			if !req.Indirect {
				reqDirect[k] = true
			}
			if reqSeen[k] {
				continue
			}
			reqSeen[k] = true
			m.Manifest.Requires = append(m.Manifest.Requires, model.Requirement{
				Path: req.Path, Version: req.Version, Line: req.Line,
			})
		}
		for _, r := range mf.Replaces {
			key := r.OldPath + "@" + r.OldVersion
			body := ruleBody{r.NewPath, r.NewVersion, r.Local}
			m.ReplaceSources[key] = appendSortedUnique(m.ReplaceSources[key], mf.ModulePath)
			replaceSrc[key] = append(replaceSrc[key], model.WorkspaceConflictEntry{
				Module: mf.ModulePath, Dir: mod.Dir,
				Rule: renderReplace(r), Line: r.Line,
			})
			if prev, ok := replaceBody[key]; ok {
				if prev != body {
					// Keep the first-seen rule as canonical; the conflict is
					// reported after the loop with every contributor listed.
					replaceConflict[key] = true
				}
				continue
			}
			replaceBody[key] = body
			replaceByKey[key] = r
		}
		for _, ex := range mf.Excludes {
			k := exKey{ex.Path, ex.Version}
			m.ExcludeSources[ex.Path+"@"+ex.Version] = appendSortedUnique(
				m.ExcludeSources[ex.Path+"@"+ex.Version], mf.ModulePath)
			if exSeen[k] {
				continue
			}
			exSeen[k] = true
			m.Manifest.Excludes = append(m.Manifest.Excludes, ex)
		}
	}
	m.Manifest.GoVersion = goVersion

	// Deterministic ordering of the merged declaration lists.
	sort.SliceStable(m.Manifest.Requires, func(i, j int) bool {
		a, b := m.Manifest.Requires[i], m.Manifest.Requires[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Version < b.Version
	})
	for i := range m.Manifest.Requires {
		k := reqKey{m.Manifest.Requires[i].Path, m.Manifest.Requires[i].Version}
		m.Manifest.Requires[i].Indirect = !reqDirect[k]
	}
	sort.SliceStable(m.Manifest.Excludes, func(i, j int) bool {
		a, b := m.Manifest.Excludes[i], m.Manifest.Excludes[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Version < b.Version
	})
	replaceKeys := make([]string, 0, len(replaceByKey))
	for k := range replaceByKey {
		replaceKeys = append(replaceKeys, k)
	}
	sort.Strings(replaceKeys)
	for _, k := range replaceKeys {
		m.Manifest.Replaces = append(m.Manifest.Replaces, replaceByKey[k])
	}

	var errs []error
	for _, k := range replaceKeys {
		if !replaceConflict[k] {
			continue
		}
		entries := replaceSrc[k]
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].Module != entries[j].Module {
				return entries[i].Module < entries[j].Module
			}
			return entries[i].Rule < entries[j].Rule
		})
		errs = append(errs, &model.WorkspaceError{
			Kind:    model.WorkspaceReplaceConflict,
			Subject: k,
			Entries: entries,
			Detail:  "submodules map the same replace source to different targets",
		})
	}
	return m, errs
}

// appendSortedUnique appends s to the list keeping it sorted and deduplicated.
func appendSortedUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	list = append(list, s)
	sort.Strings(list)
	return list
}

// renderReplace renders a replace rule in go.mod syntax for error reporting.
func renderReplace(r model.ReplaceRule) string {
	left := r.OldPath
	if r.OldVersion != "" {
		left += " " + r.OldVersion
	}
	right := r.NewPath
	if !r.Local && r.NewVersion != "" {
		right += " " + r.NewVersion
	}
	return left + " => " + right
}

// compareGoVersion compares "1.22" style go directives numerically.
func compareGoVersion(a, b string) int {
	pa := strings.SplitN(a, ".", 3)
	pb := strings.SplitN(b, ".", 3)
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
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
