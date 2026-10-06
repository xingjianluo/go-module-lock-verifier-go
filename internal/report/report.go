// Package report renders processing and audit outcomes as stable,
// machine-readable JSON for CI consumption.
//
// Stability guarantees:
//
//   - object fields always appear in the same order (struct declaration
//     order);
//   - issues are sorted by (category, kind, module, expected, actual) and
//     module entries by path, so the same outcome always serializes to
//     identical bytes;
//   - failures are expressed as structured issues carrying a category, a
//     distinguishable kind, the involved module and expected/actual values,
//     so scripts can branch without parsing free text.
package report

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// Category values classifying every issue.
const (
	CategoryManifest  = "manifest"
	CategoryWorkspace = "workspace"
	CategoryLock      = "lock"
	CategoryCache     = "cache"
	CategoryInternal  = "internal"
)

// Issue is one structured finding.
type Issue struct {
	Category string   `json:"category"`
	Kind     string   `json:"kind"`
	Module   string   `json:"module,omitempty"`
	Expected string   `json:"expected,omitempty"`
	Actual   string   `json:"actual,omitempty"`
	Sources  []string `json:"sources,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// ModuleEntry is the per-module outcome of an audit/verify run.
type ModuleEntry struct {
	Path          string   `json:"path"`
	Status        string   `json:"status"`
	LockedVersion string   `json:"locked_version,omitempty"`
	ActualVersion string   `json:"actual_version,omitempty"`
	LockedHash    string   `json:"locked_hash,omitempty"`
	ActualHash    string   `json:"actual_hash,omitempty"`
	IntroducedBy  []string `json:"introduced_by,omitempty"`
}

// Result is the top-level machine-readable outcome of one command run.
type Result struct {
	// Command is the subcommand that produced this result
	// ("lock", "verify" or "audit").
	Command string `json:"command"`
	// OK is true when the run completed with no issues.
	OK bool `json:"ok"`
	// Subject is the module path or workspace name the run acted on.
	Subject string `json:"subject,omitempty"`
	// Modules lists per-module audit entries, sorted by path.
	Modules []ModuleEntry `json:"modules,omitempty"`
	// ModulesWritten records how many modules a successful lock run pinned.
	ModulesWritten int `json:"modules_written,omitempty"`
	// LockPath records which lock file was written or audited, when any.
	LockPath string `json:"lock_path,omitempty"`
	// Issues lists every finding, sorted; empty when OK is true.
	Issues []Issue `json:"issues,omitempty"`
}

// Classify converts classified model errors into structured issues with a
// deterministic order. Unknown error types become category "internal".
func Classify(errs []error) []Issue {
	out := make([]Issue, 0, len(errs))
	for _, err := range errs {
		out = append(out, classifyOne(err))
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Module != b.Module {
			return a.Module < b.Module
		}
		if a.Expected != b.Expected {
			return a.Expected < b.Expected
		}
		return a.Actual < b.Actual
	})
	return out
}

func classifyOne(err error) Issue {
	switch e := err.(type) {
	case *model.RejectError:
		return Issue{
			Category: CategoryManifest,
			Kind:     string(e.Kind),
			Detail:   fmt.Sprintf("line %d: %s: %s", e.Line, e.Directive, e.Detail),
		}
	case *model.WorkspaceError:
		return Issue{
			Category: CategoryWorkspace,
			Kind:     string(e.Kind),
			Module:   e.Module,
			Expected: e.Expected,
			Actual:   e.Actual,
			Sources:  sortedCopy(e.Sources),
			Detail:   e.Detail,
		}
	case *model.LockError:
		return Issue{
			Category: CategoryLock,
			Kind:     string(e.Kind),
			Module:   e.Module,
			Expected: e.Expected,
			Actual:   e.Actual,
			Detail:   e.Detail,
		}
	case *model.VerifyError:
		return Issue{
			Category: CategoryCache,
			Kind:     string(e.Kind),
			Module:   e.Module,
			Expected: e.Expected,
			Actual:   e.Actual,
			Sources:  sortedCopy(e.IntroducedBy),
			Detail:   e.Detail,
		}
	default:
		return Issue{
			Category: CategoryInternal,
			Kind:     "internal_error",
			Detail:   err.Error(),
		}
	}
}

// sortedCopy returns a sorted copy of in, so issue source lists never
// depend on the producer's emission order.
func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ModulesFromReport converts a diff report into stable module entries.
func ModulesFromReport(rep *model.DiffReport) []ModuleEntry {
	if rep == nil {
		return nil
	}
	out := make([]ModuleEntry, 0, len(rep.Entries))
	for _, e := range rep.Entries {
		out = append(out, ModuleEntry{
			Path:          e.Path,
			Status:        e.Status,
			LockedVersion: e.LockedVer,
			ActualVersion: e.ActualVer,
			LockedHash:    e.LockedHash,
			ActualHash:    e.ActualHash,
			IntroducedBy:  append([]string(nil), e.IntroducedBy...),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Marshal renders the result as indented JSON with a trailing newline.
// The encoding is deterministic: identical Results produce identical bytes.
func Marshal(r *Result) ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
