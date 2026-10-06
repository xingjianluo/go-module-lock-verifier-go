// Package workspace loads a multi-module workspace description and merges
// the submodule manifests into one effective declaration set.
//
// Workspace file format (default name golock.work), line oriented:
//
//	name example.com/workspace   // optional; default "workspace"
//	go 1.23                      // optional; default max of submodule versions
//	use ./mod-a
//	use (
//		./mod-b
//		./mod-c
//	)
//
// Merge semantics:
//
//   - identical requirements (same path+version) are deduplicated; every
//     submodule that stated one is recorded as an introducer;
//   - the same module required at different versions by different
//     submodules is a requirement_conflict error naming every submodule —
//     the merge never silently picks one;
//   - replace rules for the same old path@version must agree on the target
//     exactly, otherwise replace_conflict; identical rules are deduplicated;
//   - exclude rules are unioned and deduplicated;
//   - the merged result is sorted, so the same workspace always merges to
//     byte-identical declarations.
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

// DefaultFileName is the workspace file name looked up by default.
const DefaultFileName = "golock.work"

// Load reads and parses the workspace file at path, then parses every
// submodule go.mod. Syntax problems in the workspace file come back as
// *model.RejectError; unreadable or rejected submodule manifests come back
// as *model.WorkspaceError with kind module_unreadable.
func Load(path string) (*Workspace, []error, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read workspace file: %w", err)
	}
	w := &Workspace{Dir: filepath.Dir(path)}
	rejects, err := parseFile(w, data)
	if err != nil {
		return nil, nil, err
	}
	var errs []error
	errs = append(errs, rejects...)

	seenModule := map[string]string{}
	for _, dir := range w.UseDirs {
		modPath := filepath.Join(w.Dir, dir, "go.mod")
		mf, mrejects, merr := manifest.ParseFile(modPath)
		if merr != nil {
			errs = append(errs, &model.WorkspaceError{
				Kind:   model.WorkspaceModuleUnreadable,
				Module: dir,
				Detail: fmt.Sprintf("cannot read %s: %v", modPath, merr),
			})
			continue
		}
		if len(mrejects) > 0 {
			parts := make([]string, 0, len(mrejects))
			for _, r := range mrejects {
				parts = append(parts, r.Error())
			}
			errs = append(errs, &model.WorkspaceError{
				Kind:   model.WorkspaceModuleUnreadable,
				Module: dir,
				Detail: fmt.Sprintf("manifest rejected: %s", strings.Join(parts, "; ")),
			})
			continue
		}
		if prev, dup := seenModule[mf.ModulePath]; dup {
			errs = append(errs, &model.WorkspaceError{
				Kind:    model.WorkspaceDuplicateModule,
				Module:  mf.ModulePath,
				Sources: []string{prev, dir},
				Detail:  "two workspace entries declare the same module path",
			})
			continue
		}
		seenModule[mf.ModulePath] = dir
		w.Modules = append(w.Modules, mf)
	}
	if len(errs) > 0 {
		sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
		return w, errs, nil
	}
	return w, nil, nil
}

// parseFile parses the workspace file body into w (Name, GoVersion,
// UseDirs). UseDirs is deduplicated and sorted before return.
func parseFile(w *Workspace, data []byte) ([]error, error) {
	src := manifest.StripBlockComments(data)
	lines := strings.Split(string(src), "\n")

	var rejects []error
	inUseBlock := false
	seenDir := map[string]bool{}

	addUse := func(dir string, line int) {
		if dir == "" {
			rejects = append(rejects, &model.RejectError{
				Kind: model.RejectIncompleteEntry, Line: line, Directive: "use",
				Detail: "use entry without a directory",
			})
			return
		}
		if seenDir[dir] {
			return // duplicate use entries are idempotent
		}
		seenDir[dir] = true
		w.UseDirs = append(w.UseDirs, dir)
	}

	for idx, raw := range lines {
		lineNo := idx + 1
		toks, _, ok := manifest.TokenizeLine(raw)
		if !ok {
			rejects = append(rejects, &model.RejectError{
				Kind: model.RejectBadDirective, Line: lineNo,
				Detail: "unterminated quoted string",
			})
			continue
		}
		if len(toks) == 0 {
			continue
		}
		if inUseBlock {
			if toks[0] == ")" {
				inUseBlock = false
				continue
			}
			if len(toks) != 1 {
				rejects = append(rejects, &model.RejectError{
					Kind: model.RejectIncompleteEntry, Line: lineNo, Directive: "use",
					Detail: "use block entries take exactly one directory",
				})
				continue
			}
			addUse(toks[0], lineNo)
			continue
		}
		switch toks[0] {
		case "name":
			if len(toks) != 2 {
				rejects = append(rejects, &model.RejectError{
					Kind: model.RejectIncompleteEntry, Line: lineNo, Directive: "name",
					Detail: "name expects exactly one module path",
				})
			} else {
				w.Name = toks[1]
			}
		case "go":
			if len(toks) != 2 {
				rejects = append(rejects, &model.RejectError{
					Kind: model.RejectIncompleteEntry, Line: lineNo, Directive: "go",
					Detail: "go expects exactly one version",
				})
			} else {
				w.GoVersion = toks[1]
			}
		case "use":
			if len(toks) >= 2 && toks[1] == "(" {
				inUseBlock = true
				continue
			}
			if len(toks) != 2 {
				rejects = append(rejects, &model.RejectError{
					Kind: model.RejectIncompleteEntry, Line: lineNo, Directive: "use",
					Detail: "use expects exactly one directory",
				})
				continue
			}
			addUse(toks[1], lineNo)
		default:
			rejects = append(rejects, &model.RejectError{
				Kind: model.RejectBadDirective, Line: lineNo, Directive: toks[0],
				Detail: fmt.Sprintf("unknown workspace directive %q", toks[0]),
			})
		}
	}
	if inUseBlock {
		rejects = append(rejects, &model.RejectError{
			Kind: model.RejectIncompleteEntry, Line: len(lines) + 1, Directive: "use",
			Detail: "unterminated use block",
		})
	}
	sort.Strings(w.UseDirs)
	return rejects, nil
}

// Workspace is a loaded multi-module workspace.
type Workspace struct {
	// Name is the workspace identity used as the merged module path.
	Name string
	// GoVersion is the effective go directive for the merged manifest.
	GoVersion string
	// Dir is the directory containing the workspace file; use paths are
	// resolved relative to it.
	Dir string
	// UseDirs lists the submodule directories as written, sorted.
	UseDirs []string
	// Modules holds the parsed submodule manifests in UseDirs order. When
	// Load reports errors, only the successfully parsed modules are present.
	Modules []*model.Manifest
}
