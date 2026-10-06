package workspace

import (
	"strings"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

// setupWorkspace writes a workspace file and one go.mod per submodule.
func setupWorkspace(t *testing.T, workFile string, mods map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for dir, gomod := range mods {
		testutil.WriteFile(t, root+"/"+dir+"/go.mod", gomod)
	}
	testutil.WriteFile(t, root+"/golock.work", workFile)
	return root + "/golock.work"
}

func loadOK(t *testing.T, path string) *Workspace {
	t.Helper()
	ws, errs, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(errs) > 0 {
		t.Fatalf("load errors: %v", errs)
	}
	return ws
}

func mergeOK(t *testing.T, ws *Workspace) *model.Manifest {
	t.Helper()
	mf, errs := ws.Merge()
	if len(errs) > 0 {
		t.Fatalf("merge errors: %v", errs)
	}
	if mf == nil {
		t.Fatal("merge returned nil manifest without errors")
	}
	return mf
}

func wsErrKind(errs []error, kind model.WorkspaceErrorKind) *model.WorkspaceError {
	for _, e := range errs {
		if we, ok := e.(*model.WorkspaceError); ok && we.Kind == kind {
			return we
		}
	}
	return nil
}

// TestMergeDedupesAndAttributes covers cross-submodule dedup: identical
// requirements collapse, and every submodule that stated one is recorded as
// an introducer (Source), so "who pulled this in" survives the merge.
func TestMergeDedupesAndAttributes(t *testing.T) {
	path := setupWorkspace(t,
		"name example.com/ws\ngo 1.23\nuse (\n\t./mod-a\n\t./mod-b\n)\n",
		map[string]string{
			"mod-a": "module example.com/a\ngo 1.23\nrequire example.com/dep v1.2.0\nrequire example.com/only-a v0.1.0\n",
			"mod-b": "module example.com/b\ngo 1.23\nrequire example.com/dep v1.2.0\nrequire example.com/only-b v0.2.0 // indirect\n",
		})
	ws := loadOK(t, path)
	mf := mergeOK(t, ws)

	if mf.ModulePath != "example.com/ws" {
		t.Fatalf("merged module path = %q", mf.ModulePath)
	}
	var depSources []string
	for _, r := range mf.Requires {
		t.Logf("MERGED require %s %s (source=%s indirect=%v)", r.Path, r.Version, r.Source, r.Indirect)
		if r.Path == "example.com/dep" {
			depSources = append(depSources, r.Source)
		}
		if r.Path == "example.com/only-b" && !r.Indirect {
			t.Fatalf("only-b must stay indirect")
		}
	}
	if len(depSources) != 2 {
		t.Fatalf("dep must keep one entry per introducing submodule, got %v", depSources)
	}
	t.Logf("JUDGEMENT dep introduced by %v", depSources)
}

// TestRequirementConflict ensures differing versions for the same module
// across submodules are reported explicitly, naming both submodules, and
// never silently resolved by picking one.
func TestRequirementConflict(t *testing.T) {
	path := setupWorkspace(t,
		"use ./mod-a\nuse ./mod-b\n",
		map[string]string{
			"mod-a": "module example.com/a\ngo 1.23\nrequire example.com/dep v1.2.0\n",
			"mod-b": "module example.com/b\ngo 1.23\nrequire example.com/dep v1.3.0\n",
		})
	ws := loadOK(t, path)
	mf, errs := ws.Merge()
	if mf != nil {
		t.Fatal("conflicting merge must not produce a manifest")
	}
	we := wsErrKind(errs, model.WorkspaceRequirementConflict)
	if we == nil {
		t.Fatalf("expected requirement_conflict, got %v", errs)
	}
	t.Logf("CONFLICT module=%s expected=%s actual=%s sources=%v",
		we.Module, we.Expected, we.Actual, we.Sources)
	if we.Module != "example.com/dep" {
		t.Fatalf("conflict must name the dependency, got %q", we.Module)
	}
	joined := strings.Join(we.Sources, "\n")
	for _, want := range []string{"example.com/a", "example.com/b", "v1.2.0", "v1.3.0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("conflict sources must mention %q:\n%s", want, joined)
		}
	}
}

