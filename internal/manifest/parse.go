// Package manifest parses go.mod files into the internal model and rejects
// malformed entries with specific, distinguishable reasons.
package manifest

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// canonical module version: vMAJOR.MINOR.PATCH with optional prerelease /
// build metadata / +incompatible. Go pseudo-versions match as prerelease
// strings (e.g. v0.0.0-20210101000000-abcdef123456).
var moduleVersionRe = regexp.MustCompile(
	`^v\d+\.\d+\.\d+(?:[-+][0-9A-Za-z]+(?:\.[0-9A-Za-z]+)*)?$`)

var goVersionRe = regexp.MustCompile(`^\d+\.\d+(?:\.\d+)?(?:[a-z]+\d*)?$`)

var modulePathRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-~]*(?:/[A-Za-z0-9][A-Za-z0-9._\-~]*)*$`)

// Parse reads go.mod text and returns the normalized manifest.
//
// Every unacceptable entry is returned as a *model.RejectError carrying one
// of the mutually distinct RejectKind values; valid entries are never
// silently dropped. The returned manifest may be partially populated when
// rejects exist; callers decide whether to proceed.
func Parse(data []byte) (*model.Manifest, []error, error) {
	src := stripBlockComments(data)
	lines := strings.Split(string(src), "\n")

	mf := &model.Manifest{}
	var rejects []error

	// block is "" or the directive owning the current parenthesized block.
	block := ""
	seenReplace := map[string]int{}

	for idx, raw := range lines {
		lineNo := idx + 1
		toks, comment, ok := tokenizeLine(raw)
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

		// Inside a parenthesized block.
		if block != "" {
			if toks[0] == ")" {
				block = ""
				continue
			}
			switch block {
			case "require":
				parseRequireEntry(mf, &rejects, toks, comment, lineNo)
			case "replace":
				parseReplaceEntry(mf, &rejects, seenReplace, toks, lineNo)
			case "exclude":
				parseExcludeEntry(mf, &rejects, toks, lineNo)
			default:
				rejects = append(rejects, rejectf(lineNo, "", model.RejectBadDirective,
					"unexpected entry inside %q block", block))
			}
			continue
		}

		switch toks[0] {
		case "module":
			if len(toks) < 2 {
				rejects = append(rejects, rejectf(lineNo, "module", model.RejectIncompleteEntry,
					"module directive requires a path"))
			} else if !validModulePath(toks[1]) {
				rejects = append(rejects, rejectf(lineNo, "module", model.RejectMissingPath,
					"invalid module path %q", toks[1]))
			} else {
				mf.ModulePath = toks[1]
			}
		case "go":
			if len(toks) < 2 {
				rejects = append(rejects, rejectf(lineNo, "go", model.RejectIncompleteEntry,
					"go directive requires a version"))
			} else if !goVersionRe.MatchString(toks[1]) {
				rejects = append(rejects, rejectf(lineNo, "go", model.RejectMalformedVersion,
					"invalid go version %q", toks[1]))
			} else {
				mf.GoVersion = toks[1]
			}
		case "toolchain":
			// Informational only; tolerate a single bare/quoted token.
		case "require":
			if len(toks) >= 2 && toks[1] == "(" {
				block = "require"
				continue
			}
			parseRequireEntry(mf, &rejects, toks[1:], comment, lineNo)
		case "replace":
			if len(toks) >= 2 && toks[1] == "(" {
				block = "replace"
				continue
			}
			parseReplaceEntry(mf, &rejects, seenReplace, toks[1:], lineNo)
		case "exclude":
			if len(toks) >= 2 && toks[1] == "(" {
				block = "exclude"
				continue
			}
			parseExcludeEntry(mf, &rejects, toks[1:], lineNo)
		default:
			rejects = append(rejects, rejectf(lineNo, toks[0], model.RejectBadDirective,
				"unknown directive %q", toks[0]))
		}
	}

	if block != "" {
		rejects = append(rejects, rejectf(len(lines)+1, block, model.RejectIncompleteEntry,
			"unterminated %q block", block))
	}

	sortManifest(mf)
	return mf, rejects, nil
}

// ParseFile reads and parses the go.mod file at path.
func ParseFile(path string) (*model.Manifest, []error, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return Parse(data)
}

func parseRequireEntry(mf *model.Manifest, rejects *[]error, toks []string, comment string, line int) {
	if len(toks) == 0 {
		*rejects = append(*rejects, rejectf(line, "require", model.RejectIncompleteEntry,
			"empty require entry"))
		return
	}
	path := toks[0]
	if strings.TrimSpace(path) == "" {
		*rejects = append(*rejects, rejectf(line, "require", model.RejectMissingPath,
			"require entry without module path"))
		return
	}
	if !validModulePath(path) {
		*rejects = append(*rejects, rejectf(line, "require", model.RejectMissingPath,
			"invalid module path %q", path))
		return
	}
	if len(toks) < 2 {
		*rejects = append(*rejects, rejectf(line, "require", model.RejectMissingVersion,
			"require %s is missing a version", path))
		return
	}
	ver := toks[1]
	if !ValidModuleVersion(ver) {
		*rejects = append(*rejects, rejectf(line, "require", model.RejectMalformedVersion,
			"require %s has invalid version %q", path, ver))
		return
	}
	mf.Requires = append(mf.Requires, model.Requirement{
		Path:     path,
		Version:  ver,
		Indirect: strings.TrimSpace(comment) == "indirect",
		Line:     line,
	})
}

func parseReplaceEntry(mf *model.Manifest, rejects *[]error, seen map[string]int, toks []string, line int) {
	if len(toks) == 0 {
		*rejects = append(*rejects, rejectf(line, "replace", model.RejectIncompleteEntry,
			"empty replace entry"))
		return
	}
	arrow := -1
	for i, t := range toks {
		if t == "=>" {
			arrow = i
			break
		}
	}
	if arrow < 0 {
		*rejects = append(*rejects, rejectf(line, "replace", model.RejectIncompleteEntry,
			"replace entry is missing the \"=>\" separator"))
		return
	}
	left, right := toks[:arrow], toks[arrow+1:]
	if len(left) == 0 || len(right) == 0 {
		*rejects = append(*rejects, rejectf(line, "replace", model.RejectIncompleteEntry,
			"replace entry needs modules on both sides of \"=>\""))
		return
	}
	if !validModulePath(left[0]) {
		*rejects = append(*rejects, rejectf(line, "replace", model.RejectMissingPath,
			"replace has invalid old path %q", left[0]))
		return
	}
	rule := model.ReplaceRule{OldPath: left[0], Line: line}
	if len(left) > 1 {
		if !ValidModuleVersion(left[1]) {
			*rejects = append(*rejects, rejectf(line, "replace", model.RejectMalformedVersion,
				"replace %s has invalid old version %q", rule.OldPath, left[1]))
			return
		}
		rule.OldVersion = left[1]
	}
	if len(left) > 2 {
		*rejects = append(*rejects, rejectf(line, "replace", model.RejectReplaceConflict,
			"replace of %s has too many arguments before \"=>\"", rule.OldPath))
		return
	}

	if !validModulePath(right[0]) && !isLocalPath(right[0]) {
		*rejects = append(*rejects, rejectf(line, "replace", model.RejectMissingPath,
			"replace %s has invalid new path %q", rule.OldPath, right[0]))
		return
	}
	if isLocalPath(right[0]) {
		rule.Local = true
		rule.NewPath = right[0]
		if len(right) > 1 {
			*rejects = append(*rejects, rejectf(line, "replace", model.RejectReplaceConflict,
				"local replacement %s cannot carry a version", rule.NewPath))
			return
		}
	} else {
		rule.NewPath = right[0]
		if len(right) < 2 {
			*rejects = append(*rejects, rejectf(line, "replace", model.RejectMissingVersion,
				"replace %s => %s is missing the new version", rule.OldPath, rule.NewPath))
			return
		}
		if !ValidModuleVersion(right[1]) {
			*rejects = append(*rejects, rejectf(line, "replace", model.RejectMalformedVersion,
				"replace %s has invalid new version %q", rule.OldPath, right[1]))
			return
		}
		rule.NewVersion = right[1]
		if len(right) > 2 {
			*rejects = append(*rejects, rejectf(line, "replace", model.RejectReplaceConflict,
				"replace of %s has too many arguments after \"=>\"", rule.OldPath))
			return
		}
	}

	key := rule.OldPath + "@" + rule.OldVersion
	if first, dup := seen[key]; dup {
		*rejects = append(*rejects, rejectf(line, "replace", model.RejectDuplicateReplace,
			"replace of %s duplicates the rule on line %d", key, first))
		return
	}
	seen[key] = line
	mf.Replaces = append(mf.Replaces, rule)
}

func parseExcludeEntry(mf *model.Manifest, rejects *[]error, toks []string, line int) {
	if len(toks) == 0 {
		*rejects = append(*rejects, rejectf(line, "exclude", model.RejectIncompleteEntry,
			"empty exclude entry"))
		return
	}
	path := toks[0]
	if !validModulePath(path) {
		*rejects = append(*rejects, rejectf(line, "exclude", model.RejectMissingPath,
			"invalid module path %q", path))
		return
	}
	if len(toks) < 2 {
		*rejects = append(*rejects, rejectf(line, "exclude", model.RejectExcludeNoVersion,
			"exclude %s is missing a version", path))
		return
	}
	if !ValidModuleVersion(toks[1]) {
		*rejects = append(*rejects, rejectf(line, "exclude", model.RejectMalformedVersion,
			"exclude %s has invalid version %q", path, toks[1]))
		return
	}
	mf.Excludes = append(mf.Excludes, model.ExcludeRule{
		Path: path, Version: toks[1], Line: line,
	})
}

func sortManifest(mf *model.Manifest) {
	sort.SliceStable(mf.Requires, func(i, j int) bool {
		if mf.Requires[i].Path != mf.Requires[j].Path {
			return mf.Requires[i].Path < mf.Requires[j].Path
		}
		return mf.Requires[i].Version < mf.Requires[j].Version
	})
	sort.SliceStable(mf.Replaces, func(i, j int) bool {
		if mf.Replaces[i].OldPath != mf.Replaces[j].OldPath {
			return mf.Replaces[i].OldPath < mf.Replaces[j].OldPath
		}
		return mf.Replaces[i].OldVersion < mf.Replaces[j].OldVersion
	})
	sort.SliceStable(mf.Excludes, func(i, j int) bool {
		if mf.Excludes[i].Path != mf.Excludes[j].Path {
			return mf.Excludes[i].Path < mf.Excludes[j].Path
		}
		return mf.Excludes[i].Version < mf.Excludes[j].Version
	})
}

func rejectf(line int, dir string, kind model.RejectKind, format string, args ...any) error {
	return &model.RejectError{
		Kind: kind, Line: line, Directive: dir,
		Detail: fmt.Sprintf(format, args...),
	}
}

func isLocalPath(s string) bool {
	return strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "/") || s == "." || s == ".."
}

func validModulePath(s string) bool {
	return s != "" && modulePathRe.MatchString(s)
}

// ValidModuleVersion reports whether v is a canonical, acceptable module
// version (semver, pseudo-version, or semver+incompatible).
func ValidModuleVersion(v string) bool {
	if strings.HasSuffix(v, "+incompatible") {
		v = strings.TrimSuffix(v, "+incompatible")
	}
	return moduleVersionRe.MatchString(v)
}
