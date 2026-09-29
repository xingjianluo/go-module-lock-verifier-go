package resolver

import (
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

func optsFor(t *testing.T, c *testutil.Cache) Options {
	return Options{ModuleCache: c.Root, WorkDir: t.TempDir()}
}

func mod(name, goV string, reqs ...string) string {
	s := "module " + name + "\n\ngo " + goV + "\n"
	for i := 0; i+1 < len(reqs); i += 2 {
		s += "\nrequire " + reqs[i] + " " + reqs[i+1] + "\n"
	}
	return s
}

func find(ms []model.LockedModule, path string) (model.LockedModule, bool) {
	for _, m := range ms {
		if m.Path == path {
			return m, true
		}
	}
	return model.LockedModule{}, false
}

func TestResolveMVSAndTransitive(t *testing.T) {
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/a", Version: "v1.0.0",
		ModContent: mod("example.com/a", "1.23", "example.com/c", "v1.1.0"),
		Files:      map[string]string{"a.go": "package a\n"}})
	c.Add(t, testutil.Module{Path: "example.com/b", Version: "v2.0.0",
		ModContent: mod("example.com/b", "1.23", "example.com/c", "v1.3.0"),
		Files:      map[string]string{"b.go": "package b\n"}})
	c.Add(t, testutil.Module{Path: "example.com/c", Version: "v1.1.0",
		ModContent: mod("example.com/c", "1.23"), Files: map[string]string{"c.go": "// v1.1.0\n"}})
	c.Add(t, testutil.Module{Path: "example.com/c", Version: "v1.3.0",
		ModContent: mod("example.com/c", "1.23"), Files: map[string]string{"c.go": "// v1.3.0\n"}})

	mf, errs, _ := manifest.Parse([]byte(mod("example.com/main", "1.23",
		"example.com/a", "v1.0.0",
		"example.com/b", "v2.0.0")))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	modules, resErrs, err := Resolve(mf, optsFor(t, c))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("INPUT: a->c@v1.1.0, b->c@v1.3.0; JUDGEMENT ERRORS: %v", resErrs)
	if len(resErrs) != 0 {
		t.Fatalf("expected clean resolution, got %v", resErrs)
	}
	got, ok := find(modules, "example.com/c")
	if !ok || got.Version != "v1.3.0" {
		t.Fatalf("MVS must select v1.3.0, got %+v", got)
	}
	if !got.Indirect {
		t.Fatalf("c must be indirect")
	}
	intro := map[string]bool{}
	for _, s := range got.IntroducedBy {
		intro[s] = true
	}
	if !intro["example.com/a"] || !intro["example.com/b"] {
		t.Fatalf("IntroducedBy must trace both requesters, got %v", got.IntroducedBy)
	}
	t.Logf("JUDGEMENT: selected c=%s via=%v hash=%s", got.Version, got.IntroducedBy, got.Hash)
	if got.Hash == "" {
		t.Fatal("content hash should be present for cached zip")
	}
}

func TestResolveDeterministic(t *testing.T) {
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/a", Version: "v1.0.0",
		ModContent: mod("example.com/a", "1.23"), Files: map[string]string{"x.go": "// x\n"}})
	mf, _, _ := manifest.Parse([]byte(mod("example.com/main", "1.23", "example.com/a", "v1.0.0")))

	first, e1, _ := Resolve(mf, optsFor(t, c))
	second, e2, _ := Resolve(mf, optsFor(t, c))
	if len(e1) != 0 || len(e2) != 0 {
		t.Fatalf("unexpected errors %v %v", e1, e2)
	}
	if !sameModules(first, second) {
		t.Fatalf("repeated resolution differs:\n%+v\n%+v", first, second)
	}
	t.Log("JUDGEMENT: two offline runs produce identical versions and hashes")
}

