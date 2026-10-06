package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/workspace"
)

// wsFixture builds a two-submodule workspace with a fabricated cache.
type wsFixture struct {
	t        *testing.T
	cache    *testutil.Cache
	dir      string
	workPath string
	lockPath string
}

func newWsFixture(t *testing.T) *wsFixture {
	t.Helper()
	c := testutil.NewCache(t)
	// Shared dependency used by both submodules; b depends on a.
	c.Add(t, testutil.Module{Path: "example.com/shared", Version: "v1.0.0",
		ModContent: "module example.com/shared\n\ngo 1.23\n",
		Files:      map[string]string{"s.go": "package shared\n"}})
	c.Add(t, testutil.Module{Path: "example.com/onlya", Version: "v0.1.0",
		ModContent: "module example.com/onlya\n\ngo 1.23\n",
		Files:      map[string]string{"a.go": "package onlya\n"}})

	dir := t.TempDir()
	f := &wsFixture{
		t: t, cache: c, dir: dir,
		workPath: filepath.Join(dir, "golock.work"),
		lockPath: filepath.Join(dir, "go.lock"),
	}
	testutil.WriteFile(t, filepath.Join(dir, "mod-a", "go.mod"),
		"module example.com/mod-a\ngo 1.23\n"+
			"require example.com/shared v1.0.0\n"+
			"require example.com/onlya v0.1.0\n")
	testutil.WriteFile(t, filepath.Join(dir, "mod-b", "go.mod"),
		"module example.com/mod-b\ngo 1.23\n"+
			"require example.com/shared v1.0.0\n")
	testutil.WriteFile(t, f.workPath,
		"name example.com/ws\ngo 1.23\nuse (\n\t./mod-a\n\t./mod-b\n)\n")
	return f
}

func (f *wsFixture) merged(t *testing.T) *model.Manifest {
	t.Helper()
	ws, errs, err := workspace.Load(f.workPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) > 0 {
		t.Fatalf("workspace load: %v", errs)
	}
	mf, merrs := ws.Merge()
	if len(merrs) > 0 {
		t.Fatalf("workspace merge: %v", merrs)
	}
	return mf
}

