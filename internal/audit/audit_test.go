package audit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

// fixture builds a project with a go.mod, a fabricated cache and a lock that
// matches both. It returns everything a test needs to mutate one aspect.
type fixture struct {
	dir      string
	modPath  string
	lockPath string
	cache    *testutil.Cache
	manifest string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/dep", Version: "v1.0.0",
		ModContent: "module example.com/dep\n\ngo 1.22\n",
		Files:      map[string]string{"dep.go": "package dep\n"}})
	c.Add(t, testutil.Module{Path: "example.com/dep", Version: "v1.1.0",
		ModContent: "module example.com/dep\n\ngo 1.22\n",
		Files:      map[string]string{"dep.go": "package dep // v1.1.0\n"}})

	f := &fixture{
		dir:      t.TempDir(),
		cache:    c,
		manifest: "module example.com/main\ngo 1.22\nrequire example.com/dep v1.0.0\n",
	}
	f.modPath = filepath.Join(f.dir, "go.mod")
	f.lockPath = filepath.Join(f.dir, "go.lock")
	testutil.WriteFile(t, f.modPath, f.manifest)
	f.regenLock(t)
	return f
}

// regenLock resolves and writes a fresh lock for the current go.mod.
func (f *fixture) regenLock(t *testing.T) {
	t.Helper()
	mf, rejects, err := manifest.ParseFile(f.modPath)
	if err != nil || len(rejects) > 0 {
		t.Fatalf("parse: %v %v", err, rejects)
	}
	mods, resErrs, err := resolver.Resolve(mf, resolver.Options{
		ModuleCache: f.cache.Root, WorkDir: t.TempDir(),
	})
	if err != nil || len(resErrs) > 0 {
		t.Fatalf("resolve: %v %v", err, resErrs)
	}
	data, err := lockfile.Marshal(lockfile.Generate(mf, mods))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(f.lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) opts() Options {
	return Options{
		ManifestPath: f.modPath,
		LockPath:     f.lockPath,
		Resolver:     resolver.Options{ModuleCache: f.cache.Root},
	}
}

func (f *fixture) run(t *testing.T) (*Result, []error) {
	t.Helper()
	o := f.opts()
	o.Resolver.WorkDir = t.TempDir()
	res, errs, err := Run(o)
	if err != nil {
		t.Fatalf("audit hard error: %v", err)
	}
	return res, errs
}

func TestAuditClean(t *testing.T) {
	f := newFixture(t)
	res, errs := f.run(t)
	t.Logf("clean audit: subject=%s findings=%d", res.Subject, len(errs))
	if len(errs) != 0 {
		t.Fatalf("clean audit reported: %v", errs)
	}
	if res.Lock == nil || res.Report == nil {
		t.Fatal("expected lock and report")
	}
}

func TestAuditClassifiesDeclarationDrift(t *testing.T) {
	f := newFixture(t)
	// Change declarations after locking: bump the requirement.
	testutil.WriteFile(t, f.modPath,
		"module example.com/main\ngo 1.22\nrequire example.com/dep v1.1.0\n")
	lockBefore, _ := os.ReadFile(f.lockPath)

	_, errs := f.run(t)
	var stale *model.LockError
	for _, e := range errs {
		if le, ok := e.(*model.LockError); ok && le.Kind == model.LockStale {
			stale = le
		}
		t.Logf("finding: %T %v", e, e)
	}
	if stale == nil {
		t.Fatalf("expected lock_stale, got %v", errs)
	}
	t.Logf("stale: expected=%s actual=%s", stale.Expected, stale.Actual)
	if stale.Expected == "" || stale.Actual == "" || stale.Expected == stale.Actual {
		t.Fatalf("stale must carry differing expected/actual: %+v", stale)
	}
	// Read-only guarantee: the lock file must be byte-identical afterwards.
	lockAfter, _ := os.ReadFile(f.lockPath)
	if string(lockBefore) != string(lockAfter) {
		t.Fatal("audit modified the lock file")
	}
}

func TestAuditClassifiesTamperedLock(t *testing.T) {
	f := newFixture(t)
	data, _ := os.ReadFile(f.lockPath)
	// Edit a locked version without fixing the integrity hash.
	tampered := []byte(string(data))
	for i := range tampered {
		if tampered[i] == 'v' && string(tampered[i:i+6]) == "v1.0.0" {
			tampered[i+5] = '9'
			break
		}
	}
	if err := os.WriteFile(f.lockPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	_, errs := f.run(t)
	if len(errs) != 1 {
		t.Fatalf("expected exactly one finding, got %v", errs)
	}
	le, ok := errs[0].(*model.LockError)
	if !ok || le.Kind != model.LockTampered {
		t.Fatalf("expected lock_tampered, got %v", errs)
	}
	t.Logf("tampered: expected=%s actual=%s", le.Expected, le.Actual)
	if le.Expected == "" || le.Actual == "" {
		t.Fatalf("tampered finding must carry integrity hashes: %+v", le)
	}
}

func TestAuditClassifiesCacheContentMismatch(t *testing.T) {
	f := newFixture(t)
	// Mutate cached content after locking: rewrite the extracted module dir.
	dir := filepath.Join(f.cache.Root, "example.com", "dep@v1.0.0", "dep.go")
	if err := os.WriteFile(dir, []byte("package dep // poisoned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Remove the zip so the directory hash is what gets compared.
	os.Remove(filepath.Join(f.cache.Root, "cache", "download", "example.com", "dep", "@v", "v1.0.0.zip"))

	_, errs := f.run(t)
	var mm *model.VerifyError
	for _, e := range errs {
		if ve, ok := e.(*model.VerifyError); ok && ve.Kind == model.VerifyHashMismatch {
			mm = ve
		}
		t.Logf("finding: %T %v", e, e)
	}
	if mm == nil {
		t.Fatalf("expected hash_mismatch, got %v", errs)
	}
	t.Logf("cache mismatch: module=%s expected=%s actual=%s introducedBy=%v",
		mm.Module, mm.Expected, mm.Actual, mm.IntroducedBy)
	if mm.Module != "example.com/dep" || mm.Expected == "" || mm.Actual == "" ||
		mm.Expected == mm.Actual {
		t.Fatalf("incomplete mismatch finding: %+v", mm)
	}
	if len(mm.IntroducedBy) == 0 {
		t.Fatal("mismatch must name who introduced the module")
	}
}

func TestAuditClassifiesMissingCacheModule(t *testing.T) {
	f := newFixture(t)
	// Delete all cached artifacts for the dependency.
	os.RemoveAll(filepath.Join(f.cache.Root, "cache", "download", "example.com", "dep"))
	os.RemoveAll(filepath.Join(f.cache.Root, "example.com", "dep@v1.0.0"))

	_, errs := f.run(t)
	var missing *model.VerifyError
	for _, e := range errs {
		if ve, ok := e.(*model.VerifyError); ok && ve.Kind == model.VerifyMissingModule {
			missing = ve
		}
		t.Logf("finding: %T %v", e, e)
	}
	if missing == nil {
		t.Fatalf("expected missing_module, got %v", errs)
	}
	if missing.Module != "example.com/dep" {
		t.Fatalf("missing module = %s", missing.Module)
	}
}

func TestAuditDeterministicAcrossRuns(t *testing.T) {
	f := newFixture(t)
	testutil.WriteFile(t, f.modPath,
		"module example.com/main\ngo 1.22\nrequire example.com/dep v1.1.0\n")
	_, errs1 := f.run(t)
	_, errs2 := f.run(t)
	if len(errs1) != len(errs2) {
		t.Fatalf("run1=%d findings run2=%d", len(errs1), len(errs2))
	}
	for i := range errs1 {
		if errs1[i].Error() != errs2[i].Error() {
			t.Fatalf("finding %d differs:\n%s\n%s", i, errs1[i], errs2[i])
		}
		t.Logf("finding[%d]: %v", i, errs1[i])
	}
}

func TestAuditWorkspaceConflictAbortsBeforeResolution(t *testing.T) {
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/dep", Version: "v1.0.0",
		ModContent: "module example.com/dep\n\ngo 1.22\n",
		Files:      map[string]string{"dep.go": "package dep\n"}})

	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "golock.work"), "use ./mod-a\nuse ./mod-b\n")
	testutil.WriteFile(t, filepath.Join(root, "mod-a", "go.mod"),
		"module example.com/a\ngo 1.22\nrequire example.com/dep v1.0.0\nreplace example.com/dep => example.com/fork v1.0.1\n")
	testutil.WriteFile(t, filepath.Join(root, "mod-b", "go.mod"),
		"module example.com/b\ngo 1.22\nrequire example.com/dep v1.0.0\nreplace example.com/dep => ../local\n")
	lockPath := filepath.Join(root, "go.lock")

	res, errs, err := Run(Options{
		WorkspacePath: filepath.Join(root, "golock.work"),
		LockPath:      lockPath,
		Resolver:      resolver.Options{ModuleCache: c.Root, WorkDir: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 conflict, got %v", errs)
	}
	we, ok := errs[0].(*model.WorkspaceError)
	if !ok || we.Kind != model.WorkspaceReplaceConflict {
		t.Fatalf("expected workspace_replace_conflict, got %v", errs)
	}
	t.Logf("workspace conflict: subject=%s entries=%d", we.Subject, len(we.Entries))
	if res.Subject == "" || len(res.Modules) != 2 {
		t.Fatalf("result must still identify the workspace: %+v", res)
	}
	// The conflict must not have produced a lock as a side effect.
	if _, serr := os.Stat(lockPath); !os.IsNotExist(serr) {
		t.Fatal("audit created a lock file")
	}
}
