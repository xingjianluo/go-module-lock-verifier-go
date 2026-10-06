package workspace

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

// writeWorkspace lays out a workspace file and submodule go.mod files.
func writeWorkspace(t *testing.T, workFile string, mods map[string]string) string {
	t.Helper()
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "golock.work"), workFile)
	for dir, gomod := range mods {
		testutil.WriteFile(t, filepath.Join(root, dir, "go.mod"), gomod)
	}
	return filepath.Join(root, "golock.work")
}

func loadAndMerge(t *testing.T, workPath string) (*Workspace, *Merged) {
	t.Helper()
	ws, loadErrs, err := Load(workPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loadErrs) > 0 {
		t.Fatalf("Load errors: %v", loadErrs)
	}
	merged, mergeErrs := Merge(ws)
	if len(mergeErrs) > 0 {
		t.Fatalf("Merge conflicts: %v", mergeErrs)
	}
	return ws, merged
}

func TestMergeDedupesRequirementsAcrossModules(t *testing.T) {
	workPath := writeWorkspace(t, `name example.com/ws
use ./mod-a
use ./mod-b
`, map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nrequire example.com/dep v1.2.0\nrequire example.com/only-a v1.0.0\n",
		"mod-b": "module example.com/b\ngo 1.23\nrequire example.com/dep v1.3.0\nrequire example.com/only-b v0.9.0 // indirect\n",
	})
	_, merged := loadAndMerge(t, workPath)

	t.Logf("merged requires: %+v", merged.Manifest.Requires)
	if got := len(merged.Manifest.Requires); got != 4 {
		t.Fatalf("expected 4 merged requirements, got %d", got)
	}
	// Both versions of example.com/dep survive the merge; the resolver picks
	// the highest later. Provenance records both submodules.
	src := merged.RequireSources["example.com/dep@v1.2.0"]
	if len(src) != 1 || src[0] != "example.com/a" {
		t.Fatalf("dep@v1.2.0 sources = %v", src)
	}
	src = merged.RequireSources["example.com/dep@v1.3.0"]
	if len(src) != 1 || src[0] != "example.com/b" {
		t.Fatalf("dep@v1.3.0 sources = %v", src)
	}
	// Indirect in every declarer stays indirect; direct anywhere is direct.
	for _, r := range merged.Manifest.Requires {
		if r.Path == "example.com/only-b" && !r.Indirect {
			t.Fatalf("only-b should stay indirect")
		}
		if r.Path == "example.com/dep" && r.Indirect {
			t.Fatalf("dep should be direct")
		}
	}
	// Go directive takes the highest declared version.
	if merged.Manifest.GoVersion != "1.23" {
		t.Fatalf("GoVersion = %q, want 1.23", merged.Manifest.GoVersion)
	}
	if merged.Manifest.ModulePath != "example.com/ws" {
		t.Fatalf("ModulePath = %q", merged.Manifest.ModulePath)
	}
}

func TestMergeReplaceConflictReportsAllSources(t *testing.T) {
	workPath := writeWorkspace(t, `use ./mod-a
use ./mod-b
`, map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nrequire example.com/dep v1.2.0\nreplace example.com/dep => example.com/fork v1.2.1\n",
		"mod-b": "module example.com/b\ngo 1.22\nrequire example.com/dep v1.2.0\nreplace example.com/dep => ../local/dep\n",
	})
	ws, loadErrs, err := Load(workPath)
	if err != nil || len(loadErrs) > 0 {
		t.Fatalf("Load: %v %v", err, loadErrs)
	}
	_, mergeErrs := Merge(ws)
	if len(mergeErrs) != 1 {
		t.Fatalf("expected 1 conflict, got %v", mergeErrs)
	}
	we, ok := mergeErrs[0].(*model.WorkspaceError)
	if !ok {
		t.Fatalf("error type %T", mergeErrs[0])
	}
	t.Logf("conflict: kind=%s subject=%s entries=%+v", we.Kind, we.Subject, we.Entries)
	if we.Kind != model.WorkspaceReplaceConflict {
		t.Fatalf("kind = %s", we.Kind)
	}
	if we.Subject != "example.com/dep@" {
		t.Fatalf("subject = %q", we.Subject)
	}
	if len(we.Entries) != 2 {
		t.Fatalf("expected both submodules listed, got %+v", we.Entries)
	}
	var mods []string
	for _, e := range we.Entries {
		mods = append(mods, e.Module)
		if e.Rule == "" || e.Dir == "" {
			t.Fatalf("entry missing rule/dir: %+v", e)
		}
	}
	if mods[0] != "example.com/a" || mods[1] != "example.com/b" {
		t.Fatalf("contributors = %v", mods)
	}
}