// TestReplaceConflictAndAgreement covers replace merging: identical rules
// deduplicate; diverging targets for the same old path are replace_conflict.
func TestReplaceConflictAndAgreement(t *testing.T) {
	// Agreement case: both submodules pin the same fork.
	path := setupWorkspace(t,
		"use ./mod-a\nuse ./mod-b\n",
		map[string]string{
			"mod-a": "module example.com/a\ngo 1.23\nrequire example.com/dep v1.0.0\nreplace example.com/dep => example.com/fork v1.1.0\n",
			"mod-b": "module example.com/b\ngo 1.23\nrequire example.com/dep v1.0.0\nreplace example.com/dep => example.com/fork v1.1.0\n",
		})
	mf := mergeOK(t, loadOK(t, path))
	if len(mf.Replaces) != 1 {
		t.Fatalf("identical replace rules must dedupe to one, got %d", len(mf.Replaces))
	}
	t.Logf("MERGED replace %+v", mf.Replaces[0])

	// Conflict case: different targets for the same old path.
	path2 := setupWorkspace(t,
		"use ./mod-a\nuse ./mod-b\n",
		map[string]string{
			"mod-a": "module example.com/a\ngo 1.23\nrequire example.com/dep v1.0.0\nreplace example.com/dep => example.com/fork v1.1.0\n",
			"mod-b": "module example.com/b\ngo 1.23\nrequire example.com/dep v1.0.0\nreplace example.com/dep => ../local/dep\n",
		})
	_, errs := loadOK(t, path2).Merge()
	we := wsErrKind(errs, model.WorkspaceReplaceConflict)
	if we == nil {
		t.Fatalf("expected replace_conflict, got %v", errs)
	}
	t.Logf("CONFLICT module=%s sources=%v", we.Module, we.Sources)
	joined := strings.Join(we.Sources, "\n")
	for _, want := range []string{"example.com/a", "example.com/b", "example.com/fork", "../local/dep"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("replace conflict sources must mention %q:\n%s", want, joined)
		}
	}
}

// TestExcludeUnion checks excludes are unioned and deduplicated.
func TestExcludeUnion(t *testing.T) {
	path := setupWorkspace(t,
		"use ./mod-a\nuse ./mod-b\n",
		map[string]string{
			"mod-a": "module example.com/a\ngo 1.23\nrequire example.com/dep v1.0.0\nexclude example.com/dep v1.1.0\n",
			"mod-b": "module example.com/b\ngo 1.23\nrequire example.com/dep v1.0.0\nexclude example.com/dep v1.1.0\nexclude example.com/dep v1.2.0\n",
		})
	mf := mergeOK(t, loadOK(t, path))
	if len(mf.Excludes) != 2 {
		t.Fatalf("excludes must union+dedupe to 2, got %v", mf.Excludes)
	}
	for _, ex := range mf.Excludes {
		t.Logf("MERGED exclude %s %s", ex.Path, ex.Version)
	}
}

// TestDuplicateModuleDetected ensures two entries with the same module path
// are rejected instead of silently merged.
func TestDuplicateModuleDetected(t *testing.T) {
	path := setupWorkspace(t,
		"use ./mod-a\nuse ./mod-b\n",
		map[string]string{
			"mod-a": "module example.com/same\ngo 1.23\n",
			"mod-b": "module example.com/same\ngo 1.23\n",
		})
	_, errs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	we := wsErrKind(errs, model.WorkspaceDuplicateModule)
	if we == nil {
		t.Fatalf("expected duplicate_module, got %v", errs)
	}
	t.Logf("DUPLICATE module=%s sources=%v", we.Module, we.Sources)
}