func TestResolveMissingModule(t *testing.T) {
	c := testutil.NewCache(t) // intentionally empty
	mf, _, _ := manifest.Parse([]byte(mod("example.com/main", "1.23", "example.com/missing", "v1.0.0")))
	_, errs, err := Resolve(mf, optsFor(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(errs, model.VerifyMissingModule) {
		t.Fatalf("want missing_module, got %v", errs)
	}
	for _, e := range errs {
		t.Logf("JUDGEMENT: %v", e)
	}
}

func TestResolveVersionConflictByExclude(t *testing.T) {
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/a", Version: "v1.0.0",
		ModContent: mod("example.com/a", "1.23", "example.com/c", "v1.0.0"),
		Files:      map[string]string{"a.go": "package a\n"}})
	c.Add(t, testutil.Module{Path: "example.com/c", Version: "v1.0.0",
		ModContent: mod("example.com/c", "1.23"), Files: map[string]string{"c.go": "// c\n"}})
	in := mod("example.com/main", "1.23", "example.com/a", "v1.0.0") +
		"\nexclude example.com/c v1.0.0\n"
	t.Logf("INPUT:\n%s", in)
	mf, perr, _ := manifest.Parse([]byte(in))
	if len(perr) != 0 {
		t.Fatal(perr)
	}
	_, errs, err := Resolve(mf, optsFor(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(errs, model.VerifyVersionConflict) {
		t.Fatalf("want version_conflict, got %v", errs)
	}
	if hasKind(errs, model.VerifyMissingModule) {
		t.Fatalf("conflict must not be misreported as missing: %v", errs)
	}
	for _, e := range errs {
		ve := e.(*model.VerifyError)
		t.Logf("JUDGEMENT kind=%s module=%s by=%v expected=%s actual=%s",
			ve.Kind, ve.Module, ve.IntroducedBy, ve.Expected, ve.Actual)
	}
}

func TestResolveReplaceToVersionAndLocal(t *testing.T) {
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/fork", Version: "v9.0.0",
		ModContent: mod("example.com/fork", "1.23"), Files: map[string]string{"f.go": "// fork\n"}})
	dir := t.TempDir()
	testutil.WriteFile(t, dir+"/go.mod", mod("example.com/a", "1.23"))
	testutil.WriteFile(t, dir+"/a.go", "package a\n")

	in := "module example.com/main\n" +
		"go 1.23\n" +
		"require (\n" +
		"	example.com/a v1.0.0\n" +
		"	example.com/b v1.0.0\n" +
		")\n" +
		"replace example.com/a => example.com/fork v9.0.0\n" +
		"replace example.com/b => " + dir + "\n"
	t.Logf("INPUT:\n%s", in)
	mf, perr, _ := manifest.Parse([]byte(in))
	if len(perr) != 0 {
		t.Fatal(perr)
	}
	modules, errs, err := Resolve(mf, optsFor(t, c))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ERRORS: %v", errs)
	if len(errs) != 0 {
		t.Fatalf("replaced modules must resolve, got %v", errs)
	}
	a, _ := find(modules, "example.com/a")
	if a.Replacement == nil || a.Replacement.NewPath != "example.com/fork" || a.Hash == "" {
		t.Fatalf("version replacement not applied: %+v", a)
	}
	b, _ := find(modules, "example.com/b")
	if b.Replacement == nil || !b.Replacement.Local || b.Replacement.NewPath != dir {
		t.Fatalf("local replacement not recorded: %+v", b)
	}
	t.Logf("JUDGEMENT: a->fork(hash=%s) b->local(%s)", a.Hash, b.Replacement.NewPath)
}

func TestResolvePrereleaseOrdering(t *testing.T) {
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/p", Version: "v1.0.0-rc.2",
		ModContent: mod("example.com/p", "1.23")})
	c.Add(t, testutil.Module{Path: "example.com/p", Version: "v1.0.0",
		ModContent: mod("example.com/p", "1.23"), Files: map[string]string{"x.go": "// x\n"}})
	in := mod("example.com/main", "1.23", "example.com/p", "v1.0.0-rc.2")
	mf, _, _ := manifest.Parse([]byte(in))
	_, errs, _ := Resolve(mf, optsFor(t, c))
	// rc.2 exists but v1.0.0 content requirement is not directly requested;
	// selection is exactly rc.2 here, so resolution must be clean.
	if len(errs) != 0 {
		t.Fatalf("selecting the only requested prerelease must be clean: %v", errs)
	}
	if compareVersions("v1.0.0-rc.1", "v1.0.0-rc.2") >= 0 {
		t.Fatal("prerelease ordering wrong")
	}
	if compareVersions("v1.0.0-rc.2", "v1.0.0") >= 0 {
		t.Fatal("prerelease must sort below release")
	}
	if compareVersions("v0.0.0-20210101000000-abcdef123456", "v0.0.0-20220101000000-abcdef123456") >= 0 {
		t.Fatal("pseudo-version ordering wrong")
	}
	t.Log("JUDGEMENT: semver/prerelease/pseudo-version ordering correct")
}

func hasKind(errs []error, kind model.VerifyErrorKind) bool {
	for _, e := range errs {
		if ve, ok := e.(*model.VerifyError); ok && ve.Kind == kind {
			return true
		}
	}
	return false
}

func sameModules(a, b []model.LockedModule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Version != b[i].Version || a[i].Hash != b[i].Hash {
			return false
		}
	}
	return true
}