func TestMergeIdenticalReplaceRulesDedupe(t *testing.T) {
	workPath := writeWorkspace(t, `use ./mod-a
use ./mod-b
`, map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nrequire example.com/dep v1.2.0\nreplace example.com/dep => example.com/fork v1.2.1\n",
		"mod-b": "module example.com/b\ngo 1.22\nrequire example.com/dep v1.2.0\nreplace example.com/dep => example.com/fork v1.2.1\n",
	})
	_, merged := loadAndMerge(t, workPath)
	if got := len(merged.Manifest.Replaces); got != 1 {
		t.Fatalf("expected 1 deduped replace, got %d", got)
	}
	src := merged.ReplaceSources["example.com/dep@"]
	if len(src) != 2 {
		t.Fatalf("replace provenance = %v, want both submodules", src)
	}
}

func TestMergeExcludesUnion(t *testing.T) {
	workPath := writeWorkspace(t, `use ./mod-a
use ./mod-b
`, map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nexclude example.com/dep v1.1.0\n",
		"mod-b": "module example.com/b\ngo 1.22\nexclude example.com/dep v1.2.0\nexclude example.com/dep v1.1.0\n",
	})
	_, merged := loadAndMerge(t, workPath)
	if got := len(merged.Manifest.Excludes); got != 2 {
		t.Fatalf("expected 2 unioned excludes, got %d: %+v", got, merged.Manifest.Excludes)
	}
	src := merged.ExcludeSources["example.com/dep@v1.1.0"]
	if len(src) != 2 {
		t.Fatalf("exclude provenance = %v, want both submodules", src)
	}
}

func TestMergeDeterministicAcrossRuns(t *testing.T) {
	workPath := writeWorkspace(t, `name example.com/ws
use ./mod-b
use ./mod-a
`, map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nrequire example.com/z v1.0.0\nrequire example.com/dep v1.2.0\n",
		"mod-b": "module example.com/b\ngo 1.22\nrequire example.com/dep v1.3.0\nrequire example.com/y v0.1.0\nexclude example.com/z v0.9.0\n",
	})
	_, m1 := loadAndMerge(t, workPath)
	_, m2 := loadAndMerge(t, workPath)
	h1 := manifest.CanonicalHash(m1.Manifest)
	h2 := manifest.CanonicalHash(m2.Manifest)
	t.Logf("run1 hash=%s\nrun2 hash=%s", h1, h2)
	if h1 != h2 {
		t.Fatalf("repeated merge produced different manifests:\n%s\n%s",
			manifest.CanonicalText(m1.Manifest), manifest.CanonicalText(m2.Manifest))
	}
	// Submodule processing order is sorted by dir, not file order.
	ws, _, _ := Load(workPath)
	if ws.Modules[0].Dir != "./mod-a" || ws.Modules[1].Dir != "./mod-b" {
		t.Fatalf("module order = %v, %v", ws.Modules[0].Dir, ws.Modules[1].Dir)
	}
}

func TestWorkspaceAddRemoveModuleReflected(t *testing.T) {
	mods := map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nrequire example.com/dep v1.2.0\n",
		"mod-b": "module example.com/b\ngo 1.22\nrequire example.com/extra v2.0.0\n",
	}
	withB := writeWorkspace(t, "use ./mod-a\nuse ./mod-b\n", mods)
	_, merged := loadAndMerge(t, withB)
	if got := len(merged.Manifest.Requires); got != 2 {
		t.Fatalf("with mod-b: %d requirements", got)
	}
	hWith := manifest.CanonicalHash(merged.Manifest)

	withoutB := writeWorkspace(t, "use ./mod-a\n", mods)
	_, merged2 := loadAndMerge(t, withoutB)
	t.Logf("with mod-b: %d reqs hash=%s; without: %d reqs hash=%s",
		len(merged.Manifest.Requires), hWith, len(merged2.Manifest.Requires),
		manifest.CanonicalHash(merged2.Manifest))
	if got := len(merged2.Manifest.Requires); got != 1 {
		t.Fatalf("without mod-b: %d requirements, want exactly 1 (no leftovers)", got)
	}
	if merged2.Manifest.Requires[0].Path != "example.com/dep" {
		t.Fatalf("unexpected leftover requirement: %+v", merged2.Manifest.Requires[0])
	}
	if hWith == manifest.CanonicalHash(merged2.Manifest) {
		t.Fatalf("hash did not change after removing a submodule")
	}
}