// TestUnreadableSubmodule ensures a use entry without a parseable go.mod is
// reported as module_unreadable naming the directory.
func TestUnreadableSubmodule(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, root+"/golock.work", "use ./missing\n")
	_, errs, err := Load(root + "/golock.work")
	if err != nil {
		t.Fatal(err)
	}
	we := wsErrKind(errs, model.WorkspaceModuleUnreadable)
	if we == nil {
		t.Fatalf("expected module_unreadable, got %v", errs)
	}
	if we.Module != "./missing" {
		t.Fatalf("error must name the use directory, got %q", we.Module)
	}
	t.Logf("UNREADABLE %s: %s", we.Module, we.Detail)
}

// TestMergeDeterminismAndMembership: the same workspace merges to identical
// canonical declarations on repeated runs, and adding/removing a submodule
// is reflected exactly in the merged requirements.
func TestMergeDeterminismAndMembership(t *testing.T) {
	mods := map[string]string{
		"mod-a": "module example.com/a\ngo 1.23\nrequire example.com/dep v1.2.0\n",
		"mod-b": "module example.com/b\ngo 1.23\nrequire example.com/other v0.3.0\n",
	}
	work := "name example.com/ws\ngo 1.23\nuse (\n\t./mod-a\n\t./mod-b\n)\n"
	path := setupWorkspace(t, work, mods)

	first := mergeOK(t, loadOK(t, path))
	second := mergeOK(t, loadOK(t, path))
	h1, h2 := manifest.CanonicalHash(first), manifest.CanonicalHash(second)
	t.Logf("DETERMINISM hash1=%s hash2=%s", h1, h2)
	if h1 != h2 {
		t.Fatal("repeated merges of the same workspace must be identical")
	}

	// Remove mod-b: its unique requirement must disappear.
	root := strings.TrimSuffix(path, "/golock.work")
	testutil.WriteFile(t, root+"/golock.work", "name example.com/ws\ngo 1.23\nuse ./mod-a\n")
	withoutB := mergeOK(t, loadOK(t, root+"/golock.work"))
	for _, r := range withoutB.Requires {
		if r.Path == "example.com/other" {
			t.Fatal("removed submodule's requirement must disappear")
		}
		t.Logf("AFTER REMOVE require %s %s", r.Path, r.Version)
	}

	// Add it back: the requirement returns, nothing is duplicated.
	testutil.WriteFile(t, root+"/golock.work", work)
	restored := mergeOK(t, loadOK(t, root+"/golock.work"))
	if manifest.CanonicalHash(restored) != h1 {
		t.Fatal("re-adding the submodule must restore the original merge")
	}
	count := 0
	for _, r := range restored.Requires {
		if r.Path == "example.com/other" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("restored requirement must appear exactly once, got %d", count)
	}
	t.Logf("JUDGEMENT membership add/remove reflected exactly")
}

// TestLegalVersionForms ensures every legal version spelling (prerelease,
// build metadata, pseudo-version, +incompatible) survives the merge without
// being misjudged as invalid.
func TestLegalVersionForms(t *testing.T) {
	versions := []string{
		"v1.2.3",
		"v1.2.3-rc.1",
		"v1.2.3+build.5",
		"v0.0.0-20210101000000-abcdef123456",
		"v2.0.0+incompatible",
		"v1.2.3-rc.1+meta",
	}
	mods := map[string]string{}
	var uses []string
	for i, v := range versions {
		dir := "m" + string(rune('a'+i))
		uses = append(uses, "\t./"+dir)
		mods[dir] = "module example.com/m" + string(rune('a'+i)) + "\ngo 1.23\n" +
			"require example.com/dep" + string(rune('a'+i)) + " " + v + "\n"
	}
	work := "use (\n" + strings.Join(uses, "\n") + "\n)\n"
	t.Logf("WORKSPACE INPUT:\n%s", work)
	path := setupWorkspace(t, work, mods)
	mf := mergeOK(t, loadOK(t, path))
	if len(mf.Requires) != len(versions) {
		t.Fatalf("every legal version must merge, got %d of %d", len(mf.Requires), len(versions))
	}
	for _, r := range mf.Requires {
		t.Logf("MERGED require %s %s", r.Path, r.Version)
	}
}
