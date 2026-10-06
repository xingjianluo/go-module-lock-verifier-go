// Package report renders machine-readable (JSON) results for lock, verify
// and audit runs. Field order and finding order are deterministic so CI
// scripts can diff and branch on stable output.
package report

import (
	"encoding/json"
	"sort"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// SchemaVersion is the machine-report schema version.
const SchemaVersion = "1"

// Finding categories. Every finding carries exactly one category so scripts
// can branch without parsing free text.
const (
	CategoryManifest  = "manifest"
	CategoryLock      = "lock"
	CategoryCache     = "cache"
	CategoryWorkspace = "workspace"
)

// Finding is one structured, classified problem.
type Finding struct {
	Category string `json:"category"`
	Kind     string `json:"kind"`
	// Module is the affected dependency module path, when applicable.
	Module string `json:"module,omitempty"`
	// Subject names the workspace submodule or replace source the finding
	// is about, when it is not a dependency module.
	Subject      string   `json:"subject,omitempty"`
	Expected     string   `json:"expected,omitempty"`
	Actual       string   `json:"actual,omitempty"`
	IntroducedBy []string `json:"introduced_by,omitempty"`
	Detail       string   `json:"detail,omitempty"`
}

// Report is the top-level machine-readable result of one run.
type Report struct {
	SchemaVersion string `json:"schema_version"`
	// Command is "lock", "verify" or "audit".
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	// Subject is the module path or workspace name the run was about.
	Subject string `json:"subject"`
	// Modules lists workspace submodule module paths in deterministic
	// order; empty for single-module runs.
	Modules []string `json:"modules,omitempty"`
	// LockedModules counts modules in the resulting/observed lock.
	LockedModules int       `json:"locked_modules"`
	Findings      []Finding `json:"findings"`
}

// FromErrors classifies model errors into findings. The result is sorted
// deterministically by (category, kind, module, expected, actual).
func FromErrors(errs []error) []Finding {
	var out []Finding
	for _, err := range errs {
		out = append(out, classify(err))
	}
	SortFindings(out)
	return out
}

// SortFindings orders findings deterministically in place.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		for _, c := range [][2]string{
			{a.Category, b.Category}, {a.Kind, b.Kind},
			{a.Module, b.Module}, {a.Subject, b.Subject},
			{a.Expected, b.Expected}, {a.Actual, b.Actual},
		} {
			if c[0] != c[1] {
				return c[0] < c[1]
			}
		}
		return false
	})
}

func classify(err error) Finding {
	switch e := err.(type) {
	case *model.RejectError:
		return Finding{
			Category: CategoryManifest, Kind: string(e.Kind),
			Expected: "valid " + e.Directive + " entry",
			Actual:   e.Detail,
			Detail:   "manifest line rejected",
		}
	case *model.LockError:
		return Finding{
			Category: CategoryLock, Kind: string(e.Kind),
			Module: e.Module, Expected: e.Expected, Actual: e.Actual,
			Detail: e.Detail,
		}
	case *model.VerifyError:
		return Finding{
			Category: CategoryCache, Kind: string(e.Kind),
			Module: e.Module, Expected: e.Expected, Actual: e.Actual,
			IntroducedBy: append([]string(nil), e.IntroducedBy...),
			Detail:       e.Detail,
		}
	case *model.WorkspaceError:
		f := Finding{
			Category: CategoryWorkspace, Kind: string(e.Kind),
			Module: e.Module, Subject: e.Subject, Detail: e.Detail,
		}
		for _, en := range e.Entries {
			f.IntroducedBy = append(f.IntroducedBy, en.Module)
		}
		return f
	default:
		return Finding{Category: "error", Kind: "internal", Detail: err.Error()}
	}
}

// Marshal renders the report as deterministic, indented JSON with a trailing
// newline. Struct field order is fixed and Findings are pre-sorted, so the
// same report always produces identical bytes.
func Marshal(r *Report) ([]byte, error) {
	if r.Findings == nil {
		r.Findings = []Finding{}
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