func TestMergeAcceptsAllLegalVersionForms(t *testing.T) {
	workPath := writeWorkspace(t, `use ./mod-a
use ./mod-b
`, map[string]string{
		"mod-a": `module example.com/a
go 1.22
require (
	example.com/pre v1.2.3-rc.1
	example.com/meta v1.2.3+build.5
	example.com/pseudo v0.0.0-20210101000000-abcdef123456
)
`,
		"mod-b": `module example.com/b
go 1.22
require (
	example.com/inc v2.0.0+incompatible
	example.com/pre2 v1.2.3-0.20210101000000-abcdef123456
)
`,
	})
	_, merged := loadAndMerge(t, workPath)
	t.Logf("merged requires: %+v", merged.Manifest.Requires)
	if got := len(merged.Manifest.Requires); got != 5 {
		t.Fatalf("legal version forms rejected or lost: got %d requirements: %+v",
			got, merged.Manifest.Requires)
	}
}

func TestLoadRejectsMalformedWorkspace(t *testing.T) {
	workPath := writeWorkspace(t, "use\n", map[string]string{})
	_, _, err := Load(workPath)
	we, ok := err.(*model.WorkspaceError)
	if !ok || we.Kind != model.WorkspaceMalformed {
		t.Fatalf("err = %v", err)
	}
	t.Logf("malformed workspace: %v", we)
}

func TestLoadRejectsDuplicateModulePath(t *testing.T) {
	workPath := writeWorkspace(t, "use ./mod-a\nuse ./mod-b\n", map[string]string{
		"mod-a": "module example.com/same\ngo 1.22\n",
		"mod-b": "module example.com/same\ngo 1.22\n",
	})
	_, errs, err := Load(workPath)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
	we := errs[0].(*model.WorkspaceError)
	t.Logf("duplicate: kind=%s module=%s detail=%s", we.Kind, we.Module, we.Detail)
	if we.Kind != model.WorkspaceDuplicateModule || we.Module != "example.com/same" {
		t.Fatalf("unexpected: %+v", we)
	}
}

func TestLoadReportsSubmoduleManifestRejects(t *testing.T) {
	workPath := writeWorkspace(t, "use ./mod-a\n", map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nrequire example.com/bad not-a-version\n",
	})
	_, errs, err := Load(workPath)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
	re, ok := errs[0].(*model.RejectError)
	if !ok {
		t.Fatalf("err type %T: %v", errs[0], errs[0])
	}
	t.Logf("submodule reject: kind=%s detail=%s", re.Kind, re.Detail)
	if re.Kind != model.RejectMalformedVersion {
		t.Fatalf("kind = %s", re.Kind)
	}
	if !strings.Contains(re.Detail, "example.com/a") {
		t.Fatalf("reject not attributed to submodule: %s", re.Detail)
	}
}

func TestAttributeRewritesWorkspaceIntroducer(t *testing.T) {
	workPath := writeWorkspace(t, `name example.com/ws
use ./mod-a
use ./mod-b
`, map[string]string{
		"mod-a": "module example.com/a\ngo 1.22\nrequire example.com/dep v1.2.0\n",
		"mod-b": "module example.com/b\ngo 1.22\nrequire example.com/dep v1.3.0\nrequire example.com/other v0.1.0\n",
	})
	_, merged := loadAndMerge(t, workPath)

	modules := []model.LockedModule{
		{Path: "example.com/dep", Version: "v1.3.0",
			IntroducedBy: []string{"example.com/ws"}},
		{Path: "example.com/other", Version: "v0.1.0",
			IntroducedBy: []string{"example.com/ws"}},
		{Path: "example.com/transitive", Version: "v0.0.1",
			IntroducedBy: []string{"example.com/dep", "example.com/ws"}},
	}
	merged.Attribute(modules)
	for _, m := range modules {
		t.Logf("%s introduced by %v", m.Path, m.IntroducedBy)
	}
	// dep is required by both submodules (at different versions): both named.
	if got := modules[0].IntroducedBy; len(got) != 2 ||
		got[0] != "example.com/a" || got[1] != "example.com/b" {
		t.Fatalf("dep introducedBy = %v", got)
	}
	// other is required only by mod-b.
	if got := modules[1].IntroducedBy; len(got) != 1 || got[0] != "example.com/b" {
		t.Fatalf("other introducedBy = %v", got)
	}
	// Transitive introducers that are real modules stay untouched.
	if got := modules[2].IntroducedBy; len(got) != 1 || got[0] != "example.com/dep" {
		t.Fatalf("transitive introducedBy = %v", got)
	}
}