func (f *wsFixture) lockNow(t *testing.T) {
	t.Helper()
	mf := f.merged(t)
	modules, resErrs, err := resolver.Resolve(mf, resolver.Options{
		ModuleCache: f.cache.Root, WorkDir: t.TempDir()})
	if err != nil || len(resErrs) > 0 {
		t.Fatalf("resolve: %v %v", err, resErrs)
	}
	data, err := lockfile.Marshal(lockfile.Generate(mf, modules))
	if err != nil {
		t.Fatal(err)
	}
	if err := lockfile.WriteAtomic(f.lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *wsFixture) audit(t *testing.T) (*Result, []error) {
	t.Helper()
	mf := f.merged(t)
	res, errs, err := RunManifest(mf, f.lockPath, resolver.Options{
		ModuleCache: f.cache.Root, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return res, errs
}

// TestWorkspaceAuditHappyPath locks a workspace and audits it: every module
// is attributed to the submodule(s) that introduced it.
func TestWorkspaceAuditHappyPath(t *testing.T) {
	f := newWsFixture(t)
	f.lockNow(t)

	res, errs := f.audit(t)
	if len(errs) != 0 {
		t.Fatalf("clean workspace audit must pass, got %v", errs)
	}
	byPath := map[string]model.DiffEntry{}
	for _, e := range res.Report.Entries {
		byPath[e.Path] = e
		t.Logf("ENTRY %s status=%s via=%v", e.Path, e.Status, e.IntroducedBy)
	}
	shared := byPath["example.com/shared"]
	if len(shared.IntroducedBy) != 2 {
		t.Fatalf("shared dep must name both submodules, got %v", shared.IntroducedBy)
	}
	onlya := byPath["example.com/onlya"]
	if len(onlya.IntroducedBy) != 1 || onlya.IntroducedBy[0] != "example.com/mod-a" {
		t.Fatalf("onlya must be attributed to mod-a, got %v", onlya.IntroducedBy)
	}
	t.Logf("JUDGEMENT attribution ok: shared=%v onlya=%v", shared.IntroducedBy, onlya.IntroducedBy)
}

// TestWorkspaceAuditClassifies verifies the audit distinguishes the three
// drift classes and never modifies the lock while doing so.
func TestWorkspaceAuditClassifies(t *testing.T) {
	f := newWsFixture(t)
	f.lockNow(t)
	clean, err := os.ReadFile(f.lockPath)
	if err != nil {
		t.Fatal(err)
	}

	assertReadOnly := func(label string) {
		t.Helper()
		after, err := os.ReadFile(f.lockPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(clean) {
			t.Fatalf("%s: audit modified the lock file", label)
		}
	}

	t.Run("declarations changed -> lock_stale", func(t *testing.T) {
		testutil.WriteFile(t, filepath.Join(f.dir, "mod-b", "go.mod"),
			"module example.com/mod-b\ngo 1.23\nrequire example.com/shared v1.0.0\nrequire example.com/onlya v0.1.0\n")
		defer testutil.WriteFile(t, filepath.Join(f.dir, "mod-b", "go.mod"),
			"module example.com/mod-b\ngo 1.23\nrequire example.com/shared v1.0.0\n")
		_, errs := f.audit(t)
		if !lockKind(errs, model.LockStale) {
			t.Fatalf("declaration drift must be lock_stale, got %v", errs)
		}
		for _, e := range errs {
			if le, ok := e.(*model.LockError); ok {
				t.Logf("JUDGEMENT kind=%s module=%s expected=%s actual=%s",
					le.Kind, le.Module, le.Expected, le.Actual)
			}
		}
		assertReadOnly("lock_stale")
	})

	t.Run("lock edited externally -> lock_tampered", func(t *testing.T) {
		tampered := strings.Replace(string(clean), "example.com/onlya v0.1.0", "example.com/onlya v0.1.1", 1)
		if tampered == string(clean) {
			t.Fatal("tamper did not change lock content")
		}
		if err := os.WriteFile(f.lockPath, []byte(tampered), 0o644); err != nil {
			t.Fatal(err)
		}
		_, errs := f.audit(t)
		if !lockKind(errs, model.LockTampered) {
			t.Fatalf("edited lock must be lock_tampered, got %v", errs)
		}
		for _, e := range errs {
			if le, ok := e.(*model.LockError); ok {
				t.Logf("JUDGEMENT kind=%s expected=%s actual=%s", le.Kind, le.Expected, le.Actual)
			}
		}
		// restore
		if err := os.WriteFile(f.lockPath, clean, 0o644); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cache content drift -> hash_mismatch", func(t *testing.T) {
		f.cache.Add(t, testutil.Module{Path: "example.com/shared", Version: "v1.0.0",
			ModContent: "module example.com/shared\n\ngo 1.23\n",
			Files:      map[string]string{"s.go": "package shared // CHANGED\n"}})
		defer f.cache.Add(t, testutil.Module{Path: "example.com/shared", Version: "v1.0.0",
			ModContent: "module example.com/shared\n\ngo 1.23\n",
			Files:      map[string]string{"s.go": "package shared\n"}})
		_, errs := f.audit(t)
		if !verifyKind(errs, model.VerifyHashMismatch) {
			t.Fatalf("cache drift must be hash_mismatch, got %v", errs)
		}
		for _, e := range errs {
			if ve, ok := e.(*model.VerifyError); ok {
				t.Logf("JUDGEMENT kind=%s module=%s expected=%s actual=%s by=%v",
					ve.Kind, ve.Module, ve.Expected, ve.Actual, ve.IntroducedBy)
			}
		}
		assertReadOnly("hash_mismatch")
	})
}

// TestWorkspaceAuditConcurrentAndRepeatable runs many audits (against the
// same cache and lock) plus repeated locks concurrently, and requires every
// conclusion and every produced lock to be byte-identical.
func TestWorkspaceAuditConcurrentAndRepeatable(t *testing.T) {
	f := newWsFixture(t)
	f.lockNow(t)
	baseline, err := os.ReadFile(f.lockPath)
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	reports := make([]string, n)
	errCounts := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			mf := f.merged(t)
			res, errs, err := RunManifest(mf, f.lockPath, resolver.Options{
				ModuleCache: f.cache.Root, WorkDir: t.TempDir()})
			if err != nil {
				errCounts[i] = -1
				return
			}
			errCounts[i] = len(errs)
			var b strings.Builder
			for _, e := range res.Report.Entries {
				fmt.Fprintf(&b, "%s:%s:%s:%s\n", e.Path, e.Status, e.LockedVer, e.ActualVer)
			}
			reports[i] = b.String()
		}(i)
	}
	// Concurrent re-locks to the same path must each leave a whole, valid lock.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mf := f.merged(t)
			modules, _, err := resolver.Resolve(mf, resolver.Options{
				ModuleCache: f.cache.Root, WorkDir: t.TempDir()})
			if err != nil {
				return
			}
			data, err := lockfile.Marshal(lockfile.Generate(mf, modules))
			if err != nil {
				return
			}
			_ = lockfile.WriteAtomic(f.lockPath, data, 0o644)
		}()
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errCounts[i] != 0 {
			t.Fatalf("audit %d got %d errors", i, errCounts[i])
		}
		if reports[i] != reports[0] {
			t.Fatalf("audit %d report differs:\n%s\nvs\n%s", i, reports[i], reports[0])
		}
	}
	final, err := os.ReadFile(f.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(final) != string(baseline) {
		t.Fatal("concurrent identical locks must leave byte-identical lock")
	}
	if _, err := lockfile.Unmarshal(final); err != nil {
		t.Fatalf("final lock must parse cleanly: %v", err)
	}
	// No temporary files may be left behind.
	leftovers, err := filepath.Glob(filepath.Join(f.dir, ".golock-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) > 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
	t.Logf("JUDGEMENT %d concurrent audits identical; %d concurrent locks byte-identical; no temp files", n, n)
}
